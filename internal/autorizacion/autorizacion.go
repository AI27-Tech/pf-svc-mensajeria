// Package autorizacion decide si un actor puede obrar sobre una conversación,
// desde la proyección local de mensajeria (DEC-097). Poseer el UUID de una
// conversación NO autoriza: la autoridad sale de la coincidencia de cliente o
// de la membresía efectiva del pool, verificada en servidor (addendum §7).
//
// Dos ramas por audiencia (DEC-054, DEC-097):
//   - Cliente: autoriza si el cliente_id de la conversación coincide con la
//     identidad del usuario-cliente del token (sub). El cliente es multiusuario;
//     cada usuario ve solo sus conversaciones (DEC-097 #2).
//   - Interno (Ejecutivo/Supervisor/Coordinador): autoriza si hay membresía
//     ACTIVA en el pool de la bandeja a la que la conversación está enrutada
//     (DEC-097 #1, #3). La membresía la administra a mano un Coordinador/
//     Supervisor; mensajeria es su dueño (pool_membresia).
//
// Esta guarda decide ACCESO a la conversación. La autoridad de ENVÍO exige
// además reclamo activo + versión esperada, que la impone internal/reclamos y el
// propio conversacion.Enviar, no esta capa (addendum §7, §4 punto 7).
package autorizacion

import (
	"context"
	"errors"

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
// contra cliente_id; cualquier otra audiencia (Interno) exige membresía de pool.
func (s *Servicio) PuedeEscribir(ctx context.Context, tenantID, conversacionID string, c *auth.Claims) error {
	if c == nil {
		return platformerrors.NewNoAutorizado("autorizacion: sin claims")
	}
	if c.Audiencia == auth.AudienciaCliente {
		return s.clientePuede(ctx, tenantID, conversacionID, c.Subject)
	}
	return s.internoPuede(ctx, tenantID, conversacionID, c.Subject)
}

// clientePuede autoriza al usuario-cliente dueño de la conversación. El sub del
// token es el usuario-cliente; debe igualar conversacion.cliente_id.
func (s *Servicio) clientePuede(ctx context.Context, tenantID, conversacionID, sub string) error {
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
	if clienteID == nil || *clienteID != sub {
		return platformerrors.NewNoAutorizado("autorizacion: el cliente no es dueño de la conversación")
	}
	return nil
}

// internoPuede autoriza al actor interno con membresía activa en el pool de la
// bandeja a la que la conversación está enrutada (ruta activa). Una sola consulta
// une conversacion → ruta_conversacion activa → pool_membresia activa; distingue
// "conversación inexistente" (no_encontrado) de "sin membresía" (no_autorizado)
// con un EXISTS separado sobre la conversación.
func (s *Servicio) internoPuede(ctx context.Context, tenantID, conversacionID, sub string) error {
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

	var autorizado bool
	err = s.db.QueryRow(ctx,
		`SELECT EXISTS (
		   SELECT 1
		     FROM mensajeria.ruta_conversacion r
		     JOIN mensajeria.pool_membresia m
		       ON m.bandeja_id = r.bandeja_id AND m.tenant_id = r.tenant_id
		    WHERE r.conversacion_id = $1 AND r.tenant_id = $2 AND r.activa
		      AND m.usuario_id = $3 AND m.activa)`,
		conversacionID, tenantID, sub,
	).Scan(&autorizado)
	if err != nil {
		return platformerrors.NewRotoErr(err, "autorizacion: verificar membresía")
	}
	if !autorizado {
		return platformerrors.NewNoAutorizado("autorizacion: sin membresía efectiva en el pool de la conversación")
	}
	return nil
}
