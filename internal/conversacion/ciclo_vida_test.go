package conversacion_test

import (
	"context"
	"testing"
	"time"

	"github.com/AI27-Tech/pf-pkg-platform-go/outbox"
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/conversacion"
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/dbtest"
	"github.com/google/uuid"
)

// TestCrearPorEventoEsIdempotente verifica E3-T1: crear la conversación de un
// viaje a partir de viaje.mensajeria_habilitada crea una sola fila, y reentregar
// el evento (segunda llamada) NO crea una segunda conversación (idempotencia por
// el índice único parcial una_conversacion_activa_por_viaje). tenant_id se puebla
// con el cliente_id del evento (DEC-049: cliente = tenant).
//
// Prueba de comportamiento contra Postgres real. Se salta sin MENSAJERIA_TEST_DB.
func TestCrearPorEventoEsIdempotente(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	dbtest.Serializar(t, ctx)
	admin, closeAdmin := dbtest.AdminConn(t, ctx)
	defer closeAdmin()
	dbtest.Reiniciar(t, ctx, admin)
	defer dbtest.Reiniciar(t, ctx, admin)
	dbtest.AplicarMigraciones(t, ctx, admin)

	const (
		viaje   = "11111111-1111-1111-1111-111111111111"
		cliente = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	)

	pool, closePool := dbtest.PoolComoRol(t, ctx)
	defer closePool()
	svc := conversacion.NewServicio(pool, outbox.NewPublisher())

	in := conversacion.EntradaCreacion{ViajeID: viaje, ClienteID: cliente}

	// Primera entrega: crea.
	creada, err := svc.CrearPorEvento(ctx, in)
	if err != nil {
		t.Fatalf("primera creación: %v", err)
	}
	if !creada {
		t.Fatalf("la primera creación debería devolver creada=true")
	}

	// Segunda entrega (reentrega del mismo evento): no-op, no crea otra.
	creada2, err := svc.CrearPorEvento(ctx, in)
	if err != nil {
		t.Fatalf("reentrega: %v", err)
	}
	if creada2 {
		t.Fatalf("la reentrega NO debería crear una segunda conversación")
	}

	// Exactamente una conversación para el viaje, con tenant_id = cliente_id.
	var n int
	var tenant, cli string
	if err := pool.QueryRow(ctx,
		`SELECT count(*) OVER (), tenant_id, cliente_id::text
		   FROM mensajeria.conversacion WHERE viaje_id = $1 LIMIT 1`, viaje,
	).Scan(&n, &tenant, &cli); err != nil {
		t.Fatalf("leer conversación: %v", err)
	}
	if n != 1 {
		t.Fatalf("debería haber 1 conversación para el viaje, hay %d", n)
	}
	if tenant != cliente || cli != cliente {
		t.Fatalf("tenant_id/cliente_id = %q/%q, quiere ambos = %q", tenant, cli, cliente)
	}

	// DEC-127: nació enrutada a la bandeja única del tenant (creada con ella), una
	// sola ruta activa en revisión 1 aunque el evento se reentregó.
	var rutas, revRuta, revBandeja int64
	var tipo string
	if err := pool.QueryRow(ctx,
		`SELECT count(*) OVER (), r.revision, b.revision, b.tipo
		   FROM mensajeria.ruta_conversacion r
		   JOIN mensajeria.conversacion c ON c.id = r.conversacion_id
		   JOIN mensajeria.bandeja b ON b.id = r.bandeja_id AND b.tenant_id = r.tenant_id
		  WHERE c.viaje_id = $1 AND r.activa LIMIT 1`, viaje,
	).Scan(&rutas, &revRuta, &revBandeja, &tipo); err != nil {
		t.Fatalf("leer ruta: %v", err)
	}
	if rutas != 1 || revRuta != 1 || revBandeja != 1 || tipo != "chat" {
		t.Fatalf("ruta: %d activas, revisión %d, bandeja %s en revisión %d; quiere 1, 1, chat, 1", rutas, revRuta, tipo, revBandeja)
	}
}

// TestCrearPorEventoConcurrenteUnaBandeja: ocho viajes de un tenant nuevo
// habilitados a la vez crean UNA sola bandeja de chat (el lock por tenant de
// bandejas.EnrutarABandejaUnica) y ocho rutas con revisiones 1..8 sin repetir.
//
// Prueba de comportamiento contra Postgres real. Se salta sin MENSAJERIA_TEST_DB.
func TestCrearPorEventoConcurrenteUnaBandeja(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	dbtest.Serializar(t, ctx)
	admin, closeAdmin := dbtest.AdminConn(t, ctx)
	defer closeAdmin()
	dbtest.Reiniciar(t, ctx, admin)
	defer dbtest.Reiniciar(t, ctx, admin)
	dbtest.AplicarMigraciones(t, ctx, admin)

	pool, closePool := dbtest.PoolComoRol(t, ctx)
	defer closePool()
	svc := conversacion.NewServicio(pool, outbox.NewPublisher())

	const cliente = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	const n = 8
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			_, err := svc.CrearPorEvento(ctx, conversacion.EntradaCreacion{ViajeID: uuid.NewString(), ClienteID: cliente})
			errs <- err
		}()
	}
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("crear concurrente: %v", err)
		}
	}

	var bandejas, rutas, distintas, maxRev, revBandeja int64
	if err := pool.QueryRow(ctx,
		`SELECT (SELECT count(*) FROM mensajeria.bandeja WHERE tenant_id = $1 AND tipo = 'chat'),
		        count(*), count(DISTINCT r.revision), max(r.revision),
		        (SELECT max(revision) FROM mensajeria.bandeja WHERE tenant_id = $1)
		   FROM mensajeria.ruta_conversacion r WHERE r.tenant_id = $1 AND r.activa`, cliente,
	).Scan(&bandejas, &rutas, &distintas, &maxRev, &revBandeja); err != nil {
		t.Fatalf("leer bandejas y rutas: %v", err)
	}
	if got := [5]int64{bandejas, rutas, distintas, maxRev, revBandeja}; got != [5]int64{1, n, n, n, n} {
		t.Fatalf("[bandejas, rutas, revisiones distintas, revisión máx, revisión bandeja] = %v, quiere [1 8 8 8 8]", got)
	}
}

// TestCerrarVentanaFijaEscrituraHasta verifica E3-T1: al consumir viaje.cerrado,
// escritura_hasta se fija a cerrado_en + 3 días sin incidencia, o + 6 meses con
// incidencia (DEC-030/DEC-041). Idempotente y acotado por viaje_id.
func TestCerrarVentanaFijaEscrituraHasta(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	dbtest.Serializar(t, ctx)
	admin, closeAdmin := dbtest.AdminConn(t, ctx)
	defer closeAdmin()
	dbtest.Reiniciar(t, ctx, admin)
	defer dbtest.Reiniciar(t, ctx, admin)
	dbtest.AplicarMigraciones(t, ctx, admin)

	pool, closePool := dbtest.PoolComoRol(t, ctx)
	defer closePool()
	svc := conversacion.NewServicio(pool, outbox.NewPublisher())

	cerradoEn := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	casos := []struct {
		nombre       string
		viaje        string
		cliente      string
		motivo       string
		esperaOffset time.Duration
	}{
		{"sin incidencia = 3 días", "11111111-1111-1111-1111-111111111111", "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", "normal", 3 * 24 * time.Hour},
		{"con incidencia = 6 meses", "22222222-2222-2222-2222-222222222222", "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb", "por_incidencia", 6 * 30 * 24 * time.Hour},
	}

	for _, c := range casos {
		t.Run(c.nombre, func(t *testing.T) {
			if _, err := svc.CrearPorEvento(ctx, conversacion.EntradaCreacion{ViajeID: c.viaje, ClienteID: c.cliente}); err != nil {
				t.Fatalf("crear: %v", err)
			}
			if err := svc.CerrarVentana(ctx, c.viaje, c.motivo, cerradoEn); err != nil {
				t.Fatalf("cerrar ventana: %v", err)
			}
			var escrituraHasta time.Time
			if err := pool.QueryRow(ctx,
				`SELECT escritura_hasta FROM mensajeria.conversacion WHERE viaje_id = $1`, c.viaje,
			).Scan(&escrituraHasta); err != nil {
				t.Fatalf("leer escritura_hasta: %v", err)
			}
			esperado := cerradoEn.Add(c.esperaOffset)
			if !escrituraHasta.Equal(esperado) {
				t.Fatalf("escritura_hasta = %s, quiere %s", escrituraHasta, esperado)
			}
		})
	}
}
