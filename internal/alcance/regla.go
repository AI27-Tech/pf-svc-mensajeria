// Package alcance es el alcance del monitoreo por cliente sobre las emergencias
// del viaje (DEC-149 #6 y #18; mensajeria.yaml de tracer.41 y tracer.42) y la
// proyección local de `alcance.cambiado` de identidad (sujeto → clientes
// efectivos) con la que se resuelve en servidor (DEC-053 #3).
//
// La regla (regla.go) es pura: la usa la guarda de la fachada de emergencias.
// La proyección (proyeccion.go) vive en Postgres, en el esquema mensajeria: el
// consumidor de alcance.cambiado es durable y comparte grupo entre réplicas,
// así que una proyección en memoria solo la vería actualizada la réplica que
// recibió el evento.
package alcance

import (
	"slices"

	roles "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/roles"
	"github.com/google/uuid"
)

// Acota dice si el rol interno opera sobre las emergencias del viaje solo
// dentro de sus clientes efectivos: monitorista, supervisor y coordinador
// (mensajeria.yaml, info.description). El administrador ve todo; Reacción y
// su bandeja no cambian. Qué acción tiene cada rol (403) lo decide antes
// internal/autorizacion.
func Acota(rol string) bool {
	switch roles.Rol(rol) {
	case roles.RolMonitorista, roles.RolSupervisor, roles.RolCoordinador:
		return true
	}
	return false
}

// Visible es la regla del alcance: el cliente del viaje está entre los
// clientes efectivos del sujeto. Sin clientes no ve nada, y un cliente
// desconocido ("" o no UUID) tampoco: falla cerrado. En un préstamo decide el
// cliente del viaje, no el de la unidad (DEC-149 #5).
func Visible(clientes []uuid.UUID, clienteID string) bool {
	id, err := uuid.Parse(clienteID)
	return err == nil && slices.Contains(clientes, id)
}
