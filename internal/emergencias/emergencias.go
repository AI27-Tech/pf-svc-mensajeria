// Package emergencias implementa el caso de emergencia V1 como caso de
// mensajeria en la bandeja de Reacción (ADR 21, DEC-027/028/034/099/100;
// contrato tracer.17). El activador es interno (DEC-045 #9) o el cliente; la
// atención la hace un miembro del pool de Reacción por reclamo. Las notas son
// append-only y unidireccionales; cada una se copia al histórico del viaje por
// el evento emergencia.nota_agregada, producido por el outbox en la misma tx
// (DEC-100 punto 1).
//
// Reutiliza la MECÁNICA de conversación/reclamo (idempotencia durable por clave,
// concurrencia optimista por versión) contra tablas propias del caso
// (emergencia, nota_emergencia, reclamo_emergencia): el agregado es el caso, no
// la conversación (DEC-100 punto 2).
package emergencias

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	msgclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/mensajeria"
	"github.com/AI27-Tech/pf-pkg-platform-go/outbox"
	msgtipos "github.com/AI27-Tech/pf-svc-mensajeria/internal/tipos"
)

// Begin abre una transacción. El pool de chat de svc_mensajeria la satisface.
type Begin interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Publicador inserta el evento en la tabla outbox dentro de la MISMA tx del
// cambio (P17). Lo satisface *outbox.Publisher de la plataforma.
type Publicador interface {
	Publish(ctx context.Context, tx pgx.Tx, events ...outbox.Event) error
}

// Servicio implementa el caso de emergencia sobre el pool de chat.
type Servicio struct {
	pool Begin
	pub  Publicador
}

// NewServicio construye el servicio de emergencia sobre el pool de chat y el
// publicador de outbox.
func NewServicio(pool Begin, pub Publicador) *Servicio {
	return &Servicio{pool: pool, pub: pub}
}

// ErrConflictoVersion la devuelven Reclamar/Liberar/Transferir/Resolver cuando la
// versión esperada del reclamo no coincide con la vigente. El llamador la mapea a
// 409 conflicto_version_reclamo.
type ErrConflictoVersion struct {
	Esperada msgtipos.VersionReclamo
	Actual   msgtipos.VersionReclamo
}

func (e ErrConflictoVersion) Error() string {
	return fmt.Sprintf("emergencias: conflicto de versión: esperada %d, actual %d", e.Esperada, e.Actual)
}

// ErrSinReclamo la devuelven las operaciones que exigen un reclamo activo cuando
// no lo hay. El llamador decide si es 404 o un estado válido.
var ErrSinReclamo = errors.New("emergencias: sin reclamo activo")

// ErrNoEncontrada la devuelven las operaciones sobre un caso inexistente para el
// tenant. El llamador la mapea a 404.
var ErrNoEncontrada = errors.New("emergencias: caso no encontrado")

// ErrCerrada la devuelve AgregarNota cuando el caso está cerrado: un caso cerrado
// no admite notas (ADR 21 §4). El llamador la mapea a 409 emergencia_cerrada.
var ErrCerrada = errors.New("emergencias: el caso está cerrado")

// ErrTransicionInvalida la devuelven Resolver/Cerrar cuando el caso no está en el
// estado de origen que la transición exige (ADR 21 §5; resuelta/cerrada son
// irreversibles). El llamador la mapea a 409.
var ErrTransicionInvalida = errors.New("emergencias: transición de estado inválida")

// EntradaActivacion es lo mínimo para activar un caso. ActivacionID es la clave
// de idempotencia generada por el llamante (contrato ActivarEmergenciaEntrada).
// Origen distingue cliente de operacion (ADR 21 §3). ClienteID puede ir vacío:
// mensajeria no siempre conoce el cliente del viaje (lo proyecta de viaje.*); se
// persiste NULL y la vista mínima del cliente se resuelve por el alcance del
// token. La bandeja de Reacción NO se recibe: la resuelve el dominio (es su dato).
type EntradaActivacion struct {
	TenantID     string
	ViajeID      string
	ClienteID    string
	ActorReal    string
	Origen       msgclient.OrigenActivacion
	ActivacionID string
}

// ResultadoActivacion es el caso tal como quedó: su id, y si la activación ya
// existía (reentrega idempotente de la misma ActivacionID).
type ResultadoActivacion struct {
	EmergenciaID string
	Idempotente  bool
}

// Activar crea un caso de emergencia, o devuelve el existente si se reentrega la
// misma ActivacionID (idempotente, ADR 21 §2). Escribe el caso y
// emergencia.activada en una sola tx. Una pulsación nueva con otra ActivacionID
// crea otro caso aunque ya exista uno abierto: no se deduplica por viaje.
func (s *Servicio) Activar(ctx context.Context, in EntradaActivacion) (ResultadoActivacion, error) {
	if in.ViajeID == "" || in.ActorReal == "" || in.ActivacionID == "" {
		return ResultadoActivacion{}, fmt.Errorf("emergencias: activación inválida: faltan campos obligatorios")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ResultadoActivacion{}, fmt.Errorf("emergencias: abrir transacción: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Idempotencia: si la activación ya existe, devolver el caso original.
	if id, ok, err := s.buscarPorActivacion(ctx, tx, in); err != nil {
		return ResultadoActivacion{}, err
	} else if ok {
		return ResultadoActivacion{EmergenciaID: id, Idempotente: true}, nil
	}

	// La bandeja de Reacción del tenant es dato de mensajeria: se resuelve aquí,
	// no la trae el llamante.
	bandejaID, err := bandejaReaccion(ctx, tx, in.TenantID)
	if err != nil {
		return ResultadoActivacion{}, err
	}

	var emergenciaID string
	err = tx.QueryRow(ctx,
		`INSERT INTO mensajeria.emergencia
		    (tenant_id, viaje_id, cliente_id, bandeja_id, origen, actor_real, activacion_id)
		  VALUES ($1, $2, NULLIF($3, '')::uuid, $4, $5, $6, $7)
		  RETURNING id`,
		in.TenantID, in.ViajeID, in.ClienteID, bandejaID, string(in.Origen), in.ActorReal, in.ActivacionID,
	).Scan(&emergenciaID)
	if err != nil {
		// Carrera por la misma activación: el índice único la rechaza; releer el
		// original en vez de crear un segundo caso.
		if esViolacionUnica(err) {
			if id, ok, e := s.buscarPorActivacion(ctx, tx, in); e == nil && ok {
				return ResultadoActivacion{EmergenciaID: id, Idempotente: true}, nil
			}
		}
		return ResultadoActivacion{}, fmt.Errorf("emergencias: insertar caso: %w", err)
	}

	ev := outbox.Event{
		Tipo:         "emergencia.activada",
		AgregadoID:   emergenciaID, // clave de orden por caso (ADR 29)
		Sensibilidad: outbox.SensibilidadRestringida,
		Payload: map[string]any{
			"caso_emergencia_id": emergenciaID,
			"viaje_id":           in.ViajeID,
			"cliente_id":         in.ClienteID,
			"origen":             string(in.Origen),
		},
	}
	if err := s.pub.Publish(ctx, tx, ev); err != nil {
		return ResultadoActivacion{}, fmt.Errorf("emergencias: publicar emergencia.activada: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return ResultadoActivacion{}, fmt.Errorf("emergencias: commit de la activación: %w", err)
	}
	return ResultadoActivacion{EmergenciaID: emergenciaID}, nil
}

// buscarPorActivacion devuelve el caso ya existente para la clave de idempotencia.
func (s *Servicio) buscarPorActivacion(ctx context.Context, tx pgx.Tx, in EntradaActivacion) (string, bool, error) {
	var id string
	err := tx.QueryRow(ctx,
		`SELECT id FROM mensajeria.emergencia
		  WHERE tenant_id = $1 AND viaje_id = $2 AND activacion_id = $3`,
		in.TenantID, in.ViajeID, in.ActivacionID,
	).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("emergencias: buscar por activación: %w", err)
	}
	return id, true, nil
}

// EntradaNota es lo mínimo para agregar una nota. EsResolucion la distingue como
// la nota que acompañó la resolución (ADR 21 §5).
type EntradaNota struct {
	TenantID       string
	EmergenciaID   string
	ActorReal      string
	AudienciaAutor string
	Cuerpo         string
	EsResolucion   bool
}

// AgregarNota escribe una nota append-only y emergencia.nota_agregada en una sola
// tx. El caso debe no estar cerrado (ErrCerrada). El cuerpo ya viene recortado y
// validado por la frontera (1..1000); aquí se rechaza vacío por defensa. Devuelve
// el id de la nota creada.
func (s *Servicio) AgregarNota(ctx context.Context, in EntradaNota) (string, error) {
	if in.EmergenciaID == "" || in.ActorReal == "" || in.Cuerpo == "" {
		return "", fmt.Errorf("emergencias: nota inválida: faltan emergencia_id, actor_real o cuerpo")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("emergencias: abrir transacción: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	estado, viajeID, err := leerEstadoYViaje(ctx, tx, in.TenantID, in.EmergenciaID)
	if err != nil {
		return "", err
	}
	if estado == string(msgclient.EstadoEmergenciaCerrada) {
		return "", ErrCerrada
	}

	var notaID string
	err = tx.QueryRow(ctx,
		`INSERT INTO mensajeria.nota_emergencia
		    (emergencia_id, tenant_id, audiencia_autor, actor_real, cuerpo, es_resolucion)
		  VALUES ($1, $2, $3, $4, $5, $6)
		  RETURNING id`,
		in.EmergenciaID, in.TenantID, in.AudienciaAutor, in.ActorReal, in.Cuerpo, in.EsResolucion,
	).Scan(&notaID)
	if err != nil {
		return "", fmt.Errorf("emergencias: insertar nota: %w", err)
	}

	ev := outbox.Event{
		Tipo:         "emergencia.nota_agregada",
		AgregadoID:   in.EmergenciaID,
		Sensibilidad: outbox.SensibilidadRestringida,
		Payload: map[string]any{
			"caso_emergencia_id": in.EmergenciaID,
			"nota_id":            notaID,
			"viaje_id":           viajeID,
			"audiencia_autor":    in.AudienciaAutor,
			"cuerpo":             in.Cuerpo,
			"es_resolucion":      in.EsResolucion,
		},
	}
	if err := s.pub.Publish(ctx, tx, ev); err != nil {
		return "", fmt.Errorf("emergencias: publicar emergencia.nota_agregada: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("emergencias: commit de la nota: %w", err)
	}
	return notaID, nil
}

// Reclamar toma el reclamo del caso (abierta → en_atencion) exigiendo la versión
// esperada. Avanza emergencia.version_reclamo y abre/continúa reclamo_emergencia
// en una tx, y emite emergencia.reclamada. Conflicto de versión → ErrConflictoVersion.
func (s *Servicio) Reclamar(ctx context.Context, tenantID, emergenciaID, actorReal string, esperada msgtipos.VersionReclamo) (msgtipos.VersionReclamo, error) {
	if emergenciaID == "" || actorReal == "" {
		return 0, fmt.Errorf("emergencias: reclamo inválido: faltan emergencia_id o actor_real")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("emergencias: abrir transacción: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Avanzar la versión del reclamo activo solo si la esperada coincide.
	var nueva int64
	err = tx.QueryRow(ctx,
		`UPDATE mensajeria.reclamo_emergencia
		    SET version = version + 1, actor_real = $3
		  WHERE emergencia_id = $1 AND tenant_id = $2 AND cerrado_en IS NULL
		    AND version = $4
		  RETURNING version`,
		emergenciaID, tenantID, actorReal, int64(esperada),
	).Scan(&nueva)

	if errors.Is(err, pgx.ErrNoRows) {
		actual, hay, e := versionActiva(ctx, tx, tenantID, emergenciaID)
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
		if err := abrirReclamo(ctx, tx, tenantID, emergenciaID, actorReal); err != nil {
			return 0, err
		}
		nueva = 1
	} else if err != nil {
		return 0, fmt.Errorf("emergencias: reclamar: %w", err)
	}

	// El reclamo pone el caso en atención (ADR 21 §5).
	if err := fijarEstado(ctx, tx, tenantID, emergenciaID, msgtipos.VersionReclamo(nueva),
		string(msgclient.EstadoEmergenciaEnAtencion)); err != nil {
		return 0, err
	}

	ev := outbox.Event{
		Tipo:         "emergencia.reclamada",
		AgregadoID:   emergenciaID,
		Sensibilidad: outbox.SensibilidadRestringida,
		Payload: map[string]any{
			"caso_emergencia_id": emergenciaID,
			"responsable_id":     actorReal,
			"version_reclamo":    nueva,
		},
	}
	if err := s.pub.Publish(ctx, tx, ev); err != nil {
		return 0, fmt.Errorf("emergencias: publicar emergencia.reclamada: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("emergencias: commit del reclamo: %w", err)
	}
	return msgtipos.VersionReclamo(nueva), nil
}

// Liberar cierra el reclamo activo y devuelve el caso al pool (vuelve a abierta),
// exigiendo rid y versión esperada. Conflicto → ErrConflictoVersion; sin reclamo
// activo con ese rid y versión → ErrSinReclamo.
func (s *Servicio) Liberar(ctx context.Context, tenantID, emergenciaID, reclamoID, actorReal string, esperada msgtipos.VersionReclamo) error {
	if emergenciaID == "" || reclamoID == "" {
		return fmt.Errorf("emergencias: liberación inválida: faltan emergencia_id o reclamo_id")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("emergencias: abrir transacción: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	ct, err := tx.Exec(ctx,
		`UPDATE mensajeria.reclamo_emergencia
		    SET cerrado_en = now()
		  WHERE id = $1 AND emergencia_id = $2 AND tenant_id = $3
		    AND cerrado_en IS NULL AND version = $4`,
		reclamoID, emergenciaID, tenantID, int64(esperada))
	if err != nil {
		return fmt.Errorf("emergencias: liberar: %w", err)
	}
	if ct.RowsAffected() == 0 {
		actual, hay, e := versionActiva(ctx, tx, tenantID, emergenciaID)
		if e != nil {
			return e
		}
		if hay {
			return ErrConflictoVersion{Esperada: esperada, Actual: actual}
		}
		return ErrSinReclamo
	}

	// Liberar devuelve el caso a la cola: vuelve a abierta.
	if err := fijarEstado(ctx, tx, tenantID, emergenciaID, esperada,
		string(msgclient.EstadoEmergenciaAbierta)); err != nil {
		return err
	}

	ev := outbox.Event{
		Tipo:         "emergencia.liberada",
		AgregadoID:   emergenciaID,
		Sensibilidad: outbox.SensibilidadRestringida,
		Payload: map[string]any{
			"caso_emergencia_id": emergenciaID,
			"reclamo_id":         reclamoID,
			"version_reclamo":    int64(esperada),
		},
	}
	if err := s.pub.Publish(ctx, tx, ev); err != nil {
		return fmt.Errorf("emergencias: publicar emergencia.liberada: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("emergencias: commit de la liberación: %w", err)
	}
	return nil
}

// Transferir mueve la responsabilidad del caso a otro miembro de Reacción: cierra
// el reclamo activo del origen y abre uno nuevo para el destino en una tx,
// conservando el estado del caso (ADR 21 §5). Conflicto → ErrConflictoVersion;
// sin reclamo activo → ErrSinReclamo.
func (s *Servicio) Transferir(ctx context.Context, tenantID, emergenciaID, origen, destino string, esperada msgtipos.VersionReclamo) error {
	if emergenciaID == "" || destino == "" {
		return fmt.Errorf("emergencias: transferencia inválida: faltan emergencia_id o destino")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("emergencias: abrir transacción: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	ct, err := tx.Exec(ctx,
		`UPDATE mensajeria.reclamo_emergencia
		    SET cerrado_en = now()
		  WHERE emergencia_id = $1 AND tenant_id = $2 AND cerrado_en IS NULL
		    AND version = $3`,
		emergenciaID, tenantID, int64(esperada))
	if err != nil {
		return fmt.Errorf("emergencias: cerrar reclamo en transferencia: %w", err)
	}
	if ct.RowsAffected() == 0 {
		actual, hay, e := versionActiva(ctx, tx, tenantID, emergenciaID)
		if e != nil {
			return e
		}
		if hay {
			return ErrConflictoVersion{Esperada: esperada, Actual: actual}
		}
		return ErrSinReclamo
	}

	var nuevaID string
	if err := tx.QueryRow(ctx,
		`INSERT INTO mensajeria.reclamo_emergencia (emergencia_id, tenant_id, actor_real, version)
		 VALUES ($1, $2, $3, 1) RETURNING id`,
		emergenciaID, tenantID, destino).Scan(&nuevaID); err != nil {
		return fmt.Errorf("emergencias: abrir reclamo destino: %w", err)
	}

	ev := outbox.Event{
		Tipo:         "emergencia.transferida",
		AgregadoID:   emergenciaID,
		Sensibilidad: outbox.SensibilidadRestringida,
		Payload: map[string]any{
			"caso_emergencia_id": emergenciaID,
			"reclamo_id":         nuevaID,
			"responsable_id":     destino,
		},
	}
	if err := s.pub.Publish(ctx, tx, ev); err != nil {
		return fmt.Errorf("emergencias: publicar emergencia.transferida: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("emergencias: commit de la transferencia: %w", err)
	}
	return nil
}

// Resolver pasa el caso de en_atencion a resuelta (irreversible, ADR 21 §5),
// exigiendo la versión esperada del reclamo y una nota de resolución (que el
// handler agrega aparte con AgregarNota es_resolucion=true). Conflicto →
// ErrConflictoVersion; si el caso no está en en_atencion → ErrTransicionInvalida.
func (s *Servicio) Resolver(ctx context.Context, tenantID, emergenciaID, actorReal string, esperada msgtipos.VersionReclamo) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("emergencias: abrir transacción: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	estado, _, err := leerEstadoYViaje(ctx, tx, tenantID, emergenciaID)
	if err != nil {
		return err
	}
	if estado != string(msgclient.EstadoEmergenciaEnAtencion) {
		return ErrTransicionInvalida
	}

	// El reclamo activo debe estar en la versión esperada (solo el responsable
	// vigente resuelve).
	actual, hay, err := versionActiva(ctx, tx, tenantID, emergenciaID)
	if err != nil {
		return err
	}
	if !hay {
		return ErrSinReclamo
	}
	if actual != esperada {
		return ErrConflictoVersion{Esperada: esperada, Actual: actual}
	}

	if err := fijarEstado(ctx, tx, tenantID, emergenciaID, esperada,
		string(msgclient.EstadoEmergenciaResuelta)); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE mensajeria.emergencia SET resuelta_en = now()
		  WHERE id = $1 AND tenant_id = $2`,
		emergenciaID, tenantID); err != nil {
		return fmt.Errorf("emergencias: marcar resuelta_en: %w", err)
	}

	ev := outbox.Event{
		Tipo:         "emergencia.resuelta",
		AgregadoID:   emergenciaID,
		Sensibilidad: outbox.SensibilidadRestringida,
		Payload: map[string]any{
			"caso_emergencia_id": emergenciaID,
			"responsable_id":     actorReal,
		},
	}
	if err := s.pub.Publish(ctx, tx, ev); err != nil {
		return fmt.Errorf("emergencias: publicar emergencia.resuelta: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("emergencias: commit de la resolución: %w", err)
	}
	return nil
}

// Cerrar pasa el caso de resuelta a cerrada (cierre administrativo, irreversible;
// ADR 21 §5). Si el caso no está en resuelta → ErrTransicionInvalida.
func (s *Servicio) Cerrar(ctx context.Context, tenantID, emergenciaID, actorReal string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("emergencias: abrir transacción: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	ct, err := tx.Exec(ctx,
		`UPDATE mensajeria.emergencia
		    SET estado = 'cerrada', cerrada_en = now()
		  WHERE id = $1 AND tenant_id = $2 AND estado = 'resuelta'`,
		emergenciaID, tenantID)
	if err != nil {
		return fmt.Errorf("emergencias: cerrar caso: %w", err)
	}
	if ct.RowsAffected() == 0 {
		// O no existe, o no está en resuelta.
		if _, _, e := leerEstadoYViaje(ctx, tx, tenantID, emergenciaID); errors.Is(e, ErrNoEncontrada) {
			return ErrNoEncontrada
		}
		return ErrTransicionInvalida
	}

	ev := outbox.Event{
		Tipo:         "emergencia.cerrada",
		AgregadoID:   emergenciaID,
		Sensibilidad: outbox.SensibilidadRestringida,
		Payload: map[string]any{
			"caso_emergencia_id": emergenciaID,
		},
	}
	if err := s.pub.Publish(ctx, tx, ev); err != nil {
		return fmt.Errorf("emergencias: publicar emergencia.cerrada: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("emergencias: commit del cierre: %w", err)
	}
	return nil
}

// CasoProyectado es un caso leído, con los campos que su audiencia puede ver. La
// frontera lo mapea al CasoEmergencia del contrato. Para audiencia Cliente, los
// campos internos (Origen, ActorReal, ReclamoActivo) van en cero y las cerradas
// se excluyen (ADR 21 §6).
type CasoProyectado struct {
	ID             string
	ViajeID        string
	ClienteID      string
	Estado         string
	Origen         string
	ActorReal      string
	VersionReclamo msgtipos.VersionReclamo
	ActivadaEn     time.Time
	ResueltaEn     *time.Time
	CerradaEn      *time.Time
}

// ListarPorViaje devuelve los casos de emergencia de un viaje. Para la vista
// mínima del cliente (soloActivos=true, ADR 21 §6) excluye las cerradas; la
// frontera además oculta los campos internos. El orden es por activación.
func (s *Servicio) ListarPorViaje(ctx context.Context, tenantID, viajeID string, soloActivos bool) ([]CasoProyectado, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("emergencias: abrir transacción de lectura: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	filtro := ""
	if soloActivos {
		filtro = ` AND estado <> 'cerrada'`
	}
	rows, err := tx.Query(ctx,
		`SELECT id, viaje_id, COALESCE(cliente_id::text, ''), estado, origen,
		        actor_real::text, version_reclamo, activada_en, resuelta_en, cerrada_en
		   FROM mensajeria.emergencia
		  WHERE tenant_id = $1 AND viaje_id = $2`+filtro+`
		  ORDER BY activada_en`,
		tenantID, viajeID)
	if err != nil {
		return nil, fmt.Errorf("emergencias: listar casos: %w", err)
	}
	defer rows.Close()

	var casos []CasoProyectado
	for rows.Next() {
		var c CasoProyectado
		var version int64
		if err := rows.Scan(&c.ID, &c.ViajeID, &c.ClienteID, &c.Estado, &c.Origen,
			&c.ActorReal, &version, &c.ActivadaEn, &c.ResueltaEn, &c.CerradaEn); err != nil {
			return nil, fmt.Errorf("emergencias: escanear caso: %w", err)
		}
		c.VersionReclamo = msgtipos.VersionReclamo(version)
		casos = append(casos, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("emergencias: iterar casos: %w", err)
	}
	return casos, nil
}

// bandejaReaccion resuelve la bandeja de Reacción del tenant (la de tipo
// 'emergencias'). ErrNoEncontrada si el tenant no tiene una aprovisionada: en V1
// la bandeja de Reacción se siembra como parte del alta del tenant, igual que la
// bandeja de chat.
func bandejaReaccion(ctx context.Context, tx pgx.Tx, tenantID string) (string, error) {
	var id string
	err := tx.QueryRow(ctx,
		`SELECT id FROM mensajeria.bandeja
		  WHERE tenant_id = $1 AND tipo = 'emergencias'
		  ORDER BY creado_en LIMIT 1`,
		tenantID,
	).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNoEncontrada
	}
	if err != nil {
		return "", fmt.Errorf("emergencias: resolver bandeja de Reacción: %w", err)
	}
	return id, nil
}

// abrirReclamo inserta el reclamo activo inicial del caso en versión 1. El índice
// único parcial garantiza a lo más uno activo por emergencia.
func abrirReclamo(ctx context.Context, tx pgx.Tx, tenantID, emergenciaID, actorReal string) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO mensajeria.reclamo_emergencia (emergencia_id, tenant_id, actor_real, version)
		 VALUES ($1, $2, $3, 1)`,
		emergenciaID, tenantID, actorReal)
	if err != nil {
		return fmt.Errorf("emergencias: abrir reclamo: %w", err)
	}
	return nil
}

// fijarEstado estampa el estado del caso y refleja la versión de reclamo en la
// fila de emergencia, dentro de la tx del cambio.
func fijarEstado(ctx context.Context, tx pgx.Tx, tenantID, emergenciaID string, version msgtipos.VersionReclamo, estado string) error {
	_, err := tx.Exec(ctx,
		`UPDATE mensajeria.emergencia
		    SET estado = $3, version_reclamo = $4
		  WHERE id = $1 AND tenant_id = $2`,
		emergenciaID, tenantID, estado, int64(version))
	if err != nil {
		return fmt.Errorf("emergencias: fijar estado: %w", err)
	}
	return nil
}

// leerEstadoYViaje lee el estado y el viaje_id del caso. ErrNoEncontrada si el
// caso no existe para el tenant.
func leerEstadoYViaje(ctx context.Context, tx pgx.Tx, tenantID, emergenciaID string) (string, string, error) {
	var estado, viajeID string
	err := tx.QueryRow(ctx,
		`SELECT estado, viaje_id FROM mensajeria.emergencia
		  WHERE id = $1 AND tenant_id = $2`,
		emergenciaID, tenantID,
	).Scan(&estado, &viajeID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrNoEncontrada
	}
	if err != nil {
		return "", "", fmt.Errorf("emergencias: leer estado del caso: %w", err)
	}
	return estado, viajeID, nil
}

// versionActiva devuelve la versión del reclamo activo del caso.
func versionActiva(ctx context.Context, tx pgx.Tx, tenantID, emergenciaID string) (msgtipos.VersionReclamo, bool, error) {
	var v int64
	err := tx.QueryRow(ctx,
		`SELECT version FROM mensajeria.reclamo_emergencia
		  WHERE emergencia_id = $1 AND tenant_id = $2 AND cerrado_en IS NULL`,
		emergenciaID, tenantID,
	).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("emergencias: leer versión activa: %w", err)
	}
	return msgtipos.VersionReclamo(v), true, nil
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
