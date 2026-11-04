// Package reclamos es dueño del reclamo que autoriza a responder una
// conversación: a lo más uno activo por conversación (índice único parcial
// un_reclamo_activo_por_conversacion), con concurrencia optimista por versión
// (Addendum §4 punto 5, mensajeria.md (e)). Reclamar solo procede desde el pool
// (DEC-123), con la versión esperada, de modo que dos reclamos concurrentes
// produzcan un ganador y un conflicto de versión para el perdedor.
//
// La transferencia (cerrar el reclamo anterior y abrir el siguiente en una sola
// transacción, mensajeria.md (e)) está implementada; el override auditado de
// Supervisor/Coordinador sigue cerrado en V1 (DEC-107, DEC-118).
//
// Auditoría inmutable (E3-T6): cada intento de reclamo (ganado y perdido), cada
// liberación y cada transferencia escriben su fila en auditoria_mensajeria.
//
// Perder el reclamo NO es perder la membresía (Addendum §4 puntos 7 y 8): son
// dos efectos distintos. reclamos solo quita autoridad de envío; la membresía
// vive en internal/pool.
//
// Cada mutación publica su evento de dominio por outbox EN LA MISMA tx (E3-T3):
// conversacion.reclamada al abrir, conversacion.liberada al cerrar,
// conversacion.transferida al transferir. El contrato (tracer.21) los define con
// sensibilidad restringida, agregado conversacion_id y consumidor durable
// realtime (y reportes en transferida), que es el puente hacia E3-T5.
package reclamos

import (
	"context"
	"errors"
	"fmt"
	"time"

	msgclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/mensajeria"
	"github.com/AI27-Tech/pf-pkg-platform-go/outbox"
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/auditoria"
	msgtipos "github.com/AI27-Tech/pf-svc-mensajeria/internal/tipos"
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/vinculo"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ErrConflictoVersion es el conflicto de concurrencia optimista: el perdedor de
// dos reclamos concurrentes lo recibe. El paquete raíz lo mapea a la taxonomía
// de la plataforma (409). Lleva las versiones para diagnóstico.
//
// DirigidaA no vacío: la conversación sigue dirigida a otro Ejecutivo, su dueño
// (DEC-128), y solo él la reclama. Es conflicto como el reclamo de DEC-123: el
// panel recarga su bandeja, donde esa conversación ya no aparece.
type ErrConflictoVersion struct {
	Esperada  msgtipos.VersionReclamo
	Actual    msgtipos.VersionReclamo
	DirigidaA string
}

func (e ErrConflictoVersion) Error() string {
	if e.DirigidaA != "" {
		return fmt.Sprintf("reclamos: conversación dirigida a su dueño %s", e.DirigidaA)
	}
	return fmt.Sprintf("reclamos: conflicto de versión: esperada %d, actual %d", e.Esperada, e.Actual)
}

// ErrSinReclamo lo devuelve ReclamoActivo cuando la conversación no tiene un
// reclamo activo. El llamador decide si eso es no_encontrado o un estado válido.
var ErrSinReclamo = errors.New("reclamos: sin reclamo activo")

// ErrNoEsResponsable lo devuelven Liberar y Transferir cuando el actor no tiene
// el reclamo activo: solo su responsable lo libera o transfiere (DEC-118).
var ErrNoEsResponsable = errors.New("reclamos: el actor no es el responsable activo")

// Begin abre una transacción. El pool de chat de svc_mensajeria la satisface.
type Begin interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Publicador escribe un evento de dominio en el outbox dentro de la tx dada
// (P17). Lo satisface *outbox.Publisher de pf-pkg-platform-go. Interfaz mínima
// declarada por el consumidor (golang-best-practices §6).
type Publicador interface {
	Publish(ctx context.Context, tx pgx.Tx, events ...outbox.Event) error
}

// Servicio gestiona reclamos sobre el pool de chat de svc_mensajeria.
type Servicio struct {
	pool Begin
	pub  Publicador
	// Ventana es N, la disponibilidad del dueño del cliente (DEC-128). Cero vale
	// vinculo.VentanaPorDefecto.
	Ventana vinculo.Ventana
}

// NewServicio construye el servicio de reclamos.
func NewServicio(pool Begin, pub Publicador) *Servicio {
	return &Servicio{pool: pool, pub: pub}
}

// Reclamar toma del pool el reclamo de una conversación exigiendo la versión
// esperada (concurrencia optimista). Devuelve la nueva versión si gana. Si la
// conversación ya tiene reclamo activo (aunque la versión sea la vigente,
// DEC-123) o la versión no es la vigente, devuelve ErrConflictoVersion y NO
// modifica nada. La carrera real la arbitra Postgres: de dos reclamos
// concurrentes desde el pool, el índice único parcial deja abrir a uno solo.
//
// Vínculo pegajoso (DEC-128): una conversación dirigida a otro Ejecutivo, su
// dueño disponible, devuelve ErrConflictoVersion con DirigidaA. El reclamo
// ganado registra la actividad del reclamante y puede hacerlo dueño del cliente
// (vinculo.Tomar).
//
// Cada intento queda en auditoria_mensajeria (E3-T6): el ganado en la misma tx
// del reclamo, el perdido en una tx propia tras el conflicto.
func (s *Servicio) Reclamar(ctx context.Context, tenantID, conversacionID, actorReal string, esperada msgtipos.VersionReclamo) (msgtipos.VersionReclamo, error) {
	if conversacionID == "" || actorReal == "" {
		return 0, fmt.Errorf("reclamos: reclamo inválido: faltan conversacion_id o actor_real")
	}

	nueva, err := s.reclamar(ctx, tenantID, conversacionID, actorReal, esperada)
	var conflicto ErrConflictoVersion
	if errors.As(err, &conflicto) {
		// El intento perdido se audita en su PROPIA tx, después del conflicto: la tx
		// del intento ya se descartó (en la carrera del primer reclamo Postgres la
		// abortó con 23505 y no admite más sentencias), y el perdedor no debe
		// comprometer nada de su intento salvo el hecho de que lo intentó.
		if e := s.auditarIntentoPerdido(ctx, tenantID, conversacionID, actorReal, conflicto); e != nil {
			return 0, e
		}
	}
	return nueva, err
}

// auditarIntentoPerdido registra el reclamo perdido en una tx propia.
func (s *Servicio) auditarIntentoPerdido(ctx context.Context, tenantID, conversacionID, actorReal string, c ErrConflictoVersion) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("reclamos: abrir transacción de auditoría: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	detalle := map[string]any{"version_esperada": c.Esperada, "version_actual": c.Actual}
	if c.DirigidaA != "" {
		detalle["dirigida_a"] = c.DirigidaA
	}
	if err := auditoria.Registrar(ctx, tx, auditoria.Registro{
		TenantID: tenantID, ConversacionID: conversacionID, ActorReal: actorReal,
		Accion:  auditoria.AccionReclamoPerdido,
		Detalle: detalle,
	}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("reclamos: commit de la auditoría del intento perdido: %w", err)
	}
	return nil
}

// reclamar es el intento en su tx: avanza o abre el reclamo, audita el intento
// ganado y publica conversacion.reclamada, todo en la misma tx.
func (s *Servicio) reclamar(ctx context.Context, tenantID, conversacionID, actorReal string, esperada msgtipos.VersionReclamo) (msgtipos.VersionReclamo, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("reclamos: abrir transacción: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Solo se reclama del pool (DEC-123): con reclamo activo es conflicto aunque
	// la versión sea la vigente; cambiar de responsable es liberar o transferir
	// (DEC-118, DEC-119). Del pool se reclama con la versión vigente de la
	// conversación (0 si nunca se reclamó) y el reclamo nuevo la avanza: la
	// versión es monotónica por conversación, no por fila, así que una versión
	// previa a una liberación o transferencia nunca vuelve a ser vigente (P20).
	actual, hay, err := versionConversacion(ctx, tx, tenantID, conversacionID)
	if err != nil {
		return 0, err
	}
	if hay || esperada != actual {
		return 0, ErrConflictoVersion{Esperada: esperada, Actual: actual}
	}
	// Dirigida a su dueño disponible (DEC-128 #2): solo él la reclama.
	dueno, err := vinculo.DirigidaAOtro(ctx, tx, tenantID, conversacionID, actorReal, s.Ventana)
	if err != nil {
		return 0, err
	}
	if dueno != "" {
		return 0, ErrConflictoVersion{Esperada: esperada, Actual: actual, DirigidaA: dueno}
	}
	nueva := int64(actual) + 1
	reclamoID, err := abrir(ctx, tx, tenantID, conversacionID, actorReal, nueva)
	if esViolacionUnica(err) {
		// Carrera del reclamo desde el pool (P19): otro abrió el reclamo entre la
		// lectura y el INSERT, y el índice único parcial rechazó éste. El ganador
		// abrió en la misma versión nueva.
		return 0, ErrConflictoVersion{Esperada: esperada, Actual: msgtipos.VersionReclamo(nueva)}
	}
	if err != nil {
		return 0, err
	}

	// El reclamo es actividad del Ejecutivo y puede hacerlo dueño del cliente
	// (DEC-103 #2 y #3). El cambio de dueño queda en la auditoría del reclamo.
	toma, err := vinculo.Tomar(ctx, tx, tenantID, conversacionID, actorReal, s.Ventana)
	if err != nil {
		return 0, err
	}
	detalle := map[string]any{"reclamo_id": reclamoID, "version_esperada": esperada, "version_reclamo": nueva}
	if toma.Nuevo {
		detalle["nuevo_dueno"] = true
		if toma.Anterior != "" {
			detalle["dueno_anterior"] = toma.Anterior
		}
	}
	if err := auditoria.Registrar(ctx, tx, auditoria.Registro{
		TenantID: tenantID, ConversacionID: conversacionID, ActorReal: actorReal,
		Accion:  auditoria.AccionReclamoGanado,
		Detalle: detalle,
	}); err != nil {
		return 0, err
	}

	// Publica conversacion.reclamada en la MISMA tx (E3-T3): quién responde ahora
	// y en qué versión, que realtime debe mostrar.
	if err := s.pub.Publish(ctx, tx, outbox.Event{
		Tipo:         "conversacion.reclamada",
		AgregadoID:   conversacionID,
		Sensibilidad: outbox.SensibilidadRestringida,
		Payload: map[string]any{
			"conversacion_id": conversacionID,
			"reclamo_id":      reclamoID,
			"responsable_id":  actorReal,
			"version_reclamo": nueva,
		},
	}); err != nil {
		return 0, fmt.Errorf("reclamos: publicar conversacion.reclamada: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("reclamos: commit del reclamo: %w", err)
	}
	return msgtipos.VersionReclamo(nueva), nil
}

// abrir inserta el reclamo activo en la versión dada y devuelve su id. El índice
// único parcial un_reclamo_activo_por_conversacion garantiza a lo más uno
// activo: si dos abren a la vez, uno recibe violación de unicidad, que Reclamar
// traduce a conflicto de versión.
func abrir(ctx context.Context, tx pgx.Tx, tenantID, conversacionID, actorReal string, version int64) (string, error) {
	var id string
	err := tx.QueryRow(ctx,
		`INSERT INTO mensajeria.reclamo (conversacion_id, tenant_id, actor_real, version)
		 VALUES ($1, $2, $3, $4)
		 RETURNING id`,
		conversacionID, tenantID, actorReal, version).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("reclamos: abrir reclamo: %w", err)
	}
	return id, nil
}

// Liberar cierra el reclamo activo de la conversación y lo devuelve al pool
// (addendum §4 punto 6). Sin reclamo activo, ErrSinReclamo. Si la versión
// esperada no es la vigente o `rid` no es el reclamo activo, ErrConflictoVersion
// sin cerrar nada. Solo libera el responsable activo (DEC-118: ni un Supervisor
// libera una conversación ajena en V1): otro actor recibe ErrNoEsResponsable. La
// versión de la conversación no cambia: el siguiente reclamo desde el pool la
// avanza. Perder el reclamo NO quita la membresía (addendum §4 punto 7): esto
// solo cierra la autoridad de envío.
func (s *Servicio) Liberar(ctx context.Context, tenantID, conversacionID, reclamoID, actorReal string, esperada msgtipos.VersionReclamo) error {
	if conversacionID == "" || reclamoID == "" {
		return fmt.Errorf("reclamos: liberación inválida: faltan conversacion_id o reclamo_id")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("reclamos: abrir transacción: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	activo, err := tomarActivo(ctx, tx, tenantID, conversacionID, actorReal, esperada)
	if err != nil {
		return err
	}
	if activo != reclamoID {
		return ErrConflictoVersion{Esperada: esperada, Actual: esperada}
	}
	if _, err := tx.Exec(ctx, `UPDATE mensajeria.reclamo SET cerrado_en = now() WHERE id = $1`, activo); err != nil {
		return fmt.Errorf("reclamos: liberar: %w", err)
	}

	if err := auditoria.Registrar(ctx, tx, auditoria.Registro{
		TenantID: tenantID, ConversacionID: conversacionID, ActorReal: actorReal,
		Accion:  auditoria.AccionReclamoLiberado,
		Detalle: map[string]any{"reclamo_id": reclamoID, "version_reclamo": esperada},
	}); err != nil {
		return err
	}

	// Publica conversacion.liberada en la MISMA tx (E3-T3): la conversación
	// vuelve al pool. version_reclamo es la versión cerrada (la esperada).
	if err := s.pub.Publish(ctx, tx, outbox.Event{
		Tipo:         "conversacion.liberada",
		AgregadoID:   conversacionID,
		Sensibilidad: outbox.SensibilidadRestringida,
		Payload: map[string]any{
			"conversacion_id": conversacionID,
			"reclamo_id":      reclamoID,
			"version_reclamo": int64(esperada),
		},
	}); err != nil {
		return fmt.Errorf("reclamos: publicar conversacion.liberada: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("reclamos: commit de la liberación: %w", err)
	}
	return nil
}

// Transferir mueve la responsabilidad de una conversación sin perder historia
// (addendum §4 punto 6): cierra el reclamo activo del origen y abre uno nuevo
// para el destino en una sola transacción (addendum §6). Mismas guardas que
// Liberar: sin reclamo activo, ErrSinReclamo; versión no vigente,
// ErrConflictoVersion; origen que no es el responsable activo,
// ErrNoEsResponsable (DEC-118). El reclamo nuevo abre en la versión siguiente de
// la conversación, de modo que la previa queda obsoleta (P20). El motivo, si
// viene, se audita (hecho inmutable, addendum §11).
func (s *Servicio) Transferir(ctx context.Context, tenantID, conversacionID, origen, destino, motivo string, esperada msgtipos.VersionReclamo) error {
	if conversacionID == "" || destino == "" {
		return fmt.Errorf("reclamos: transferencia inválida: faltan conversacion_id o responsable_destino")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("reclamos: abrir transacción: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	anteriorID, err := tomarActivo(ctx, tx, tenantID, conversacionID, origen, esperada)
	if err != nil {
		return err
	}
	actual, _, err := versionConversacion(ctx, tx, tenantID, conversacionID)
	if err != nil {
		return err
	}
	nueva := int64(actual) + 1
	if _, err := tx.Exec(ctx, `UPDATE mensajeria.reclamo SET cerrado_en = now() WHERE id = $1`, anteriorID); err != nil {
		return fmt.Errorf("reclamos: cerrar reclamo en transferencia: %w", err)
	}

	// Abrir el reclamo del destino en la MISMA tx. El índice único parcial
	// garantiza a lo más uno activo: como acabamos de cerrar el anterior, este
	// INSERT no colisiona.
	nuevoID, err := abrir(ctx, tx, tenantID, conversacionID, destino, nueva)
	if err != nil {
		return err
	}

	// Auditar la transferencia (hecho inmutable, addendum §11). Sin motivo, la
	// clave no se escribe y la lectura lo entrega como null.
	detalle := map[string]any{"destino": destino, "reclamo_anterior_id": anteriorID, "reclamo_id": nuevoID}
	if motivo != "" {
		detalle["motivo"] = motivo
	}
	if err := auditoria.Registrar(ctx, tx, auditoria.Registro{
		TenantID: tenantID, ConversacionID: conversacionID, ActorReal: origen,
		Accion: auditoria.AccionReclamoTransferido, Detalle: detalle,
	}); err != nil {
		return err
	}

	// Publica conversacion.transferida en la MISMA tx (E3-T3): lleva el reclamo
	// cerrado y el abierto, con la versión nueva.
	if err := s.pub.Publish(ctx, tx, outbox.Event{
		Tipo:         "conversacion.transferida",
		AgregadoID:   conversacionID,
		Sensibilidad: outbox.SensibilidadRestringida,
		Payload: map[string]any{
			"conversacion_id":     conversacionID,
			"reclamo_anterior_id": anteriorID,
			"reclamo_id":          nuevoID,
			"responsable_id":      destino,
			"version_reclamo":     nueva,
		},
	}); err != nil {
		return fmt.Errorf("reclamos: publicar conversacion.transferida: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("reclamos: commit de la transferencia: %w", err)
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

// esViolacionUnica detecta la violación de índice único de Postgres (23505).
func esViolacionUnica(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// versionConversacion devuelve la versión vigente del reclamo de la
// conversación (la más alta de sus reclamos, activos o cerrados; 0 si nunca se
// reclamó, contrato VersionReclamo) y si hay reclamo activo.
func versionConversacion(ctx context.Context, tx pgx.Tx, tenantID, conversacionID string) (msgtipos.VersionReclamo, bool, error) {
	var v int64
	var hay bool
	err := tx.QueryRow(ctx,
		`SELECT COALESCE(max(version), 0), COALESCE(bool_or(cerrado_en IS NULL), false)
		   FROM mensajeria.reclamo
		  WHERE conversacion_id = $1 AND tenant_id = $2`,
		conversacionID, tenantID,
	).Scan(&v, &hay)
	if err != nil {
		return 0, false, fmt.Errorf("reclamos: leer versión de la conversación: %w", err)
	}
	return msgtipos.VersionReclamo(v), hay, nil
}

// tomarActivo bloquea el reclamo activo (FOR UPDATE: una liberación o
// transferencia concurrente espera a esta tx) y lo valida en este orden: que
// exista (ErrSinReclamo), que la versión esperada sea la vigente
// (ErrConflictoVersion: una vista obsoleta se recarga antes de decidir) y que el
// actor sea su responsable (ErrNoEsResponsable). Devuelve su id.
func tomarActivo(ctx context.Context, tx pgx.Tx, tenantID, conversacionID, actor string, esperada msgtipos.VersionReclamo) (string, error) {
	var id, responsable string
	var version int64
	err := tx.QueryRow(ctx,
		`SELECT id, actor_real, version FROM mensajeria.reclamo
		  WHERE conversacion_id = $1 AND tenant_id = $2 AND cerrado_en IS NULL
		  FOR UPDATE`,
		conversacionID, tenantID,
	).Scan(&id, &responsable, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrSinReclamo
	}
	if err != nil {
		return "", fmt.Errorf("reclamos: leer reclamo activo: %w", err)
	}
	if version != int64(esperada) {
		return "", ErrConflictoVersion{Esperada: esperada, Actual: msgtipos.VersionReclamo(version)}
	}
	if responsable != actor {
		return "", ErrNoEsResponsable
	}
	return id, nil
}
