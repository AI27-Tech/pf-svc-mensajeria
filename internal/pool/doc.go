// Package pool proyectará la membresía efectiva y versionada del pool de
// Ejecutivos que atiende una bandeja (mensajeria.md (a); DEC-046). Se alimenta
// de los eventos de identidad (sujeto.desactivado, rol.asignado, rol.revocado) y
// publica membresia.cambiada si la membresía efectiva cambia. Perder el reclamo
// NO es perder la membresía (Addendum §4 puntos 7 y 8): son efectos distintos;
// el reclamo vive en internal/reclamos.
//
// STUB (M6 parcialmente desbloqueado). Los eventos de identidad SÍ existen en
// contracts v0.1.0-tracer.7 (eventosv1: SujetoDesactivado, RolAsignado,
// RolRevocado) y membresia.cambiada está en la familia restringida — una
// corrección respecto de la tabla del prompt, que los daba por ausentes
// (reportado). Lo que sigue SIN definir es la MECÁNICA del alcance por cliente
// del Ejecutivo (ver internal/autorizacion): sin ella no se puede calcular a qué
// clientes aplica una membresía. Por eso el cálculo de membresía efectiva queda
// sin implementar y no se abre todavía el consumidor de identidad en Workers().
//
// TODO(producto): vínculo Ejecutivo↔cliente para cerrar el cálculo de membresía.
//
// Lo único que hoy proyecta es el rol vigente de cada sujeto (roles.go,
// mensajeria.rol_sujeto), para validar el destino de una transferencia en la
// conversación de la solicitud: debe ser `cotizacion` (DEC-148 #31). La
// membresía del pool de `cotizacion` es el rol mismo, sin filas por usuario:
// un usuario nuevo lee el historial previo (DEC-148 #46).
package pool
