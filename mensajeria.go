// Package mensajeria es dueño de la conversación de negocio entre el cliente y
// AI27 —su enrutamiento a una bandeja, el reclamo que autoriza a responder, el
// orden canónico de sus mensajes y su auditoría— y del despacho de
// notificaciones a proveedores externos (mensajeria.md (a)). La comunicación NO
// es un chat con un operador nombrado: es una conversación con una bandeja
// compartida atendida por un pool de Ejecutivos (DEC-046); operacion NO asigna
// al ejecutivo de chat.
//
// Capa de soporte. Vive en su PROPIO deployable `mensajeria` por aislamiento de
// falla frente a proveedores externos (mensajeria.md (d), (k); ADR 17), no por
// volumen. Su eje de escalamiento son notificaciones/hora y conversaciones
// activas, no conexiones abiertas (eso es realtime).
//
// Aristas síncronas salientes, ambas HTTP (00, tabla de servicios): a catalogos
// (lectura por lote de clientes, mensajeria.md (f)) y la restringida a identidad
// para revocación (DEC-051). NO llama a operacion, identidad ni realtime en la
// ruta caliente: el ciclo de vida llega por evento y los sobres salen por
// outbox. Arista entrante: solo bff, siempre HTTP (su adaptador vive en pf-svc-bff).
//
// Dos pools del MISMO rol svc_mensajeria (mensajeria.md (d), (k); MSG-REC §6.2):
// uno para la ruta de chat y otro, con presupuesto propio, para el despacho
// externo (la mampara). Ninguno abre conexión hacia otro esquema (I1).
//
// Scaffolding inicial: la estructura y el cableado existen; la lógica de negocio
// real todavía no, salvo los dos caminos que el prompt 07 exige con lógica
// mínima (envío idempotente + outbox en una tx; reclamo con conflicto de
// versión). Varias superficies están bloqueadas por brechas de contrato o de
// producto (ver README): emergencia (M9) y catch-up de bandeja (M3) esperan
// tracer.9; el vínculo Ejecutivo↔cliente lo debe Producto.
package mensajeria

import (
	"context"
	"net/http"

	"github.com/AI27-Tech/pf-pkg-platform-go/bus"
	"github.com/AI27-Tech/pf-pkg-platform-go/outbox"
	"github.com/AI27-Tech/pf-pkg-platform-go/pgxpool"
	"github.com/AI27-Tech/pf-pkg-platform-go/worker"
	convhttp "github.com/AI27-Tech/pf-svc-mensajeria/adapters/http/conversacion"
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/autorizacion"
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/catalogoscache"
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/consumidores"
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/conversacion"
	"github.com/AI27-Tech/pf-svc-mensajeria/internal/reclamos"
)

// Pools agrupa los pools por rol que el módulo necesita. mensajeria usa UN solo
// esquema y UN solo rol (svc_mensajeria), pero DOS pools de ese mismo rol: Chat
// para la ruta de conversación y Despacho para la mampara de notificaciones, con
// presupuestos separados (mensajeria.md (d), (k)). Nunca comparte pool entre
// roles ni abre conexión a otro esquema (00 regla 4, I1).
type Pools struct {
	// Chat es el pool de la ruta de conversación (envío, reclamo, lectura).
	Chat *pgxpool.Pool
	// Despacho es el pool de la mampara de despacho externo, con su propio
	// presupuesto. En V1 no hay worker de despacho (DEC-023), pero el pool se
	// recibe por separado para que la separación de presupuesto sea real desde el
	// scaffold.
	Despacho *pgxpool.Pool
}

// Deps agrupa los puertos de salida que la raíz resuelve por el consumidor.
type Deps struct {
	// Catalogos es la arista HTTP a catalogos (lectura por lote de clientes).
	// Puede ser nil en el esqueleto; sin ella la caché de cliente queda sin
	// resolver.
	Catalogos CatalogosLector
	// Revocacion es la arista restringida a identidad (DEC-051). Opcional en el
	// esqueleto.
	Revocacion RevocacionPort
	// Publisher es el transporte del bus para el relay de outbox. Nil en arranque
	// standalone sin bus: en ese caso Workers() no arranca el relay.
	Publisher bus.Publisher
	// Subscriber es el bus durable del que mensajeria consume (viaje.*,
	// sesion.revocada, eventos de identidad). Nil sin bus: Workers() no arranca
	// consumidores.
	Subscriber bus.Subscriber
}

// Module es la instancia construida del módulo mensajeria.
type Module struct {
	pools        Pools
	deps         Deps
	conversacion *conversacion.Servicio
	reclamos     *reclamos.Servicio
	autz         *autorizacion.Servicio
	relayOutbox  worker.Worker // nil sin Publisher (standalone sin bus)
	// consumidorCatalogos es el worker durable de catalogo.actualizado que
	// invalida la caché en proceso de clientes (E5-T5). nil sin Subscriber+Catalogos.
	consumidorCatalogos worker.Worker
}

// New recibe los dos pools de svc_mensajeria y los puertos ya resueltos. No lee
// entorno (00 regla 4): la raíz cablea todo. Construye los servicios de los dos
// caminos calientes sobre el pool de chat y, si Deps.Publisher está presente,
// arma el relay de outbox sobre el pool/rol svc_mensajeria para que Workers() lo
// devuelva.
func New(p Pools, d Deps) (*Module, error) {
	m := &Module{pools: p, deps: d}

	pub := outbox.NewPublisher()
	m.conversacion = conversacion.NewServicio(p.Chat, pub)
	m.reclamos = reclamos.NewServicio(p.Chat)
	m.autz = autorizacion.NewServicio(p.Chat)

	if d.Publisher != nil {
		// Relay de outbox sobre el pool/rol svc_mensajeria (chat). Publica el
		// sobre de dominio; marca publicado tras el acuse del broker (ADR 29).
		m.relayOutbox = outbox.NewRelay("outbox-relay:mensajeria", p.Chat, d.Publisher, outbox.RelayOpciones{})
	}

	// Caché de clientes + su consumidor de invalidación (E5-T5): solo con
	// Subscriber y con un puerto Catalogos que envolver. Se envuelve el Catalogos
	// inyectado con la caché en proceso y se guarda de vuelta en deps, de modo que
	// toda lectura por m.deps.Catalogos use la MISMA instancia que el consumidor
	// invalida. Sin Subscriber no se cachea: sin bus no hay evento que invalide y
	// una caché sin invalidación solo acumularía obsolescencia.
	if d.Subscriber != nil && d.Catalogos != nil {
		cache := catalogoscache.Nueva(d.Catalogos, 0) // 0 ⇒ TTL por defecto
		m.deps.Catalogos = cache
		cons := consumidores.NuevoConsumidorCatalogo(cache)
		handler := func(ctx context.Context, s bus.Sobre) error {
			return cons.Procesar(ctx, s)
		}
		w, err := d.Subscriber.Subscribe(
			"mensajeria-catalogos",
			bus.SensibilidadAmplia,
			[]string{consumidores.TipoCatalogoActualizado},
			handler,
		)
		if err != nil {
			return nil, err
		}
		m.consumidorCatalogos = w
	}

	return m, nil
}

// Routes monta las rutas HTTP del módulo bajo /api/v1/... en el mux dado.
//
// Monta ReclamarConversacion y EnviarMensaje sobre internal/reclamos e
// internal/conversacion, detrás de la guarda de internal/autorizacion (DEC-097).
// El bff antepone auth.Middleware, así que los claims llegan en el contexto. El
// alias_mostrado del mensaje se resuelve en el handler desde el cliente de la
// conversación (Opción 2, DEC-097 forma 1); el dominio no llama a catalogos.
//
// Emergencia (M9) y catch-up de bandeja (M3) NO se montan: esperan tracer.9
// (DEC-089/090).
func (m *Module) Routes(mux *http.ServeMux) {
	convhttp.Handlers{
		Autz:         m.autz,
		Reclamos:     m.reclamos,
		Reclamo:      m.reclamos,
		Conversacion: m.conversacion,
		Catalogos:    m.deps.Catalogos,
	}.Montar(mux)
}

// Workers devuelve los workers embebidos del módulo (ADR 29 §6), cada uno con el
// rol y pool de svc_mensajeria. La raíz de producción (cmd/mensajeria) decide
// arrancarlos; cmd/api (standalone) no.
//
// En V1 devuelve, cuando la raíz inyectó el bus:
//   - el publicador de outbox (relay), presente si hay Deps.Publisher;
//   - (pendiente de cableado al bus) un consumidor por suscripción durable
//     contratada —viaje.*, sesion.revocada, eventos de identidad— y el difusor
//     interno de sesion.revocada a réplicas (ADR 26). Sus efectos de dominio y su
//     snapshot inicial están sin implementar en el scaffold, así que todavía no
//     se abren suscripciones que no apliquen nada.
//
// NO devuelve worker de despacho externo (DEC-023 #2): la mampara existe en
// internal/despacho sin worker activo ni destinatario cliente.
func (m *Module) Workers() []worker.Worker {
	var ws []worker.Worker
	if m.relayOutbox != nil {
		ws = append(ws, m.relayOutbox)
	}
	if m.consumidorCatalogos != nil {
		ws = append(ws, m.consumidorCatalogos)
	}
	return ws
}

// NOTA — SIN Consultas(): 00 dice que Consultas() existe solo si el módulo es
// destino de una arista síncrona ENTRANTE EN PROCESO. La única arista entrante
// de mensajeria es bff → mensajeria, y es SIEMPRE HTTP (05, 09): su adaptador
// vive en pf-svc-bff, no hay consumidor en proceso. Por eso mensajeria no expone
// Consultas(). (Interpretación registrada para que 00 la precise; ver README.)
