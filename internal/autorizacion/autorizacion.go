// Package autorizacion decide si un actor puede obrar sobre una conversación,
// una bandeja o un caso de emergencia, desde la proyección local de mensajeria.
// Poseer el UUID de un recurso NO autoriza: la autoridad sale de la empresa del
// cliente o del rol del actor, verificada en servidor (addendum §7).
//
// Tenant (DEC-129): cada guarda devuelve el tenant bajo el que vive el recurso,
// y es ese tenant (no el del token) el que usan las consultas y escrituras
// siguientes. Un interno no se acota por el tenant de quien pregunta: lo que
// puede hacer lo decide su rol. El Cliente sigue acotado a su empresa: tenant del
// token igual al del recurso (DEC-124, DEC-112).
//
// Respuestas sin acceso (DEC-132): recurso inexistente → no_encontrado; Cliente
// sobre un recurso de otra empresa → no_encontrado (no se revela que existe);
// interno sin permiso sobre un recurso que existe → prohibido. Sin claims →
// no_autorizado.
//
// Membresía por rol, sin filas en pool_membresia: la bandeja única de
// conversaciones la atiende el rol `ejecutivo` (DEC-127) y la de emergencias el
// rol `reaccion` (DEC-130). `supervisor` y `coordinador` leen conversaciones y
// ven, liberan y transfieren casos de emergencia, sin verificar mando (DEC-131,
// DEC-126; DT-06).
//
// Estas guardas deciden ACCESO. La autoridad de ENVÍO exige además reclamo
// activo + versión esperada, y "ser el responsable" para liberar, transferir o
// resolver lo decide el dominio (internal/reclamos, internal/emergencias).
package autorizacion

import (
	"context"
	"errors"
	"slices"

	roles "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/roles"
	"github.com/AI27-Tech/pf-pkg-platform-go/auth"
	platformerrors "github.com/AI27-Tech/pf-pkg-platform-go/errors"
	"github.com/jackc/pgx/v5"
)

// Consultor es la lectura mínima que la guarda necesita: una consulta de una
// fila. El pool de chat de svc_mensajeria la satisface. Interfaz declarada por
// el consumidor (golang-best-practices §6).
type Consultor interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Servicio resuelve el acceso a los recursos contra la proyección local.
type Servicio struct {
	db Consultor
}

// NewServicio construye la guarda de autorización sobre el pool de chat.
func NewServicio(db Consultor) *Servicio {
	return &Servicio{db: db}
}

// Accion es lo que un interno pide sobre una conversación (DEC-131).
type Accion int

const (
	// Leer: detalle, mensajes y bandeja.
	Leer Accion = iota
	// Escribir: enviar, reclamar, liberar, transferir, atendida y recibos.
	Escribir
)

// Permiso es lo que un interno pide sobre un caso de emergencia.
type Permiso int

const (
	// VerCaso: bandeja de emergencias, detalle y notas (DEC-130, DEC-131).
	VerCaso Permiso = iota
	// AtenderCaso: reclamar y resolver; solo Reacción (DEC-028, DEC-131).
	AtenderCaso
	// MoverCaso: liberar y transferir. Reacción solo si es el responsable (lo
	// decide el dominio); Supervisor y Coordinador siempre (DEC-126).
	MoverCaso
	// CerrarCaso: monitoreo o Administrador (ADR 21 §5, E8-T18); nunca Reacción
	// ni Ejecutivo.
	CerrarCaso
)

// rolesConversacion y rolesEmergencia son la tabla de quién hace qué. La rama
// Cliente no pasa por aquí: su límite es la empresa.
var (
	rolesConversacion = map[Accion][]roles.Rol{
		Leer:     {roles.RolEjecutivo, roles.RolSupervisor, roles.RolCoordinador},
		Escribir: {roles.RolEjecutivo},
	}
	rolesEmergencia = map[Permiso][]roles.Rol{
		VerCaso:     {roles.RolReaccion, roles.RolSupervisor, roles.RolCoordinador},
		AtenderCaso: {roles.RolReaccion},
		MoverCaso:   {roles.RolReaccion, roles.RolSupervisor, roles.RolCoordinador},
		CerrarCaso:  {roles.RolMonitorista, roles.RolSupervisor, roles.RolCoordinador, roles.RolAdministrador},
	}
)

func tiene(rs []roles.Rol, rol string) bool { return slices.Contains(rs, roles.Rol(rol)) }

// LeeBandejaChat dice si el rol ve la bandeja única de conversaciones: el
// Ejecutivo por ser miembro (DEC-127) y Supervisor/Coordinador en lectura
// (DEC-131).
func LeeBandejaChat(rol string) bool { return tiene(rolesConversacion[Leer], rol) }

// VeBandejaEmergencias dice si el rol ve la bandeja única de emergencias:
// Reacción por ser miembro (DEC-130) y Supervisor/Coordinador (DEC-131).
func VeBandejaEmergencias(rol string) bool { return tiene(rolesEmergencia[VerCaso], rol) }

// TenantDeConversacion devuelve el tenant bajo el que vive la conversación. Al
// Cliente solo se la resuelve si es de su empresa: tenant y cliente_id iguales
// al tenant del token (DEC-124); si no, no_encontrado (DEC-132 #2). Al interno se
// la resuelve sin acotar por su tenant (DEC-129). No decide el rol.
func (s *Servicio) TenantDeConversacion(ctx context.Context, conversacionID string, c *auth.Claims) (string, error) {
	if c == nil {
		return "", platformerrors.NewNoAutorizado("autorizacion: sin claims")
	}
	var tenant string
	var clienteID *string
	err := s.db.QueryRow(ctx,
		`SELECT tenant_id, cliente_id::text FROM mensajeria.conversacion WHERE id = $1`,
		conversacionID,
	).Scan(&tenant, &clienteID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", platformerrors.NewNoEncontrado("conversacion no encontrada")
	}
	if err != nil {
		return "", platformerrors.NewRotoErr(err, "autorizacion: leer conversacion")
	}
	if c.Audiencia == auth.AudienciaCliente &&
		(tenant != c.TenantID || clienteID == nil || *clienteID != c.TenantID) {
		return "", platformerrors.NewNoEncontrado("conversacion no encontrada")
	}
	return tenant, nil
}

// Conversacion autoriza la acción sobre la conversación y devuelve su tenant.
// Cualquier usuario de la empresa cliente dueña lee y escribe (DEC-124). Un
// interno necesita el rol de la acción (rolesConversacion) y que la conversación
// esté enrutada a la bandeja de conversaciones (DEC-127); si no, prohibido.
func (s *Servicio) Conversacion(ctx context.Context, conversacionID string, c *auth.Claims, a Accion) (string, error) {
	tenant, err := s.TenantDeConversacion(ctx, conversacionID, c)
	if err != nil || c.Audiencia == auth.AudienciaCliente {
		return tenant, err
	}
	if !tiene(rolesConversacion[a], c.Rol) {
		return "", platformerrors.NewProhibido("autorizacion: el rol no tiene esa acción sobre la conversación")
	}

	var enrutada bool
	err = s.db.QueryRow(ctx,
		`SELECT EXISTS (
		   SELECT 1
		     FROM mensajeria.ruta_conversacion r
		     JOIN mensajeria.bandeja b
		       ON b.id = r.bandeja_id AND b.tenant_id = r.tenant_id
		    WHERE r.conversacion_id = $1 AND r.tenant_id = $2 AND r.activa
		      AND b.tipo = 'chat')`,
		conversacionID, tenant,
	).Scan(&enrutada)
	if err != nil {
		return "", platformerrors.NewRotoErr(err, "autorizacion: verificar ruta de la conversación")
	}
	if !enrutada {
		return "", platformerrors.NewProhibido("autorizacion: la conversación no está en la bandeja de conversaciones")
	}
	return tenant, nil
}

// Bandeja autoriza a listar el trabajo de una bandeja por su id y devuelve su
// tenant. Un Cliente no lista bandejas: no_encontrado, sin revelar si existe
// (DEC-132 #2). La de chat la leen LeeBandejaChat; la de emergencias,
// VeBandejaEmergencias. Otro rol interno: prohibido.
func (s *Servicio) Bandeja(ctx context.Context, bandejaID string, c *auth.Claims) (string, error) {
	if c == nil {
		return "", platformerrors.NewNoAutorizado("autorizacion: sin claims")
	}
	noEncontrada := platformerrors.NewNoEncontrado("bandeja no encontrada")
	if c.Audiencia == auth.AudienciaCliente {
		return "", noEncontrada
	}

	var tipo, tenant string
	err := s.db.QueryRow(ctx,
		`SELECT tipo, tenant_id FROM mensajeria.bandeja WHERE id = $1`,
		bandejaID,
	).Scan(&tipo, &tenant)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", noEncontrada
	}
	if err != nil {
		return "", platformerrors.NewRotoErr(err, "autorizacion: verificar bandeja")
	}
	puede := VeBandejaEmergencias
	if tipo == "chat" {
		puede = LeeBandejaChat
	}
	if !puede(c.Rol) {
		return "", platformerrors.NewProhibido("autorizacion: el rol no ve esta bandeja")
	}
	return tenant, nil
}

// TenantDeEmergencia devuelve el tenant bajo el que vive el caso. Al Cliente solo
// se lo resuelve si es de su empresa; si no, no_encontrado (DEC-132 #2). Al
// interno, sin acotar por su tenant (DEC-129). No decide el rol.
func (s *Servicio) TenantDeEmergencia(ctx context.Context, emergenciaID string, c *auth.Claims) (string, error) {
	if c == nil {
		return "", platformerrors.NewNoAutorizado("autorizacion: sin claims")
	}
	var tenant string
	err := s.db.QueryRow(ctx,
		`SELECT tenant_id FROM mensajeria.emergencia WHERE id = $1`,
		emergenciaID,
	).Scan(&tenant)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", platformerrors.NewNoEncontrado("emergencia no encontrada")
	}
	if err != nil {
		return "", platformerrors.NewRotoErr(err, "autorizacion: leer emergencia")
	}
	if c.Audiencia == auth.AudienciaCliente && tenant != c.TenantID {
		return "", platformerrors.NewNoEncontrado("emergencia no encontrada")
	}
	return tenant, nil
}

// Emergencia autoriza el permiso sobre el caso y devuelve su tenant. Ninguno de
// estos permisos es del Cliente: sobre un caso de su empresa recibe prohibido.
// Un interno necesita un rol de rolesEmergencia para el permiso; si no,
// prohibido. La membresía de Reacción es el rol, sin filas (DEC-130).
func (s *Servicio) Emergencia(ctx context.Context, emergenciaID string, c *auth.Claims, p Permiso) (string, error) {
	tenant, err := s.TenantDeEmergencia(ctx, emergenciaID, c)
	if err != nil {
		return "", err
	}
	if c.Audiencia == auth.AudienciaCliente || !tiene(rolesEmergencia[p], c.Rol) {
		return "", platformerrors.NewProhibido("autorizacion: el rol no tiene ese permiso sobre el caso de emergencia")
	}
	return tenant, nil
}
