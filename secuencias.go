package mensajeria

import "github.com/AI27-Tech/pf-svc-mensajeria/internal/tipos"

// Las cuatro contabilidades durables de mensajeria. Su definición canónica vive
// en internal/tipos (para que la use también el código interno sin ciclo de
// import); aquí se RE-EXPORTAN por alias. Un alias conserva el MISMO tipo
// nombrado: mensajeria.SecuenciaMensaje y tipos.SecuenciaMensaje son idénticos,
// y por tanto siguen siendo DISTINTOS de VersionReclamo, RevisionBandeja y
// RevisionEmergencia. La no intercambiabilidad se prueba por compilación en
// secuencias_arch_test.go.
//
// Nota sobre el contrato: pf-pkg-contracts/mensajeria genera SecuenciaMensaje y
// VersionReclamo como alias de int64. Esos alias son del wire; los tipos de
// abajo son los de la contabilidad interna y NO colapsan en int64 ni entre sí.
// No existe ninguna función de conversión de dominio entre ellos ni hacia el
// `seq` de transporte de realtime (00 P5): todo cruce es un cast explícito,
// visible en la revisión.

// SecuenciaMensaje — orden canónico de mensaje, monotónico por conversación.
type SecuenciaMensaje = tipos.SecuenciaMensaje

// VersionReclamo — versión de concurrencia optimista del reclamo.
type VersionReclamo = tipos.VersionReclamo

// RevisionBandeja — revisión monotónica de la colección de bandeja.
type RevisionBandeja = tipos.RevisionBandeja

// RevisionEmergencia — revisión del caso de emergencia (stub de tracer.9).
type RevisionEmergencia = tipos.RevisionEmergencia
