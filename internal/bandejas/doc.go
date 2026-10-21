// Package bandejas modelará la cola operativa de trabajo de una bandeja
// compartida: una ruta activa por conversación (índice único parcial
// una_ruta_activa_por_conversacion) y la revisión monotónica de la colección
// (mensajeria.RevisionBandeja). El ciclo de vida de la conversación es separado
// del estado operativo de cola (dos columnas, no una).
//
// STUB. El catch-up de bandeja (M3 del prompt 07: bootstrap/catch-up por
// agregado con cursor_dominio y 409 stream_resync_required) NO está en contracts
// v0.1.0-tracer.7 y las rutas de bandeja siguen sin congelar en la propia
// arquitectura; espera tracer.9. Por "contrato primero" (00 regla 6) no se
// montan rutas de bandeja aquí. Las tablas de ruta/bandeja se crean por forma en
// la migración; la lógica de dominio queda sin implementar.
package bandejas
