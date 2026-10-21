// Package adjuntos modelará la intención de subida y los metadatos de un
// adjunto; los BYTES no pasan por el servicio (mensajeria.md (d)): van a un
// almacenamiento de objetos con API S3 (on-premise; propuesta ADR 32: MinIO)
// detrás de un puerto propio, con endpoint, bucket y credenciales por
// configuración y metadatos en Postgres. La clave de object storage se alcanza
// por tenant; no hay FK ni lectura a esquemas ajenos.
//
// STUB. La operación CrearIntencionSubida existe en contracts v0.1.0-tracer.7,
// pero el ALCANCE de adjuntos (MSG-REC §13: solo si Operación confirma la
// necesidad; límites de tamaño/tipo sin fijar) está pendiente de Producto: la
// tabla `adjunto` se crea por FORMA en la migración, sin límites inventados, y
// el puerto de almacenamiento de objetos con su doble en memoria se agrega
// cuando el alcance esté confirmado. El adaptador real espera ADR 32.
package adjuntos
