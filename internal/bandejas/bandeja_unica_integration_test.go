package bandejas_test

import (
	"context"
	"testing"
	"time"

	"github.com/AI27-Tech/pf-svc-mensajeria/internal/dbtest"
	"github.com/AI27-Tech/pf-svc-mensajeria/migrations"
)

// TestMigracion0008EnrutaConversacionesSinRuta aplica la migración 0008 sobre
// conversaciones creadas antes de DEC-127 y afirma, contra Postgres real:
//   - tenant A, sin bandeja: nace una bandeja de chat y sus dos conversaciones
//     entran con revisiones 1 y 2; bandeja.revision queda en 2;
//   - tenant B, con dos bandejas de chat (la vieja en revisión 5) y una ruta
//     activa en la nueva: la conversación sin ruta entra a la más antigua con
//     revisión 6; la ruta existente no se toca;
//   - tenant C, con solo bandeja de emergencias: nace su bandeja de chat; nada
//     entra a la de Reacción;
//   - reaplicar 0008 (cmd/migrate reaplica todo en cada despliegue) no cambia nada.
//
// Se salta si MENSAJERIA_TEST_DB no está definido.
func TestMigracion0008EnrutaConversacionesSinRuta(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	dbtest.Serializar(t, ctx)
	admin, closeAdmin := dbtest.AdminConn(t, ctx)
	defer closeAdmin()
	dbtest.Reiniciar(t, ctx, admin)
	defer dbtest.Reiniciar(t, ctx, admin)
	dbtest.AplicarMigraciones(t, ctx, admin)

	const (
		tA       = "a0000000-0000-0000-0000-00000000000a"
		tB       = "b0000000-0000-0000-0000-00000000000b"
		tC       = "c0000000-0000-0000-0000-00000000000c"
		a1       = "a1000000-0000-0000-0000-000000000001"
		a2       = "a2000000-0000-0000-0000-000000000002"
		b1       = "b1000000-0000-0000-0000-000000000001"
		b2       = "b2000000-0000-0000-0000-000000000002"
		c1       = "c1000000-0000-0000-0000-000000000001"
		bViejaB  = "bb000000-0000-0000-0000-0000000000b1"
		bNuevaB  = "bb000000-0000-0000-0000-0000000000b2"
		bEmergC  = "cc000000-0000-0000-0000-0000000000ce"
		rutaPrev = "dd000000-0000-0000-0000-0000000000d1"
	)
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.conversacion (id, tenant_id, viaje_id, cliente_id, creado_en) VALUES
		 ('`+a1+`', '`+tA+`', gen_random_uuid(), '`+tA+`', '2026-10-01T00:00:00Z'),
		 ('`+a2+`', '`+tA+`', gen_random_uuid(), '`+tA+`', '2026-10-02T00:00:00Z'),
		 ('`+b1+`', '`+tB+`', gen_random_uuid(), '`+tB+`', '2026-10-01T00:00:00Z'),
		 ('`+b2+`', '`+tB+`', gen_random_uuid(), '`+tB+`', '2026-10-02T00:00:00Z'),
		 ('`+c1+`', '`+tC+`', gen_random_uuid(), '`+tC+`', '2026-10-01T00:00:00Z')`)
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.bandeja (id, tenant_id, tipo, nombre, revision, creado_en) VALUES
		 ('`+bViejaB+`', '`+tB+`', 'chat', 'cliente B viejo', 5, '2026-01-01T00:00:00Z'),
		 ('`+bNuevaB+`', '`+tB+`', 'chat', 'cliente B nuevo', 1, '2026-02-01T00:00:00Z'),
		 ('`+bEmergC+`', '`+tC+`', 'emergencias', 'Reacción', 0, '2026-01-01T00:00:00Z')`)
	dbtest.Exec(t, ctx, admin,
		`INSERT INTO mensajeria.ruta_conversacion (id, conversacion_id, tenant_id, bandeja_id, activa, revision)
		 VALUES ('`+rutaPrev+`', '`+b1+`', '`+tB+`', '`+bNuevaB+`', true, 1)`)

	sql, err := migrations.FS.ReadFile("mensajeria/0008_bandeja_unica.sql")
	if err != nil {
		t.Fatalf("leer 0008: %v", err)
	}
	// AplicarMigraciones ya corrió 0008 sobre la base vacía; esta es la pasada
	// que encuentra los datos previos, como el siguiente despliegue.
	if _, err := admin.Exec(ctx, string(sql)); err != nil {
		t.Fatalf("aplicar 0008: %v", err)
	}

	// estado resume rutas activas y bandejas como texto comparable.
	estado := func() (rutas map[string]string, bandejas map[string]string) {
		t.Helper()
		rutas, bandejas = map[string]string{}, map[string]string{}
		rows, err := admin.Query(ctx,
			`SELECT r.conversacion_id::text, b.tenant_id || '/' || b.tipo || '/' || b.nombre || '/rev' || r.revision
			   FROM mensajeria.ruta_conversacion r JOIN mensajeria.bandeja b ON b.id = r.bandeja_id
			  WHERE r.activa`)
		if err != nil {
			t.Fatalf("leer rutas: %v", err)
		}
		for rows.Next() {
			var c, v string
			if err := rows.Scan(&c, &v); err != nil {
				t.Fatalf("escanear ruta: %v", err)
			}
			rutas[c] = v
		}
		rows.Close()
		rows, err = admin.Query(ctx,
			`SELECT tenant_id || '/' || tipo || '/' || nombre, revision::text FROM mensajeria.bandeja`)
		if err != nil {
			t.Fatalf("leer bandejas: %v", err)
		}
		for rows.Next() {
			var k, v string
			if err := rows.Scan(&k, &v); err != nil {
				t.Fatalf("escanear bandeja: %v", err)
			}
			bandejas[k] = v
		}
		rows.Close()
		return rutas, bandejas
	}

	quiereRutas := map[string]string{
		a1: tA + "/chat/Conversaciones/rev1",
		a2: tA + "/chat/Conversaciones/rev2",
		b1: tB + "/chat/cliente B nuevo/rev1", // ruta previa, intacta
		b2: tB + "/chat/cliente B viejo/rev6",
		c1: tC + "/chat/Conversaciones/rev1",
	}
	quiereBandejas := map[string]string{
		tA + "/chat/Conversaciones":  "2",
		tB + "/chat/cliente B viejo": "6",
		tB + "/chat/cliente B nuevo": "1",
		tC + "/emergencias/Reacción": "0",
		tC + "/chat/Conversaciones":  "1",
	}
	verificar := func(paso string) {
		t.Helper()
		rutas, bandejas := estado()
		if len(rutas) != len(quiereRutas) || len(bandejas) != len(quiereBandejas) {
			t.Fatalf("%s: rutas %v bandejas %v; quiere %v y %v", paso, rutas, bandejas, quiereRutas, quiereBandejas)
		}
		for k, v := range quiereRutas {
			if rutas[k] != v {
				t.Fatalf("%s: ruta de %s = %q, quiere %q", paso, k, rutas[k], v)
			}
		}
		for k, v := range quiereBandejas {
			if bandejas[k] != v {
				t.Fatalf("%s: revisión de %s = %q, quiere %q", paso, k, bandejas[k], v)
			}
		}
	}
	verificar("primera pasada")

	if _, err := admin.Exec(ctx, string(sql)); err != nil {
		t.Fatalf("reaplicar 0008: %v", err)
	}
	verificar("reaplicada")
}
