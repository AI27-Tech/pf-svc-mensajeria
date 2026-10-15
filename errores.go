package mensajeria

import (
	platformerrors "github.com/AI27-Tech/pf-pkg-platform-go/errors"
)

// errNoImplementado marca un caso de uso aún no implementado en el scaffold.
// Usa la taxonomía de la plataforma (I3) para que la respuesta sea coherente
// entre el adaptador HTTP y cualquier otra entrada.
func errNoImplementado(op string) error {
	return platformerrors.NewRoto("mensajeria: %s no implementado todavía (scaffold)", op)
}

// errConflictoVersion es el conflicto de concurrencia optimista del reclamo
// (Addendum §4 punto 5): el perdedor de dos reclamos concurrentes recibe este
// error. Se mapea a conflicto en la taxonomía de la plataforma (409 en HTTP).
func errConflictoVersion(esperada, actual VersionReclamo) error {
	return platformerrors.NewConflicto(
		"mensajeria: conflicto de versión del reclamo: esperada %d, actual %d", esperada, actual)
}
