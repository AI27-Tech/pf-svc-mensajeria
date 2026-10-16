package consumidores

import (
	"context"
	"encoding/json"
	"testing"

	catclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/catalogos"
	"github.com/AI27-Tech/pf-pkg-platform-go/bus"
	"github.com/google/uuid"
)

// invalidadorFake registra las invalidaciones recibidas para contarlas y
// verificar la entidad/id exactos.
type invalidadorFake struct {
	llamadas []invocacion
}

type invocacion struct {
	entidad string
	id      catclient.Id
}

func (f *invalidadorFake) Invalidar(entidad string, id catclient.Id) {
	f.llamadas = append(f.llamadas, invocacion{entidad: entidad, id: id})
}

func sobreCatalogo(t *testing.T, eventoID, entidad, entidadID string) bus.Sobre {
	t.Helper()
	payload, err := json.Marshal(map[string]string{"entidad": entidad, "entidad_id": entidadID})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return bus.Sobre{
		EventoID:     eventoID,
		Tipo:         TipoCatalogoActualizado,
		Version:      1,
		AgregadoID:   entidadID,
		Sensibilidad: bus.SensibilidadAmplia,
		Payload:      payload,
	}
}

// TestProcesarInvalidaUnaVez: un catalogo.actualizado de cliente invalida esa
// entrada exactamente una vez.
func TestProcesarInvalidaUnaVez(t *testing.T) {
	inv := &invalidadorFake{}
	c := NuevoConsumidorCatalogo(inv)

	id := uuid.New()
	if err := c.Procesar(context.Background(), sobreCatalogo(t, "ev-1", "cliente", id.String())); err != nil {
		t.Fatalf("Procesar: %v", err)
	}
	if len(inv.llamadas) != 1 {
		t.Fatalf("se esperaba 1 invalidación; hubo %d", len(inv.llamadas))
	}
	if inv.llamadas[0].entidad != "cliente" || inv.llamadas[0].id != id {
		t.Errorf("invalidación inesperada: %+v (esperaba cliente/%s)", inv.llamadas[0], id)
	}
}

// TestDobleEntregaInvalidaUnaVez es la propiedad P13: reentregar el MISMO
// evento_id solo surte efecto una vez.
func TestDobleEntregaInvalidaUnaVez(t *testing.T) {
	inv := &invalidadorFake{}
	c := NuevoConsumidorCatalogo(inv)

	s := sobreCatalogo(t, "ev-dup", "cliente", uuid.New().String())
	if err := c.Procesar(context.Background(), s); err != nil {
		t.Fatalf("primera entrega: %v", err)
	}
	if err := c.Procesar(context.Background(), s); err != nil {
		t.Fatalf("segunda entrega: %v", err)
	}
	if len(inv.llamadas) != 1 {
		t.Errorf("P13: doble entrega del mismo evento_id debe invalidar una vez; hubo %d", len(inv.llamadas))
	}
}

// TestTipoDistintoNoInvalida: un sobre de otro tipo se acusa sin invalidar.
func TestTipoDistintoNoInvalida(t *testing.T) {
	inv := &invalidadorFake{}
	c := NuevoConsumidorCatalogo(inv)

	s := sobreCatalogo(t, "ev-x", "cliente", uuid.New().String())
	s.Tipo = "viaje.creado"
	if err := c.Procesar(context.Background(), s); err != nil {
		t.Fatalf("Procesar: %v", err)
	}
	if len(inv.llamadas) != 0 {
		t.Errorf("un tipo distinto no debe invalidar; hubo %d", len(inv.llamadas))
	}
}

// TestPayloadInvalidoNoInvalidaYAcusa: payloads inservibles se acusan (nil) sin
// invalidar y sin reintentarse.
func TestPayloadInvalidoNoInvalidaYAcusa(t *testing.T) {
	casos := []struct {
		nombre  string
		payload []byte
	}{
		{"json ilegible", []byte("{no es json")},
		{"entidad vacía", []byte(`{"entidad":"","entidad_id":"` + uuid.New().String() + `"}`)},
		{"entidad_id vacío", []byte(`{"entidad":"cliente","entidad_id":""}`)},
		{"uuid mal formado", []byte(`{"entidad":"cliente","entidad_id":"no-es-uuid"}`)},
	}
	for _, caso := range casos {
		t.Run(caso.nombre, func(t *testing.T) {
			inv := &invalidadorFake{}
			c := NuevoConsumidorCatalogo(inv)
			s := bus.Sobre{
				EventoID:     "ev-" + caso.nombre,
				Tipo:         TipoCatalogoActualizado,
				Version:      1,
				Sensibilidad: bus.SensibilidadAmplia,
				Payload:      caso.payload,
			}
			if err := c.Procesar(context.Background(), s); err != nil {
				t.Fatalf("Procesar debe acusar (nil) un payload inválido; got %v", err)
			}
			if len(inv.llamadas) != 0 {
				t.Errorf("un payload inválido no debe invalidar; hubo %d", len(inv.llamadas))
			}
		})
	}
}
