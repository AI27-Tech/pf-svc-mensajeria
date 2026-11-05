// Package bandejas es dueño de la cola operativa de trabajo de una bandeja
// compartida: la ruta activa por conversación y la revisión monotónica de la
// colección (RevisionBandeja). Sirve el bootstrap y el catch-up incremental que
// ADR 28 §3 exige para M3 (contrato tracer.14): el cliente obtiene el trabajo
// actual con su revisión, abre SSE, y pide con desde_revision los cambios
// posteriores mientras bufferiza los deltas vivos.
//
// La revisión vive en dos lugares complementarios: bandeja.revision es el
// contador monotónico de la colección; ruta_conversacion.revision estampa, en
// cada cambio, la revisión en que esa ruta entró, salió o se movió. El catch-up
// es entonces "rutas de la bandeja con revision > desde", incluidas las
// inactivas (bajas), para que el cliente retire lo que salió de la colección.
//
// El ciclo de vida de la conversación (conversacion.estado) es separado del
// estado operativo de cola (ruta_conversacion): dos cosas distintas (Addendum §6).
package bandejas

import (
	"context"
	"errors"
	"fmt"
	"time"

	msgclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/mensajeria"
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/vinculo"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Consultor es la lectura mínima que el bootstrap/catch-up necesita. El pool de
// chat de svc_mensajeria la satisface. Interfaz del consumidor (golang-best-practices §6).
type Consultor interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Servicio sirve el trabajo de una bandeja desde la proyección local.
type Servicio struct {
	db Consultor
	// Ventana es N, la disponibilidad del dueño del cliente: decide si una
	// conversación sigue dirigida a él (DEC-128). Cero vale
	// vinculo.VentanaPorDefecto.
	Ventana vinculo.Ventana
}

// NewServicio construye el servicio de bandejas sobre el pool de chat.
func NewServicio(db Consultor) *Servicio {
	return &Servicio{db: db}
}

// ErrBandejaNoEncontrada la devuelve el bootstrap/catch-up cuando la bandeja no
// existe para el tenant. El llamador la mapea a no_encontrado.
var ErrBandejaNoEncontrada = errors.New("bandejas: bandeja no encontrada")

// sqlBandejaChat elige la bandeja única de conversaciones del tenant.
const sqlBandejaChat = `SELECT id FROM mensajeria.bandeja
	 WHERE tenant_id = $1 AND tipo = 'chat'
	 ORDER BY creado_en, id LIMIT 1`

// Ejecutor es lo que EnrutarABandejaUnica necesita de la transacción del
// llamador. pgx.Tx lo satisface.
type Ejecutor interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// EnrutarABandejaUnica enruta la conversación a la bandeja única de
// conversaciones de su tenant, dentro de la transacción del llamador (DEC-127
// #1: en la misma transacción que la crea). Crea la bandeja si el tenant no
// tiene. Avanza bandeja.revision y estampa esa revisión en la ruta nueva, para
// que el catch-up la reporte como alta (0003_ruta_revision).
//
// La revisión que estampa es la siguiente de TODAS las bandejas de chat, no solo
// la de su tenant: así la unión de bandejas que ve un interno (DEC-129) tiene una
// revisión coherente, la máxima, y su catch-up es "rutas con revisión mayor"
// (TrabajoChat, CambiosChatDesde). La revisión de cada bandeja sigue siendo
// monotónica, así que el catch-up por {id} no cambia.
//
// Un lock transaccional global serializa el enrutamiento: dos conversaciones
// nuevas no crean dos bandejas para un tenant ni toman la misma revisión.
func EnrutarABandejaUnica(ctx context.Context, tx Ejecutor, tenantID, conversacionID string) error {
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtext('mensajeria.bandeja_chat'))`,
	); err != nil {
		return fmt.Errorf("bandejas: bloquear bandejas de conversaciones: %w", err)
	}

	var bandejaID string
	err := tx.QueryRow(ctx, sqlBandejaChat, tenantID).Scan(&bandejaID)
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx,
			`INSERT INTO mensajeria.bandeja (tenant_id, tipo, nombre)
			 VALUES ($1, 'chat', $2) RETURNING id`,
			tenantID, NombreBandejaUnica,
		).Scan(&bandejaID)
	}
	if err != nil {
		return fmt.Errorf("bandejas: resolver o crear bandeja de conversaciones: %w", err)
	}

	if _, err := tx.Exec(ctx,
		`WITH rev AS (
		   UPDATE mensajeria.bandeja
		      SET revision = (SELECT max(revision) + 1 FROM mensajeria.bandeja WHERE tipo = 'chat')
		    WHERE id = $1 AND tenant_id = $2
		   RETURNING revision)
		 INSERT INTO mensajeria.ruta_conversacion (conversacion_id, tenant_id, bandeja_id, revision)
		 SELECT $3, $2, $1, revision FROM rev`,
		bandejaID, tenantID, conversacionID,
	); err != nil {
		return fmt.Errorf("bandejas: enrutar conversación: %w", err)
	}
	return nil
}

// NombreBandejaUnica es el nombre con que nace la bandeja única de un tenant.
// La migración 0008 usa el mismo literal.
const NombreBandejaUnica = "Conversaciones"

// Trabajo es el BOOTSTRAP de una bandeja (ADR 28 §2): las conversaciones con
// ruta ACTIVA en la bandeja y la revisión actual de la colección, que el cliente
// usa como desde_revision en el siguiente catch-up. El orden es por revisión de
// ruta (el orden en que entraron a la colección).
//
// lector es el Ejecutivo que lee: no ve las conversaciones dirigidas a otro
// Ejecutivo (DEC-128). Vacío para Supervisor y Coordinador, que ven todo
// (DEC-131). Igual en los cuatro métodos de lectura.
func (s *Servicio) Trabajo(ctx context.Context, tenantID, bandejaID, lector string) ([]msgclient.Conversacion, int64, error) {
	rev, err := s.revisionColeccion(ctx, tenantID, bandejaID)
	if err != nil {
		return nil, 0, err
	}
	items, err := s.consultar(ctx, lector,
		`r.bandeja_id = $1 AND r.tenant_id = $2 AND r.activa`, bandejaID, tenantID)
	if err != nil {
		return nil, 0, err
	}
	return items, rev, nil
}

// CambiosDesde es el CATCH-UP incremental (ADR 28 §3): las rutas de la bandeja
// con revisión mayor a `desde`, incluidas las INACTIVAS (bajas), para que el
// cliente aplique altas y retire bajas. Devuelve la revisión actual de la
// colección y fueraDeVentana si el catch-up ya no es posible.
//
// En V1, con Opción 1 (revisión por fila durable, sin purga), el catch-up
// siempre puede reconstruir desde cualquier revisión: fueraDeVentana es false.
// El 409 stream_resync_required del contrato queda como capacidad para cuando se
// introduzca una ventana de retención con purga (ADR 28 §5, ventana configurable).
func (s *Servicio) CambiosDesde(ctx context.Context, tenantID, bandejaID, lector string, desde int64) ([]msgclient.Conversacion, int64, bool, error) {
	rev, err := s.revisionColeccion(ctx, tenantID, bandejaID)
	if err != nil {
		return nil, 0, false, err
	}
	items, err := s.consultar(ctx, lector,
		`r.bandeja_id = $1 AND r.tenant_id = $2 AND r.revision > $3`, bandejaID, tenantID, desde)
	if err != nil {
		return nil, 0, false, err
	}
	return items, rev, false, nil
}

// TrabajoChat es el BOOTSTRAP de la bandeja de conversaciones de un interno
// (listarTrabajoMiBandeja, DEC-129): la unión de las bandejas de chat de todos los
// tenants, sin acotar por el tenant de quien pregunta. Su revisión es la máxima
// de esas bandejas; EnrutarABandejaUnica estampa cada alta por encima de ella,
// así que todo lo que cambie después del bootstrap tiene revisión mayor. La
// revisión se lee ANTES que las filas, como en Trabajo: una alta concurrente
// queda en las filas con una revisión que el catch-up vuelve a entregar, nunca al
// revés.
func (s *Servicio) TrabajoChat(ctx context.Context, lector string) ([]msgclient.Conversacion, int64, error) {
	rev, err := s.revisionChat(ctx)
	if err != nil {
		return nil, 0, err
	}
	items, err := s.consultar(ctx, lector, `b.tipo = 'chat' AND r.activa`)
	if err != nil {
		return nil, 0, err
	}
	return items, rev, nil
}

// CambiosChatDesde es el CATCH-UP de la unión de TrabajoChat: las rutas de
// cualquier bandeja de chat con revisión mayor a `desde`, incluidas las
// inactivas. Igual que CambiosDesde, sin purga no hay fuera de ventana.
//
// shortcut: cuando una dirección vence (el dueño pasa N sin actividad, DEC-128),
// la revisión no avanza, así que el catch-up de los demás Ejecutivos no la
// reporta; la ven en su siguiente bootstrap. Lo mismo cuando el dueño la
// reclama. Upgrade: si el panel necesita verla antes, avanzar la revisión de la
// ruta al vencer (exige un barrido) o al reclamarla.
func (s *Servicio) CambiosChatDesde(ctx context.Context, lector string, desde int64) ([]msgclient.Conversacion, int64, bool, error) {
	rev, err := s.revisionChat(ctx)
	if err != nil {
		return nil, 0, false, err
	}
	items, err := s.consultar(ctx, lector, `b.tipo = 'chat' AND r.revision > $1`, desde)
	if err != nil {
		return nil, 0, false, err
	}
	return items, rev, false, nil
}

// revisionChat es la revisión de la unión de bandejas de chat: la máxima (0 si
// no hay ninguna).
func (s *Servicio) revisionChat(ctx context.Context) (int64, error) {
	var rev int64
	if err := s.db.QueryRow(ctx,
		`SELECT COALESCE(max(revision), 0) FROM mensajeria.bandeja WHERE tipo = 'chat'`,
	).Scan(&rev); err != nil {
		return 0, fmt.Errorf("bandejas: leer revisión de las bandejas de conversaciones: %w", err)
	}
	return rev, nil
}

// revisionColeccion lee bandeja.revision, el cursor de dominio de la colección.
// Una bandeja inexistente para el tenant es no_encontrado (el llamador lo mapea).
func (s *Servicio) revisionColeccion(ctx context.Context, tenantID, bandejaID string) (int64, error) {
	var rev int64
	err := s.db.QueryRow(ctx,
		`SELECT revision FROM mensajeria.bandeja WHERE id = $1 AND tenant_id = $2`,
		bandejaID, tenantID,
	).Scan(&rev)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrBandejaNoEncontrada
	}
	if err != nil {
		return 0, fmt.Errorf("bandejas: leer revisión de colección: %w", err)
	}
	return rev, nil
}

// consultar lee las conversaciones con el filtro dado sobre la ruta (r), en
// orden de revisión. Une ruta_conversacion → conversacion y un LEFT JOIN al
// reclamo activo para version_reclamo (required en el contrato); sin reclamo
// activo, la versión más alta de la conversación (0 si nunca se reclamó), con la
// que se reclama desde el pool. El filtro y sus args los pone el llamador
// (bootstrap: activa; catch-up: revision > desde); consultar le agrega la
// visibilidad del lector (vinculo.CondicionVisible). b es la bandeja de la ruta.
func (s *Servicio) consultar(ctx context.Context, lector, filtro string, args ...any) ([]msgclient.Conversacion, error) {
	n := len(args)
	donde := `WHERE ` + filtro + ` AND ` +
		vinculo.CondicionVisible(fmt.Sprintf("$%d", n+1), fmt.Sprintf("$%d", n+2)) +
		` ORDER BY r.revision, r.id`
	args = append(args, lector, s.Ventana.Segundos())
	rows, err := s.db.Query(ctx,
		`SELECT c.id, c.viaje_id, c.cliente_id, c.estado, c.siguiente_secuencia,
		        c.escritura_hasta, c.creado_en, r.bandeja_id,
		        COALESCE(rec.version,
		                 (SELECT max(h.version) FROM mensajeria.reclamo h
		                   WHERE h.conversacion_id = c.id AND h.tenant_id = c.tenant_id),
		                 0) AS version_reclamo
		   FROM mensajeria.ruta_conversacion r
		   JOIN mensajeria.conversacion c
		     ON c.id = r.conversacion_id AND c.tenant_id = r.tenant_id
		   JOIN mensajeria.bandeja b
		     ON b.id = r.bandeja_id AND b.tenant_id = r.tenant_id
		   LEFT JOIN mensajeria.reclamo rec
		     ON rec.conversacion_id = c.id AND rec.tenant_id = c.tenant_id AND rec.cerrado_en IS NULL
		 `+donde,
		args...)
	if err != nil {
		return nil, fmt.Errorf("bandejas: consultar trabajo: %w", err)
	}
	defer rows.Close()

	var items []msgclient.Conversacion
	for rows.Next() {
		var (
			id, viajeID, clienteID, estado, bandejaCol string
			siguienteSeq, versionReclamo               int64
			escrituraHasta                             *time.Time
			creadoEn                                   time.Time
		)
		if err := rows.Scan(&id, &viajeID, &clienteID, &estado, &siguienteSeq,
			&escrituraHasta, &creadoEn, &bandejaCol, &versionReclamo); err != nil {
			return nil, fmt.Errorf("bandejas: escanear conversación: %w", err)
		}
		bid := uuid.MustParse(bandejaCol)
		conv := msgclient.Conversacion{
			Id:              uuid.MustParse(id),
			ViajeId:         uuid.MustParse(viajeID),
			ClienteId:       uuid.MustParse(clienteID),
			BandejaId:       &bid,
			Estado:          msgclient.EstadoConversacion(estado),
			UltimaSecuencia: siguienteSeq,
			VersionReclamo:  versionReclamo,
			AbiertaEn:       creadoEn,
		}
		if escrituraHasta != nil {
			conv.EscrituraHasta = escrituraHasta
		}
		items = append(items, conv)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("bandejas: iterar trabajo: %w", err)
	}
	return items, nil
}
