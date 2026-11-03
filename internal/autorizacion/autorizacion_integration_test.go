package autorizacion_test

import (
	"context"
	"testing"
	"time"

	"github.com/AI27-Tech/pf-pkg-platform-go/auth"
	platformerrors "github.com/AI27-Tech/pf-pkg-platform-go/errors"
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/autorizacion"
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/dbtest"
)

// TestPuedeEscribir es la prueba de comportamiento de la guarda de autorización
// (DEC-124, DEC-127). Afirma, contra Postgres real, las dos ramas por audiencia:
//   - Cliente: autoriza a cualquier usuario cuyo tenant sea el cliente_id de la
//     conversación, aunque su sub sea otro (la conversación es de la empresa). Un
//     cliente de otro tenant no la encuentra (no_encontrado: la consulta se acota
//     por tenant). Una fila con tenant del cliente pero cliente_id de otra
//     empresa no autoriza (addendum §7: poseer el UUID no autoriza).
//   - Interno: autoriza al rol `ejecutivo` sin filas en pool_membresia si la
//     conversación está enrutada a la bandeja de chat; otro rol interno
//     (monitorista, supervisor) no_autorizado, aunque tenga una fila sembrada;
//     una conversación sin ruta a chat (o enrutada a Reacción) no_autorizado.
//
// Una conversación inexistente es no_encontrado, no no_autorizado: el servidor
// no revela existencia por el código de error.
//
// Se salta si MENSAJERIA_TEST_DB no está definido.
func TestPuedeEscribir(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	dbtest.Serializar(t, ctx)
	admin, closeAdmin := dbtest.AdminConn(t, ctx)
	defer closeAdmin()

	dbtest.Reiniciar(t, ctx, admin)
	defer dbtest.Reiniciar(t, ctx, admin)
	dbtest.AplicarMigraciones(t, ctx, admin)

	const (
		// DEC-112: el tenant de un usuario cliente ES su cliente_id.
		clienteA    = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
		clienteB    = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
		convCli     = "11111111-1111-1111-1111-111111111111"
		convSinRuta = "22222222-2222-2222-2222-222222222222"
		convReacc   = "33333333-3333-3333-3333-333333333333"
		convAjena   = "44444444-4444-4444-4444-444444444444"
		bandeja     = "cccccccc-cccc-cccc-cccc-cccccccccccc"
		bandejaEmer = "c0000000-0000-0000-0000-00000000000e"
		usuarioA1   = "a1000000-0000-0000-0000-000000000001"
		usuarioA2   = "a2000000-0000-0000-0000-000000000002"
		usuarioB1   = "b1000000-0000-0000-0000-000000000001"
		ejecutivo   = "dddddddd-dddd-dddd-dddd-dddddddddddd"
		monitorista = "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee"
		supervisor  = "ffffffff-ffff-ffff-ffff-ffffffffffff"
	)

	// convCli: de la empresa A, enrutada a la bandeja de chat.
	// convSinRuta: de A, sin ruta. convReacc: de A, enrutada a una bandeja de
	// emergencias. convAjena: fila del tenant A con cliente_id de B (dato
	// inconsistente; prueba que la guarda compara cliente_id, no solo el filtro).
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.conversacion (id, tenant_id, viaje_id, cliente_id, estado, siguiente_secuencia) VALUES
		 ('`+convCli+`', '`+clienteA+`', gen_random_uuid(), '`+clienteA+`', 'abierta', 0),
		 ('`+convSinRuta+`', '`+clienteA+`', gen_random_uuid(), '`+clienteA+`', 'abierta', 0),
		 ('`+convReacc+`', '`+clienteA+`', gen_random_uuid(), '`+clienteA+`', 'abierta', 0),
		 ('`+convAjena+`', '`+clienteA+`', gen_random_uuid(), '`+clienteB+`', 'abierta', 0)`)
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.bandeja (id, tenant_id, tipo, nombre) VALUES
		 ('`+bandeja+`', '`+clienteA+`', 'chat', 'Conversaciones'),
		 ('`+bandejaEmer+`', '`+clienteA+`', 'emergencias', 'Reacción')`)
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.ruta_conversacion (conversacion_id, tenant_id, bandeja_id, activa) VALUES
		 ('`+convCli+`', '`+clienteA+`', '`+bandeja+`', true),
		 ('`+convReacc+`', '`+clienteA+`', '`+bandejaEmer+`', true)`)
	// Fila sembrada al estilo DEC-097 para un supervisor: ya no es fuente de
	// membresía (DEC-127 #3) y no debe conceder acceso.
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.pool_membresia (bandeja_id, usuario_id, tenant_id, rol, version, activa)
		 VALUES ('`+bandeja+`', '`+supervisor+`', '`+clienteA+`', 'supervisor', 1, true)`)

	pool, closePool := dbtest.PoolComoRol(t, ctx)
	defer closePool()

	svc := autorizacion.NewServicio(pool)

	cliente := func(sub, tenant string) *auth.Claims {
		return &auth.Claims{Subject: sub, TenantID: tenant, Rol: "cliente", Audiencia: auth.AudienciaCliente}
	}
	interno := func(sub, rol string) *auth.Claims {
		return &auth.Claims{Subject: sub, TenantID: clienteA, Rol: rol, Audiencia: auth.AudienciaInterno}
	}

	casos := []struct {
		nombre string
		conv   string
		claims *auth.Claims
		quiere platformerrors.Category // CategoryUnknown ⇒ autoriza (nil)
	}{
		{"usuario 1 de la empresa autoriza (sub ≠ cliente_id)", convCli, cliente(usuarioA1, clienteA), platformerrors.CategoryUnknown},
		{"usuario 2 de la misma empresa autoriza", convCli, cliente(usuarioA2, clienteA), platformerrors.CategoryUnknown},
		{"cliente de otro tenant no la encuentra", convCli, cliente(usuarioB1, clienteB), platformerrors.CategoryNoEncontrado},
		{"fila del tenant con cliente_id de otra empresa no autoriza", convAjena, cliente(usuarioA1, clienteA), platformerrors.CategoryNoAutorizado},
		{"ejecutivo sin filas de membresía autoriza", convCli, interno(ejecutivo, "ejecutivo"), platformerrors.CategoryUnknown},
		{"monitorista no autoriza", convCli, interno(monitorista, "monitorista"), platformerrors.CategoryNoAutorizado},
		{"supervisor con fila sembrada no autoriza", convCli, interno(supervisor, "supervisor"), platformerrors.CategoryNoAutorizado},
		{"ejecutivo sobre conversación sin ruta no autoriza", convSinRuta, interno(ejecutivo, "ejecutivo"), platformerrors.CategoryNoAutorizado},
		{"ejecutivo sobre conversación enrutada a Reacción no autoriza", convReacc, interno(ejecutivo, "ejecutivo"), platformerrors.CategoryNoAutorizado},
		{"conversación inexistente es no_encontrado", "99999999-9999-9999-9999-999999999999", interno(ejecutivo, "ejecutivo"), platformerrors.CategoryNoEncontrado},
	}

	for _, c := range casos {
		t.Run(c.nombre, func(t *testing.T) {
			err := svc.PuedeEscribir(ctx, c.claims.TenantID, c.conv, c.claims)
			got := platformerrors.AsCategory(err)
			if got != c.quiere {
				t.Fatalf("PuedeEscribir = %v (err %v), quiere categoría %v", got, err, c.quiere)
			}
		})
	}
}

// TestPuedeVerBandeja fija la guarda de la bandeja por id (DEC-127): la de chat
// la ve todo Ejecutivo sin filas de membresía y ningún otro rol; la de Reacción
// sigue exigiendo membresía en su pool (DEC-028, sin cambio).
func TestPuedeVerBandeja(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	dbtest.Serializar(t, ctx)
	admin, closeAdmin := dbtest.AdminConn(t, ctx)
	defer closeAdmin()

	dbtest.Reiniciar(t, ctx, admin)
	defer dbtest.Reiniciar(t, ctx, admin)
	dbtest.AplicarMigraciones(t, ctx, admin)

	const (
		tenant      = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
		chat        = "cccccccc-cccc-cccc-cccc-cccccccccccc"
		emer        = "c0000000-0000-0000-0000-00000000000e"
		ejecutivo   = "dddddddd-dddd-dddd-dddd-dddddddddddd"
		monitorista = "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee"
		reaccion    = "11111111-1111-1111-1111-111111111111"
		reaccionNo  = "22222222-2222-2222-2222-222222222222"
	)
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.bandeja (id, tenant_id, tipo, nombre) VALUES
		 ('`+chat+`', '`+tenant+`', 'chat', 'Conversaciones'),
		 ('`+emer+`', '`+tenant+`', 'emergencias', 'Reacción')`)
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.pool_membresia (bandeja_id, usuario_id, tenant_id, rol, activa)
		 VALUES ('`+emer+`', '`+reaccion+`', '`+tenant+`', 'reaccion', true)`)

	pool, closePool := dbtest.PoolComoRol(t, ctx)
	defer closePool()
	svc := autorizacion.NewServicio(pool)

	interno := func(sub, rol string) *auth.Claims {
		return &auth.Claims{Subject: sub, TenantID: tenant, Rol: rol, Audiencia: auth.AudienciaInterno}
	}
	casos := []struct {
		nombre  string
		bandeja string
		claims  *auth.Claims
		quiere  platformerrors.Category
	}{
		{"ejecutivo ve la de chat", chat, interno(ejecutivo, "ejecutivo"), platformerrors.CategoryUnknown},
		{"monitorista no ve la de chat", chat, interno(monitorista, "monitorista"), platformerrors.CategoryNoAutorizado},
		{"cliente no lista bandejas", chat, &auth.Claims{Subject: ejecutivo, TenantID: tenant, Rol: "cliente", Audiencia: auth.AudienciaCliente}, platformerrors.CategoryNoAutorizado},
		{"miembro de Reacción ve la de emergencias", emer, interno(reaccion, "reaccion"), platformerrors.CategoryUnknown},
		{"Reacción sin membresía no ve la de emergencias", emer, interno(reaccionNo, "reaccion"), platformerrors.CategoryNoAutorizado},
		{"ejecutivo no ve la de emergencias", emer, interno(ejecutivo, "ejecutivo"), platformerrors.CategoryNoAutorizado},
		{"bandeja inexistente es no_encontrado", "99999999-9999-9999-9999-999999999999", interno(ejecutivo, "ejecutivo"), platformerrors.CategoryNoEncontrado},
	}
	for _, c := range casos {
		t.Run(c.nombre, func(t *testing.T) {
			got := platformerrors.AsCategory(svc.PuedeVerBandeja(ctx, tenant, c.bandeja, c.claims))
			if got != c.quiere {
				t.Fatalf("PuedeVerBandeja = %v, quiere %v", got, c.quiere)
			}
		})
	}
}
