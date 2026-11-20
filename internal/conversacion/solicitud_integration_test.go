package conversacion_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/AI27-Tech/pf-pkg-platform-go/outbox"
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/conversacion"
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/dbtest"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	solCliente = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	solID      = "11111111-1111-1111-1111-111111111111"
)

// entornoSolicitud deja la base limpia, migrada, y devuelve el servicio con un
// reloj controlable.
func entornoSolicitud(t *testing.T) (context.Context, *conversacion.Servicio, *pgxpool.Pool, *time.Time) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	dbtest.Serializar(t, ctx)
	admin, closeAdmin := dbtest.AdminConn(t, ctx)
	t.Cleanup(closeAdmin)
	dbtest.Reiniciar(t, ctx, admin)
	t.Cleanup(func() { dbtest.Reiniciar(t, ctx, admin) })
	dbtest.AplicarMigraciones(t, ctx, admin)
	pool, closePool := dbtest.PoolComoRol(t, ctx)
	t.Cleanup(closePool)
	ahora := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	svc := conversacion.NewServicio(pool, outbox.NewPublisher())
	svc.Ahora = func() time.Time { return ahora }
	return ctx, svc, pool, &ahora
}

// La solicitud y el viaje comparten id (DEC-148 #3): dos conversaciones con el
// mismo viaje_id, cada una idempotente por su tipo, y cada resolución devuelve la suya.
func TestSolicitudYViajeConviveConElMismoId(t *testing.T) {
	ctx, svc, pool, _ := entornoSolicitud(t)
	in := conversacion.EntradaCreacion{ViajeID: solID, ClienteID: solCliente}

	for i, quiere := range []bool{true, false} {
		if got, err := svc.CrearSolicitudPorEvento(ctx, in); err != nil || got != quiere {
			t.Fatalf("solicitud, entrega %d: creada=%v err=%v; quiere %v", i+1, got, err, quiere)
		}
	}
	for i, quiere := range []bool{true, false} {
		if got, err := svc.CrearPorEvento(ctx, in); err != nil || got != quiere {
			t.Fatalf("viaje, entrega %d: creada=%v err=%v; quiere %v", i+1, got, err, quiere)
		}
	}

	idSol, err := svc.ConversacionDeSolicitud(ctx, solID)
	if err != nil {
		t.Fatal(err)
	}
	idViaje, err := svc.ConversacionDeViaje(ctx, solID)
	if err != nil {
		t.Fatal(err)
	}
	if idSol == idViaje {
		t.Fatalf("la conversación de la solicitud y la del viaje son la misma: %s", idSol)
	}

	convSol, err := svc.ObtenerConversacion(ctx, solCliente, idSol)
	if err != nil || convSol.Tipo == nil || *convSol.Tipo != "solicitud" {
		t.Fatalf("tipo de la solicitud: %+v err=%v", convSol, err)
	}
	convViaje, err := svc.ObtenerConversacion(ctx, solCliente, idViaje)
	if err != nil || convViaje.Tipo == nil || *convViaje.Tipo != "viaje" {
		t.Fatalf("tipo del viaje: %+v err=%v", convViaje, err)
	}

	// La solicitud nace en la bandeja única, sin dirigirla a ningún dueño.
	var dirigidas int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM mensajeria.ruta_conversacion WHERE conversacion_id = $1 AND activa AND dirigida_a IS NULL`, idSol,
	).Scan(&dirigidas); err != nil || dirigidas != 1 {
		t.Fatalf("ruta de la solicitud: %d sin dirigir (err %v); quiere 1", dirigidas, err)
	}

	// Un viaje sin solicitud no tiene conversación de solicitud, y al revés.
	otro := uuid.NewString()
	if _, err := svc.CrearPorEvento(ctx, conversacion.EntradaCreacion{ViajeID: otro, ClienteID: solCliente}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ConversacionDeSolicitud(ctx, otro); !errors.Is(err, conversacion.ErrConversacionNoEncontrada) {
		t.Fatalf("solicitud inexistente: err = %v", err)
	}
}

// La ventana de 3 días corre desde que la solicitud termina (APROBADO incluido) o
// vence, y se prueba en su frontera con el reloj inyectado: a 3 días - 1 s el
// cliente aún escribe; a 3 días ya no (DEC-148 #30, #48).
func TestVentanaDeTresDiasEnLaFrontera(t *testing.T) {
	for _, estado := range []string{"APROBADO", "RECHAZADO_POR_AI27", "RECHAZADO_POR_CLIENTE", "CANCELADO_POR_CLIENTE", "VENCIDA"} {
		t.Run(estado, func(t *testing.T) {
			ctx, svc, pool, ahora := entornoSolicitud(t)
			if _, err := svc.CrearSolicitudPorEvento(ctx, conversacion.EntradaCreacion{ViajeID: solID, ClienteID: solCliente}); err != nil {
				t.Fatal(err)
			}
			conv, _ := svc.ConversacionDeSolicitud(ctx, solID)
			termina := *ahora
			if err := svc.AplicarEstadoSolicitud(ctx, solID, solCliente, estado, termina); err != nil {
				t.Fatal(err)
			}
			var hasta time.Time
			if err := pool.QueryRow(ctx, `SELECT escritura_hasta FROM mensajeria.conversacion WHERE id = $1`, conv).Scan(&hasta); err != nil {
				t.Fatal(err)
			}
			if want := termina.Add(72 * time.Hour); !hasta.Equal(want) {
				t.Fatalf("escritura_hasta = %v, quiere %v", hasta, want)
			}

			envia := func() error {
				_, err := svc.Enviar(ctx, conversacion.EntradaEnvio{
					TenantID: solCliente, ConversacionID: conv, ActorReal: solCliente,
					ClientMessageID: uuid.NewString(), Cuerpo: "hola", RolAlEnviar: "cliente",
				})
				return err
			}
			*ahora = termina.Add(72*time.Hour - time.Second)
			if err := envia(); err != nil {
				t.Fatalf("a 3 días - 1 s: %v; quiere que acepte", err)
			}
			*ahora = termina.Add(72 * time.Hour)
			if err := envia(); !errors.Is(err, conversacion.ErrConversacionSoloLectura) {
				t.Fatalf("a 3 días: err = %v; quiere solo lectura", err)
			}
		})
	}
}

// VENCIDA → PAGADO reabre la conversación (DEC-148 #35). Un evento más viejo que
// el ya aplicado no la pisa, y reentregar el mismo converge.
func TestPagadoReabreYLosEventosViejosNoPisan(t *testing.T) {
	ctx, svc, pool, ahora := entornoSolicitud(t)
	if _, err := svc.CrearSolicitudPorEvento(ctx, conversacion.EntradaCreacion{ViajeID: solID, ClienteID: solCliente}); err != nil {
		t.Fatal(err)
	}
	conv, _ := svc.ConversacionDeSolicitud(ctx, solID)
	hasta := func() *time.Time {
		var h *time.Time
		if err := pool.QueryRow(ctx, `SELECT escritura_hasta FROM mensajeria.conversacion WHERE id = $1`, conv).Scan(&h); err != nil {
			t.Fatal(err)
		}
		return h
	}
	t0 := *ahora
	aplicar := func(estado string, en time.Time) {
		t.Helper()
		if err := svc.AplicarEstadoSolicitud(ctx, solID, solCliente, estado, en); err != nil {
			t.Fatal(err)
		}
	}

	aplicar("VENCIDA", t0)
	if h := hasta(); h == nil || !h.Equal(t0.Add(72*time.Hour)) {
		t.Fatalf("tras VENCIDA: %v", h)
	}
	aplicar("VENCIDA", t0) // reentrega
	if h := hasta(); h == nil || !h.Equal(t0.Add(72*time.Hour)) {
		t.Fatalf("tras la reentrega: %v", h)
	}
	aplicar("PAGADO", t0.Add(time.Hour))
	if h := hasta(); h != nil {
		t.Fatalf("tras PAGADO la ventana debe quedar abierta, escritura_hasta = %v", h)
	}
	aplicar("VENCIDA", t0) // llega tarde: más vieja que PAGADO
	if h := hasta(); h != nil {
		t.Fatalf("un VENCIDA anterior a PAGADO no debe cerrarla, escritura_hasta = %v", h)
	}
	if _, err := svc.Enviar(ctx, conversacion.EntradaEnvio{
		TenantID: solCliente, ConversacionID: conv, ActorReal: solCliente,
		ClientMessageID: uuid.NewString(), Cuerpo: "pagué", RolAlEnviar: "cliente",
	}); err != nil {
		t.Fatalf("enviar tras reabrir: %v", err)
	}
}

// Un estado que el contrato no declara no mueve la ventana, ni abierta ni cerrada
// (falla cerrado: no reabre una conversación por un estado desconocido).
func TestEstadoDesconocidoNoMueveLaVentana(t *testing.T) {
	ctx, svc, pool, ahora := entornoSolicitud(t)
	if _, err := svc.CrearSolicitudPorEvento(ctx, conversacion.EntradaCreacion{ViajeID: solID, ClienteID: solCliente}); err != nil {
		t.Fatal(err)
	}
	conv, _ := svc.ConversacionDeSolicitud(ctx, solID)
	t0 := *ahora
	if err := svc.AplicarEstadoSolicitud(ctx, solID, solCliente, "APROBADO", t0); err != nil {
		t.Fatal(err)
	}
	if err := svc.AplicarEstadoSolicitud(ctx, solID, solCliente, "ESTADO_NUEVO", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var hasta *time.Time
	if err := pool.QueryRow(ctx, `SELECT escritura_hasta FROM mensajeria.conversacion WHERE id = $1`, conv).Scan(&hasta); err != nil {
		t.Fatal(err)
	}
	if hasta == nil || !hasta.Equal(t0.Add(72*time.Hour)) {
		t.Fatalf("escritura_hasta = %v tras un estado desconocido; quiere %v", hasta, t0.Add(72*time.Hour))
	}
}

// Si solicitud.estado_cambiado llega antes que solicitud.creada, el efecto no se
// pierde: crea la conversación, y la creación tardía la reutiliza sin reabrirla.
func TestEstadoAntesDeLaCreacionNoSePierde(t *testing.T) {
	ctx, svc, pool, ahora := entornoSolicitud(t)
	if err := svc.AplicarEstadoSolicitud(ctx, solID, solCliente, "APROBADO", *ahora); err != nil {
		t.Fatal(err)
	}
	if creada, err := svc.CrearSolicitudPorEvento(ctx, conversacion.EntradaCreacion{ViajeID: solID, ClienteID: solCliente}); err != nil || creada {
		t.Fatalf("la creación tardía: creada=%v err=%v; quiere reutilizar", creada, err)
	}
	var n int
	var hasta *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT count(*) OVER (), escritura_hasta FROM mensajeria.conversacion WHERE viaje_id = $1 AND tipo = 'solicitud'`, solID,
	).Scan(&n, &hasta); err != nil || n != 1 || hasta == nil {
		t.Fatalf("n=%d hasta=%v err=%v", n, hasta, err)
	}
}

// viaje.cerrado fija la ventana de la conversación del VIAJE; no toca la de la
// solicitud, que comparte viaje_id.
func TestCerrarVentanaDelViajeNoTocaLaSolicitud(t *testing.T) {
	ctx, svc, pool, ahora := entornoSolicitud(t)
	in := conversacion.EntradaCreacion{ViajeID: solID, ClienteID: solCliente}
	if _, err := svc.CrearSolicitudPorEvento(ctx, in); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CrearPorEvento(ctx, in); err != nil {
		t.Fatal(err)
	}
	if err := svc.CerrarVentana(ctx, solID, "por_incidencia", *ahora); err != nil {
		t.Fatal(err)
	}
	rows, err := pool.Query(ctx, `SELECT tipo, escritura_hasta IS NOT NULL FROM mensajeria.conversacion WHERE viaje_id = $1`, solID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]bool{}
	for rows.Next() {
		var tipo string
		var fijada bool
		if err := rows.Scan(&tipo, &fijada); err != nil {
			t.Fatal(err)
		}
		got[tipo] = fijada
	}
	if !got["viaje"] || got["solicitud"] {
		t.Fatalf("escritura_hasta fijada por tipo = %v; quiere solo viaje", got)
	}
}

// La migración 0011 pasa DOS veces con datos de los dos tipos presentes: 0001
// recrea su índice con IF NOT EXISTS, y no debe fallar por las dos conversaciones
// del mismo viaje_id. La unicidad es por (tenant, viaje, tipo).
func TestMigracionesPasanDosVecesYLaUnicidadEsPorTipo(t *testing.T) {
	ctx, svc, pool, _ := entornoSolicitud(t)
	in := conversacion.EntradaCreacion{ViajeID: solID, ClienteID: solCliente}
	if _, err := svc.CrearSolicitudPorEvento(ctx, in); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CrearPorEvento(ctx, in); err != nil {
		t.Fatal(err)
	}

	admin, closeAdmin := dbtest.AdminConn(t, ctx)
	defer closeAdmin()
	dbtest.AplicarMigraciones(t, ctx, admin) // segunda pasada, con datos

	var def string
	if err := pool.QueryRow(ctx,
		`SELECT indexdef FROM pg_indexes WHERE schemaname = 'mensajeria' AND indexname = 'una_conversacion_activa_por_viaje'`,
	).Scan(&def); err != nil {
		t.Fatalf("índice de unicidad: %v", err)
	}
	if want := "(tenant_id, viaje_id, tipo)"; !strings.Contains(def, want) {
		t.Fatalf("indexdef = %s; quiere %s", def, want)
	}
	// Una segunda activa del mismo tipo sigue prohibida.
	if _, err := pool.Exec(ctx,
		`INSERT INTO mensajeria.conversacion (tenant_id, viaje_id, cliente_id, tipo) VALUES ($1, $2, $1::uuid, 'solicitud')`,
		solCliente, solID); err == nil {
		t.Fatal("el índice dejó una segunda conversación activa de la solicitud")
	}
	// El tipo es cerrado.
	if _, err := pool.Exec(ctx,
		`INSERT INTO mensajeria.conversacion (tenant_id, viaje_id, cliente_id, tipo) VALUES ($1, $2, $1::uuid, 'otro')`,
		solCliente, uuid.NewString()); err == nil {
		t.Fatal("el CHECK dejó pasar un tipo desconocido")
	}
}
