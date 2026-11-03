// Package autorizacion decide si un actor puede obrar sobre una conversación,
// desde la proyección local de mensajeria. Poseer el UUID de una conversación NO
// autoriza: la autoridad sale de la coincidencia de cliente o del rol del actor
// sobre la bandeja, verificada en servidor (addendum §7).
//
// Dos ramas por audiencia (DEC-054):
//   - Cliente: autoriza si el cliente_id de la conversación es el tenant del
//     token. La conversación es de la empresa cliente: cualquier usuario de esa
//     empresa la lee y escribe (DEC-124, enmienda DEC-097 #2; DEC-112).
//   - Interno: autoriza al rol `ejecutivo` si la conversación está enrutada a
//     una bandeja de chat del tenant. Hay una sola bandeja de conversaciones y
//     todo Ejecutivo es miembro por su rol, sin filas en pool_membresia (DEC-127,
//     DEC-103 #1). Otro rol interno no tiene acceso por esta vía.
//
// La bandeja de Reacción (emergencias) sigue con membresía en pool_membresia
// (DEC-028); DEC-127 no la cambia.
//
// Esta guarda decide ACCESO a la conversación. La autoridad de ENVÍO exige
// además reclamo activo + versión esperada, que la impone internal/reclamos y el
// propio conversacion.Enviar, no esta capa (addendum §7, §4 punto 7).
package autorizacion

import (
	"context"
	"errors"

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

// Servicio resuelve el acceso a una conversación contra la proyección local.
type Servicio struct {
	db Consultor
}

// NewServicio construye la guarda de autorización sobre el pool de chat.
func NewServicio(db Consultor) *Servicio {
	return &Servicio{db: db}
}

// PuedeEscribir devuelve nil si el actor de claims puede obrar sobre la
// conversación, o un error de la taxonomía de la plataforma: no_encontrado si la
// conversación no existe para el tenant (el servidor no revela existencia por el
// código), no_autorizado si existe pero el actor no tiene acceso.
//
// La rama la decide la Audiencia de dominio del token (DEC-054): Cliente compara
// el tenant contra cliente_id; cualquier otra audiencia (Interno) exige el rol
// `ejecutivo` sobre la bandeja de chat (DEC-127).
func (s *Servicio) PuedeEscribir(ctx context.Context, tenantID, conversacionID string, c *auth.Claims) error {
	if c == nil {
		return platformerrors.NewNoAutorizado("autorizacion: sin claims")
	}
	if c.Audiencia == auth.AudienciaCliente {
		return s.clientePuede(ctx, tenantID, conversacionID)
	}
	return s.internoPuede(ctx, tenantID, conversacionID, c.Rol)
}

// EsMiembroBandejaChat dice si un rol es miembro de la bandeja única de
// conversaciones: todo Ejecutivo lo es por su rol (DEC-127 #2).
func EsMiembroBandejaChat(rol string) bool {
	return rol == string(roles.RolEjecutivo)
}

// clientePuede autoriza a cualquier usuario de la empresa cliente dueña de la
// conversación: el tenant del token debe igualar conversacion.cliente_id
// (DEC-124, DEC-112). El sub (la persona) no decide.
func (s *Servicio) clientePuede(ctx context.Context, tenantID, conversacionID string) error {
	var clienteID *string
	err := s.db.QueryRow(ctx,
		`SELECT cliente_id FROM mensajeria.conversacion WHERE id = $1 AND tenant_id = $2`,
		conversacionID, tenantID,
	).Scan(&clienteID)
	if errors.Is(err, pgx.ErrNoRows) {
		return platformerrors.NewNoEncontrado("conversacion no encontrada")
	}
	if err != nil {
		return platformerrors.NewRotoErr(err, "autorizacion: leer conversacion")
	}
	if clienteID == nil || *clienteID != tenantID {
		return platformerrors.NewNoAutorizado("autorizacion: la empresa cliente no es dueña de la conversación")
	}
	return nil
}

// internoPuede autoriza al Ejecutivo sobre una conversación enrutada (ruta
// activa) a una bandeja de chat del tenant (DEC-127). Distingue "conversación
// inexistente" (no_encontrado) de "sin acceso" (no_autorizado) con un EXISTS
// separado sobre la conversación, así que un rol sin acceso recibe la misma
// respuesta que antes recibía quien no tenía membresía.
func (s *Servicio) internoPuede(ctx context.Context, tenantID, conversacionID, rol string) error {
	var existe bool
	err := s.db.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM mensajeria.conversacion WHERE id = $1 AND tenant_id = $2)`,
		conversacionID, tenantID,
	).Scan(&existe)
	if err != nil {
		return platformerrors.NewRotoErr(err, "autorizacion: verificar conversacion")
	}
	if !existe {
		return platformerrors.NewNoEncontrado("conversacion no encontrada")
	}
	if !EsMiembroBandejaChat(rol) {
		return platformerrors.NewNoAutorizado("autorizacion: el rol no es miembro de la bandeja de conversaciones")
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
		conversacionID, tenantID,
	).Scan(&enrutada)
	if err != nil {
		return platformerrors.NewRotoErr(err, "autorizacion: verificar ruta de la conversación")
	}
	if !enrutada {
		return platformerrors.NewNoAutorizado("autorizacion: la conversación no está en la bandeja de conversaciones")
	}
	return nil
}

// PuedeVerBandeja autoriza a listar el trabajo de una bandeja por su id. Exige
// audiencia interna. La bandeja de chat la ve todo Ejecutivo por su rol
// (DEC-127); cualquier otra bandeja (Reacción) sigue exigiendo membresía ACTIVA
// en su pool (contrato listarTrabajoBandeja, Addendum §7, DEC-028). Un cliente
// no lista bandejas (no_autorizado). La bandeja inexistente para el tenant es
// no_encontrado, distinguida de la falta de acceso.
func (s *Servicio) PuedeVerBandeja(ctx context.Context, tenantID, bandejaID string, c *auth.Claims) error {
	if c == nil {
		return platformerrors.NewNoAutorizado("autorizacion: sin claims")
	}
	if c.Audiencia == auth.AudienciaCliente {
		return platformerrors.NewNoAutorizado("autorizacion: el cliente no lista bandejas")
	}

	var tipo string
	err := s.db.QueryRow(ctx,
		`SELECT tipo FROM mensajeria.bandeja WHERE id = $1 AND tenant_id = $2`,
		bandejaID, tenantID,
	).Scan(&tipo)
	if errors.Is(err, pgx.ErrNoRows) {
		return platformerrors.NewNoEncontrado("bandeja no encontrada")
	}
	if err != nil {
		return platformerrors.NewRotoErr(err, "autorizacion: verificar bandeja")
	}
	if tipo == "chat" {
		if !EsMiembroBandejaChat(c.Rol) {
			return platformerrors.NewNoAutorizado("autorizacion: el rol no es miembro de la bandeja de conversaciones")
		}
		return nil
	}

	var autorizado bool
	err = s.db.QueryRow(ctx,
		`SELECT EXISTS (
		   SELECT 1 FROM mensajeria.pool_membresia
		    WHERE bandeja_id = $1 AND tenant_id = $2 AND usuario_id = $3 AND activa)`,
		bandejaID, tenantID, c.Subject,
	).Scan(&autorizado)
	if err != nil {
		return platformerrors.NewRotoErr(err, "autorizacion: verificar membresía de bandeja")
	}
	if !autorizado {
		return platformerrors.NewNoAutorizado("autorizacion: sin membresía efectiva en la bandeja")
	}
	return nil
}

// PuedeAtenderEmergencia autoriza a obrar sobre un caso de emergencia que está
// en la cola de Reacción (reclamar, liberar, transferir, resolver): exige
// audiencia interna y membresía ACTIVA en el pool de la bandeja del caso
// (DEC-028: solo miembros efectivos de Reacción atienden). Un cliente no atiende
// (no_autorizado). El caso inexistente para el tenant es no_encontrado,
// distinguido de la falta de membresía con un EXISTS aparte, igual que las demás
// guardas. El alcance jerárquico de mando sobre Reacción (DEC-099) no viaja en el
// token (DEC-053) y no se decide aquí.
func (s *Servicio) PuedeAtenderEmergencia(ctx context.Context, tenantID, emergenciaID string, c *auth.Claims) error {
	if c == nil {
		return platformerrors.NewNoAutorizado("autorizacion: sin claims")
	}
	if c.Audiencia == auth.AudienciaCliente {
		return platformerrors.NewNoAutorizado("autorizacion: el cliente no atiende emergencias")
	}

	var existe bool
	err := s.db.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM mensajeria.emergencia WHERE id = $1 AND tenant_id = $2)`,
		emergenciaID, tenantID,
	).Scan(&existe)
	if err != nil {
		return platformerrors.NewRotoErr(err, "autorizacion: verificar emergencia")
	}
	if !existe {
		return platformerrors.NewNoEncontrado("emergencia no encontrada")
	}

	var autorizado bool
	err = s.db.QueryRow(ctx,
		`SELECT EXISTS (
		   SELECT 1
		     FROM mensajeria.emergencia e
		     JOIN mensajeria.pool_membresia m
		       ON m.bandeja_id = e.bandeja_id AND m.tenant_id = e.tenant_id
		    WHERE e.id = $1 AND e.tenant_id = $2
		      AND m.usuario_id = $3 AND m.activa)`,
		emergenciaID, tenantID, c.Subject,
	).Scan(&autorizado)
	if err != nil {
		return platformerrors.NewRotoErr(err, "autorizacion: verificar membresía de Reacción")
	}
	if !autorizado {
		return platformerrors.NewNoAutorizado("autorizacion: sin membresía efectiva en el pool de Reacción")
	}
	return nil
}
