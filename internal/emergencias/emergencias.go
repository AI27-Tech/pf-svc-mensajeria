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
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/auditoria"
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

// ErrNoEsResponsable la devuelven Liberar y Transferir cuando el actor no tiene el
// reclamo activo ni mando (DEC-126). El llamador la mapea a 403 prohibido.
var ErrNoEsResponsable = errors.New("emergencias: el actor no es el responsable activo")

// TieneMando dice si el rol libera o transfiere casos ajenos: en V1, todo
// Supervisor o Coordinador, sin verificar el mando sobre el responsable.
// shortcut: no hay relación de mando en mensajeria (DEC-126), se verifica cuando exista (DT-06).
func TieneMando(rol string) bool { return rol == "supervisor" || rol == "coordinador" }

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

// ErrViajeNoEncontrado la devuelve Activar cuando mensajeria no conoce la empresa
// del viaje (no hay conversación proyectada de viaje.mensajeria_habilitada) o el
// viaje es de otra empresa que la del cliente que activa. El llamador la mapea a
// 404, sin revelar cuál de las dos (DEC-132 #2).
var ErrViajeNoEncontrado = errors.New("emergencias: viaje no encontrado")

// EntradaActivacion es lo mínimo para activar un caso. ActivacionID es la clave
// de idempotencia generada por el llamante (contrato ActivarEmergenciaEntrada).
// Origen distingue cliente de operacion (ADR 21 §3). El tenant y el cliente_id
// del caso NO se reciben: son los del viaje (DEC-133) y los resuelve el dominio,
// igual que la bandeja de Reacción (DEC-130). ClienteActor es la empresa del
// cliente que activa (tenant de su token); solo cuenta con Origen cliente y debe
// ser la empresa del viaje.
type EntradaActivacion struct {
	ViajeID      string
	ClienteActor string
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
// crea otro caso aunque ya exista uno abierto: no se deduplica por viaje. El caso
// queda bajo la empresa del viaje (DEC-133); si mensajeria no la conoce, o el
// cliente que activa es de otra empresa, ErrViajeNoEncontrado y no se crea nada.
func (s *Servicio) Activar(ctx context.Context, in EntradaActivacion) (ResultadoActivacion, error) {
	if in.ViajeID == "" || in.ActorReal == "" || in.ActivacionID == "" {
		return ResultadoActivacion{}, fmt.Errorf("emergencias: activación inválida: faltan campos obligatorios")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ResultadoActivacion{}, fmt.Errorf("emergencias: abrir transacción: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// El caso vive bajo la empresa del viaje, lo active quien lo active (DEC-133).
	tenantID, clienteID, err := empresaDelViaje(ctx, tx, in.ViajeID)
	if err != nil {
		return ResultadoActivacion{}, err
	}
	if in.Origen == msgclient.OrigenActivacionCliente && (in.ClienteActor != clienteID || in.ClienteActor != tenantID) {
		return ResultadoActivacion{}, ErrViajeNoEncontrado
	}

	// Idempotencia: si la activación ya existe, devolver el caso original.
	if id, ok, err := s.buscarPorActivacion(ctx, tx, tenantID, in); err != nil {
		return ResultadoActivacion{}, err
	} else if ok {
		return ResultadoActivacion{EmergenciaID: id, Idempotente: true}, nil
	}

	// La bandeja única de emergencias es dato de mensajeria: se resuelve (o se
	// crea) aquí, en la tx de la activación, no la trae el llamante (DEC-130).
	bandejaID, err := bandejaEmergencias(ctx, tx, tenantID)
	if err != nil {
		return ResultadoActivacion{}, err
	}

	var emergenciaID string
	err = tx.QueryRow(ctx,
		`INSERT INTO mensajeria.emergencia
		    (tenant_id, viaje_id, cliente_id, bandeja_id, origen, actor_real, activacion_id)
		  VALUES ($1, $2, $3::uuid, $4, $5, $6, $7)
		  RETURNING id`,
		tenantID, in.ViajeID, clienteID, bandejaID, string(in.Origen), in.ActorReal, in.ActivacionID,
	).Scan(&emergenciaID)
	if err != nil {
		// Carrera por la misma activación: el índice único la rechaza; releer el
		// original en vez de crear un segundo caso.
		if esViolacionUnica(err) {
			if id, ok, e := s.buscarPorActivacion(ctx, tx, tenantID, in); e == nil && ok {
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
			"cliente_id":         clienteID,
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

// empresaDelViaje devuelve el tenant y el cliente_id del viaje. La única
// proyección del viaje en mensajeria es su conversación, que nace de
// viaje.mensajeria_habilitada (operacion la publica al crear el viaje, en la tx
// de viaje.creado) con tenant_id = cliente_id (DEC-049/112). Si hay varias (una
// cerrada y otra activa), todas son del mismo viaje; se toma la más reciente.
// Sin conversación, ErrViajeNoEncontrado: no se inventa la empresa.
//
// shortcut: filtra solo por viaje_id, sin índice propio (el único es (tenant_id,
// viaje_id)); se indexa viaje_id si la activación se vuelve camino caliente.
func empresaDelViaje(ctx context.Context, tx pgx.Tx, viajeID string) (string, string, error) {
	var tenant, cliente string
	err := tx.QueryRow(ctx,
		`SELECT tenant_id, cliente_id::text FROM mensajeria.conversacion
		  WHERE viaje_id = $1 AND cliente_id IS NOT NULL
		  ORDER BY creado_en DESC, id DESC LIMIT 1`,
		viajeID,
	).Scan(&tenant, &cliente)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrViajeNoEncontrado
	}
	if err != nil {
		return "", "", fmt.Errorf("emergencias: leer la empresa del viaje: %w", err)
	}
	return tenant, cliente, nil
}

// buscarPorActivacion devuelve el caso ya existente para la clave de idempotencia.
func (s *Servicio) buscarPorActivacion(ctx context.Context, tx pgx.Tx, tenantID string, in EntradaActivacion) (string, bool, error) {
	var id string
	err := tx.QueryRow(ctx,
		`SELECT id FROM mensajeria.emergencia
		  WHERE tenant_id = $1 AND viaje_id = $2 AND activacion_id = $3`,
		tenantID, in.ViajeID, in.ActivacionID,
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

// Reclamar toma del pool el reclamo del caso (abierta → en_atencion) exigiendo la
// versión esperada, y emite emergencia.reclamada. Si el caso ya tiene reclamo
// activo (aunque la versión sea la vigente, DEC-125) o la versión no es la
// vigente, devuelve ErrConflictoVersion sin modificar nada: cambiar de
// responsable es liberar o transferir. Cada intento queda en
// auditoria_mensajeria, como en la conversación (DEC-123): el ganado en la tx del
// reclamo, el perdido en una tx propia tras el conflicto.
func (s *Servicio) Reclamar(ctx context.Context, tenantID, emergenciaID, actorReal string, esperada msgtipos.VersionReclamo) (msgtipos.VersionReclamo, error) {
	if emergenciaID == "" || actorReal == "" {
		return 0, fmt.Errorf("emergencias: reclamo inválido: faltan emergencia_id o actor_real")
	}

	nueva, err := s.reclamar(ctx, tenantID, emergenciaID, actorReal, esperada)
	var conflicto ErrConflictoVersion
	if errors.As(err, &conflicto) {
		// La tx del intento ya se descartó (en la carrera Postgres la abortó con
		// 23505): el perdedor solo compromete el hecho de que lo intentó.
		if e := s.auditarIntentoPerdido(ctx, tenantID, emergenciaID, actorReal, conflicto); e != nil {
			return 0, e
		}
	}
	return nueva, err
}

// auditarIntentoPerdido registra el reclamo perdido en una tx propia.
func (s *Servicio) auditarIntentoPerdido(ctx context.Context, tenantID, emergenciaID, actorReal string, c ErrConflictoVersion) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("emergencias: abrir transacción de auditoría: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := auditoria.Registrar(ctx, tx, auditoria.Registro{
		TenantID: tenantID, ActorReal: actorReal,
		Accion: auditoria.AccionEmergenciaReclamoPerdido,
		Detalle: map[string]any{
			"caso_emergencia_id": emergenciaID, "version_esperada": c.Esperada, "version_actual": c.Actual,
		},
	}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("emergencias: commit de la auditoría del intento perdido: %w", err)
	}
	return nil
}

// reclamar es el intento en su tx: abre el reclamo desde el pool, audita el
// intento ganado, pone el caso en atención y publica emergencia.reclamada.
func (s *Servicio) reclamar(ctx context.Context, tenantID, emergenciaID, actorReal string, esperada msgtipos.VersionReclamo) (msgtipos.VersionReclamo, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("emergencias: abrir transacción: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Solo del pool (DEC-125), con la versión vigente del caso (0 si nunca se
	// reclamó); el reclamo nuevo la avanza: monotónica por caso, no por fila (P20).
	vigente, err := versionMaxima(ctx, tx, tenantID, emergenciaID)
	if err != nil {
		return 0, err
	}
	_, hay, err := versionActiva(ctx, tx, tenantID, emergenciaID)
	if err != nil {
		return 0, err
	}
	if hay || esperada != vigente {
		return 0, ErrConflictoVersion{Esperada: esperada, Actual: vigente}
	}
	nueva := int64(vigente) + 1
	reclamoID, err := abrirReclamo(ctx, tx, tenantID, emergenciaID, actorReal, nueva)
	if err != nil {
		return 0, err
	}

	if err := auditoria.Registrar(ctx, tx, auditoria.Registro{
		TenantID: tenantID, ActorReal: actorReal,
		Accion: auditoria.AccionEmergenciaReclamoGanado,
		Detalle: map[string]any{
			"caso_emergencia_id": emergenciaID, "reclamo_id": reclamoID,
			"version_esperada": esperada, "version_reclamo": nueva,
		},
	}); err != nil {
		return 0, err
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
// exigiendo rid y versión esperada. Sin reclamo activo → ErrSinReclamo; versión
// no vigente o rid que no es el activo → ErrConflictoVersion; actor que no es el
// responsable y no tiene mando → ErrNoEsResponsable (DEC-126).
func (s *Servicio) Liberar(ctx context.Context, tenantID, emergenciaID, reclamoID, actorReal string, esperada msgtipos.VersionReclamo, mando bool) error {
	if emergenciaID == "" || reclamoID == "" {
		return fmt.Errorf("emergencias: liberación inválida: faltan emergencia_id o reclamo_id")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("emergencias: abrir transacción: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	activo, err := tomarActivo(ctx, tx, tenantID, emergenciaID, actorReal, esperada, mando)
	if err != nil {
		return err
	}
	if activo != reclamoID {
		return ErrConflictoVersion{Esperada: esperada, Actual: esperada}
	}
	if _, err := tx.Exec(ctx,
		`UPDATE mensajeria.reclamo_emergencia SET cerrado_en = now() WHERE id = $1`, activo); err != nil {
		return fmt.Errorf("emergencias: liberar: %w", err)
	}

	// Liberar devuelve el caso a la cola: vuelve a abierta. La fila del caso queda
	// en la versión vigente (la más alta), que es la que exige el siguiente reclamo.
	vigente, err := versionMaxima(ctx, tx, tenantID, emergenciaID)
	if err != nil {
		return err
	}
	if err := fijarEstado(ctx, tx, tenantID, emergenciaID, vigente,
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
// el reclamo activo y abre uno nuevo para el destino en una tx, conservando el
// estado del caso (ADR 21 §5). El reclamo nuevo abre en la versión siguiente del
// caso, así que la previa queda obsoleta (P20). Mismas guardas que Liberar: sin
// reclamo activo → ErrSinReclamo; versión no vigente → ErrConflictoVersion; actor
// que no es el responsable y no tiene mando → ErrNoEsResponsable (DEC-126).
func (s *Servicio) Transferir(ctx context.Context, tenantID, emergenciaID, actor, destino string, esperada msgtipos.VersionReclamo, mando bool) error {
	if emergenciaID == "" || destino == "" {
		return fmt.Errorf("emergencias: transferencia inválida: faltan emergencia_id o destino")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("emergencias: abrir transacción: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	anteriorID, err := tomarActivo(ctx, tx, tenantID, emergenciaID, actor, esperada, mando)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE mensajeria.reclamo_emergencia SET cerrado_en = now() WHERE id = $1`, anteriorID); err != nil {
		return fmt.Errorf("emergencias: cerrar reclamo en transferencia: %w", err)
	}

	// El reclamo del destino abre en la versión siguiente del caso, no en 1: tras
	// A→B→A una versión previa de A no vuelve a ser vigente (P20). La fila del caso
	// refleja la versión nueva; el estado se conserva (ADR 21 §5).
	vigente, err := versionMaxima(ctx, tx, tenantID, emergenciaID)
	if err != nil {
		return err
	}
	nueva := int64(vigente) + 1
	nuevaID, err := abrirReclamo(ctx, tx, tenantID, emergenciaID, destino, nueva)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE mensajeria.emergencia SET version_reclamo = $3 WHERE id = $1 AND tenant_id = $2`,
		emergenciaID, tenantID, nueva); err != nil {
		return fmt.Errorf("emergencias: reflejar versión en transferencia: %w", err)
	}

	ev := outbox.Event{
		Tipo:         "emergencia.transferida",
		AgregadoID:   emergenciaID,
		Sensibilidad: outbox.SensibilidadRestringida,
		Payload: map[string]any{
			"caso_emergencia_id":  emergenciaID,
			"reclamo_anterior_id": anteriorID,
			"reclamo_id":          nuevaID,
			"responsable_id":      destino,
			"version_reclamo":     nueva,
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
// ErrConflictoVersion; si el caso no está en en_atencion → ErrTransicionInvalida;
// actor que no es el responsable vigente → ErrNoEsResponsable (ADR 21 §5).
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

	// Solo el responsable vigente resuelve, con la versión vigente (sin mando:
	// Supervisor y Coordinador no resuelven, DEC-131).
	if _, err := tomarActivo(ctx, tx, tenantID, emergenciaID, actorReal, esperada, false); err != nil {
		return err
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

// CasoProyectado es un caso leído, con todo lo que el caso registra. La frontera
// lo proyecta por audiencia al CasoEmergencia o EmergenciaReaccion del contrato:
// el Cliente ve solo los suyos y no cerrados, sin campos internos (ADR 21 §6).
type CasoProyectado struct {
	ID             string
	TenantID       string
	ViajeID        string
	ClienteID      string
	Estado         string
	Origen         string
	ActorReal      string
	VersionReclamo msgtipos.VersionReclamo
	ActivadaEn     time.Time
	ResueltaEn     *time.Time
	CerradaEn      *time.Time
	// Reclamo es el reclamo activo; nil si el caso está en el pool.
	Reclamo *ReclamoProyectado
}

// ReclamoProyectado es el reclamo activo de un caso: quién lo tiene y en qué
// versión, lo que Supervisor y Coordinador necesitan para liberarlo o
// transferirlo (DEC-131).
type ReclamoProyectado struct {
	ID          string
	Responsable string
	Version     msgtipos.VersionReclamo
	AbiertoEn   time.Time
}

// NotaProyectada es una nota del caso tal como se guardó.
type NotaProyectada struct {
	ID             string
	EmergenciaID   string
	AudienciaAutor string
	ActorReal      string
	Cuerpo         string
	EsResolucion   bool
	CreadoEn       time.Time
}

// sqlCaso lee el caso con su reclamo activo; cada lectura añade su WHERE.
const sqlCaso = `SELECT e.id, e.tenant_id, e.viaje_id, COALESCE(e.cliente_id::text, ''), e.estado, e.origen,
	        e.actor_real::text, e.version_reclamo, e.activada_en, e.resuelta_en, e.cerrada_en,
	        r.id::text, r.actor_real::text, r.version, r.abierto_en
	   FROM mensajeria.emergencia e
	   LEFT JOIN mensajeria.reclamo_emergencia r
	     ON r.emergencia_id = e.id AND r.tenant_id = e.tenant_id AND r.cerrado_en IS NULL `

// ListarPorViaje devuelve todos los casos de emergencia de un viaje, de cualquier
// tenant y estado, por orden de activación. No acota por audiencia: la frontera
// deja al Cliente solo los de su empresa y no cerrados (ADR 21 §6, DEC-129).
func (s *Servicio) ListarPorViaje(ctx context.Context, viajeID string) ([]CasoProyectado, error) {
	return s.leerCasos(ctx, `WHERE e.viaje_id = $1 ORDER BY e.activada_en, e.id`, viajeID)
}

// Bandeja devuelve los casos de la bandeja única de emergencias (DEC-130), sin
// acotar por tenant (DEC-129): todos menos los cerrados, del más antiguo al más
// reciente por activada_en y, a igual activada_en, por id (DEC-121 #3). Todo caso
// está en la bandeja única, así que no se filtra por bandeja_id: los casos
// activados antes de DEC-130 en una bandeja de Reacción por tenant también salen.
//
// shortcut: devuelve todos los casos abiertos sin paginar (cursor_siguiente null);
// paginar cuando la bandeja pase de unos cientos de casos abiertos.
func (s *Servicio) Bandeja(ctx context.Context) ([]CasoProyectado, error) {
	return s.leerCasos(ctx, `WHERE e.estado <> 'cerrada' ORDER BY e.activada_en, e.id`)
}

// Obtener devuelve un caso por su id. ErrNoEncontrada si no existe en el tenant.
func (s *Servicio) Obtener(ctx context.Context, tenantID, emergenciaID string) (CasoProyectado, error) {
	casos, err := s.leerCasos(ctx, `WHERE e.id = $1 AND e.tenant_id = $2`, emergenciaID, tenantID)
	if err != nil {
		return CasoProyectado{}, err
	}
	if len(casos) == 0 {
		return CasoProyectado{}, ErrNoEncontrada
	}
	return casos[0], nil
}

func (s *Servicio) leerCasos(ctx context.Context, donde string, args ...any) ([]CasoProyectado, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("emergencias: abrir transacción de lectura: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, sqlCaso+donde, args...)
	if err != nil {
		return nil, fmt.Errorf("emergencias: listar casos: %w", err)
	}
	defer rows.Close()

	casos := []CasoProyectado{}
	for rows.Next() {
		var (
			c                      CasoProyectado
			version                int64
			reclamoID, responsable *string
			reclamoVersion         *int64
			abiertoEn              *time.Time
		)
		if err := rows.Scan(&c.ID, &c.TenantID, &c.ViajeID, &c.ClienteID, &c.Estado, &c.Origen,
			&c.ActorReal, &version, &c.ActivadaEn, &c.ResueltaEn, &c.CerradaEn,
			&reclamoID, &responsable, &reclamoVersion, &abiertoEn); err != nil {
			return nil, fmt.Errorf("emergencias: escanear caso: %w", err)
		}
		c.VersionReclamo = msgtipos.VersionReclamo(version)
		if reclamoID != nil {
			c.Reclamo = &ReclamoProyectado{
				ID: *reclamoID, Responsable: *responsable,
				Version: msgtipos.VersionReclamo(*reclamoVersion), AbiertoEn: *abiertoEn,
			}
		}
		casos = append(casos, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("emergencias: iterar casos: %w", err)
	}
	return casos, nil
}

// ListarNotas devuelve las notas del caso de la más antigua a la más reciente
// (creado_en, desempate por id), incluida la de resolución. El llamador ya
// verificó que el caso existe.
//
// shortcut: sin paginar (cursor_siguiente null); un caso lleva pocas notas, se
// pagina si alguna vez pasa de unos cientos.
func (s *Servicio) ListarNotas(ctx context.Context, tenantID, emergenciaID string) ([]NotaProyectada, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("emergencias: abrir transacción de lectura: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx,
		`SELECT id, emergencia_id, audiencia_autor, actor_real::text, cuerpo, es_resolucion, creado_en
		   FROM mensajeria.nota_emergencia
		  WHERE emergencia_id = $1 AND tenant_id = $2
		  ORDER BY creado_en, id`,
		emergenciaID, tenantID)
	if err != nil {
		return nil, fmt.Errorf("emergencias: listar notas: %w", err)
	}
	defer rows.Close()

	notas := []NotaProyectada{}
	for rows.Next() {
		var n NotaProyectada
		if err := rows.Scan(&n.ID, &n.EmergenciaID, &n.AudienciaAutor, &n.ActorReal,
			&n.Cuerpo, &n.EsResolucion, &n.CreadoEn); err != nil {
			return nil, fmt.Errorf("emergencias: escanear nota: %w", err)
		}
		notas = append(notas, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("emergencias: iterar notas: %w", err)
	}
	return notas, nil
}

// bandejaEmergencias resuelve la bandeja única de emergencias (DEC-130 #1), sin
// acotar por tenant (DEC-129), y la crea si todavía no existe, dentro de la tx de
// la activación. La bandeja nace con el tenant del primer caso solo porque la
// columna lo exige; nadie la busca por él. Si hay varias (sembradas por tenant
// antes de DEC-130), la única es la más antigua, el mismo criterio que la de
// conversaciones. Un lock transaccional serializa la creación: dos primeras
// activaciones concurrentes no crean dos bandejas.
func bandejaEmergencias(ctx context.Context, tx pgx.Tx, tenantID string) (string, error) {
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtext('mensajeria.bandeja_emergencias'))`); err != nil {
		return "", fmt.Errorf("emergencias: bloquear bandeja de emergencias: %w", err)
	}
	var id string
	err := tx.QueryRow(ctx,
		`SELECT id FROM mensajeria.bandeja
		  WHERE tipo = 'emergencias'
		  ORDER BY creado_en, id LIMIT 1`,
	).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx,
			`INSERT INTO mensajeria.bandeja (tenant_id, tipo, nombre)
			 VALUES ($1, 'emergencias', 'Emergencias') RETURNING id`,
			tenantID,
		).Scan(&id)
	}
	if err != nil {
		return "", fmt.Errorf("emergencias: resolver o crear la bandeja de emergencias: %w", err)
	}
	return id, nil
}

// abrirReclamo inserta el reclamo activo del caso en la versión dada y devuelve
// su id. El índice único parcial garantiza a lo más uno activo por emergencia:
// si dos abren a la vez, el perdedor recibe ErrConflictoVersion.
func abrirReclamo(ctx context.Context, tx pgx.Tx, tenantID, emergenciaID, actorReal string, version int64) (string, error) {
	var id string
	err := tx.QueryRow(ctx,
		`INSERT INTO mensajeria.reclamo_emergencia (emergencia_id, tenant_id, actor_real, version)
		 VALUES ($1, $2, $3, $4) RETURNING id`,
		emergenciaID, tenantID, actorReal, version).Scan(&id)
	if esViolacionUnica(err) {
		return "", ErrConflictoVersion{Esperada: msgtipos.VersionReclamo(version - 1), Actual: msgtipos.VersionReclamo(version)}
	}
	if err != nil {
		return "", fmt.Errorf("emergencias: abrir reclamo: %w", err)
	}
	return id, nil
}

// versionMaxima devuelve la versión vigente del reclamo del caso: la más alta de
// sus reclamos, activos o cerrados (0 si nunca se reclamó). El reclamo nuevo,
// desde el pool o por transferencia, abre en esta + 1 (P20).
func versionMaxima(ctx context.Context, tx pgx.Tx, tenantID, emergenciaID string) (msgtipos.VersionReclamo, error) {
	var v int64
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(max(version), 0) FROM mensajeria.reclamo_emergencia
		  WHERE emergencia_id = $1 AND tenant_id = $2`,
		emergenciaID, tenantID,
	).Scan(&v); err != nil {
		return 0, fmt.Errorf("emergencias: leer versión del caso: %w", err)
	}
	return msgtipos.VersionReclamo(v), nil
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

// tomarActivo bloquea el reclamo activo del caso (FOR UPDATE: una liberación o
// transferencia concurrente espera a esta tx) y lo valida en este orden: que
// exista (ErrSinReclamo), que la versión esperada sea la vigente
// (ErrConflictoVersion) y que el actor sea su responsable o tenga mando
// (ErrNoEsResponsable, DEC-126). Devuelve su id.
func tomarActivo(ctx context.Context, tx pgx.Tx, tenantID, emergenciaID, actor string, esperada msgtipos.VersionReclamo, mando bool) (string, error) {
	var id, responsable string
	var version int64
	err := tx.QueryRow(ctx,
		`SELECT id::text, actor_real::text, version FROM mensajeria.reclamo_emergencia
		  WHERE emergencia_id = $1 AND tenant_id = $2 AND cerrado_en IS NULL
		  FOR UPDATE`,
		emergenciaID, tenantID,
	).Scan(&id, &responsable, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrSinReclamo
	}
	if err != nil {
		return "", fmt.Errorf("emergencias: leer reclamo activo: %w", err)
	}
	if version != int64(esperada) {
		return "", ErrConflictoVersion{Esperada: esperada, Actual: msgtipos.VersionReclamo(version)}
	}
	if responsable != actor && !mando {
		return "", ErrNoEsResponsable
	}
	return id, nil
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
