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

// TestIntervencionDelAdministrador cubre, contra Postgres real, el envío del
// administrador en la conversación de la solicitud (DEC-148 #20, #47): interviene
// con auditoría solo si NO es el responsable del reclamo; si lo es y su versión es
// obsoleta recibe el conflicto de versión; la intervención lee al responsable con
// bloqueo y no toca el vínculo pegajoso del Ejecutivo (ADR 36).
func TestIntervencionDelAdministrador(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	dbtest.Serializar(t, ctx)
	admin, closeAdmin := dbtest.AdminConn(t, ctx)
	defer closeAdmin()
	dbtest.Reiniciar(t, ctx, admin)
	defer dbtest.Reiniciar(t, ctx, admin)
	dbtest.AplicarMigraciones(t, ctx, admin)

	const (
		tenant = "ai27"
		adm    = "dddddddd-dddd-dddd-dddd-dddddddddddd"
		cot1   = "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee"
		cot2   = "ffffffff-ffff-ffff-ffff-ffffffffffff"
		cli    = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	)
	pool, closePool := dbtest.PoolComoRol(t, ctx)
	defer closePool()
	svc := conversacion.NewServicio(pool, outbox.NewPublisher())
	v := func(n int64) *int64 { return &n }

	// Un ejecutivo con actividad previa: si un envío sobre la solicitud tocara el
	// vínculo, ultima_en cambiaría.
	dbtest.Exec(t, ctx, admin, `INSERT INTO mensajeria.actividad_ejecutivo (ejecutivo_id, ultima_en, disponible_desde)
		VALUES ('`+adm+`', '2020-01-01', '2020-01-01'), ('`+cot1+`', '2020-01-01', '2020-01-01')`)

	crear := func(conv, tipo, responsable string) {
		dbtest.Exec(t, ctx, admin,
			`INSERT INTO mensajeria.conversacion (id, tenant_id, viaje_id, cliente_id, tipo, estado, siguiente_secuencia)
			 VALUES ('`+conv+`', '`+tenant+`', gen_random_uuid(), '`+cli+`', '`+tipo+`', 'abierta', 0)`)
		if responsable != "" {
			dbtest.Exec(t, ctx, admin,
				`INSERT INTO mensajeria.reclamo (conversacion_id, tenant_id, actor_real, version)
				 VALUES ('`+conv+`', '`+tenant+`', '`+responsable+`', 3)`)
		}
	}
	envio := func(conv, actor, rol, msg string, ver *int64) error {
		_, err := svc.Enviar(ctx, conversacion.EntradaEnvio{
			TenantID: tenant, ConversacionID: conv, ActorReal: actor, RolAlEnviar: rol,
			ClientMessageID: msg, Cuerpo: "x", VersionReclamoEsperada: ver,
		})
		return err
	}
	contar := func(q string, args ...any) (n int) {
		t.Helper()
		if err := pool.QueryRow(ctx, q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	overrides := func(conv string) int {
		return contar(`SELECT count(*) FROM mensajeria.auditoria_mensajeria WHERE conversacion_id = $1 AND accion = 'override.administrador'`, conv)
	}
	mensajes := func(conv string) int {
		return contar(`SELECT count(*) FROM mensajeria.mensaje WHERE conversacion_id = $1`, conv)
	}

	t.Run("el administrador responsable con versión obsoleta recibe el conflicto, sin override", func(t *testing.T) {
		const conv = "11111111-1111-1111-1111-111111111111"
		crear(conv, "solicitud", adm) // responsable por una transferencia, en v3
		for _, ver := range []*int64{v(2), nil} {
			err := envio(conv, adm, "administrador", "33333333-3333-3333-3333-33333333333"+map[bool]string{true: "1", false: "2"}[ver != nil], ver)
			if !errors.Is(err, conversacion.ErrSinReclamoActivo) {
				t.Fatalf("versión %v: err = %v, quiere ErrSinReclamoActivo", ver, err)
			}
		}
		if n := mensajes(conv); n != 0 {
			t.Fatalf("mensajes = %d, quiere 0", n)
		}
		if n := overrides(conv); n != 0 {
			t.Fatalf("overrides = %d, quiere 0: el responsable no interviene", n)
		}
		// Con su versión vigente envía como cualquier responsable, sin override.
		if err := envio(conv, adm, "administrador", "33333333-3333-3333-3333-333333333333", v(3)); err != nil {
			t.Fatalf("responsable con versión vigente: %v", err)
		}
		if mensajes(conv) != 1 || overrides(conv) != 0 {
			t.Fatalf("mensajes=%d overrides=%d; quiere 1 y 0", mensajes(conv), overrides(conv))
		}
	})

	t.Run("el administrador que no es responsable interviene y queda auditado el desplazado", func(t *testing.T) {
		const conv = "22222222-2222-2222-2222-222222222222"
		crear(conv, "solicitud", cot1)
		if err := envio(conv, adm, "administrador", "44444444-4444-4444-4444-444444444441", nil); err != nil {
			t.Fatalf("intervención: %v", err)
		}
		var desplazado string
		if err := pool.QueryRow(ctx, `SELECT detalle->>'responsable_desplazado' FROM mensajeria.auditoria_mensajeria
			WHERE conversacion_id = $1 AND accion = 'override.administrador'`, conv).Scan(&desplazado); err != nil || desplazado != cot1 {
			t.Fatalf("responsable_desplazado = %q (err %v), quiere %s", desplazado, err, cot1)
		}
	})

	t.Run("en la conversación del viaje el administrador sin reclamo no interviene", func(t *testing.T) {
		const conv = "33333333-3333-3333-3333-333333333330"
		crear(conv, "viaje", cot1)
		if err := envio(conv, adm, "administrador", "44444444-4444-4444-4444-444444444442", nil); !errors.Is(err, conversacion.ErrSinReclamoActivo) {
			t.Fatalf("err = %v, quiere ErrSinReclamoActivo", err)
		}
		if mensajes(conv) != 0 || overrides(conv) != 0 {
			t.Fatalf("mensajes=%d overrides=%d; quiere 0 y 0", mensajes(conv), overrides(conv))
		}
	})

	t.Run("enviar en la solicitud no toca el vínculo del ejecutivo, y en el viaje sí", func(t *testing.T) {
		antiguo := func(id string) bool {
			var viejo bool
			if err := pool.QueryRow(ctx, `SELECT ultima_en < '2021-01-01' FROM mensajeria.actividad_ejecutivo WHERE ejecutivo_id = $1`, id).Scan(&viejo); err != nil {
				t.Fatal(err)
			}
			return viejo
		}
		const sol = "55555555-5555-5555-5555-555555555550"
		crear(sol, "solicitud", cot1)
		if err := envio(sol, cot1, "cotizacion", "44444444-4444-4444-4444-444444444443", v(3)); err != nil {
			t.Fatal(err)
		}
		if err := envio(sol, adm, "administrador", "44444444-4444-4444-4444-444444444444", nil); err != nil {
			t.Fatal(err)
		}
		if !antiguo(adm) || !antiguo(cot1) {
			t.Fatal("el envío en la solicitud tocó actividad_ejecutivo")
		}
		const viaje = "55555555-5555-5555-5555-555555555551"
		crear(viaje, "viaje", cot1)
		if err := envio(viaje, cot1, "ejecutivo", "44444444-4444-4444-4444-444444444445", v(3)); err != nil {
			t.Fatal(err)
		}
		if antiguo(cot1) {
			t.Fatal("el envío en el viaje debe seguir registrando la actividad del ejecutivo")
		}
	})

	t.Run("la intervención lee al responsable con bloqueo", func(t *testing.T) {
		const conv = "66666666-6666-6666-6666-666666666666"
		crear(conv, "solicitud", cot1)
		// Otra transacción cambia al responsable y aún no confirma: la intervención
		// espera y audita al responsable ya confirmado.
		tx, err := admin.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, `UPDATE mensajeria.reclamo SET actor_real = $2 WHERE conversacion_id = $1 AND cerrado_en IS NULL`, conv, cot2); err != nil {
			t.Fatal(err)
		}
		hecho := make(chan error, 1)
		go func() { hecho <- envio(conv, adm, "administrador", "44444444-4444-4444-4444-444444444446", nil) }()
		select {
		case err := <-hecho:
			t.Fatalf("la intervención no esperó al bloqueo del reclamo (err %v)", err)
		case <-time.After(500 * time.Millisecond):
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err := <-hecho; err != nil {
			t.Fatal(err)
		}
		var desplazado string
		if err := pool.QueryRow(ctx, `SELECT detalle->>'responsable_desplazado' FROM mensajeria.auditoria_mensajeria
			WHERE conversacion_id = $1 AND accion = 'override.administrador'`, conv).Scan(&desplazado); err != nil || desplazado != cot2 {
			t.Fatalf("responsable_desplazado = %q (err %v), quiere %s", desplazado, err, cot2)
		}
	})
}
