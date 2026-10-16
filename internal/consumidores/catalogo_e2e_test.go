package consumidores_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	catclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/catalogos"
	"github.com/AI27-Tech/pf-pkg-platform-go/bus"
	"github.com/AI27-Tech/pf-pkg-platform-go/bus/memory"
	"github.com/AI27-Tech/pf-pkg-platform-go/worker"
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/catalogoscache"
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/consumidores"
	"github.com/google/uuid"
)

// Prueba de EXTREMO A EXTREMO del DoD de E5-T5 con el bus en memoria real: un
// catalogo.actualizado publicado por el bus llega al consumidor durable, que
// invalida la caché de clientes, y la siguiente lectura reconsulta a catalogos.

type registroPrueba struct{}

func (registroPrueba) Allowed(tipo string, version int) bool {
	return tipo == consumidores.TipoCatalogoActualizado && version == 1
}
func (registroPrueba) OrderingKey(s bus.Sobre) string { return s.AgregadoID }

// subyacenteContador cuenta viajes a catalogos de forma segura para concurrencia.
type subyacenteContador struct {
	mu       sync.Mutex
	llamadas int
}

func (s *subyacenteContador) ListarClientes(ctx context.Context, ids []catclient.Id) (*catclient.PaginaClientes, error) {
	s.mu.Lock()
	s.llamadas++
	s.mu.Unlock()
	items := make([]catclient.Cliente, 0, len(ids))
	for _, id := range ids {
		items = append(items, catclient.Cliente{Id: id})
	}
	return &catclient.PaginaClientes{Items: items}, nil
}

func (s *subyacenteContador) conteo() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.llamadas
}

func TestInvalidacionDeExtremoAExtremo(t *testing.T) {
	sub := &subyacenteContador{}
	cache := catalogoscache.Nueva(sub, time.Hour) // TTL largo: solo invalida el evento
	cons := consumidores.NuevoConsumidorCatalogo(cache)

	b := memory.Nuevo(registroPrueba{}, memory.Opciones{})
	w, err := b.Subscribe(
		"mensajeria-catalogos",
		bus.SensibilidadAmplia,
		[]string{consumidores.TipoCatalogoActualizado},
		func(ctx context.Context, s bus.Sobre) error { return cons.Procesar(ctx, s) },
	)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hecho := make(chan struct{})
	go func() {
		_ = worker.RunGroup(ctx, nil, w)
		close(hecho)
	}()

	id := uuid.New()
	// Primera lectura: va a catalogos y cachea (1 viaje).
	if _, err := cache.ListarClientes(ctx, []catclient.Id{id}); err != nil {
		t.Fatalf("primera lectura: %v", err)
	}
	if got := sub.conteo(); got != 1 {
		t.Fatalf("tras primera lectura, viajes=%d, want 1", got)
	}

	// Publicar catalogo.actualizado del cliente.
	payload, _ := json.Marshal(map[string]string{"entidad": "cliente", "entidad_id": id.String()})
	if err := b.Publish(ctx, bus.Sobre{
		EventoID:     "ev-e2e-1",
		Tipo:         consumidores.TipoCatalogoActualizado,
		Version:      1,
		AgregadoID:   id.String(),
		Sensibilidad: bus.SensibilidadAmplia,
		Payload:      payload,
	}); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	// La relectura debe reconsultar una vez invalidado. Polling corto (entrega async).
	if !esperarReconsulta(ctx, cache, sub, id, 2) {
		t.Fatalf("tras el evento, la caché no se invalidó: viajes=%d, want 2", sub.conteo())
	}

	cancel()
	<-hecho
}

func esperarReconsulta(ctx context.Context, cache *catalogoscache.CacheClientes, sub *subyacenteContador, id catclient.Id, quiere int) bool {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := cache.ListarClientes(ctx, []catclient.Id{id}); err != nil {
			return false
		}
		if sub.conteo() >= quiere {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}
