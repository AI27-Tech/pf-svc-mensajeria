package conversacion_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/AI27-Tech/pf-pkg-platform-go/outbox"
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/conversacion"
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/dbtest"
)

// TestEnviarRechazaConversacionNoAbierta verifica la guarda de estado del envío
// (E3-T2, criterio de aceptación "solo_lectura"): un envío sobre una
// conversación que no está `abierta` se rechaza SIN insertar mensaje, SIN
// incrementar siguiente_secuencia y SIN publicar mensaje.creado. El contrato
// modela el estado como enum [abierta, solo_lectura, cerrada] y exige el `codigo`
// de máquina conversacion_solo_lectura (mensajeria.yaml, DEC-030/DEC-041).
//
// Prueba de comportamiento contra Postgres real: afirma el error esperado y los
// efectos nulos (conteos literales). Se salta si MENSAJERIA_TEST_DB no está.
func TestEnviarRechazaConversacionNoAbierta(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	dbtest.Serializar(t, ctx)
	admin, closeAdmin := dbtest.AdminConn(t, ctx)
	defer closeAdmin()

	dbtest.Reiniciar(t, ctx, admin)
	defer dbtest.Reiniciar(t, ctx, admin)
	dbtest.AplicarMigraciones(t, ctx, admin)

	const (
		tenant = "t1"
		actor  = "22222222-2222-2222-2222-222222222222"
	)

	pool, closePool := dbtest.PoolComoRol(t, ctx)
	defer closePool()

	contar := func(sql, arg string) int {
		var n int
		if err := pool.QueryRow(ctx, sql, arg).Scan(&n); err != nil {
			t.Fatalf("contar (%s): %v", sql, err)
		}
		return n
	}
	secuenciaDe := func(convID string) int64 {
		var s int64
		if err := pool.QueryRow(ctx,
			`SELECT siguiente_secuencia FROM mensajeria.conversacion WHERE id = $1`, convID,
		).Scan(&s); err != nil {
			t.Fatalf("leer siguiente_secuencia: %v", err)
		}
		return s
	}

	svc := conversacion.NewServicio(pool, outbox.NewPublisher())

	// Un caso por estado no-abierto: ambos deben rechazarse igual.
	casos := []struct {
		nombre string
		convID string
		estado string
	}{
		{"solo_lectura", "11111111-1111-1111-1111-111111111111", "solo_lectura"},
		{"cerrada", "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", "cerrada"},
	}

	for _, c := range casos {
		t.Run(c.nombre, func(t *testing.T) {
			dbtest.Exec(t, ctx, admin,
				`INSERT INTO mensajeria.conversacion (id, tenant_id, viaje_id, estado, siguiente_secuencia)
				 VALUES ('`+c.convID+`', '`+tenant+`', gen_random_uuid(), '`+c.estado+`', 0)`)

			_, err := svc.Enviar(ctx, conversacion.EntradaEnvio{
				TenantID:        tenant,
				ConversacionID:  c.convID,
				ActorReal:       actor,
				ClientMessageID: "33333333-3333-3333-3333-333333333333",
				Cuerpo:          "no debería entrar",
			})
			if !errors.Is(err, conversacion.ErrConversacionSoloLectura) {
				t.Fatalf("estado %s: error = %v, quiere ErrConversacionSoloLectura", c.estado, err)
			}

			// Efectos nulos: ni mensaje, ni outbox, ni avance de secuencia.
			if n := contar(`SELECT count(*) FROM mensajeria.mensaje WHERE conversacion_id = $1`, c.convID); n != 0 {
				t.Fatalf("estado %s: debería haber 0 mensajes, hay %d", c.estado, n)
			}
			if n := contar(`SELECT count(*) FROM mensajeria.outbox WHERE agregado_id = $1`, c.convID); n != 0 {
				t.Fatalf("estado %s: debería haber 0 outbox, hay %d", c.estado, n)
			}
			if s := secuenciaDe(c.convID); s != 0 {
				t.Fatalf("estado %s: siguiente_secuencia debería seguir en 0, es %d", c.estado, s)
			}
		})
	}
}

// TestEnviarRechazaVentanaVencida cierra el hueco de E3-T1: una conversación que
// sigue en estado `abierta` pero cuyo escritura_hasta ya pasó NO admite envío
// (DEC-030/041). La guarda de Enviar lo rechaza con ErrConversacionSoloLectura
// sin efecto, aunque el barrido que materializa el estado `solo_lectura` aún no
// haya corrido. Se salta sin MENSAJERIA_TEST_DB.
func TestEnviarRechazaVentanaVencida(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	dbtest.Serializar(t, ctx)
	admin, closeAdmin := dbtest.AdminConn(t, ctx)
	defer closeAdmin()
	dbtest.Reiniciar(t, ctx, admin)
	defer dbtest.Reiniciar(t, ctx, admin)
	dbtest.AplicarMigraciones(t, ctx, admin)

	const (
		tenant = "t1"
		conv   = "11111111-1111-1111-1111-111111111111"
		actor  = "22222222-2222-2222-2222-222222222222"
	)
	// Conversación abierta, pero con la ventana de escritura vencida (ayer).
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.conversacion (id, tenant_id, viaje_id, estado, siguiente_secuencia, escritura_hasta)
		 VALUES ('`+conv+`', '`+tenant+`', gen_random_uuid(), 'abierta', 0, now() - interval '1 day')`)

	pool, closePool := dbtest.PoolComoRol(t, ctx)
	defer closePool()
	svc := conversacion.NewServicio(pool, outbox.NewPublisher())

	_, err := svc.Enviar(ctx, conversacion.EntradaEnvio{
		TenantID:        tenant,
		ConversacionID:  conv,
		ActorReal:       actor,
		ClientMessageID: "33333333-3333-3333-3333-333333333333",
		Cuerpo:          "la ventana ya venció",
	})
	if !errors.Is(err, conversacion.ErrConversacionSoloLectura) {
		t.Fatalf("error = %v, quiere ErrConversacionSoloLectura", err)
	}

	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM mensajeria.mensaje WHERE conversacion_id = $1`, conv,
	).Scan(&n); err != nil {
		t.Fatalf("contar mensajes: %v", err)
	}
	if n != 0 {
		t.Fatalf("no debería crear mensaje con la ventana vencida, hay %d", n)
	}
	// La secuencia no avanzó (el rechazo no tocó la fila).
	var seq int64
	if err := pool.QueryRow(ctx,
		`SELECT siguiente_secuencia FROM mensajeria.conversacion WHERE id = $1`, conv,
	).Scan(&seq); err != nil {
		t.Fatalf("leer secuencia: %v", err)
	}
	if seq != 0 {
		t.Fatalf("siguiente_secuencia debería seguir en 0, es %d", seq)
	}
}
