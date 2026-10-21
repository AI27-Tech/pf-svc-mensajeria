// Package reclamos es dueño del reclamo que autoriza a responder una
// conversación: a lo más uno activo por conversación (índice único parcial
// un_reclamo_activo_por_conversacion), con concurrencia optimista por versión
// (Addendum §4 punto 5, mensajeria.md (e)). El scaffold implementa el camino que
// el prompt 07 exige con lógica real: reclamar con la versión esperada, de modo
// que dos reclamos concurrentes produzcan un ganador y un conflicto de versión
// para el perdedor.
//
// La transferencia (cerrar el reclamo anterior y abrir el siguiente en una sola
// transacción, mensajeria.md (e)) y el override auditado de Supervisor/
// Coordinador quedan declarados y sin implementar en el scaffold.
//
// Perder el reclamo NO es perder la membresía (Addendum §4 puntos 7 y 8): son
// dos efectos distintos. reclamos solo quita autoridad de envío; la membresía
// vive en internal/pool.
package reclamos

import (
	"context"
	"errors"
	"fmt"
	"time"

	msgclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/mensajeria"
	msgtipos "github.com/AI27-Tech/pf-svc-mensajeria/internal/tipos"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ErrConflictoVersion es el conflicto de concurrencia optimista: el perdedor de
// dos reclamos concurrentes lo recibe. El paquete raíz lo mapea a la taxonomía
// de la plataforma (409). Lleva las versiones para diagnóstico.
type ErrConflictoVersion struct {
	Esperada msgtipos.VersionReclamo
	Actual   msgtipos.VersionReclamo
}

func (e ErrConflictoVersion) Error() string {
	return fmt.Sprintf("reclamos: conflicto de versión: esperada %d, actual %d", e.Esperada, e.Actual)
}

// ErrSinReclamo lo devuelve ReclamoActivo cuando la conversación no tiene un
// reclamo activo. El llamador decide si eso es no_encontrado o un estado válido.
var ErrSinReclamo = errors.New("reclamos: sin reclamo activo")

// Begin abre una transacción. El pool de chat de svc_mensajeria la satisface.
type Begin interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Servicio gestiona reclamos sobre el pool de chat de svc_mensajeria.
type Servicio struct {
	pool Begin
}

// NewServicio construye el servicio de reclamos.
func NewServicio(pool Begin) *Servicio {
	return &Servicio{pool: pool}
}

// Reclamar toma el reclamo activo de una conversación exigiendo la versión
// esperada (concurrencia optimista). Devuelve la nueva versión si gana; si otro
// reclamo avanzó la versión primero, devuelve ErrConflictoVersion y NO modifica
// nada. La condición de carrera real la arbitra Postgres: el UPDATE condicional
// por (conversacion_id, version) solo afecta una fila para un valor de versión,
// así que de dos reclamos concurrentes con la misma versión esperada exactamente
// uno encuentra la fila y el otro no.
func (s *Servicio) Reclamar(ctx context.Context, tenantID, conversacionID, actorReal string, esperada msgtipos.VersionReclamo) (msgtipos.VersionReclamo, error) {
	if conversacionID == "" || actorReal == "" {
		return 0, fmt.Errorf("reclamos: reclamo inválido: faltan conversacion_id o actor_real")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("reclamos: abrir transacción: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Avanza la versión del reclamo activo solo si la versión esperada coincide.
	var nueva int64
	err = tx.QueryRow(ctx,
		`UPDATE mensajeria.reclamo
		    SET version = version + 1, actor_real = $3
		  WHERE conversacion_id = $1 AND tenant_id = $2 AND cerrado_en IS NULL
		    AND version = $4
		  RETURNING version`,
		conversacionID, tenantID, actorReal, int64(esperada),
	).Scan(&nueva)

	if errors.Is(err, pgx.ErrNoRows) {
		// O no hay reclamo activo con esa versión (lo tomó otro: conflicto), o no
		// hay reclamo activo todavía. Distinguir leyendo la versión actual.
		actual, hay, e := versionActiva(ctx, tx, tenantID, conversacionID)
		if e != nil {
			return 0, e
		}
		if hay {
			return 0, ErrConflictoVersion{Esperada: esperada, Actual: actual}
		}
		// No hay reclamo activo: abrir el primero exige esperada == 0.
		if esperada != 0 {
			return 0, ErrConflictoVersion{Esperada: esperada, Actual: 0}
		}
		if err := abrir(ctx, tx, tenantID, conversacionID, actorReal); err != nil {
			return 0, err
		}
		nueva = 1
	} else if err != nil {
		return 0, fmt.Errorf("reclamos: reclamar: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("reclamos: commit del reclamo: %w", err)
	}
	return msgtipos.VersionReclamo(nueva), nil
}

// abrir inserta el reclamo activo inicial. El índice único parcial
// un_reclamo_activo_por_conversacion garantiza a lo más uno activo: si dos
// abren a la vez, uno recibe violación de unicidad (lo trata el llamador como
// conflicto en una versión completa; en el scaffold se propaga).
func abrir(ctx context.Context, tx pgx.Tx, tenantID, conversacionID, actorReal string) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO mensajeria.reclamo (conversacion_id, tenant_id, actor_real, version)
		 VALUES ($1, $2, $3, 1)`,
		conversacionID, tenantID, actorReal)
	if err != nil {
		return fmt.Errorf("reclamos: abrir reclamo: %w", err)
	}
	return nil
}

// ReclamoActivo lee el reclamo activo de la conversación y lo mapea al Reclamo
// del contrato (hidratación para el handler HTTP; el camino caliente de Reclamar
// solo devuelve la versión). Devuelve ErrSinReclamo si no hay reclamo activo.
func (s *Servicio) ReclamoActivo(ctx context.Context, tenantID, conversacionID string) (*msgclient.Reclamo, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("reclamos: abrir transacción de lectura: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		id, convID, responsable string
		version                 int64
		abiertoEn               time.Time
		cerradoEn               *time.Time
	)
	err = tx.QueryRow(ctx,
		`SELECT id, conversacion_id, actor_real, version, abierto_en, cerrado_en
		   FROM mensajeria.reclamo
		  WHERE conversacion_id = $1 AND tenant_id = $2 AND cerrado_en IS NULL`,
		conversacionID, tenantID,
	).Scan(&id, &convID, &responsable, &version, &abiertoEn, &cerradoEn)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrSinReclamo
	}
	if err != nil {
		return nil, fmt.Errorf("reclamos: leer reclamo activo: %w", err)
	}

	return &msgclient.Reclamo{
		Id:             uuid.MustParse(id),
		ConversacionId: uuid.MustParse(convID),
		ResponsableId:  uuid.MustParse(responsable),
		Version:        version,
		AbiertoEn:      abiertoEn,
		CerradoEn:      cerradoEn,
	}, nil
}

// versionActiva devuelve la versión del reclamo activo de la conversación.
func versionActiva(ctx context.Context, tx pgx.Tx, tenantID, conversacionID string) (msgtipos.VersionReclamo, bool, error) {
	var v int64
	err := tx.QueryRow(ctx,
		`SELECT version FROM mensajeria.reclamo
		  WHERE conversacion_id = $1 AND tenant_id = $2 AND cerrado_en IS NULL`,
		conversacionID, tenantID,
	).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("reclamos: leer versión activa: %w", err)
	}
	return msgtipos.VersionReclamo(v), true, nil
}
