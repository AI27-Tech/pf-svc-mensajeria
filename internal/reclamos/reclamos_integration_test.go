package reclamos_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/AI27-Tech/pf-svc-mensajeria/internal/dbtest"
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/reclamos"
)

// TestReclamoConcurrenteProduceGanadorYConflicto es la segunda prueba que exige
// el prompt 07: dos reclamos concurrentes sobre la misma conversación, con la
// misma versión esperada, producen EXACTAMENTE un ganador (versión avanzada) y
// EXACTAMENTE un conflicto de versión para el perdedor (ErrConflictoVersion).
//
// Prueba de comportamiento contra Postgres real: afirma el conteo literal de
// ganadores (1) y conflictos (1) y la versión resultante del ganador (2). El
// árbitro real es el UPDATE condicional por (conversacion_id, version) sobre el
// reclamo activo, serializado por el bloqueo de fila de Postgres. Se salta si
// MENSAJERIA_TEST_DB no está definido.
func TestReclamoConcurrenteProduceGanadorYConflicto(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	dbtest.Serializar(t, ctx)
	admin, closeAdmin := dbtest.AdminConn(t, ctx)
	defer closeAdmin()

	dbtest.Reiniciar(t, ctx, admin)
	defer dbtest.Reiniciar(t, ctx, admin)
	dbtest.AplicarMigraciones(t, ctx, admin)

	tenant := "t1"
	convID := "11111111-1111-1111-1111-111111111111"
	a := "22222222-2222-2222-2222-222222222222"
	b := "33333333-3333-3333-3333-333333333333"

	// Semilla: conversación abierta con un reclamo ACTIVO en versión 1.
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.conversacion (id, tenant_id, viaje_id, estado, siguiente_secuencia)
		 VALUES ('`+convID+`', '`+tenant+`', gen_random_uuid(), 'abierta', 0)`)
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.reclamo (conversacion_id, tenant_id, actor_real, version)
		 VALUES ('`+convID+`', '`+tenant+`', '`+a+`', 1)`)

	pool, closePool := dbtest.PoolComoRol(t, ctx)
	defer closePool()
	svc := reclamos.NewServicio(pool)

	// Dos reclamos concurrentes exigiendo la MISMA versión esperada (1).
	type salida struct {
		version int64
		err     error
	}
	var wg sync.WaitGroup
	res := make([]salida, 2)
	actores := []string{a, b}
	inicio := make(chan struct{})
	for i := range actores {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-inicio // soltar las dos goroutines a la vez
			v, err := svc.Reclamar(ctx, tenant, convID, actores[i], 1)
			res[i] = salida{version: int64(v), err: err}
		}(i)
	}
	close(inicio)
	wg.Wait()

	var ganadores, conflictos int
	for _, r := range res {
		switch {
		case r.err == nil:
			ganadores++
			if r.version != 2 {
				t.Fatalf("el ganador debería dejar la versión en 2, dejó %d", r.version)
			}
		case esConflicto(r.err):
			conflictos++
		default:
			t.Fatalf("error inesperado (ni éxito ni conflicto): %v", r.err)
		}
	}
	if ganadores != 1 || conflictos != 1 {
		t.Fatalf("se esperaba 1 ganador y 1 conflicto; hubo %d ganadores y %d conflictos", ganadores, conflictos)
	}
}

func esConflicto(err error) bool {
	var c reclamos.ErrConflictoVersion
	return errors.As(err, &c)
}

// TestLiberarCierraElReclamoYDevuelveAlPool prueba Liberar contra Postgres real
// (addendum §4 punto 6: liberar devuelve el trabajo al pool). Afirma: liberar
// con la versión esperada cierra el reclamo activo (deja de haber uno activo);
// liberar con versión vieja da conflicto y no cierra nada; liberar cuando no hay
// reclamo activo es no_encontrado. Se salta si MENSAJERIA_TEST_DB no está.
func TestLiberarCierraElReclamoYDevuelveAlPool(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	dbtest.Serializar(t, ctx)
	admin, closeAdmin := dbtest.AdminConn(t, ctx)
	defer closeAdmin()

	dbtest.Reiniciar(t, ctx, admin)
	defer dbtest.Reiniciar(t, ctx, admin)
	dbtest.AplicarMigraciones(t, ctx, admin)

	tenant := "t1"
	convID := "11111111-1111-1111-1111-111111111111"
	actor := "22222222-2222-2222-2222-222222222222"

	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.conversacion (id, tenant_id, viaje_id, estado, siguiente_secuencia)
		 VALUES ('`+convID+`', '`+tenant+`', gen_random_uuid(), 'abierta', 0)`)
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.reclamo (conversacion_id, tenant_id, actor_real, version)
		 VALUES ('`+convID+`', '`+tenant+`', '`+actor+`', 1)`)

	pool, closePool := dbtest.PoolComoRol(t, ctx)
	defer closePool()
	svc := reclamos.NewServicio(pool)

	// El reclamo activo existe; obtener su id para liberar por rid.
	activo, err := svc.ReclamoActivo(ctx, tenant, convID)
	if err != nil {
		t.Fatalf("leer reclamo activo: %v", err)
	}
	rid := activo.Id.String()

	// Liberar con versión vieja (0): conflicto, no cierra.
	if err := svc.Liberar(ctx, tenant, convID, rid, actor, 0); !esConflicto(err) {
		t.Fatalf("liberar con versión vieja: quiere conflicto, fue %v", err)
	}
	if _, err := svc.ReclamoActivo(ctx, tenant, convID); err != nil {
		t.Fatalf("tras conflicto el reclamo debe seguir activo, ReclamoActivo dio: %v", err)
	}

	// Liberar con la versión esperada (1): cierra el reclamo.
	if err := svc.Liberar(ctx, tenant, convID, rid, actor, 1); err != nil {
		t.Fatalf("liberar con versión correcta: %v", err)
	}
	if _, err := svc.ReclamoActivo(ctx, tenant, convID); !errors.Is(err, reclamos.ErrSinReclamo) {
		t.Fatalf("tras liberar no debe haber reclamo activo, ReclamoActivo dio: %v", err)
	}

	// Liberar de nuevo: ya no hay reclamo activo con ese rid → no_encontrado.
	if err := svc.Liberar(ctx, tenant, convID, rid, actor, 1); !errors.Is(err, reclamos.ErrSinReclamo) {
		t.Fatalf("liberar sin reclamo activo: quiere ErrSinReclamo, fue %v", err)
	}
}

// TestTransferirCierraYAbreEnUnaTx prueba Transferir contra Postgres real
// (addendum §4 punto 6 y §6: transferir cierra el reclamo anterior y abre el
// siguiente en una sola transacción). Afirma: transferir con la versión esperada
// cierra el reclamo del actor y abre uno activo para el destino, en versión 1;
// con versión vieja da conflicto y no cambia nada; sin reclamo activo da
// ErrSinReclamo. Se salta si MENSAJERIA_TEST_DB no está.
func TestTransferirCierraYAbreEnUnaTx(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	dbtest.Serializar(t, ctx)
	admin, closeAdmin := dbtest.AdminConn(t, ctx)
	defer closeAdmin()

	dbtest.Reiniciar(t, ctx, admin)
	defer dbtest.Reiniciar(t, ctx, admin)
	dbtest.AplicarMigraciones(t, ctx, admin)

	tenant := "t1"
	convID := "11111111-1111-1111-1111-111111111111"
	origen := "22222222-2222-2222-2222-222222222222"
	destino := "33333333-3333-3333-3333-333333333333"

	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.conversacion (id, tenant_id, viaje_id, estado, siguiente_secuencia)
		 VALUES ('`+convID+`', '`+tenant+`', gen_random_uuid(), 'abierta', 0)`)
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.reclamo (conversacion_id, tenant_id, actor_real, version)
		 VALUES ('`+convID+`', '`+tenant+`', '`+origen+`', 1)`)

	pool, closePool := dbtest.PoolComoRol(t, ctx)
	defer closePool()
	svc := reclamos.NewServicio(pool)

	// Versión vieja (0): conflicto, el reclamo del origen sigue activo.
	if err := svc.Transferir(ctx, tenant, convID, origen, destino, "", 0); !esConflicto(err) {
		t.Fatalf("transferir con versión vieja: quiere conflicto, fue %v", err)
	}
	activo, err := svc.ReclamoActivo(ctx, tenant, convID)
	if err != nil {
		t.Fatalf("tras conflicto debe seguir el reclamo del origen: %v", err)
	}
	if activo.ResponsableId.String() != origen {
		t.Fatalf("tras conflicto el responsable debe ser el origen, es %s", activo.ResponsableId)
	}

	// Versión esperada (1): cierra el del origen, abre el del destino en versión 1.
	if err := svc.Transferir(ctx, tenant, convID, origen, destino, "escalamiento", 1); err != nil {
		t.Fatalf("transferir con versión correcta: %v", err)
	}
	activo, err = svc.ReclamoActivo(ctx, tenant, convID)
	if err != nil {
		t.Fatalf("tras transferir debe haber un reclamo activo: %v", err)
	}
	if activo.ResponsableId.String() != destino {
		t.Fatalf("tras transferir el responsable debe ser el destino, es %s", activo.ResponsableId)
	}
	if activo.Version != 1 {
		t.Fatalf("el reclamo del destino debe abrir en versión 1, abrió en %d", activo.Version)
	}

	// Verificar que hay EXACTAMENTE un reclamo activo (no dos): el índice único
	// un_reclamo_activo_por_conversacion lo garantiza; esto lo afirma de cara al
	// comportamiento observable.
	var activos int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM mensajeria.reclamo WHERE conversacion_id = $1 AND cerrado_en IS NULL`,
		convID,
	).Scan(&activos); err != nil {
		t.Fatalf("contar reclamos activos: %v", err)
	}
	if activos != 1 {
		t.Fatalf("debe haber exactamente 1 reclamo activo tras transferir, hay %d", activos)
	}
}
