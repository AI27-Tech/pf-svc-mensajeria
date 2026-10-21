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
// (DEC-097). Afirma, contra Postgres real, las dos ramas por audiencia:
//   - Cliente: autoriza solo si el cliente_id de la conversación coincide con el
//     Subject del token; de lo contrario no_autorizado (addendum §7: poseer el
//     UUID no autoriza).
//   - Interno: autoriza solo si hay membresía ACTIVA en el pool de la bandeja a
//     la que la conversación está enrutada; sin membresía, no_autorizado.
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
		tenant   = "t1"
		convCli  = "11111111-1111-1111-1111-111111111111"
		convInt  = "22222222-2222-2222-2222-222222222222"
		clienteA = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
		clienteB = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
		bandeja  = "cccccccc-cccc-cccc-cccc-cccccccccccc"
		ejecMiem = "dddddddd-dddd-dddd-dddd-dddddddddddd"
		ejecOtro = "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee"
	)

	// Conversación del cliente A, enrutada a la bandeja.
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.conversacion (id, tenant_id, viaje_id, cliente_id, estado, siguiente_secuencia)
		 VALUES ('`+convCli+`', '`+tenant+`', gen_random_uuid(), '`+clienteA+`', 'abierta', 0)`)
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.conversacion (id, tenant_id, viaje_id, cliente_id, estado, siguiente_secuencia)
		 VALUES ('`+convInt+`', '`+tenant+`', gen_random_uuid(), '`+clienteA+`', 'abierta', 0)`)
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.bandeja (id, tenant_id, tipo, nombre)
		 VALUES ('`+bandeja+`', '`+tenant+`', 'chat', 'cliente A')`)
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.ruta_conversacion (conversacion_id, tenant_id, bandeja_id, activa)
		 VALUES ('`+convCli+`', '`+tenant+`', '`+bandeja+`', true),
		        ('`+convInt+`', '`+tenant+`', '`+bandeja+`', true)`)
	// Ejecutivo miembro activo del pool de la bandeja; otro ejecutivo sin membresía.
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.pool_membresia (bandeja_id, usuario_id, tenant_id, rol, version, activa)
		 VALUES ('`+bandeja+`', '`+ejecMiem+`', '`+tenant+`', 'ejecutivo', 1, true)`)

	pool, closePool := dbtest.PoolComoRol(t, ctx)
	defer closePool()

	svc := autorizacion.NewServicio(pool)

	cliente := func(sub string) *auth.Claims {
		return &auth.Claims{Subject: sub, TenantID: tenant, Rol: "cliente", Audiencia: auth.AudienciaCliente}
	}
	interno := func(sub string) *auth.Claims {
		return &auth.Claims{Subject: sub, TenantID: tenant, Rol: "ejecutivo", Audiencia: auth.AudienciaInterno}
	}

	casos := []struct {
		nombre string
		conv   string
		claims *auth.Claims
		quiere platformerrors.Category // CategoryUnknown ⇒ autoriza (nil)
	}{
		{"cliente dueño autoriza", convCli, cliente(clienteA), platformerrors.CategoryUnknown},
		{"cliente ajeno no autoriza", convCli, cliente(clienteB), platformerrors.CategoryNoAutorizado},
		{"interno con membresía autoriza", convInt, interno(ejecMiem), platformerrors.CategoryUnknown},
		{"interno sin membresía no autoriza", convInt, interno(ejecOtro), platformerrors.CategoryNoAutorizado},
		{"conversación inexistente es no_encontrado", "99999999-9999-9999-9999-999999999999", interno(ejecMiem), platformerrors.CategoryNoEncontrado},
	}

	for _, c := range casos {
		t.Run(c.nombre, func(t *testing.T) {
			err := svc.PuedeEscribir(ctx, tenant, c.conv, c.claims)
			got := platformerrors.AsCategory(err)
			if got != c.quiere {
				t.Fatalf("PuedeEscribir = %v (err %v), quiere categoría %v", got, err, c.quiere)
			}
		})
	}
}
