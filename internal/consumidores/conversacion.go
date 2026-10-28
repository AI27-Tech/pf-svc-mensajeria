// conversacion.go aloja los consumidores durables del CICLO DE VIDA de la
// conversación dirigido por eventos del viaje (E3-T1):
//
//   - viaje.mensajeria_habilitada → crea la conversación del viaje (ConsumidorHabilitada).
//   - viaje.cerrado → fija la ventana de escritura según el motivo (ConsumidorCerrado).
//
// A diferencia del consumidor de catálogo (efecto en memoria, dedup en memoria),
// el efecto aquí es DURABLE (INSERT/UPDATE en mensajeria.conversacion), así que la
// idempotencia la da el estado durable, no un conjunto en memoria ni inbox_evento
// (DEC-067, mismo criterio que el envío y el consumidor de emergencia): la
// creación la arbitra el índice único parcial una_conversacion_activa_por_viaje;
// el cierre es un UPDATE convergente. Reentregar cualquiera de los dos eventos no
// duplica efecto.
//
// viaje.acceso_revocado (corte de fanout al cliente) NO se implementa aquí: su
// productor no existe en operacion todavía (E3-T1 lo marca bloqueado).
package consumidores

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/AI27-Tech/pf-pkg-platform-go/bus"
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/conversacion"
)

// Tipos de evento del viaje que estos consumidores procesan. Coinciden con el
// registro del contrato (registro.tipos.yaml), productor operacion.
const (
	TipoViajeMensajeriaHabilitada = "viaje.mensajeria_habilitada"
	TipoViajeCerrado              = "viaje.cerrado"
)

// CreadorConversacion es lo que el consumidor de habilitación necesita: crear la
// conversación de un viaje idempotentemente. Lo satisface *conversacion.Servicio.
// Puerto estrecho declarado por el consumidor (golang-best-practices §6).
type CreadorConversacion interface {
	CrearPorEvento(ctx context.Context, in conversacion.EntradaCreacion) (bool, error)
}

// CerradorVentana es lo que el consumidor de cierre necesita: fijar la ventana de
// escritura de la conversación del viaje. Lo satisface *conversacion.Servicio.
type CerradorVentana interface {
	CerrarVentana(ctx context.Context, viajeID, motivoCierre string, cerradoEn time.Time) error
}

// --- viaje.mensajeria_habilitada → crear conversación ----------------------

// payloadMensajeriaHabilitada es la vista del cuerpo del evento que publica
// operacion. El agregado es el viaje; cliente_id acota la conversación.
type payloadMensajeriaHabilitada struct {
	ViajeID   string `json:"viaje_id"`
	ClienteID string `json:"cliente_id"`
}

// ConsumidorHabilitada crea la conversación del viaje al recibir
// viaje.mensajeria_habilitada. Idempotente por el índice único parcial; no
// deduplica por su cuenta.
type ConsumidorHabilitada struct {
	creador CreadorConversacion
}

// NuevoConsumidorHabilitada construye el consumidor sobre el servicio de creación.
func NuevoConsumidorHabilitada(creador CreadorConversacion) *ConsumidorHabilitada {
	return &ConsumidorHabilitada{creador: creador}
}

// Procesar crea la conversación del viaje. Un tipo distinto se ignora (el filtro
// del Subscribe ya restringe; defensivo). Payload ilegible o sin viaje_id se acusa
// sin crear (no reintentable). Sin tenant en el sobre no se puede acotar: se
// rechaza como reintentable (defecto de publicación, no del consumidor). La
// creación es idempotente por el índice único: reentregar no crea una segunda.
func (c *ConsumidorHabilitada) Procesar(ctx context.Context, s bus.Sobre) error {
	if s.Tipo != TipoViajeMensajeriaHabilitada {
		return nil // no es lo nuestro; acuse sin efecto
	}
	var p payloadMensajeriaHabilitada
	if err := json.Unmarshal(s.Payload, &p); err != nil {
		return nil // payload inválido: acuse sin crear (no reintentable)
	}
	if p.ViajeID == "" {
		return nil // sin viaje no hay conversación que crear
	}
	if _, err := c.creador.CrearPorEvento(ctx, conversacion.EntradaCreacion{
		ViajeID:   p.ViajeID,
		ClienteID: p.ClienteID,
	}); err != nil {
		return fmt.Errorf("consumidores: crear conversación por viaje.mensajeria_habilitada: %w", err)
	}
	return nil
}

// --- viaje.cerrado → fijar ventana de escritura ----------------------------

// payloadViajeCerrado es la vista del cuerpo del evento que publica operacion al
// cerrar un viaje. motivo_cierre decide la ventana (por_incidencia ⇒ 6 meses).
type payloadViajeCerrado struct {
	ViajeID      string    `json:"viaje_id"`
	MotivoCierre string    `json:"motivo_cierre"`
	CerradoEn    time.Time `json:"cerrado_en"`
}

// ConsumidorCerrado fija la ventana de escritura de la conversación del viaje al
// recibir viaje.cerrado. Idempotente: el UPDATE converge al mismo escritura_hasta.
type ConsumidorCerrado struct {
	cerrador CerradorVentana
}

// NuevoConsumidorCerrado construye el consumidor sobre el servicio de cierre.
func NuevoConsumidorCerrado(cerrador CerradorVentana) *ConsumidorCerrado {
	return &ConsumidorCerrado{cerrador: cerrador}
}

// Procesar fija la ventana de escritura. Payload ilegible o sin viaje_id/cerrado_en
// se acusa sin efecto. Un error de BD se propaga (reintentable; el UPDATE es
// idempotente). Un motivo vacío cae a la ventana sin incidencia (3 días), el caso
// conservador por defecto.
func (c *ConsumidorCerrado) Procesar(ctx context.Context, s bus.Sobre) error {
	if s.Tipo != TipoViajeCerrado {
		return nil // no es lo nuestro; acuse sin efecto
	}
	var p payloadViajeCerrado
	if err := json.Unmarshal(s.Payload, &p); err != nil {
		return nil // payload inválido: acuse sin efecto (no reintentable)
	}
	if p.ViajeID == "" || p.CerradoEn.IsZero() {
		return nil // metadato incompleto: nada que fijar
	}
	if err := c.cerrador.CerrarVentana(ctx, p.ViajeID, p.MotivoCierre, p.CerradoEn); err != nil {
		return fmt.Errorf("consumidores: fijar ventana por viaje.cerrado: %w", err)
	}
	return nil
}
