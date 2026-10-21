// catalogo.go es el consumidor durable de mensajeria para catalogo.actualizado
// (ADR 29, DEC-057): escucha las mutaciones de catálogo que publica catalogos e
// invalida la entrada correspondiente de la caché en proceso de clientes
// (internal/catalogoscache), para que la próxima lectura por lote vaya a
// catalogos y recachee.
//
// Idempotencia EN MEMORIA (ADR 34, DEC-067): el efecto es en memoria (invalidar
// una entrada de caché), así que la deduplicación por evento_id también es en
// memoria —un conjunto de evento_id vistos—, sin tabla inbox_evento. Si el
// proceso reinicia, la caché arranca vacía y no hay nada que invalidar;
// reprocesar un evento tras el reinicio solo volvería a invalidar algo ausente
// (no-op). Reentregar el mismo evento dos veces invalida como mucho una vez, y
// una invalidación repetida no tiene efecto adicional (Property P13).
//
// NOTA: este consumidor es distinto del scaffold durable de consumidores.go, que
// modela idempotencia por tabla inbox_evento para efectos de dominio (membresía,
// enrutamiento). catalogo.actualizado NO escribe BD: su efecto es invalidar
// caché en memoria, así que no usa inbox ni DLQ.
package consumidores

import (
	"context"
	"encoding/json"
	"sync"

	catclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/catalogos"
	"github.com/AI27-Tech/pf-pkg-platform-go/bus"
	"github.com/google/uuid"
)

// TipoCatalogoActualizado es el único tipo que este consumidor procesa. Coincide
// con el tipo que catalogos registra y publica (contrato: registro.tipos.yaml).
const TipoCatalogoActualizado = "catalogo.actualizado"

// InvalidadorClientes es lo que el consumidor necesita de la caché: borrar la
// entrada de una entidad+id. Lo satisface *catalogoscache.CacheClientes. Se
// declara aquí (en el consumidor) para depender de un puerto estrecho y no del
// tipo concreto de la caché.
type InvalidadorClientes interface {
	Invalidar(entidad string, id catclient.Id)
}

// payloadCatalogoActualizado es la vista del cuerpo JSON del evento. catalogos
// publica solo metadato —tipo + identidad, nunca el registro completo—: `entidad`
// (p. ej. "cliente") y `entidad_id`. La versión NO viaja en el payload (brecha
// C10 del contrato): llega en bus.Sobre.Version; este consumidor no la necesita,
// porque invalidar es incondicional (no compara versiones).
type payloadCatalogoActualizado struct {
	Entidad   string `json:"entidad"`
	EntidadID string `json:"entidad_id"`
}

// ConsumidorCatalogo invalida la caché de clientes ante catalogo.actualizado, con
// deduplicación en memoria por evento_id. Es seguro para uso concurrente.
type ConsumidorCatalogo struct {
	cache InvalidadorClientes

	mu     sync.Mutex
	vistos map[string]struct{} // evento_id ya procesados (dedup en memoria)
}

// NuevoConsumidorCatalogo construye el consumidor sobre la caché a invalidar.
func NuevoConsumidorCatalogo(cache InvalidadorClientes) *ConsumidorCatalogo {
	return &ConsumidorCatalogo{
		cache:  cache,
		vistos: make(map[string]struct{}),
	}
}

// Procesar maneja un sobre entregado por el bus. Deduplica por evento_id: un
// evento ya visto es un no-op (P13). Un sobre de otro tipo se ignora (acuse sin
// efecto): el filtro del Subscribe ya restringe a catalogo.actualizado, pero el
// consumidor es defensivo. Un payload ilegible o incompleto se acusa sin
// invalidar: no es reintentable y no debe bloquear la cola; el peor efecto de NO
// invalidar es servir un cliente algo obsoleto hasta que venza el TTL.
func (c *ConsumidorCatalogo) Procesar(ctx context.Context, s bus.Sobre) error {
	if s.Tipo != TipoCatalogoActualizado {
		return nil // no es lo nuestro; acuse sin efecto
	}

	if s.EventoID != "" {
		c.mu.Lock()
		_, ya := c.vistos[s.EventoID]
		if !ya {
			c.vistos[s.EventoID] = struct{}{}
		}
		c.mu.Unlock()
		if ya {
			return nil // duplicado: no-op idempotente (P13)
		}
	}

	var p payloadCatalogoActualizado
	if err := json.Unmarshal(s.Payload, &p); err != nil {
		return nil // payload inválido: acuse sin invalidar (no reintentable)
	}
	if p.Entidad == "" || p.EntidadID == "" {
		return nil // metadato incompleto: nada que invalidar
	}

	id, err := uuid.Parse(p.EntidadID)
	if err != nil {
		return nil // id mal formado: acuse sin invalidar
	}

	// Invalidación incondicional. La caché ignora entidades distintas de "cliente"
	// (contrato, sla, vehiculo, …): para ellas es un no-op. No se compara versión:
	// una invalidación de más solo provoca una relectura, nunca un dato incorrecto.
	c.cache.Invalidar(p.Entidad, id)
	return nil
}
