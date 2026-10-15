package mensajeria

import (
	"context"
	"testing"

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
