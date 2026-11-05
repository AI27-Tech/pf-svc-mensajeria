package bandejas_test

import (
	"context"
	"slices"
	"testing"
	"time"

	msgclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/mensajeria"
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/bandejas"
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/dbtest"
)

// TestTrabajoYCatchUp prueba el bootstrap y el catch-up incremental de la
// colección de bandeja (M3, ADR 28 §3) contra Postgres real. Afirma:
//   - Trabajo (bootstrap): devuelve las conversaciones con ruta ACTIVA en la
//     bandeja y la revisión actual de la colección.
//   - CambiosDesde (catch-up): devuelve solo las rutas con revisión > desde,
//     incluyendo una BAJA (ruta inactiva) para que el cliente la retire.
//
// Se salta si MENSAJERIA_TEST_DB no está definido.
func TestTrabajoYCatchUp(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	dbtest.Serializar(t, ctx)
	admin, closeAdmin := dbtest.AdminConn(t, ctx)
	defer closeAdmin()

	dbtest.Reiniciar(t, ctx, admin)
	defer dbtest.Reiniciar(t, ctx, admin)
	dbtest.AplicarMigraciones(t, ctx, admin)

	const (
		tenant  = "t1"
		bandeja = "cccccccc-cccc-cccc-cccc-cccccccccccc"
		convA   = "11111111-1111-1111-1111-111111111111"
		convB   = "22222222-2222-2222-2222-222222222222"
		cliente = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	)
	// Bandeja en revisión 2 (dos altas ocurrieron). Dos conversaciones: A entró
	// en revisión 1, B en revisión 2.
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.bandeja (id, tenant_id, tipo, nombre, revision)
		 VALUES ('`+bandeja+`', '`+tenant+`', 'chat', 'cliente A', 2)`)
	for _, c := range []string{convA, convB} {
		dbtest.Exec(t, ctx, admin,
			`INSERT INTO mensajeria.conversacion (id, tenant_id, viaje_id, cliente_id, estado, siguiente_secuencia)
			 VALUES ('`+c+`', '`+tenant+`', gen_random_uuid(), '`+cliente+`', 'abierta', 0)`)
	}
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.ruta_conversacion (conversacion_id, tenant_id, bandeja_id, activa, revision)
		 VALUES ('`+convA+`', '`+tenant+`', '`+bandeja+`', true, 1),
		        ('`+convB+`', '`+tenant+`', '`+bandeja+`', true, 2)`)

	pool, closePool := dbtest.PoolComoRol(t, ctx)
	defer closePool()
	svc := bandejas.NewServicio(pool)

	// Bootstrap: ambas conversaciones activas, revisión de colección = 2.
	items, rev, err := svc.Trabajo(ctx, tenant, bandeja, "")
	if err != nil {
		t.Fatalf("Trabajo: %v", err)
	}
	if rev != 2 {
		t.Fatalf("revisión de bootstrap = %d, quiere 2", rev)
	}
	if len(items) != 2 {
		t.Fatalf("bootstrap trae %d conversaciones, quiere 2", len(items))
	}

	// Catch-up desde la revisión 1: solo lo posterior (convB, que entró en 2).
	cambios, rev2, fuera, err := svc.CambiosDesde(ctx, tenant, bandeja, "", 1)
	if err != nil {
		t.Fatalf("CambiosDesde: %v", err)
	}
	if fuera {
		t.Fatalf("revisión 1 no debería estar fuera de ventana")
	}
	if rev2 != 2 {
		t.Fatalf("revisión de catch-up = %d, quiere 2", rev2)
	}
	if len(cambios) != 1 || cambios[0].Id.String() != convB {
		t.Fatalf("catch-up desde 1 debe traer solo convB, trajo %d items", len(cambios))
	}

	// Una BAJA: convA sale de la bandeja (activa=false) y avanza la revisión a 3.
	dbtest.Exec(t, ctx, admin, `UPDATE mensajeria.bandeja SET revision = 3 WHERE id = '`+bandeja+`'`)
	dbtest.Exec(t, ctx, admin,
		`UPDATE mensajeria.ruta_conversacion SET activa = false, revision = 3
		  WHERE conversacion_id = '`+convA+`' AND bandeja_id = '`+bandeja+`'`)

	// Catch-up desde 2: trae convA como baja (ruta inactiva en revisión 3).
	cambios2, rev3, _, err := svc.CambiosDesde(ctx, tenant, bandeja, "", 2)
	if err != nil {
		t.Fatalf("CambiosDesde tras baja: %v", err)
	}
	if rev3 != 3 {
		t.Fatalf("revisión tras baja = %d, quiere 3", rev3)
	}
	if len(cambios2) != 1 || cambios2[0].Id.String() != convA {
		t.Fatalf("catch-up desde 2 debe traer la baja de convA, trajo %d items", len(cambios2))
	}
	// Tras la baja, el bootstrap ya no incluye convA (solo rutas activas).
	activas, _, err := svc.Trabajo(ctx, tenant, bandeja, "")
	if err != nil {
		t.Fatalf("Trabajo tras baja: %v", err)
	}
	if len(activas) != 1 || activas[0].Id.String() != convB {
		t.Fatalf("bootstrap tras baja debe traer solo convB, trajo %d", len(activas))
	}
}

// TestTrabajoChatUneTenants prueba la bandeja de conversaciones del interno
// (DEC-129) contra Postgres real: la unión de las bandejas de chat de dos tenants
// con una revisión coherente. EnrutarABandejaUnica estampa cada alta con la
// revisión siguiente de TODAS las bandejas de chat, así que:
//   - altas A1, B1, A2 toman las revisiones 1, 2 y 3;
//   - TrabajoChat trae las tres con revisión 3; CambiosChatDesde(1) trae B1 y A2;
//   - la bandeja de A sigue con catch-up propio monotónico: revisión 3,
//     CambiosDesde(A, 1) trae A2;
//   - una bandeja de emergencias no entra a la unión.
//
// Se salta si MENSAJERIA_TEST_DB no está definido.
func TestTrabajoChatUneTenants(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	dbtest.Serializar(t, ctx)
	admin, closeAdmin := dbtest.AdminConn(t, ctx)
	defer closeAdmin()

	dbtest.Reiniciar(t, ctx, admin)
	defer dbtest.Reiniciar(t, ctx, admin)
	dbtest.AplicarMigraciones(t, ctx, admin)

	const (
		tA = "a0000000-0000-0000-0000-00000000000a"
		tB = "b0000000-0000-0000-0000-00000000000b"
		a1 = "a1000000-0000-0000-0000-000000000001"
		a2 = "a2000000-0000-0000-0000-000000000002"
		b1 = "b1000000-0000-0000-0000-000000000001"
	)
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.conversacion (id, tenant_id, viaje_id, cliente_id) VALUES
		 ('`+a1+`', '`+tA+`', gen_random_uuid(), '`+tA+`'),
		 ('`+a2+`', '`+tA+`', gen_random_uuid(), '`+tA+`'),
		 ('`+b1+`', '`+tB+`', gen_random_uuid(), '`+tB+`')`)
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.bandeja (tenant_id, tipo, nombre, revision)
		 VALUES ('`+tA+`', 'emergencias', 'Emergencias', 9)`)

	pool, closePool := dbtest.PoolComoRol(t, ctx)
	defer closePool()
	for _, c := range [][2]string{{tA, a1}, {tB, b1}, {tA, a2}} {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("abrir tx: %v", err)
		}
		if err := bandejas.EnrutarABandejaUnica(ctx, tx, c[0], c[1]); err != nil {
			t.Fatalf("enrutar %s: %v", c[1], err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}
	svc := bandejas.NewServicio(pool)

	ids := func(items []msgclient.Conversacion) []string {
		var out []string
		for _, c := range items {
			out = append(out, c.Id.String())
		}
		return out
	}

	items, rev, err := svc.TrabajoChat(ctx, "")
	if err != nil {
		t.Fatalf("TrabajoChat: %v", err)
	}
	if got, want := ids(items), []string{a1, b1, a2}; rev != 3 || !slices.Equal(got, want) {
		t.Fatalf("TrabajoChat = %v rev %d, quiere %v rev 3", got, rev, want)
	}

	items, rev, fuera, err := svc.CambiosChatDesde(ctx, "", 1)
	if err != nil {
		t.Fatalf("CambiosChatDesde: %v", err)
	}
	if got, want := ids(items), []string{b1, a2}; rev != 3 || fuera || !slices.Equal(got, want) {
		t.Fatalf("CambiosChatDesde(1) = %v rev %d fuera %v, quiere %v rev 3", got, rev, fuera, want)
	}

	var bandejaA string
	if err := admin.QueryRow(ctx,
		`SELECT id::text FROM mensajeria.bandeja WHERE tenant_id = $1 AND tipo = 'chat'`, tA).Scan(&bandejaA); err != nil {
		t.Fatalf("leer bandeja de A: %v", err)
	}
	items, rev, _, err = svc.CambiosDesde(ctx, tA, bandejaA, "", 1)
	if err != nil {
		t.Fatalf("CambiosDesde(A): %v", err)
	}
	if got, want := ids(items), []string{a2}; rev != 3 || !slices.Equal(got, want) {
		t.Fatalf("CambiosDesde(A, 1) = %v rev %d, quiere %v rev 3", got, rev, want)
	}
}
