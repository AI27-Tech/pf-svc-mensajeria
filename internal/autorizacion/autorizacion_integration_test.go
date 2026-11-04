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

// TestConversacion es la prueba de comportamiento de la guarda de conversación
// (DEC-124, DEC-127, DEC-129, DEC-131, DEC-132), contra Postgres real:
//   - Cliente: cualquier usuario cuyo tenant sea el cliente_id lee y escribe; un
//     cliente de otra empresa recibe no_encontrado, también ante una fila con
//     cliente_id de otra empresa.
//   - Interno con el tenant de AI27 (no el del cliente): el Ejecutivo lee y
//     escribe sin filas de membresía; Supervisor y Coordinador solo leen; otro
//     rol, o una conversación sin ruta a chat, es prohibido. La guarda devuelve
//     el tenant de la conversación, no el del token.
//   - Inexistente: no_encontrado para todos.
//
// Se salta si MENSAJERIA_TEST_DB no está definido.
func TestConversacion(t *testing.T) {
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
		// tenantAI27 es el tenant con que llega un usuario interno (DEC-129).
		tenantAI27 = "ai27"
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
	// Fila sembrada al estilo DEC-097 para un monitorista: ya no es fuente de
	// membresía (DEC-127 #3) y no debe conceder acceso.
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.pool_membresia (bandeja_id, usuario_id, tenant_id, rol, version, activa)
		 VALUES ('`+bandeja+`', '`+monitorista+`', '`+clienteA+`', 'monitorista', 1, true)`)

	pool, closePool := dbtest.PoolComoRol(t, ctx)
	defer closePool()

	svc := autorizacion.NewServicio(pool)

	cliente := func(sub, tenant string) *auth.Claims {
		return &auth.Claims{Subject: sub, TenantID: tenant, Rol: "cliente", Audiencia: auth.AudienciaCliente}
	}
	interno := func(sub, rol string) *auth.Claims {
		return &auth.Claims{Subject: sub, TenantID: tenantAI27, Rol: rol, Audiencia: auth.AudienciaInterno}
	}
	const (
		ok          = platformerrors.CategoryUnknown // autoriza
		noExiste    = platformerrors.CategoryNoEncontrado
		prohibido   = platformerrors.CategoryProhibido
		inexistente = "99999999-9999-9999-9999-999999999999"
	)

	casos := []struct {
		nombre string
		conv   string
		claims *auth.Claims
		accion autorizacion.Accion
		quiere platformerrors.Category
	}{
		{"usuario 1 de la empresa escribe (sub ≠ cliente_id)", convCli, cliente(usuarioA1, clienteA), autorizacion.Escribir, ok},
		{"usuario 2 de la misma empresa lee", convCli, cliente(usuarioA2, clienteA), autorizacion.Leer, ok},
		{"cliente de otra empresa no la encuentra", convCli, cliente(usuarioB1, clienteB), autorizacion.Leer, noExiste},
		{"fila del tenant con cliente_id de otra empresa no la encuentra", convAjena, cliente(usuarioA1, clienteA), autorizacion.Leer, noExiste},
		{"ejecutivo con tenant de AI27 escribe sin filas de membresía", convCli, interno(ejecutivo, "ejecutivo"), autorizacion.Escribir, ok},
		{"supervisor lee", convCli, interno(supervisor, "supervisor"), autorizacion.Leer, ok},
		{"coordinador lee", convCli, interno(supervisor, "coordinador"), autorizacion.Leer, ok},
		{"supervisor no escribe", convCli, interno(supervisor, "supervisor"), autorizacion.Escribir, prohibido},
		{"coordinador no escribe", convCli, interno(supervisor, "coordinador"), autorizacion.Escribir, prohibido},
		{"monitorista con fila sembrada no lee", convCli, interno(monitorista, "monitorista"), autorizacion.Leer, prohibido},
		{"ejecutivo sobre conversación sin ruta", convSinRuta, interno(ejecutivo, "ejecutivo"), autorizacion.Leer, prohibido},
		{"ejecutivo sobre conversación enrutada a Reacción", convReacc, interno(ejecutivo, "ejecutivo"), autorizacion.Leer, prohibido},
		{"inexistente para el interno", inexistente, interno(ejecutivo, "ejecutivo"), autorizacion.Leer, noExiste},
		{"inexistente para el cliente", inexistente, cliente(usuarioA1, clienteA), autorizacion.Leer, noExiste},
	}

	for _, c := range casos {
		t.Run(c.nombre, func(t *testing.T) {
			tenant, err := svc.Conversacion(ctx, c.conv, c.claims, c.accion)
			if got := platformerrors.AsCategory(err); got != c.quiere {
				t.Fatalf("Conversacion = %v (err %v), quiere categoría %v", got, err, c.quiere)
			}
			if c.quiere == ok && tenant != clienteA {
				t.Fatalf("tenant devuelto = %q, quiere el de la conversación %q", tenant, clienteA)
			}
		})
	}
}

// TestBandeja fija la guarda de la bandeja por id (DEC-127, DEC-129, DEC-130,
// DEC-131, DEC-132): la de chat la leen Ejecutivo, Supervisor y Coordinador; la
// de emergencias Reacción por su rol (sin filas), Supervisor y Coordinador. El
// interno llega con el tenant de AI27 y recibe el tenant de la bandeja. Otro rol
// es prohibido; el cliente no la encuentra.
func TestBandeja(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	dbtest.Serializar(t, ctx)
	admin, closeAdmin := dbtest.AdminConn(t, ctx)
	defer closeAdmin()

	dbtest.Reiniciar(t, ctx, admin)
	defer dbtest.Reiniciar(t, ctx, admin)
	dbtest.AplicarMigraciones(t, ctx, admin)

	const (
		tenant   = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
		chat     = "cccccccc-cccc-cccc-cccc-cccccccccccc"
		emer     = "c0000000-0000-0000-0000-00000000000e"
		sujeto   = "dddddddd-dddd-dddd-dddd-dddddddddddd"
		ok       = platformerrors.CategoryUnknown
		noExiste = platformerrors.CategoryNoEncontrado
		prohib   = platformerrors.CategoryProhibido
	)
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.bandeja (id, tenant_id, tipo, nombre) VALUES
		 ('`+chat+`', '`+tenant+`', 'chat', 'Conversaciones'),
		 ('`+emer+`', '`+tenant+`', 'emergencias', 'Reacción')`)

	pool, closePool := dbtest.PoolComoRol(t, ctx)
	defer closePool()
	svc := autorizacion.NewServicio(pool)

	interno := func(rol string) *auth.Claims {
		return &auth.Claims{Subject: sujeto, TenantID: "ai27", Rol: rol, Audiencia: auth.AudienciaInterno}
	}
	casos := []struct {
		nombre  string
		bandeja string
		claims  *auth.Claims
		quiere  platformerrors.Category
	}{
		{"ejecutivo ve la de chat", chat, interno("ejecutivo"), ok},
		{"supervisor ve la de chat", chat, interno("supervisor"), ok},
		{"coordinador ve la de chat", chat, interno("coordinador"), ok},
		{"monitorista no ve la de chat", chat, interno("monitorista"), prohib},
		{"reaccion no ve la de chat", chat, interno("reaccion"), prohib},
		{"cliente no encuentra bandejas", chat, &auth.Claims{Subject: sujeto, TenantID: tenant, Rol: "cliente", Audiencia: auth.AudienciaCliente}, noExiste},
		{"reaccion ve la de emergencias sin filas de membresía", emer, interno("reaccion"), ok},
		{"supervisor ve la de emergencias", emer, interno("supervisor"), ok},
		{"ejecutivo no ve la de emergencias", emer, interno("ejecutivo"), prohib},
		{"bandeja inexistente es no_encontrado", "99999999-9999-9999-9999-999999999999", interno("ejecutivo"), noExiste},
	}
	for _, c := range casos {
		t.Run(c.nombre, func(t *testing.T) {
			got, err := svc.Bandeja(ctx, c.bandeja, c.claims)
			if cat := platformerrors.AsCategory(err); cat != c.quiere {
				t.Fatalf("Bandeja = %v (err %v), quiere %v", cat, err, c.quiere)
			}
			if c.quiere == ok && got != tenant {
				t.Fatalf("tenant devuelto = %q, quiere %q", got, tenant)
			}
		})
	}
}

// TestEmergencia fija la guarda del caso de emergencia por permiso (DEC-129 a
// DEC-132): Reacción es miembro por su rol sin filas en pool_membresia;
// Supervisor y Coordinador ven y mueven pero no atienden; cierra monitoreo o
// Administrador. Interno sin permiso → prohibido; Cliente sobre un caso de otra
// empresa → no_encontrado; Cliente sobre uno suyo → prohibido; inexistente →
// no_encontrado.
func TestEmergencia(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	dbtest.Serializar(t, ctx)
	admin, closeAdmin := dbtest.AdminConn(t, ctx)
	defer closeAdmin()

	dbtest.Reiniciar(t, ctx, admin)
	defer dbtest.Reiniciar(t, ctx, admin)
	dbtest.AplicarMigraciones(t, ctx, admin)

	const (
		clienteA = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
		clienteB = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
		emer     = "c0000000-0000-0000-0000-00000000000e"
		caso     = "e0000000-0000-0000-0000-000000000001"
		sujeto   = "dddddddd-dddd-dddd-dddd-dddddddddddd"
		ok       = platformerrors.CategoryUnknown
		noExiste = platformerrors.CategoryNoEncontrado
		prohib   = platformerrors.CategoryProhibido
	)
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.bandeja (id, tenant_id, tipo, nombre)
		 VALUES ('`+emer+`', '`+clienteA+`', 'emergencias', 'Emergencias')`)
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.emergencia (id, tenant_id, viaje_id, bandeja_id, origen, actor_real, activacion_id)
		 VALUES ('`+caso+`', '`+clienteA+`', gen_random_uuid(), '`+emer+`', 'cliente', '`+sujeto+`', gen_random_uuid())`)

	pool, closePool := dbtest.PoolComoRol(t, ctx)
	defer closePool()
	svc := autorizacion.NewServicio(pool)

	interno := func(rol string) *auth.Claims {
		return &auth.Claims{Subject: sujeto, TenantID: "ai27", Rol: rol, Audiencia: auth.AudienciaInterno}
	}
	cliente := func(tenant string) *auth.Claims {
		return &auth.Claims{Subject: sujeto, TenantID: tenant, Rol: "cliente", Audiencia: auth.AudienciaCliente}
	}
	type fila struct {
		claims *auth.Claims
		quiere [4]platformerrors.Category // VerCaso, AtenderCaso, MoverCaso, CerrarCaso
	}
	matriz := map[string]fila{
		"reaccion":                {interno("reaccion"), [4]platformerrors.Category{ok, ok, ok, prohib}},
		"supervisor":              {interno("supervisor"), [4]platformerrors.Category{ok, prohib, ok, ok}},
		"coordinador":             {interno("coordinador"), [4]platformerrors.Category{ok, prohib, ok, ok}},
		"monitorista":             {interno("monitorista"), [4]platformerrors.Category{prohib, prohib, prohib, ok}},
		"administrador":           {interno("administrador"), [4]platformerrors.Category{prohib, prohib, prohib, ok}},
		"ejecutivo":               {interno("ejecutivo"), [4]platformerrors.Category{prohib, prohib, prohib, prohib}},
		"cliente de la empresa":   {cliente(clienteA), [4]platformerrors.Category{prohib, prohib, prohib, prohib}},
		"cliente de otra empresa": {cliente(clienteB), [4]platformerrors.Category{noExiste, noExiste, noExiste, noExiste}},
	}
	permisos := []autorizacion.Permiso{autorizacion.VerCaso, autorizacion.AtenderCaso, autorizacion.MoverCaso, autorizacion.CerrarCaso}
	for nombre, f := range matriz {
		t.Run(nombre, func(t *testing.T) {
			var got [4]platformerrors.Category
			for i, p := range permisos {
				tenant, err := svc.Emergencia(ctx, caso, f.claims, p)
				got[i] = platformerrors.AsCategory(err)
				if err == nil && tenant != clienteA {
					t.Fatalf("permiso %d: tenant %q, quiere %q", i, tenant, clienteA)
				}
			}
			if got != f.quiere {
				t.Fatalf("categorías [ver atender mover cerrar] = %v, quiere %v", got, f.quiere)
			}
		})
	}

	if _, err := svc.Emergencia(ctx, "99999999-9999-9999-9999-999999999999", interno("reaccion"), autorizacion.VerCaso); platformerrors.AsCategory(err) != noExiste {
		t.Fatalf("caso inexistente: %v, quiere no_encontrado", err)
	}
	if tenant, err := svc.TenantDeEmergencia(ctx, caso, cliente(clienteA)); err != nil || tenant != clienteA {
		t.Fatalf("TenantDeEmergencia del cliente dueño = %q, %v; quiere %q", tenant, err, clienteA)
	}
}
