package mensajeria

import (
	"context"
	"testing"

	catclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/catalogos"
	"github.com/AI27-Tech/pf-pkg-platform-go/bus"
	"github.com/AI27-Tech/pf-pkg-platform-go/worker"
)

// publisherFake implementa bus.Publisher sin transporte real: solo activa el
// camino en que New() construye el relay de outbox.
type publisherFake struct{}

func (publisherFake) Publish(context.Context, bus.Sobre) error { return nil }

// TestWorkersIncluyeRelayConPublisher verifica que, cuando la raíz inyecta un
// Publisher, New() construye el relay de outbox y Workers() lo devuelve. Es el
// publicador de outbox que exige Workers() en V1 (ADR 29).
func TestWorkersIncluyeRelayConPublisher(t *testing.T) {
	m, err := New(Pools{}, Deps{Publisher: publisherFake{}})
	if err != nil {
		t.Fatalf("New con Publisher: %v", err)
	}
	ws := m.Workers()
	if len(ws) != 1 {
		t.Fatalf("Workers() devolvió %d; se esperaba 1 (el relay de outbox)", len(ws))
	}
	if got := ws[0].Name(); got != "outbox-relay:mensajeria" {
		t.Fatalf("nombre del worker = %q; se esperaba \"outbox-relay:mensajeria\"", got)
	}
	var _ worker.Worker = ws[0]
}

// TestWorkersVacioSinBus verifica que sin bus (arranque standalone, cmd/api) no
// se arranca ningún worker: Workers() va vacío. En V1 el relay es el único
// worker; sin Publisher no hay nada que drenar.
func TestWorkersVacioSinBus(t *testing.T) {
	m, err := New(Pools{}, Deps{})
	if err != nil {
		t.Fatalf("New sin bus: %v", err)
	}
	if got := m.Workers(); len(got) != 0 {
		t.Fatalf("Workers() devolvió %d; se esperaba 0 sin bus", len(got))
	}
}

// suscriptorFake implementa bus.Subscriber capturando los parámetros del
// Subscribe para verificar el cableado del consumidor de catálogos. Devuelve un
// worker cuyo nombre refleja el durableName (como el adaptador real:
// "bus-durable:"+durableName).
type suscriptorFake struct {
	durableName string
	superficie  bus.Sensibilidad
	filtro      []string
}

func (s *suscriptorFake) Subscribe(durableName string, superficie bus.Sensibilidad, filtroTipos []string, h bus.Handler) (worker.Worker, error) {
	s.durableName = durableName
	s.superficie = superficie
	s.filtro = filtroTipos
	return workerConNombre{nombre: "bus-durable:" + durableName}, nil
}

type workerConNombre struct{ nombre string }

func (w workerConNombre) Name() string                  { return w.nombre }
func (workerConNombre) Ready(ctx context.Context) error { return nil }
func (workerConNombre) Run(ctx context.Context) error   { <-ctx.Done(); return nil }

// catalogosFake satisface mensajeria.CatalogosLector para que New envuelva un
// puerto real con la caché. No necesita devolver nada útil: el test solo
// comprueba el cableado del worker.
type catalogosFake struct{}

func (catalogosFake) ListarClientes(ctx context.Context, ids []catclient.Id) (*catclient.PaginaClientes, error) {
	return &catclient.PaginaClientes{}, nil
}

// TestWorkersConSubscriberArrancaConsumidorCatalogos fija que, con Subscriber y
// un puerto Catalogos que envolver, mensajeria expone el consumidor durable de
// catalogo.actualizado (E5-T5), suscrito al tipo correcto sobre la superficie
// amplia, con el durableName del módulo.
func TestWorkersConSubscriberArrancaConsumidorCatalogos(t *testing.T) {
	sub := &suscriptorFake{}
	m, err := New(Pools{}, Deps{Subscriber: sub, Catalogos: catalogosFake{}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if sub.durableName != "mensajeria-catalogos" {
		t.Errorf("durableName = %q, want mensajeria-catalogos", sub.durableName)
	}
	if sub.superficie != bus.SensibilidadAmplia {
		t.Errorf("superficie = %q, want amplia", sub.superficie)
	}
	if len(sub.filtro) != 1 || sub.filtro[0] != "catalogo.actualizado" {
		t.Errorf("filtro = %v, want [catalogo.actualizado]", sub.filtro)
	}
	var tiene bool
	for _, w := range m.Workers() {
		if w.Name() == "bus-durable:mensajeria-catalogos" {
			tiene = true
		}
	}
	if !tiene {
		t.Error("falta el consumidor de catálogos en Workers()")
	}
}

// TestWorkersSinSubscriberNoArrancaConsumidorCatalogos fija que sin Subscriber no
// hay consumidor de catálogos (y Catalogos se usa sin caché).
func TestWorkersSinSubscriberNoArrancaConsumidorCatalogos(t *testing.T) {
	m, err := New(Pools{}, Deps{Catalogos: catalogosFake{}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, w := range m.Workers() {
		if w.Name() == "bus-durable:mensajeria-catalogos" {
			t.Error("sin Subscriber no debe haber consumidor de catálogos")
		}
	}
}
