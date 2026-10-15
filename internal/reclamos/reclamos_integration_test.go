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
