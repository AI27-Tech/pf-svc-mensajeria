// solicitud.go aloja el consumidor durable de la conversación de la SOLICITUD
// (DEC-148 #5, #30, #48), dirigido por los dos eventos de operacion:
//
//   - solicitud.creada → crea o reutiliza la conversación `tipo=solicitud`.
//   - solicitud.estado_cambiado → fija la ventana de escritura: 3 días desde que
//     la solicitud termina (incluido APROBADO) o vence (VENCIDA); un posterior
//     PAGADO la reabre (DEC-148 #35).
//
// Los dos tipos van en UNA suscripción durable: el bus ordena por agregado
// (solicitud_id) dentro de una suscripción, no entre dos, y así el cambio de
// estado nunca se procesa antes que la creación. Aun así el servicio crea la
// conversación si falta, así que un orden distinto no pierde el efecto.
//
// Como los consumidores del viaje (conversacion.go), la idempotencia es del
// estado durable y no de inbox_evento (DEC-067): la creación la arbitra el
// índice único parcial y la ventana converge por cambiado_en.
package consumidores

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/AI27-Tech/pf-pkg-platform-go/bus"
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/conversacion"
	"github.com/google/uuid"
)

// Tipos de evento de la solicitud, productor operacion (registro.tipos.yaml).
const (
	TipoSolicitudCreada         = "solicitud.creada"
	TipoSolicitudEstadoCambiado = "solicitud.estado_cambiado"
)

// TiposSolicitud son los eventos que consume ConsumidorSolicitud.
var TiposSolicitud = []string{TipoSolicitudCreada, TipoSolicitudEstadoCambiado}

// ConversacionSolicitud es lo que el consumidor necesita del servicio de
// conversación. Lo satisface *conversacion.Servicio.
type ConversacionSolicitud interface {
	CrearSolicitudPorEvento(ctx context.Context, in conversacion.EntradaCreacion) (bool, error)
	AplicarEstadoSolicitud(ctx context.Context, solicitudID, clienteID, aEstado string, cambiadoEn time.Time) error
}

// payloadSolicitudCreada y payloadSolicitudEstado son la vista del cuerpo JSON
// (nombres del .proto; marcas de tiempo en RFC 3339).
type payloadSolicitudCreada struct {
	SolicitudID string `json:"solicitud_id"`
	ClienteID   string `json:"cliente_id"`
}

type payloadSolicitudEstado struct {
	SolicitudID string    `json:"solicitud_id"`
	ClienteID   string    `json:"cliente_id"`
	AEstado     string    `json:"a_estado"`
	CambiadoEn  time.Time `json:"cambiado_en"`
}

// ConsumidorSolicitud aplica los eventos de la solicitud a su conversación.
type ConsumidorSolicitud struct {
	conv ConversacionSolicitud
}

// NuevoConsumidorSolicitud construye el consumidor sobre el servicio.
func NuevoConsumidorSolicitud(conv ConversacionSolicitud) *ConsumidorSolicitud {
	return &ConsumidorSolicitud{conv: conv}
}

// idsValidos dice si todos los ids son UUID. Las columnas son uuid: uno que no lo
// sea haría fallar a Postgres en cada entrega y el reintento nunca terminaría.
func idsValidos(ids ...string) bool {
	for _, id := range ids {
		if _, err := uuid.Parse(id); err != nil {
			return false
		}
	}
	return true
}

// Procesar despacha por tipo. Un tipo ajeno, un payload ilegible, sin los datos
// que el efecto necesita o con ids que no son UUID se acusa sin efecto y se
// registra (no reintentable). Un error de BD se propaga para reintentar: ambos
// efectos son idempotentes.
func (c *ConsumidorSolicitud) Procesar(ctx context.Context, s bus.Sobre) error {
	switch s.Tipo {
	case TipoSolicitudCreada:
		var p payloadSolicitudCreada
		if json.Unmarshal(s.Payload, &p) != nil || p.SolicitudID == "" || p.ClienteID == "" {
			return nil
		}
		if !idsValidos(p.SolicitudID, p.ClienteID) {
			slog.WarnContext(ctx, "mensajeria: solicitud.creada con ids que no son UUID, se descarta",
				slog.String("evento_id", s.EventoID))
			return nil
		}
		if _, err := c.conv.CrearSolicitudPorEvento(ctx, conversacion.EntradaCreacion{ViajeID: p.SolicitudID, ClienteID: p.ClienteID}); err != nil {
			return fmt.Errorf("consumidores: crear conversación por solicitud.creada: %w", err)
		}
	case TipoSolicitudEstadoCambiado:
		var p payloadSolicitudEstado
		if json.Unmarshal(s.Payload, &p) != nil || p.SolicitudID == "" || p.ClienteID == "" || p.AEstado == "" || p.CambiadoEn.IsZero() {
			return nil
		}
		if !idsValidos(p.SolicitudID, p.ClienteID) {
			slog.WarnContext(ctx, "mensajeria: solicitud.estado_cambiado con ids que no son UUID, se descarta",
				slog.String("evento_id", s.EventoID))
			return nil
		}
		if err := c.conv.AplicarEstadoSolicitud(ctx, p.SolicitudID, p.ClienteID, p.AEstado, p.CambiadoEn); err != nil {
			return fmt.Errorf("consumidores: fijar ventana por solicitud.estado_cambiado: %w", err)
		}
	}
	return nil
}
