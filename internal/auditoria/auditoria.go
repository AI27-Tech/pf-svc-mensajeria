// Package auditoria registra, de forma INMUTABLE, los hechos auditables de la
// conversación (Addendum §11, E3-T6): cada intento de reclamo (ganado y
// perdido), liberación, transferencia y marca de atendida, con su actor real.
//
// Append-only por construcción: este paquete solo tiene INSERT y SELECT, y el
// rol svc_mensajeria no tiene UPDATE ni DELETE sobre auditoria_mensajeria
// (0002_grants.sql). No hay ruta PATCH/DELETE de auditoría.
//
// Registrar escribe en la tx que le pasa el llamador, para que el hecho quede
// comprometido junto con el cambio que audita. Listar es la lectura de
// listarAuditoriaConversacion; la autorización (solo administrador, DEC-036) la
// aplica la frontera.
package auditoria

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	msgclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/mensajeria"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Códigos de máquina de las acciones auditadas. El catálogo no es contrato
// todavía (RegistroAuditoria.accion); estos son los que mensajeria emite.
const (
	AccionReclamoGanado        = "reclamo.intento_ganado"
	AccionReclamoPerdido       = "reclamo.intento_perdido"
	AccionReclamoLiberado      = "reclamo.liberado"
	AccionReclamoTransferido   = "reclamo.transferido"
	AccionConversacionAtendida = "conversacion.atendida"

	// El reclamo de emergencia no tiene conversación: va con conversacion_id NULL
	// y el caso en detalle.caso_emergencia_id (DEC-125).
	AccionEmergenciaReclamoGanado  = "emergencia.reclamo.intento_ganado"
	AccionEmergenciaReclamoPerdido = "emergencia.reclamo.intento_perdido"
)

// Ejecutor es lo mínimo para insertar: lo cumplen pgx.Tx y el pool.
type Ejecutor interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Registro es un hecho auditable. Detalle va a la columna jsonb; su clave
// "motivo", si existe, es la que listarAuditoriaConversacion expone como motivo.
// ConversacionID vacío se guarda NULL (hechos que no son de una conversación).
type Registro struct {
	TenantID       string
	ConversacionID string
	ActorReal      string
	Accion         string
	Detalle        map[string]any
}

// Registrar inserta el hecho con el ejecutor dado (normalmente la tx del cambio).
func Registrar(ctx context.Context, db Ejecutor, r Registro) error {
	_, err := db.Exec(ctx,
		`INSERT INTO mensajeria.auditoria_mensajeria (tenant_id, conversacion_id, actor_real, accion, detalle)
		 VALUES ($1, NULLIF($2, '')::uuid, $3, $4, $5)`,
		r.TenantID, r.ConversacionID, r.ActorReal, r.Accion, r.Detalle)
	if err != nil {
		return fmt.Errorf("auditoria: registrar %s: %w", r.Accion, err)
	}
	return nil
}

// ErrConversacionNoEncontrada: la conversación no existe para el tenant.
var ErrConversacionNoEncontrada = errors.New("auditoria: conversación no encontrada")

// ErrCursorInvalido: el cursor de paginación no es uno que Listar haya emitido.
var ErrCursorInvalido = errors.New("auditoria: cursor de paginación inválido")

// Consultor es la lectura que necesita Listar; la cumple el pool de chat.
type Consultor interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Servicio lee la auditoría sobre el pool de chat de svc_mensajeria.
type Servicio struct {
	db Consultor
}

// NewServicio construye el lector de auditoría.
func NewServicio(db Consultor) *Servicio {
	return &Servicio{db: db}
}

// tamanoPagina fija el tamaño de página de Listar (el contrato no lo parametriza).
const tamanoPagina = 50

// Listar devuelve una página de la auditoría de la conversación en orden de
// inserción. cursor vacío es la primera página; el cursor siguiente es opaco
// para el llamador (internamente, el último id interno devuelto).
func (s *Servicio) Listar(ctx context.Context, tenantID, conversacionID, cursor string) (*msgclient.PaginaAuditoria, error) {
	var desde int64
	if cursor != "" {
		v, err := strconv.ParseInt(cursor, 10, 64)
		if err != nil || v < 0 {
			return nil, ErrCursorInvalido
		}
		desde = v
	}

	var existe bool
	if err := s.db.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM mensajeria.conversacion WHERE id = $1 AND tenant_id = $2)`,
		conversacionID, tenantID,
	).Scan(&existe); err != nil {
		return nil, fmt.Errorf("auditoria: verificar conversación: %w", err)
	}
	if !existe {
		return nil, ErrConversacionNoEncontrada
	}

	rows, err := s.db.Query(ctx,
		`SELECT id, registro_id, conversacion_id, actor_real, accion, detalle->>'motivo', ocurrido_en
		   FROM mensajeria.auditoria_mensajeria
		  WHERE tenant_id = $1 AND conversacion_id = $2 AND id > $3
		  ORDER BY id
		  LIMIT $4`,
		tenantID, conversacionID, desde, tamanoPagina+1)
	if err != nil {
		return nil, fmt.Errorf("auditoria: listar: %w", err)
	}
	defer rows.Close()

	items := []msgclient.RegistroAuditoria{}
	var ids []int64
	for rows.Next() {
		var (
			id                       int64
			registroID, convID, acto string
			accion                   string
			motivo                   *string
			ocurrido                 time.Time
		)
		if err := rows.Scan(&id, &registroID, &convID, &acto, &accion, &motivo, &ocurrido); err != nil {
			return nil, fmt.Errorf("auditoria: escanear registro: %w", err)
		}
		ids = append(ids, id)
		items = append(items, msgclient.RegistroAuditoria{
			Id:             uuid.MustParse(registroID),
			ConversacionId: uuid.MustParse(convID),
			ActorReal:      uuid.MustParse(acto),
			Accion:         accion,
			Motivo:         motivo,
			OcurridoEn:     ocurrido,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("auditoria: filas de auditoría: %w", err)
	}

	// Se pidió una fila extra: si llegó, hay más páginas y se recorta.
	pagina := &msgclient.PaginaAuditoria{Items: items}
	if len(items) > tamanoPagina {
		pagina.Items = items[:tamanoPagina]
		siguiente := strconv.FormatInt(ids[tamanoPagina-1], 10)
		pagina.CursorSiguiente = &siguiente
	}
	return pagina, nil
}
