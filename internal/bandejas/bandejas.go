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
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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
}

// NewServicio construye el servicio de bandejas sobre el pool de chat.
func NewServicio(db Consultor) *Servicio {
	return &Servicio{db: db}
}

// ErrBandejaNoEncontrada la devuelve el bootstrap/catch-up cuando la bandeja no
// existe para el tenant. El llamador la mapea a no_encontrado.
var ErrBandejaNoEncontrada = errors.New("bandejas: bandeja no encontrada")

// ErrMembresiaAmbigua la devuelve ResolverMiBandeja cuando un sujeto tiene
// membresía activa en MÁS de una bandeja de chat. Con cola única e implícita
// (DEC-103) el invariante es exactamente una; más de una es un dato
// inconsistente que un administrador debe corregir, no una ambigüedad que el
// servidor pueda resolver eligiendo una en silencio.
var ErrMembresiaAmbigua = errors.New("bandejas: el sujeto tiene membresía activa en más de una bandeja de chat")

// ResolverMiBandeja devuelve el id de la bandeja de chat del sujeto, derivado de
// su membresía efectiva del pool (cola única e implícita, DEC-103): el llamante
// no nombra la bandeja. Filtra por tipo 'chat' para no devolver una bandeja de
// Reacción. Sin membresía activa en ninguna bandeja de chat -> ErrBandejaNoEncontrada
// (el llamador lo mapea a 404, como una bandeja inexistente). Más de una ->
// ErrMembresiaAmbigua (dato inconsistente; no se elige una en silencio).
func (s *Servicio) ResolverMiBandeja(ctx context.Context, tenantID, usuarioID string) (string, error) {
	rows, err := s.db.Query(ctx,
		`SELECT b.id
		   FROM mensajeria.pool_membresia m
		   JOIN mensajeria.bandeja b
		     ON b.id = m.bandeja_id AND b.tenant_id = m.tenant_id
		  WHERE m.usuario_id = $1 AND m.tenant_id = $2 AND m.activa
		    AND b.tipo = 'chat'`,
		usuarioID, tenantID)
	if err != nil {
		return "", fmt.Errorf("bandejas: resolver bandeja del sujeto: %w", err)
	}
	defer rows.Close()

	var bandejaID string
	n := 0
	for rows.Next() {
		n++
		if n > 1 {
			return "", ErrMembresiaAmbigua
		}
		if err := rows.Scan(&bandejaID); err != nil {
			return "", fmt.Errorf("bandejas: escanear bandeja del sujeto: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("bandejas: iterar membresía: %w", err)
	}
	if n == 0 {
		return "", ErrBandejaNoEncontrada
	}
	return bandejaID, nil
}

// Trabajo es el BOOTSTRAP de una bandeja (ADR 28 §2): las conversaciones con
// ruta ACTIVA en la bandeja y la revisión actual de la colección, que el cliente
// usa como desde_revision en el siguiente catch-up. El orden es por revisión de
// ruta (el orden en que entraron a la colección).
func (s *Servicio) Trabajo(ctx context.Context, tenantID, bandejaID string) ([]msgclient.Conversacion, int64, error) {
	rev, err := s.revisionColeccion(ctx, tenantID, bandejaID)
	if err != nil {
		return nil, 0, err
	}
	items, err := s.consultar(ctx, tenantID, bandejaID,
		`AND r.activa ORDER BY r.revision`)
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
func (s *Servicio) CambiosDesde(ctx context.Context, tenantID, bandejaID string, desde int64) ([]msgclient.Conversacion, int64, bool, error) {
	rev, err := s.revisionColeccion(ctx, tenantID, bandejaID)
	if err != nil {
		return nil, 0, false, err
	}
	items, err := s.consultar(ctx, tenantID, bandejaID,
		`AND r.revision > $3 ORDER BY r.revision`, desde)
	if err != nil {
		return nil, 0, false, err
	}
	return items, rev, false, nil
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

// consultar lee las conversaciones de la bandeja con el filtro dado sobre la
// ruta (r). Une ruta_conversacion → conversacion y un LEFT JOIN al reclamo
// activo para version_reclamo (required en el contrato); sin reclamo activo, la
// versión más alta de la conversación (0 si nunca se reclamó), con la que se
// reclama desde el pool. El filtro y sus args extra los pone el llamador
// (bootstrap: activa; catch-up: revision > desde).
func (s *Servicio) consultar(ctx context.Context, tenantID, bandejaID, filtro string, extraArgs ...any) ([]msgclient.Conversacion, error) {
	args := append([]any{bandejaID, tenantID}, extraArgs...)
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
		   LEFT JOIN mensajeria.reclamo rec
		     ON rec.conversacion_id = c.id AND rec.tenant_id = c.tenant_id AND rec.cerrado_en IS NULL
		  WHERE r.bandeja_id = $1 AND r.tenant_id = $2 `+filtro,
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
