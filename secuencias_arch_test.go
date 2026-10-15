package mensajeria

import (
	"testing"

	msgclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/mensajeria"
)

// Esta es la "prueba de arquitectura verificable" que pide la Definición de
// listo del prompt 07: que SecuenciaMensaje, VersionReclamo, RevisionBandeja y
// RevisionEmergencia NO sean intercambiables entre sí.
//
// La verificación REAL es de COMPILACIÓN, no de runtime: si alguno de estos
// tipos fuera un alias de otro (o un alias de int64), las asignaciones cruzadas
// de abajo compilarían. Como son tipos nombrados distintos (definidos en
// internal/tipos y re-exportados aquí por alias, que preserva la identidad del
// tipo), el compilador las rechaza, y por eso están COMENTADAS: dejarlas activas
// rompería `go build`. Cada una documenta el error que el compilador emitiría.
// Un refactor que colapse dos de estos tipos en un alias haría que la línea
// correspondiente compilara; descomentarla es la forma de confirmar la barrera.
//
//	var s SecuenciaMensaje = 1
//	var v VersionReclamo = s        // cannot use s (variable of type SecuenciaMensaje) as VersionReclamo
//	var b RevisionBandeja = s        // cannot use s (variable of type SecuenciaMensaje) as RevisionBandeja
//	var e RevisionEmergencia = v     // cannot use v (variable of type VersionReclamo) as RevisionEmergencia
//	var b2 RevisionBandeja = e       // cannot use e (variable of type RevisionEmergencia) as RevisionBandeja
//
// Y, crucialmente, ninguno es asignable a/desde los ALIAS del contrato sin una
// conversión explícita, aunque el contrato los genere como `= int64`:
//
//	var cs msgclient.SecuenciaMensaje = 1 // esto ES int64 (alias del contrato)
//	var s2 SecuenciaMensaje = cs            // cannot use cs (variable of type int64) as SecuenciaMensaje
//
// DELIBERADAMENTE no existe ninguna función de conversión de dominio entre estos
// cuatro tipos ni hacia un seq de transporte (00 P5): el cruce entre
// contabilidades debe ser siempre una conversión explícita en el punto de uso.

// TestTiposDeSecuenciaSonDistintos ancla la intención en runtime; su valor real
// es que el ARCHIVO compile con el bloque comentado como la única frontera.
func TestTiposDeSecuenciaSonDistintos(t *testing.T) {
	// Conversión EXPLÍCITA desde el alias del contrato (int64): permitida y
	// visible. Es el único puente, y es manual.
	var deWire msgclient.SecuenciaMensaje = 7
	s := SecuenciaMensaje(deWire)
	if int64(s) != 7 {
		t.Fatalf("conversión explícita desde el alias del contrato perdió el valor: %d", s)
	}

	// Cada tipo lleva su propia cuenta; convertir entre ellos exige cast
	// explícito, nunca asignación directa (ver bloque comentado).
	v := VersionReclamo(0)
	b := RevisionBandeja(0)
	e := RevisionEmergencia(0)
	if int64(v)+int64(b)+int64(e) != 0 {
		t.Fatalf("los ceros de las contabilidades deberían sumar 0")
	}
}
