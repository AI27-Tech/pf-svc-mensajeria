// Package tipos declara las cuatro contabilidades durables de mensajeria como
// TIPOS NOMBRADOS distintos (mensajeria.md (b); ADR 28; 00 P5). Vive en internal/
// para que tanto la superficie pública del módulo (paquete mensajeria, que las
// re-exporta por alias) como los paquetes internos las usen sin ciclo de import.
//
// La trampa que estos tipos cierran: el contrato (pf-pkg-contracts/mensajeria)
// genera SecuenciaMensaje y VersionReclamo como ALIAS de int64
// (`type SecuenciaMensaje = int64`), de modo que allí son el mismo tipo que
// int64 y entre sí. Correcto para el wire; peligroso para la contabilidad
// interna. Aquí son tipos nombrados propios (`type X int64`, NO alias): dos
// tipos nombrados distintos sobre int64 no son asignables entre sí sin
// conversión explícita, y DELIBERADAMENTE no hay ninguna función de conversión
// entre ellos ni hacia el `seq` de transporte de realtime. La prueba de
// arquitectura (raíz, secuencias_arch_test.go) lo verifica por compilación.
//
// Tres contabilidades independientes —mensaje, bandeja, emergencia— más la
// versión del reclamo. No existe una secuencia global de mensajeria.
package tipos

// SecuenciaMensaje es el orden canónico de un mensaje, monotónico POR
// CONVERSACIÓN (MSG-REC §7.2). No es una versión de reclamo ni el `seq` de
// transporte de realtime.
type SecuenciaMensaje int64

// VersionReclamo es la versión de concurrencia optimista del reclamo de una
// conversación (Addendum §4 punto 5). 0 si nunca se ha reclamado. No es una
// secuencia.
type VersionReclamo int64

// RevisionBandeja es la revisión monotónica de la COLECCIÓN de una bandeja
// (ADR 28). No es la secuencia de mensajes de ninguna de sus conversaciones.
type RevisionBandeja int64

// RevisionEmergencia es la revisión monotónica del caso de emergencia y su
// presencia en bandeja (ADR 28). STUB de tracer.9: el caso de emergencia (M9)
// no tiene forma contractual todavía (DEC-089); el tipo existe para fijar desde
// ahora que su revisión NO es intercambiable con las demás, pero no se usa en
// ninguna tabla ni ruta hasta que M9 congele su contrato.
type RevisionEmergencia int64
