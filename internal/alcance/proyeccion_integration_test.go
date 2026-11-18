package alcance

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	identidad "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/identidad"
	"github.com/AI27-Tech/pf-pkg-platform-go/bus"
	"github.com/google/uuid"

	"github.com/AI27-Tech/pf-svc-mensajeria/internal/dbtest"
)

func proyeccionDePrueba(t *testing.T) (context.Context, *Proyeccion) {
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
	return ctx, NuevaProyeccion(pool)
}

func sobre(t *testing.T, sub string, clientes []uuid.UUID, en time.Time) bus.Sobre {
	t.Helper()
	ids := make([]string, 0, len(clientes))
	for _, c := range clientes {
		ids = append(ids, c.String())
	}
	raw, err := json.Marshal(map[string]any{
		"sujeto_id": uuid.NewString(), "idp_subject": sub,
		"clientes_efectivos_ids": ids, "cambiado_en": en.Format(time.RFC3339Nano),
	})
	if err != nil {
		t.Fatal(err)
	}
	return bus.Sobre{Tipo: TipoAlcanceCambiado, Payload: raw}
}

func quiereClientes(t *testing.T, ctx context.Context, p *Proyeccion, sub string, quiere ...uuid.UUID) {
	t.Helper()
	got, err := p.Clientes(ctx, sub)
	if err != nil {
		t.Fatalf("Clientes(%s): %v", sub, err)
	}
	if len(got) != len(quiere) {
		t.Fatalf("Clientes(%s) = %v; quiere %v", sub, got, quiere)
	}
	for i := range quiere {
		if got[i] != quiere[i] {
			t.Fatalf("Clientes(%s) = %v; quiere %v", sub, got, quiere)
		}
	}
}

// TestProyeccion_DesdeEvento: el evento fija el conjunto completo por
// idp_subject; reentregar no cambia nada, uno más viejo no gana, vacío quita, y
// un payload sin idp_subject se acusa sin efecto (DEC-149 #20 y #22).
func TestProyeccion_DesdeEvento(t *testing.T) {
	ctx, p := proyeccionDePrueba(t)
	cons := NuevoConsumidor(p)
	a, b := uuid.New(), uuid.New()
	t0 := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)

	quiereClientes(t, ctx, p, "s-1") // sin fila: nada
	for range 2 {
		if err := cons.Procesar(ctx, sobre(t, "s-1", []uuid.UUID{a, b}, t0)); err != nil {
			t.Fatal(err)
		}
	}
	quiereClientes(t, ctx, p, "s-1", a, b)

	if err := cons.Procesar(ctx, sobre(t, "s-1", []uuid.UUID{b}, t0.Add(-time.Minute))); err != nil {
		t.Fatal(err)
	}
	quiereClientes(t, ctx, p, "s-1", a, b) // el viejo no gana

	if err := cons.Procesar(ctx, sobre(t, "s-1", nil, t0.Add(time.Minute))); err != nil {
		t.Fatal(err)
	}
	quiereClientes(t, ctx, p, "s-1") // vacío = quitar

	sinSub := bus.Sobre{Tipo: TipoAlcanceCambiado, Payload: []byte(`{"sujeto_id":"` + uuid.NewString() +
		`","clientes_efectivos_ids":["` + a.String() + `"],"cambiado_en":"2026-11-17T12:00:00Z"}`)}
	if err := cons.Procesar(ctx, sinSub); err != nil {
		t.Fatalf("payload sin idp_subject: %v; quiere acuse sin efecto", err)
	}
	quiereClientes(t, ctx, p, "")
}

// snapshotFake responde el snapshot sin cursor y la continuación con él.
type snapshotFake struct {
	snap, cont identidad.PaginaAlcances
}

func (f snapshotFake) ListarAlcances(_ context.Context, params identidad.ListarAlcancesParams) (*identidad.PaginaAlcances, error) {
	if params.Cursor == nil {
		return &f.snap, nil
	}
	return &f.cont, nil
}

type suscripcionFake struct{}

func (suscripcionFake) Name() string                { return "bus-durable:mensajeria-alcance" }
func (suscripcionFake) Ready(context.Context) error { return nil }
func (suscripcionFake) Run(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

// TestReconstruccion_SnapshotYCursor: el snapshot reemplaza la proyección (el
// ausente se vacía), la continuación agrega lo nuevo y su item vacío quita al
// sujeto. La réplica es not-ready hasta converger.
func TestReconstruccion_SnapshotYCursor(t *testing.T) {
	ctx, p := proyeccionDePrueba(t)
	a, b, c := uuid.New(), uuid.New(), uuid.New()
	t0 := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	// Estado rancio previo: s-viejo ya no está en identidad.
	if err := p.Aplicar(ctx, Alcance{IdpSubject: "s-viejo", Clientes: []uuid.UUID{c}, CambiadoEn: t0}); err != nil {
		t.Fatal(err)
	}
	cursor := "c1"
	snap := snapshotFake{
		snap: identidad.PaginaAlcances{CursorSiguiente: &cursor, Items: []identidad.AlcanceEfectivo{
			{IdpSubject: "s-1", ClientesEfectivosIds: []uuid.UUID{a}, CambiadoEn: t0},
			{IdpSubject: "s-2", ClientesEfectivosIds: []uuid.UUID{b}, CambiadoEn: t0},
		}},
		cont: identidad.PaginaAlcances{Items: []identidad.AlcanceEfectivo{
			{IdpSubject: "s-1", ClientesEfectivosIds: []uuid.UUID{}, CambiadoEn: t0.Add(time.Minute)},
			{IdpSubject: "s-3", ClientesEfectivosIds: []uuid.UUID{a, c}, CambiadoEn: t0.Add(time.Minute)},
		}},
	}
	w := NuevoReconstructor(suscripcionFake{}, snap, p)
	if w.Name() != NombreWorker {
		t.Errorf("Name = %q; quiere %q", w.Name(), NombreWorker)
	}
	if w.Ready(ctx) == nil {
		t.Fatal("antes de correr: Ready nil; quiere not-ready")
	}
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	go func() { _ = w.Run(runCtx) }()
	limite := time.Now().Add(10 * time.Second)
	for w.Ready(ctx) != nil {
		if time.Now().After(limite) {
			t.Fatalf("no convergió: %v", w.Ready(ctx))
		}
		time.Sleep(20 * time.Millisecond)
	}
	quiereClientes(t, ctx, p, "s-viejo") // ausente del snapshot: vaciado
	quiereClientes(t, ctx, p, "s-1")     // item vacío en la continuación: quitado
	quiereClientes(t, ctx, p, "s-2", b)
	quiereClientes(t, ctx, p, "s-3", a, c)
}
