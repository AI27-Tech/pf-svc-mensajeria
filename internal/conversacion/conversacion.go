// Package conversacion es dueño del ciclo de vida de la conversación de negocio,
// de la secuencia canónica de sus mensajes, de los cursores de dominio, del
// estado "atendida" y de escritura_hasta (mensajeria.md (a), (e)). El scaffold
// implementa el único camino caliente que el prompt 07 exige con lógica real:
// enviar un mensaje escribiendo `mensaje` y su fila de outbox en la MISMA
// transacción, con idempotencia durable por (conversacion_id, actor_real,
// client_message_id).
//
// Lo demás del ciclo de vida (abrir, enrutar, atender, cerrar, transferir) es
// del dominio y queda sin implementar en el scaffold.
//
// Pendientes de producto citados por mensajeria.md (l), sin resolver aquí:
// visibilidad de recibos (Addendum §8), ADR 12 (sin E2EE por defecto), LFPDPPP,
// modelo de amenazas y SLOs candidatos de MSG-REC §14.
package conversacion

import (
	"context"
	"errors"
	"fmt"
	"time"

	msgclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/mensajeria"
	"github.com/AI27-Tech/pf-pkg-platform-go/outbox"
	msgmensajeria "github.com/AI27-Tech/pf-svc-mensajeria/internal/tipos"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Begin es la capa mínima de acceso a Postgres que necesita el envío: abrir una
// transacción. El pool de svc_mensajeria (chat) la satisface. Interfaz pequeña
// declarada por el consumidor (golang-best-practices §6).
type Begin interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Publicador inserta el evento en la tabla outbox dentro de la MISMA tx del
// mensaje (P17). Lo satisface *outbox.Publisher de la plataforma.
type Publicador interface {
	Publish(ctx context.Context, tx pgx.Tx, events ...outbox.Event) error
}

// Servicio enruta el envío de mensajes. Usa el pool de chat de svc_mensajeria.
type Servicio struct {
	pool Begin
	pub  Publicador
}

// NewServicio construye el servicio de conversación sobre el pool de chat y el
// publicador de outbox.
func NewServicio(pool Begin, pub Publicador) *Servicio {
	return &Servicio{pool: pool, pub: pub}
}

// EntradaEnvio es lo mínimo que el caso de uso necesita para enviar. tenantID,
// conversacionID y actorReal acotan; clientMessageID es la clave de idempotencia
// generada por el cliente (contrato: EnviarMensajeEntrada.client_message_id).
type EntradaEnvio struct {
	TenantID        string
	ConversacionID  string
	ActorReal       string
	ClientMessageID string
	Cuerpo          string
	// AliasMostrado es la identidad comercial presentada al cliente (nombre del
	// cliente de la conversación, Opción 2; DEC-097). Inmutable: se persiste al
	// enviar (addendum §6). La resuelve la frontera (handler) desde catalogos; el
	// dominio no llama a catalogos en la ruta caliente.
	AliasMostrado string
	// RolAlEnviar es el rol del autor en el momento del envío, inmutable
	// (addendum §6). Vacío ⇒ NULL. La audiencia_autor del contrato se deriva de
	// este rol al leer, no se persiste (no está en el esquema ni es un campo
	// inmutable del addendum).
	RolAlEnviar string
}

// ResultadoEnvio es el mensaje tal como quedó: su id y su secuencia canónica.
type ResultadoEnvio struct {
	MensajeID string
	Secuencia msgmensajeria.SecuenciaMensaje
	// Idempotente es true si el envío ya existía (reintento con el mismo
	// client_message_id): se devuelve el original, no se crea un segundo.
	Idempotente bool
}

// Enviar escribe el mensaje y su outbox_evento en una sola transacción (P17,
// invariante "Mensaje + outbox en una sola transacción"). La secuencia se asigna
// incrementando conversacion.siguiente_secuencia bajo el bloqueo de fila de la
// conversación, que serializa envíos concurrentes SOLO dentro de esa
// conversación (MSG-REC §7.2). Un reintento con el mismo client_message_id
// devuelve el mensaje original gracias al índice único de idempotencia
// un_mensaje_por_clave_de_cliente, sin crear un segundo mensaje ni un segundo
// evento.
func (s *Servicio) Enviar(ctx context.Context, in EntradaEnvio) (ResultadoEnvio, error) {
	if in.ConversacionID == "" || in.ActorReal == "" || in.ClientMessageID == "" {
		return ResultadoEnvio{}, fmt.Errorf("conversacion: envío inválido: faltan conversacion_id, actor_real o client_message_id")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ResultadoEnvio{}, fmt.Errorf("conversacion: abrir transacción: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op tras commit

	// Idempotencia primero: si el mensaje de esta clave ya existe, devolver el
	// original. Es la lectura dentro de la tx para que compita correctamente con
	// envíos concurrentes de la misma clave (el índice único es el árbitro
	// durable; esto evita el trabajo cuando ya está).
	if res, ok, err := buscarPorClave(ctx, tx, in); err != nil {
		return ResultadoEnvio{}, err
	} else if ok {
		res.Idempotente = true
		return res, nil
	}

	// Asignar secuencia bajo el bloqueo de fila de la conversación.
	var seq int64
	err = tx.QueryRow(ctx,
		`UPDATE mensajeria.conversacion
		    SET siguiente_secuencia = siguiente_secuencia + 1
		  WHERE id = $1 AND tenant_id = $2
		  RETURNING siguiente_secuencia`,
		in.ConversacionID, in.TenantID,
	).Scan(&seq)
	if errors.Is(err, pgx.ErrNoRows) {
		return ResultadoEnvio{}, fmt.Errorf("conversacion: %s no existe para el tenant %s", in.ConversacionID, in.TenantID)
	}
	if err != nil {
		return ResultadoEnvio{}, fmt.Errorf("conversacion: asignar secuencia: %w", err)
	}

	var mensajeID string
	err = tx.QueryRow(ctx,
		`INSERT INTO mensajeria.mensaje
		    (conversacion_id, tenant_id, actor_real, client_message_id, secuencia, cuerpo, alias_mostrado, rol_al_enviar)
		  VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, ''), NULLIF($8, ''))
		  RETURNING id`,
		in.ConversacionID, in.TenantID, in.ActorReal, in.ClientMessageID, seq, in.Cuerpo, in.AliasMostrado, in.RolAlEnviar,
	).Scan(&mensajeID)
	if err != nil {
		// Carrera por la misma clave: el índice único la rechaza. El reintento
		// correcto es releer el original (otra transacción lo creó). No se crea
		// un segundo mensaje ni un segundo evento.
		if esViolacionUnica(err) {
			if res, ok, e := buscarPorClave(ctx, tx, in); e == nil && ok {
				res.Idempotente = true
				return res, nil
			}
		}
		return ResultadoEnvio{}, fmt.Errorf("conversacion: insertar mensaje: %w", err)
	}

	// mensaje.creado en la MISMA tx (contrato tracer.7; sensibilidad RESTRINGIDA).
	// La capa viva (audience_version, target_sujeto_ids) la agrega el productor
	// del sobre restringido; aquí el outbox lleva la capa de dominio (ADR 28).
	ev := outbox.Event{
		Tipo:         "mensaje.creado",
		AgregadoID:   in.ConversacionID, // clave de orden por conversación (ADR 29)
		Sensibilidad: outbox.SensibilidadRestringida,
		Payload: map[string]any{
			"conversacion_id": in.ConversacionID,
			"mensaje_id":      mensajeID,
			"secuencia":       seq,
			"actor_real":      in.ActorReal,
		},
	}
	if err := s.pub.Publish(ctx, tx, ev); err != nil {
		return ResultadoEnvio{}, fmt.Errorf("conversacion: publicar mensaje.creado en outbox: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return ResultadoEnvio{}, fmt.Errorf("conversacion: commit del envío: %w", err)
	}

	return ResultadoEnvio{
		MensajeID: mensajeID,
		Secuencia: msgmensajeria.SecuenciaMensaje(seq),
	}, nil
}

// buscarPorClave devuelve el mensaje ya existente para la clave de idempotencia,
// si lo hay.
func buscarPorClave(ctx context.Context, tx pgx.Tx, in EntradaEnvio) (ResultadoEnvio, bool, error) {
	var id string
	var seq int64
	err := tx.QueryRow(ctx,
		`SELECT id, secuencia FROM mensajeria.mensaje
		  WHERE conversacion_id = $1 AND actor_real = $2 AND client_message_id = $3`,
		in.ConversacionID, in.ActorReal, in.ClientMessageID,
	).Scan(&id, &seq)
	if errors.Is(err, pgx.ErrNoRows) {
		return ResultadoEnvio{}, false, nil
	}
	if err != nil {
		return ResultadoEnvio{}, false, fmt.Errorf("conversacion: buscar por clave de idempotencia: %w", err)
	}
	return ResultadoEnvio{MensajeID: id, Secuencia: msgmensajeria.SecuenciaMensaje(seq)}, true, nil
}

// esViolacionUnica detecta el error de violación de índice único de Postgres
// (SQLSTATE 23505) sin acoplarse al texto del mensaje.
func esViolacionUnica(err error) bool {
	var pgErr interface{ SQLState() string }
	if errors.As(err, &pgErr) {
		return pgErr.SQLState() == "23505"
	}
	return false
}

// ErrMensajeNoEncontrado lo devuelve MensajePorID cuando el mensaje no existe.
var ErrMensajeNoEncontrado = errors.New("conversacion: mensaje no encontrado")

// MensajePorID lee un mensaje y lo mapea al Mensaje del contrato (hidratación
// para el handler HTTP; Enviar solo devuelve id y secuencia). La audiencia_autor
// del contrato se DERIVA del rol_al_enviar (cliente ⇒ Cliente; resto ⇒ Interno):
// es un campo estructural, no se persiste (no está en el esquema ni es inmutable
// del addendum §6). alias_mostrado y rol_al_enviar salen de sus columnas.
func (s *Servicio) MensajePorID(ctx context.Context, tenantID, mensajeID string) (*msgclient.Mensaje, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("conversacion: abrir transacción de lectura: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		id, convID, actorReal, clientMsgID, cuerpo string
		alias, rol                                 *string
		secuencia                                  int64
		creadoEn                                   time.Time
	)
	err = tx.QueryRow(ctx,
		`SELECT id, conversacion_id, actor_real, client_message_id, secuencia, cuerpo,
		        alias_mostrado, rol_al_enviar, creado_en
		   FROM mensajeria.mensaje
		  WHERE id = $1 AND tenant_id = $2`,
		mensajeID, tenantID,
	).Scan(&id, &convID, &actorReal, &clientMsgID, &secuencia, &cuerpo, &alias, &rol, &creadoEn)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrMensajeNoEncontrado
	}
	if err != nil {
		return nil, fmt.Errorf("conversacion: leer mensaje: %w", err)
	}

	actor := uuid.MustParse(actorReal)
	m := &msgclient.Mensaje{
		Id:              uuid.MustParse(id),
		ConversacionId:  uuid.MustParse(convID),
		ClientMessageId: uuid.MustParse(clientMsgID),
		ActorReal:       &actor,
		Secuencia:       secuencia,
		Cuerpo:          cuerpo,
		CreadoEn:        creadoEn,
		AudienciaAutor:  audienciaDesdeRol(rol),
	}
	if alias != nil {
		m.AliasMostrado = *alias
	}
	if rol != nil {
		r := msgclient.Rol(*rol)
		m.RolAlEnviar = &r
	}
	return m, nil
}

// audienciaDesdeRol mapea el rol a la Audiencia de dominio (DEC-054): solo
// `cliente` es Cliente; los otros siete roles son Interno. Un rol nil (mensaje
// sin rol_al_enviar) se trata como Cliente: en V1 solo el cliente escribe sin
// rol interno registrado.
func audienciaDesdeRol(rol *string) msgclient.Audiencia {
	if rol != nil && *rol != "cliente" {
		return msgclient.Audiencia("Interno")
	}
	return msgclient.Audiencia("Cliente")
}

// ClienteDeConversacion devuelve el cliente_id de la conversación, o cadena
// vacía si la conversación no tiene cliente. Lo usa la frontera para resolver el
// alias comercial del cliente (Opción 2, DEC-097) sin que el dominio llame a
// catalogos. ErrConversacionNoEncontrada si la conversación no existe.
func (s *Servicio) ClienteDeConversacion(ctx context.Context, tenantID, conversacionID string) (string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("conversacion: abrir transacción de lectura: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var clienteID *string
	err = tx.QueryRow(ctx,
		`SELECT cliente_id FROM mensajeria.conversacion WHERE id = $1 AND tenant_id = $2`,
		conversacionID, tenantID,
	).Scan(&clienteID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrConversacionNoEncontrada
	}
	if err != nil {
		return "", fmt.Errorf("conversacion: leer cliente de conversacion: %w", err)
	}
	if clienteID == nil {
		return "", nil
	}
	return *clienteID, nil
}

// ErrConversacionNoEncontrada la devuelve ClienteDeConversacion cuando la
// conversación no existe para el tenant.
var ErrConversacionNoEncontrada = errors.New("conversacion: no encontrada")
