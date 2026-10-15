// Package consumidores aloja los consumidores durables de mensajeria (ADR 29).
// Cada consumidor aplica su efecto y registra el evento_id en inbox_evento
// dentro de la MISMA transacción; el acuse al broker ocurre después (al menos
// una vez, mensajeria.md (g)). Un evento duplicado no produce un segundo efecto:
// el inbox lo impide.
//
// Orden por agregado (sin clave global, ADR 29 §2). Un consumidor por
// suscripción durable CONTRATADA. En tracer.7 están contratados los eventos de
// operacion (viaje.*), sesion.revocada de identidad, y los de identidad
// sujeto.desactivado/rol.asignado/rol.revocado. Los efectos de dominio de cada
// uno son del dominio y no se implementan en el scaffold; aquí vive la mecánica
// de idempotencia y de DLQ que no depende del dominio.
//
// Evento venenoso (tipo/versión no registrada o reintentos agotados) → DLQ/
// cuarentena propia, con solo metadatos sanitizados en logs/métricas (ADR 29
// §4); la suscripción avanza solo tras confirmar la cuarentena. Los parámetros
// operativos (reintentos, backoff, retención) quedan configurables y pendientes.
package consumidores

import (
	"context"
	"errors"
)

// ErrEventoVenenoso marca un evento cuyo tipo/versión no está registrado o que
// agotó reintentos: debe ir a DLQ/cuarentena, no reintentarse en vivo.
var ErrEventoVenenoso = errors.New("evento venenoso: tipo/versión no registrada")

// Evento es la vista mínima que el consumidor necesita del Sobre (P13). Los
// tipos ricos del dominio llegan en Payload; el scaffold solo usa la identidad y
// el tipo para demostrar la idempotencia y el enrutamiento a DLQ.
type Evento struct {
	EventoID   string // dedup key del Sobre (evento_id)
	Tipo       string // p. ej. "viaje.mensajeria_habilitada"
	Version    int
	AgregadoID string // clave de orden
	Payload    []byte
}

// Inbox registra y consulta evento_id procesados (inbox_evento). En producción
// el registro y el efecto ocurren en la MISMA transacción.
type Inbox interface {
	// YaProcesado indica si el evento_id ya está en inbox_evento.
	YaProcesado(ctx context.Context, eventoID string) (bool, error)
	// Marcar registra el evento_id como procesado (en la misma tx que el efecto).
	Marcar(ctx context.Context, eventoID, tipo, consumidor string) error
}

// Efecto aplica el cambio de dominio de un evento ya deduplicado. Scaffolding:
// las implementaciones reales (proyección de membresía, enrutamiento de
// conversación, escritura_hasta) viven en sus paquetes de dominio y están sin
// implementar; aquí es un puerto para probar la idempotencia sin DB.
type Efecto interface {
	Aplicar(ctx context.Context, e Evento) error
}

// TiposViajeContratados son los eventos de operacion que mensajeria consume en
// tracer.7 según mensajeria.md (g): crea/enruta la conversación o fija
// escritura_hasta. NO incluye viaje.estado_cambiado/.asignado/.reasignado/
// .condicion_*, que INVENTARIO.md atribuye a mensajeria pero mensajeria.md (g)
// no lista (contradicción reportada): no se abre un durable que no se usa.
var TiposViajeContratados = []string{
	"viaje.creado",
	"viaje.mensajeria_habilitada",
	"viaje.cliente_actualizado",
	"viaje.cerrado",
	"viaje.acceso_revocado",
}

// TiposIdentidadContratados son los eventos de identidad que recalculan la
// membresía efectiva del Ejecutivo (mensajeria.md (g), DEC-045/046). Presentes
// en tracer.7 (eventosv1: SujetoDesactivado, RolAsignado, RolRevocado).
var TiposIdentidadContratados = []string{
	"sujeto.desactivado",
	"rol.asignado",
	"rol.revocado",
}
