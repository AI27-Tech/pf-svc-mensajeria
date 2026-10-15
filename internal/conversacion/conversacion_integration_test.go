package conversacion_test

import (
	"context"
	"testing"
	"time"

	"github.com/AI27-Tech/pf-pkg-platform-go/outbox"
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/conversacion"
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/dbtest"
)

// TestEnviarEscribeMensajeYOutboxEnLaMismaTx es la primera prueba que exige el
// prompt 07: enviar un mensaje escribe `mensaje` y su fila de outbox en la MISMA
// transacción, y un reintento con el mismo client_message_id devuelve el mensaje
// original sin crear un segundo mensaje ni un segundo evento (idempotencia por
// client_message_id).
//
// Es una prueba de COMPORTAMIENTO contra Postgres real (como el dbtest de
// reportes): afirma valores literales observables —conteos de filas, la
// secuencia asignada, la igualdad del id entre envío y reintento—. Se salta si
// MENSAJERIA_TEST_DB no está definido.
func TestEnviarEscribeMensajeYOutboxEnLaMismaTx(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	dbtest.Serializar(t, ctx)
	admin, closeAdmin := dbtest.AdminConn(t, ctx)
	defer closeAdmin()

	dbtest.Reiniciar(t, ctx, admin)
	defer dbtest.Reiniciar(t, ctx, admin)
	dbtest.AplicarMigraciones(t, ctx, admin)

	// Semilla: una conversación abierta con secuencia en 0.
	tenant := "t1"
	convID := "11111111-1111-1111-1111-111111111111"
	actor := "22222222-2222-2222-2222-222222222222"
	cmid := "33333333-3333-3333-3333-333333333333"
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.conversacion (id, tenant_id, viaje_id, estado, siguiente_secuencia)
		 VALUES ('`+convID+`', '`+tenant+`', gen_random_uuid(), 'abierta', 0)`)

	pool, closePool := dbtest.PoolComoRol(t, ctx)
	defer closePool()

	contar := func(sql, arg string) int {
		var n int
		if err := pool.QueryRow(ctx, sql, arg).Scan(&n); err != nil {
			t.Fatalf("contar (%s): %v", sql, err)
		}
		return n
	}

	svc := conversacion.NewServicio(pool, outbox.NewPublisher())
	in := conversacion.EntradaEnvio{
		TenantID:        tenant,
		ConversacionID:  convID,
		ActorReal:       actor,
		ClientMessageID: cmid,
		Cuerpo:          "hola",
	}

	// Primer envío: crea el mensaje y su outbox en la misma tx.
	res1, err := svc.Enviar(ctx, in)
	if err != nil {
		t.Fatalf("primer envío: %v", err)
	}
	if res1.Idempotente {
		t.Fatalf("el primer envío no debería marcarse idempotente")
	}
	if res1.Secuencia != 1 {
		t.Fatalf("la primera secuencia debería ser 1, fue %d", res1.Secuencia)
	}

	// Exactamente un mensaje y exactamente una fila de outbox del tipo mensaje.creado.
	if n := contar(`SELECT count(*) FROM mensajeria.mensaje WHERE conversacion_id = $1`, convID); n != 1 {
		t.Fatalf("tras el primer envío debería haber 1 mensaje, hay %d", n)
	}
	if n := contar(`SELECT count(*) FROM mensajeria.outbox WHERE tipo = 'mensaje.creado' AND agregado_id = $1`, convID); n != 1 {
		t.Fatalf("tras el primer envío debería haber 1 outbox mensaje.creado, hay %d", n)
	}

	// Reintento con el mismo client_message_id: devuelve el original, no crea un
	// segundo mensaje ni un segundo evento (idempotencia durable).
	res2, err := svc.Enviar(ctx, in)
	if err != nil {
		t.Fatalf("reintento: %v", err)
	}
	if !res2.Idempotente {
		t.Fatalf("el reintento debería marcarse idempotente")
	}
	if res2.MensajeID != res1.MensajeID {
		t.Fatalf("el reintento devolvió un mensaje distinto: %s != %s", res2.MensajeID, res1.MensajeID)
	}
	if n := contar(`SELECT count(*) FROM mensajeria.mensaje WHERE conversacion_id = $1`, convID); n != 1 {
		t.Fatalf("tras el reintento debería seguir habiendo 1 mensaje, hay %d", n)
	}
	if n := contar(`SELECT count(*) FROM mensajeria.outbox WHERE tipo = 'mensaje.creado' AND agregado_id = $1`, convID); n != 1 {
		t.Fatalf("tras el reintento debería seguir habiendo 1 outbox mensaje.creado, hay %d", n)
	}
}
