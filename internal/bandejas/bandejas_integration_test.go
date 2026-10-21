package bandejas_test

import (
	"context"
	"testing"
	"time"

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
	items, rev, err := svc.Trabajo(ctx, tenant, bandeja)
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
	cambios, rev2, fuera, err := svc.CambiosDesde(ctx, tenant, bandeja, 1)
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
	cambios2, rev3, _, err := svc.CambiosDesde(ctx, tenant, bandeja, 2)
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
	activas, _, err := svc.Trabajo(ctx, tenant, bandeja)
	if err != nil {
		t.Fatalf("Trabajo tras baja: %v", err)
	}
	if len(activas) != 1 || activas[0].Id.String() != convB {
		t.Fatalf("bootstrap tras baja debe traer solo convB, trajo %d", len(activas))
	}
}
