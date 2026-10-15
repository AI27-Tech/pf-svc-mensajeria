// Package despacho es la MAMPARA del despacho de notificaciones a proveedores
// externos (mensajeria.md (d), (k); MSG-REC §6.2): pool de BD, cola, política de
// reintento, cortacircuitos y presupuesto SEPARADOS de la ruta de chat, para que
// una caída de proveedor no degrade la creación de mensajes ni la lectura de
// historia. La mampara no es opcional.
//
// En V1 NO hay worker activo ni destinatario cliente (DEC-023 #2: sin
// notificaciones externas al cliente). Este paquete existe por FORMA: su
// configuración de pool/cola/cortacircuitos y la interfaz de proveedor ("una
// interfaz, N implementaciones", mensajeria.md (h) R2), SIN ningún adaptador de
// proveedor, SIN worker en Workers() y SIN destinatario cliente. Las tablas
// (trabajo_notificacion, intento_entrega) se crean por forma en la migración.
//
// El único destinatario externo que las fuentes describen es el subconjunto de
// guardia/escalamiento interno (Addendum §10), cuya política (subconjunto y
// ventanas) está PENDIENTE de Operación y bloquea trabajo_notificacion "en su
// configuración, no en su forma" (mensajeria.md (l)). No se inventa aquí.
//
// Es DISTINGUIBLE en código de conversacion y reclamos (que corren sobre el pool
// de chat): despacho declara su propio ConfiguracionMampara con un pool y una
// cola propios, y su propio tipo de error de cortacircuitos.
package despacho

import (
	"context"
	"errors"
	"time"
)

// ErrCortacircuitosAbierto lo devuelve la mampara cuando el proveedor está
// marcado como caído: el despacho se detiene SIN tocar la ruta de chat.
var ErrCortacircuitosAbierto = errors.New("despacho: cortacircuitos abierto, proveedor no disponible")

// ConfiguracionMampara son los presupuestos propios del despacho, separados del
// chat (MSG-REC §6.2). Los valores concretos son defaults de forma, no números
// de negocio aprobados; se fijan por configuración cuando exista el worker.
type ConfiguracionMampara struct {
	// TamanoPoolBD es el presupuesto de conexiones del rol svc_mensajeria
	// dedicado al despacho, independiente del pool de chat.
	TamanoPoolBD int
	// ProfundidadCola es el máximo de trabajos de notificación en vuelo.
	ProfundidadCola int
	// MaxReintentos acota los reintentos de un trabajo antes de la DLQ.
	MaxReintentos int
	// UmbralCortacircuitos es la racha de fallos que abre el cortacircuitos.
	UmbralCortacircuitos int
	// VentanaReset es cuánto espera el cortacircuitos antes de probar de nuevo.
	VentanaReset time.Duration
}

// ConfiguracionPorDefecto da una mampara de forma inerte: cola y pool mínimos.
// No arranca nada; solo describe la separación de presupuesto.
func ConfiguracionPorDefecto() ConfiguracionMampara {
	return ConfiguracionMampara{
		TamanoPoolBD:         4,
		ProfundidadCola:      256,
		MaxReintentos:        5,
		UmbralCortacircuitos: 10,
		VentanaReset:         30 * time.Second,
	}
}

// Destinatario es el objetivo de un despacho. En V1 el único objetivo descrito
// es el subconjunto de guardia/escalamiento INTERNO; el cliente NO es
// destinatario (DEC-023). El scaffold no fija su política.
type Destinatario struct {
	// SujetoID identifica al destinatario interno de guardia/escalamiento.
	SujetoID string
}

// Trabajo es una notificación encolada para despacho externo. Forma mínima;
// los campos ricos se agregan cuando exista el worker y su contrato
// (notificacion.entrega_exitosa/.entrega_fallida, M10, ausente en tracer.7).
type Trabajo struct {
	ID           string
	Destinatario Destinatario
	Payload      []byte
}

// Proveedor es la interfaz del canal externo ("una interfaz, N
// implementaciones", mensajeria.md (h) R2). En V1 NO existe ninguna
// implementación: ni correo, ni SMS, ni push. Se declara para fijar la frontera;
// el adaptador real llega con su alcance justificado por separado (mensajeria.md
// (g)) y nunca con destinatario cliente.
type Proveedor interface {
	// Enviar entrega el trabajo por el canal externo. Idempotente por Trabajo.ID.
	Enviar(ctx context.Context, t Trabajo) error
	// Nombre identifica el canal para métricas y selección.
	Nombre() string
}
