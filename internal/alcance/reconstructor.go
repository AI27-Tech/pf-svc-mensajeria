package alcance

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	identidad "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/identidad"
	"github.com/AI27-Tech/pf-pkg-platform-go/worker"
)

// Snapshot es identidad.listarAlcances. Lo satisface el puerto
// mensajeria.Alcances (y, en core, la sub-fachada Alcances() de identidad).
type Snapshot interface {
	ListarAlcances(ctx context.Context, params identidad.ListarAlcancesParams) (*identidad.PaginaAlcances, error)
}

// presupuesto es el tiempo de cada llamada al snapshot. Se aplica aquí, y no en
// el adaptador, para que el HTTP y el en proceso de core lo compartan (I3).
const presupuesto = 2 * time.Second

// NombreWorker es el nombre del worker de reconstrucción en Workers() y en la
// readiness del proceso.
const NombreWorker = "mensajeria-alcance"

// reconstructor envuelve la suscripción durable a alcance.cambiado con la
// reconstrucción al arrancar (patrón ADR 26 §4, DEC-149 #20): not-ready hasta
// converger. Implementa worker.Worker.
type reconstructor struct {
	suscripcion worker.Worker
	snapshot    Snapshot
	proy        *Proyeccion
	convergido  atomic.Bool
	errPrecarga atomic.Pointer[error]
}

// NuevoReconstructor envuelve el worker de suscripción. La raíz lo arranca en
// lugar de la suscripción desnuda.
func NuevoReconstructor(suscripcion worker.Worker, snapshot Snapshot, proy *Proyeccion) worker.Worker {
	return &reconstructor{suscripcion: suscripcion, snapshot: snapshot, proy: proy}
}

func (r *reconstructor) Name() string { return NombreWorker }

// Ready es not-ready hasta que la precarga converge (con su causa si falló);
// después delega en la suscripción.
func (r *reconstructor) Ready(ctx context.Context) error {
	if !r.convergido.Load() {
		if ep := r.errPrecarga.Load(); ep != nil {
			return fmt.Errorf("reconstrucción de alcance no convergió: %w", *ep)
		}
		return fmt.Errorf("reconstrucción de alcance en curso: not-ready")
	}
	return r.suscripcion.Ready(ctx)
}

// Run arranca la suscripción ANTES de la precarga y luego converge. Si la
// precarga falla queda not-ready y espera a la suscripción (sin reintento, como
// la de revocación: la réplica no recibe tráfico hasta reiniciar).
func (r *reconstructor) Run(ctx context.Context) error {
	errSub := make(chan error, 1)
	go func() { errSub <- r.suscripcion.Run(ctx) }()
	if err := r.precargar(ctx); err != nil {
		r.errPrecarga.Store(&err)
		return <-errSub
	}
	r.convergido.Store(true)
	return <-errSub
}

// precargar carga el snapshot (reemplaza la proyección) y luego la
// continuación con su cursor: todo lo que cambió después del snapshot,
// incluidos los sujetos que quedaron vacíos. Así no queda ventana entre el
// snapshot y el arranque de la suscripción.
func (r *reconstructor) precargar(ctx context.Context) error {
	snap, err := r.listar(ctx, nil)
	if err != nil {
		return fmt.Errorf("precargar snapshot de alcances: %w", err)
	}
	if err := r.proy.Reemplazar(ctx, deWire(snap.Items)); err != nil {
		return err
	}
	if snap.CursorSiguiente == nil {
		return nil
	}
	cont, err := r.listar(ctx, snap.CursorSiguiente)
	if err != nil {
		return fmt.Errorf("continuar snapshot de alcances: %w", err)
	}
	for _, a := range deWire(cont.Items) {
		if err := r.proy.Aplicar(ctx, a); err != nil {
			return err
		}
	}
	return nil
}

func (r *reconstructor) listar(ctx context.Context, cursor *string) (*identidad.PaginaAlcances, error) {
	ctx, cancel := context.WithTimeout(ctx, presupuesto)
	defer cancel()
	p, err := r.snapshot.ListarAlcances(ctx, identidad.ListarAlcancesParams{Cursor: cursor})
	if err == nil && p == nil {
		err = fmt.Errorf("identidad devolvió una página vacía")
	}
	return p, err
}

func deWire(items []identidad.AlcanceEfectivo) []Alcance {
	out := make([]Alcance, 0, len(items))
	for _, it := range items {
		out = append(out, Alcance{IdpSubject: it.IdpSubject, Clientes: it.ClientesEfectivosIds, CambiadoEn: it.CambiadoEn})
	}
	return out
}
