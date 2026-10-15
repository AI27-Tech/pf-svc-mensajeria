// Package autorizacion resolverá tenant/alcance, rol y membresía efectiva para
// decidir qué conversaciones puede ver y atender un actor, y la ruta a bandeja
// (mensajeria.md (a); DEC-045/046). Poseer el UUID de una conversación NO
// autoriza: la autoridad sale de la membresía efectiva y el alcance, verificados
// en servidor desde la proyección local (DEC-053), nunca del token.
//
// STUB. La MECÁNICA del alcance por `cliente` del Ejecutivo (DEC-045 #8, DEC-046
// #2) NO está definida en ninguna fuente: cómo se vincula un Ejecutivo con sus
// clientes y qué relación hay entre `bandeja` y `cliente`. Tampoco está definido
// el alcance de Supervisor/Coordinador sobre conversaciones (su alcance de
// DEC-045 es por `unidad`/subordinados, no por `cliente`). Pendientes de
// Producto: no se inventa tabla de vínculo ni se deriva un alcance del otro.
//
// TODO(producto): vínculo Ejecutivo↔cliente y alcance de Supervisor/Coordinador
// sobre conversaciones.
package autorizacion
