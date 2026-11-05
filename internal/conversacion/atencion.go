package conversacion

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/AI27-Tech/pf-pkg-platform-go/outbox"
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/auditoria"
	msgtipos "github.com/AI27-Tech/pf-svc-mensajeria/internal/tipos"
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/vinculo"
	"github.com/jackc/pgx/v5"
)

// Seis estados de entrega de un mensaje (E3-T6) y dónde vive cada uno:
//
//  1. aceptado: la fila de mensaje comprometida (Enviar).
//  2. disponible en bandeja: la ruta activa de la conversación (ruta_conversacion).
//  3. entregado internamente: algún interno con acceso avanzó su
//     cursor_participante.entregado_hasta (recibo entregado).
//  4. visto internamente: algún interno avanzó su cursor_participante.leido_hasta.
//     Es interno; el cliente no lo ve.
//  5. leído por AI27: conversacion.leido_ai27_hasta. SOLO lo avanza el recibo de
//     lectura del responsable activo (P23); una vista incidental no lo mueve.
//  6. atendido: estado_atendido, por la acción explícita MarcarAtendida (DEC-117).
//
// Leer la historia (ObtenerConversacion, ListarMensajes) no escribe ningún
// cursor: solo el recibo explícito lo hace.

// ErrNoEsResponsable lo devuelve MarcarAtendida cuando hay reclamo activo pero
// es de otro actor: solo el responsable marca atendida.
var ErrNoEsResponsable = errors.New("conversacion: el actor no es el responsable activo")

// MarcarAtendida marca la conversación como atendida por su responsable activo
// (DEC-117: acción explícita; enviar no la implica). Exige reclamo activo del
// actor en la versión esperada. Sin reclamo activo o con otra versión devuelve
// ErrSinReclamoActivo; con reclamo de otro actor, ErrNoEsResponsable. La marca,
// su auditoría y conversacion.atendida se comprometen en una sola tx.
func (s *Servicio) MarcarAtendida(ctx context.Context, tenantID, conversacionID, actorReal string, esperada msgtipos.VersionReclamo) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("conversacion: abrir transacción de atendida: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// FOR UPDATE: una transferencia o liberación concurrente espera a esta tx (o
	// esta a aquélla), así que no se marca atendida sobre un reclamo ya cerrado.
	var responsable string
	var version int64
	err = tx.QueryRow(ctx,
		`SELECT actor_real, version FROM mensajeria.reclamo
		  WHERE conversacion_id = $1 AND tenant_id = $2 AND cerrado_en IS NULL
		  FOR UPDATE`,
		conversacionID, tenantID,
	).Scan(&responsable, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("conversacion: atendida sin reclamo activo: %w", ErrSinReclamoActivo)
	}
	if err != nil {
		return fmt.Errorf("conversacion: leer reclamo para atendida: %w", err)
	}
	// Versión antes que responsable, como Liberar y Transferir: una vista obsoleta
	// recibe 409 y recarga; con la versión vigente, otro actor recibe 403.
	if version != int64(esperada) {
		return fmt.Errorf("conversacion: atendida con versión %d, vigente %d: %w", esperada, version, ErrSinReclamoActivo)
	}
	if responsable != actorReal {
		return ErrNoEsResponsable
	}

	var atendidaEn time.Time
	err = tx.QueryRow(ctx,
		`INSERT INTO mensajeria.estado_atendido (conversacion_id, tenant_id, atendida, atendida_por, atendida_en)
		 VALUES ($1, $2, true, $3, now())
		 ON CONFLICT (conversacion_id) DO UPDATE
		   SET atendida = true, atendida_por = EXCLUDED.atendida_por, atendida_en = EXCLUDED.atendida_en
		 RETURNING atendida_en`,
		conversacionID, tenantID, actorReal,
	).Scan(&atendidaEn)
	if err != nil {
		return fmt.Errorf("conversacion: marcar atendida: %w", err)
	}
	if err := vinculo.Actividad(ctx, tx, actorReal, s.Ventana); err != nil {
		return fmt.Errorf("conversacion: %w", err)
	}

	if err := auditoria.Registrar(ctx, tx, auditoria.Registro{
		TenantID: tenantID, ConversacionID: conversacionID, ActorReal: actorReal,
		Accion:  auditoria.AccionConversacionAtendida,
		Detalle: map[string]any{"version_reclamo": version},
	}); err != nil {
		return err
	}

	if err := s.pub.Publish(ctx, tx, outbox.Event{
		Tipo:         "conversacion.atendida",
		AgregadoID:   conversacionID,
		Sensibilidad: outbox.SensibilidadRestringida,
		Payload: map[string]any{
			"conversacion_id": conversacionID,
			"responsable_id":  actorReal,
			"atendida_en":     atendidaEn,
		},
	}); err != nil {
		return fmt.Errorf("conversacion: publicar conversacion.atendida: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("conversacion: commit de atendida: %w", err)
	}
	return nil
}

// Recibo es el tipo de recibo que avanza un cursor del participante.
type Recibo string

const (
	// ReciboEntregado avanza entregado_hasta (contrato registrarReciboEntregado).
	ReciboEntregado Recibo = "entregado"
	// ReciboLeido avanza leido_hasta y, si lo envía el responsable activo, el
	// "leído por AI27" visible al cliente (contrato registrarReciboLeido).
	ReciboLeido Recibo = "leido"
)

// RegistrarRecibo avanza el cursor del sujeto hasta `hasta` (nunca retrocede).
// `hasta` se recorta a la última secuencia de la conversación: no se acusa un
// mensaje que no existe. Leer implica entregado, así que un recibo de lectura
// también sube entregado_hasta. Si el sujeto es el responsable activo, el recibo
// de lectura avanza además conversacion.leido_ai27_hasta (P23). Publica
// recibo.entregado o recibo.leido solo si el cursor personal avanzó.
// ErrConversacionNoEncontrada si la conversación no existe para el tenant.
func (s *Servicio) RegistrarRecibo(ctx context.Context, tenantID, conversacionID, sujetoID string, tipo Recibo, hasta msgtipos.SecuenciaMensaje) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("conversacion: abrir transacción de recibo: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var ultima int64
	err = tx.QueryRow(ctx,
		`SELECT siguiente_secuencia FROM mensajeria.conversacion WHERE id = $1 AND tenant_id = $2`,
		conversacionID, tenantID,
	).Scan(&ultima)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrConversacionNoEncontrada
	}
	if err != nil {
		return fmt.Errorf("conversacion: leer última secuencia para recibo: %w", err)
	}
	h := min(int64(hasta), ultima)
	if h <= 0 {
		return nil // nada que acusar
	}

	// Columna que este recibo avanza; la otra se pasa en 0 y GREATEST la conserva.
	entregado, leido, avanza := h, int64(0), "EXCLUDED.entregado_hasta > c.entregado_hasta"
	if tipo == ReciboLeido {
		leido, avanza = h, "EXCLUDED.leido_hasta > c.leido_hasta"
	}
	var avanzo bool
	err = tx.QueryRow(ctx,
		`INSERT INTO mensajeria.cursor_participante AS c
		        (conversacion_id, participante_id, tenant_id, entregado_hasta, leido_hasta)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (conversacion_id, participante_id) DO UPDATE
		   SET entregado_hasta = GREATEST(c.entregado_hasta, EXCLUDED.entregado_hasta),
		       leido_hasta     = GREATEST(c.leido_hasta, EXCLUDED.leido_hasta),
		       actualizado_en  = now()
		 WHERE `+avanza+`
		 RETURNING true`,
		conversacionID, sujetoID, tenantID, entregado, leido,
	).Scan(&avanzo)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("conversacion: avanzar cursor de %s: %w", tipo, err)
	}
	// Un recibo es actividad del Ejecutivo (DEC-128 #3); para un cliente es no-op.
	if err := vinculo.Actividad(ctx, tx, sujetoID, s.Ventana); err != nil {
		return fmt.Errorf("conversacion: %w", err)
	}

	// "Leído por AI27" (P23): solo si el sujeto es el responsable activo. Va aparte
	// del cursor personal: quien hoy es responsable pudo leer antes como miembro
	// no responsable, y esa vista no contó; su recibo de ahora sí cuenta.
	if tipo == ReciboLeido {
		if _, err := tx.Exec(ctx,
			`UPDATE mensajeria.conversacion c
			    SET leido_ai27_hasta = $3
			  WHERE c.id = $1 AND c.tenant_id = $2 AND c.leido_ai27_hasta < $3
			    AND EXISTS (SELECT 1 FROM mensajeria.reclamo r
			                 WHERE r.conversacion_id = c.id AND r.tenant_id = c.tenant_id
			                   AND r.cerrado_en IS NULL AND r.actor_real = $4)`,
			conversacionID, tenantID, h, sujetoID,
		); err != nil {
			return fmt.Errorf("conversacion: avanzar leído por AI27: %w", err)
		}
	}

	if avanzo {
		if err := s.pub.Publish(ctx, tx, outbox.Event{
			Tipo:         "recibo." + string(tipo),
			AgregadoID:   conversacionID,
			Sensibilidad: outbox.SensibilidadRestringida,
			Payload: map[string]any{
				"conversacion_id": conversacionID,
				"sujeto_id":       sujetoID,
				"hasta_secuencia": h,
			},
		}); err != nil {
			return fmt.Errorf("conversacion: publicar recibo.%s: %w", tipo, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("conversacion: commit del recibo: %w", err)
	}
	return nil
}
