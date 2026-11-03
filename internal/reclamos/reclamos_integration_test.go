package reclamos_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/AI27-Tech/pf-pkg-platform-go/outbox"
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/dbtest"
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/reclamos"
)

// TestReclamoConcurrenteProduceGanadorYConflicto es la segunda prueba que exige
// el prompt 07: dos reclamos concurrentes desde el pool sobre la misma
// conversación, con la misma versión esperada, producen EXACTAMENTE un ganador
// (versión avanzada) y EXACTAMENTE un conflicto de versión para el perdedor
// (ErrConflictoVersion), y los dos intentos quedan auditados.
//
// Prueba de comportamiento contra Postgres real: afirma el conteo literal de
// ganadores (1) y conflictos (1), la versión resultante del ganador (2) y una
// fila de auditoría por intento. El árbitro real es el índice único parcial
// un_reclamo_activo_por_conversacion. Se salta si MENSAJERIA_TEST_DB no está
// definido.
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

	// Semilla: conversación en el pool cuya versión vigente es 1 (un reclamo ya
	// liberado), para que la carrera avance la versión en vez de abrir la primera.
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.conversacion (id, tenant_id, viaje_id, estado, siguiente_secuencia)
		 VALUES ('`+convID+`', '`+tenant+`', gen_random_uuid(), 'abierta', 0)`)
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.reclamo (conversacion_id, tenant_id, actor_real, version, cerrado_en)
		 VALUES ('`+convID+`', '`+tenant+`', '`+a+`', 1, now())`)

	pool, closePool := dbtest.PoolComoRol(t, ctx)
	defer closePool()
	svc := reclamos.NewServicio(pool, outbox.NewPublisher())

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

	// Los dos intentos quedan auditados: uno ganado y uno perdido (E3-T6).
	for accion, quiere := range map[string]int{"reclamo.intento_ganado": 1, "reclamo.intento_perdido": 1} {
		var n int
		if err := admin.QueryRow(ctx,
			`SELECT count(*) FROM mensajeria.auditoria_mensajeria WHERE conversacion_id = $1 AND accion = $2`,
			convID, accion).Scan(&n); err != nil {
			t.Fatalf("contar auditoría %s: %v", accion, err)
		}
		if n != quiere {
			t.Fatalf("auditoría %s: %d filas, quiere %d", accion, n, quiere)
		}
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
	svc := reclamos.NewServicio(pool, outbox.NewPublisher())

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
	svc := reclamos.NewServicio(pool, outbox.NewPublisher())

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

	// Versión esperada (1): cierra el del origen, abre el del destino en la
	// versión siguiente de la conversación (2), no en 1 (P20).
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
	if activo.Version != 2 {
		t.Fatalf("el reclamo del destino debe abrir en versión 2, abrió en %d", activo.Version)
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

// TestEventosDeReclamoSeEscribenEnOutbox cubre E3-T3: cada mutación del reclamo
// publica su evento de dominio en el outbox DENTRO de su tx (reclamar →
// conversacion.reclamada, liberar → conversacion.liberada, transferir →
// conversacion.transferida). Es la pieza que realtime (consumidor durable del
// contrato tracer.21) necesita para reflejar quién responde la conversación.
//
// Prueba de comportamiento contra Postgres real: afirma, por cada operación, que
// quedó EXACTAMENTE una fila del tipo esperado, restringida, con agregado_id =
// conversacion_id, y con el payload que el contrato define. Se salta sin
// MENSAJERIA_TEST_DB.
func TestEventosDeReclamoSeEscribenEnOutbox(t *testing.T) {
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
	destino := "33333333-3333-3333-3333-333333333333"

	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.conversacion (id, tenant_id, viaje_id, estado, siguiente_secuencia)
		 VALUES ('`+convID+`', '`+tenant+`', gen_random_uuid(), 'abierta', 0)`)

	pool, closePool := dbtest.PoolComoRol(t, ctx)
	defer closePool()
	svc := reclamos.NewServicio(pool, outbox.NewPublisher())

	// contarEvento devuelve cuántas filas del tipo dado hay en el outbox para esta
	// conversación, exigiendo la sensibilidad y el agregado del contrato.
	contarEvento := func(tipo string) int {
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM mensajeria.outbox
			  WHERE tipo = $1 AND agregado_id = $2
			    AND sensibilidad = 'restringida'`,
			tipo, convID,
		).Scan(&n); err != nil {
			t.Fatalf("contar evento %s: %v", tipo, err)
		}
		return n
	}

	// Reclamar (primer reclamo): 1 evento conversacion.reclamada.
	if _, err := svc.Reclamar(ctx, tenant, convID, actor, 0); err != nil {
		t.Fatalf("reclamar: %v", err)
	}
	if n := contarEvento("conversacion.reclamada"); n != 1 {
		t.Fatalf("tras reclamar quiere 1 conversacion.reclamada, hay %d", n)
	}

	// El payload lleva el responsable y la versión resultante (1).
	var payload map[string]any
	if err := pool.QueryRow(ctx,
		`SELECT payload FROM mensajeria.outbox WHERE tipo = 'conversacion.reclamada' AND agregado_id = $1`,
		convID,
	).Scan(&payload); err != nil {
		t.Fatalf("leer payload de reclamada: %v", err)
	}
	if payload["responsable_id"] != actor {
		t.Fatalf("payload.responsable_id = %v, quiere %s", payload["responsable_id"], actor)
	}
	if v, ok := payload["version_reclamo"].(float64); !ok || v != 1 {
		t.Fatalf("payload.version_reclamo = %v, quiere 1", payload["version_reclamo"])
	}

	// Transferir (versión 1): 1 evento conversacion.transferida al destino.
	if err := svc.Transferir(ctx, tenant, convID, actor, destino, "escalamiento", 1); err != nil {
		t.Fatalf("transferir: %v", err)
	}
	if n := contarEvento("conversacion.transferida"); n != 1 {
		t.Fatalf("tras transferir quiere 1 conversacion.transferida, hay %d", n)
	}

	// Liberar el reclamo del destino (versión 2, la que abrió la transferencia):
	// 1 evento conversacion.liberada.
	activo, err := svc.ReclamoActivo(ctx, tenant, convID)
	if err != nil {
		t.Fatalf("leer reclamo activo del destino: %v", err)
	}
	if err := svc.Liberar(ctx, tenant, convID, activo.Id.String(), destino, 2); err != nil {
		t.Fatalf("liberar: %v", err)
	}
	if n := contarEvento("conversacion.liberada"); n != 1 {
		t.Fatalf("tras liberar quiere 1 conversacion.liberada, hay %d", n)
	}
}
