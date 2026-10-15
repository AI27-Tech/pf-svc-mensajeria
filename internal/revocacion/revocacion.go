// Package revocacion es la proyección local de sesiones revocadas de mensajeria
// y su difusión interna a réplicas (ADR 26 §3, ADR 29 §3, DEC-057). mensajeria
// tiene su PROPIA suscripción durable de sesion.revocada, su propio consumidor,
// su propia proyección de tombstones, su snapshot al arrancar y su readiness: la
// raíz no comparte consumidor ni proyección entre módulos.
//
// El difusor reparte la revocación a TODAS las réplicas activas y se acusa al
// broker solo tras aceptar la difusión; nunca un consumer group donde una sola
// réplica aplique la revocación. El mecanismo físico va detrás del puerto
// Difusor para poder probar fanout, duplicados, pérdida de una réplica y canal
// obsoleto. Corte máximo 30 s desde el commit en identidad hasta el rechazo HTTP
// (ADR 26). La réplica queda not-ready hasta converger.
//
// Scaffolding: la proyección en memoria y el puerto de difusión existen; el
// cableado del consumidor durable al bus y el snapshot inicial contra identidad
// se completan cuando la raíz inyecte el adaptador de bus (bus/memory en
// desarrollo; broker real en espera de ADR 32) y la arista a identidad.
package revocacion

import (
	"context"
	"sync"
)

// Difusor difunde una revocación a todas las réplicas activas (ADR 29 §3). El
// scaffold lo deja sin adaptador real hasta ADR 32.
type Difusor interface {
	Difundir(ctx context.Context, sesionID string) error
}

// Proyeccion mantiene los tombstones locales de sesiones revocadas. Su valor
// cero es usable (mapa perezoso); seguro para uso concurrente.
type Proyeccion struct {
	mu          sync.RWMutex
	revocadas   map[string]struct{}
	convergente bool
}

// Revocar marca una sesión como revocada (tombstone local).
func (p *Proyeccion) Revocar(sesionID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.revocadas == nil {
		p.revocadas = make(map[string]struct{})
	}
	p.revocadas[sesionID] = struct{}{}
}

// EstaRevocada indica si una sesión está revocada según la proyección local.
func (p *Proyeccion) EstaRevocada(sesionID string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	_, ok := p.revocadas[sesionID]
	return ok
}

// MarcarConvergente la declara lista tras aplicar el snapshot inicial + cursor
// (ADR 26 §4). Antes de converger, la réplica queda not-ready.
func (p *Proyeccion) MarcarConvergente() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.convergente = true
}

// Convergente informa la readiness de la proyección.
func (p *Proyeccion) Convergente() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.convergente
}
