package consumidores_test

import (
	"context"
	"testing"
	"time"

	"github.com/AI27-Tech/pf-pkg-platform-go/bus"
	"github.com/AI27-Tech/pf-pkg-platform-go/outbox"
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/consumidores"
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/conversacion"
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/dbtest"
)

// La reentrega at-least-once de los dos eventos de la solicitud, contra Postgres
// real, no duplica la conversación ni mueve la ventana ya aplicada.
func TestConsumidorSolicitudReentregaContraLaBase(t *testing.T) {
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

	c := consumidores.NuevoConsumidorSolicitud(conversacion.NewServicio(pool, outbox.NewPublisher()))
	en := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	creada := bus.Sobre{Tipo: consumidores.TipoSolicitudCreada,
		Payload: []byte(`{"solicitud_id":"` + sol + `","cliente_id":"` + cli + `","creada_en":"2026-11-20T11:00:00Z"}`)}
	cambiado := bus.Sobre{Tipo: consumidores.TipoSolicitudEstadoCambiado,
		Payload: []byte(`{"solicitud_id":"` + sol + `","cliente_id":"` + cli + `","a_estado":"APROBADO","cambiado_en":"2026-11-20T12:00:00Z"}`)}

	for i := 0; i < 3; i++ {
		for _, s := range []bus.Sobre{creada, cambiado} {
			if err := c.Procesar(ctx, s); err != nil {
				t.Fatalf("entrega %d de %s: %v", i+1, s.Tipo, err)
			}
		}
	}
	var n, rutas int
	var hasta time.Time
	if err := pool.QueryRow(ctx,
		`SELECT count(*), max(escritura_hasta) FROM mensajeria.conversacion WHERE viaje_id = $1 AND tipo = 'solicitud'`, sol,
	).Scan(&n, &hasta); err != nil {
		t.Fatal(err)
	}
	if n != 1 || !hasta.Equal(en.Add(72*time.Hour)) {
		t.Fatalf("conversaciones=%d escritura_hasta=%v; quiere 1 y %v", n, hasta, en.Add(72*time.Hour))
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM mensajeria.ruta_conversacion r JOIN mensajeria.conversacion c ON c.id = r.conversacion_id
		  WHERE c.viaje_id = $1 AND c.tipo = 'solicitud'`, sol,
	).Scan(&rutas); err != nil || rutas != 1 {
		t.Fatalf("rutas=%d err=%v; quiere 1", rutas, err)
	}
}
