// Package emergencias modelará el caso de emergencia como un caso de mensajeria
// en la bandeja de Reacción (ADR 21, ADR 28, DEC-034): reutiliza el modelo de
// bandeja/pool/reclamo, con notas UNIDIRECCIONALES y sin conversación
// bidireccional con los activadores (mensajeria.md (f)). El activador es interno
// (DEC-045 #9), no el cliente.
//
// STUB DECLARADO — ESPERA tracer.9 (DEC-089, M9 del prompt 07). La familia
// versionada de emergencia —sus nombres de eventos, rutas y esquemas— NO está en
// contracts v0.1.0-tracer.7 y sigue pendiente de congelar en la propia
// arquitectura (mensajeria.md (f), (g), (l)). Por "contrato primero" (00 regla
// 6) este paquete NO declara tipos de wire, NO crea tablas, NO monta rutas y NO
// abre consumidores: hacerlo sería inventar el contrato.
//
// La revisión del caso de emergencia ya tiene tipo propio no intercambiable
// (mensajeria.RevisionEmergencia, en internal/tipos) para que, cuando M9 llegue,
// la contabilidad de emergencia entre sin colapsar con las demás.
package emergencias
