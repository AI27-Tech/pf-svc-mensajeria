// Package auditoria registrará, de forma INMUTABLE, los hechos auditables de la
// conversación (Addendum §11): quién reclamó, liberó, transfirió, intervino con
// override, y las marcas actor_real/rol_al_enviar/alias_mostrado separadas e
// inmutables. Append-only: sin edición ni borrado en firme (I1 del esquema,
// DEC-030/DEC-041).
//
// STUB. La tabla auditoria_mensajeria se crea por forma en la migración; el
// grabado de cada hecho auditable es del dominio y se implementa junto a los
// casos de uso que lo generan (reclamo, transferencia, override). No hay ruta de
// lectura de auditoría montada hasta que su operación de contrato
// (ListarAuditoriaConversacion, presente en tracer.7) tenga su caso de uso.
package auditoria
