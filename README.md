# pf-svc-mensajeria

Módulo de la **conversación de negocio** entre el cliente y AI27: su enrutamiento
a una bandeja compartida, el reclamo que autoriza a responder, el orden canónico
de sus mensajes, su auditoría, y la mampara de despacho a proveedores externos
(`docs/arquitectura/services/mensajeria.md` (a)). Capa de soporte; vive en su
**propio deployable** `mensajeria` por aislamiento de falla frente a proveedores
externos (ADR 17/29, DEC-050), no por volumen.

Esto es **scaffolding** (prompt `.scratch/backend-services-scaffold/07-mensajeria.md`,
DEC-050/DEC-089). La estructura, la migración, la superficie pública, las dos
raíces y los dos caminos calientes que el prompt exige con lógica mínima existen;
el resto de la lógica de dominio todavía no.

Idioma: prosa, comentarios y commits en español (México); identificadores en
inglés salvo sustantivos de dominio (`naming-conventions.md`, DEC-024).

## Superficie pública (`mensajeria.go`)

- `New(Pools, Deps)` — recibe **dos pools del mismo rol `svc_mensajeria`**: `Chat`
  (ruta de conversación) y `Despacho` (mampara, presupuesto propio).
- `Routes(mux)` — hoy no monta rutas (ver "Rutas").
- `Workers()` — en V1 devuelve el **publicador de outbox** (relay) cuando hay bus.
- **Sin `Consultas()`**: la única arista entrante (`bff → mensajeria`) es siempre
  HTTP (00, 05, 09), no hay consumidor en proceso. Interpretación registrada para
  que `00` la precise.

## Las cuatro contabilidades (la trampa del contrato)

`internal/tipos` declara cuatro **tipos nombrados distintos** sobre `int64`,
re-exportados por alias desde el paquete raíz:

| Tipo | Qué cuenta |
|---|---|
| `SecuenciaMensaje` | orden de mensaje, monotónico por conversación |
| `VersionReclamo` | concurrencia optimista del reclamo |
| `RevisionBandeja` | revisión de la colección de bandeja |
| `RevisionEmergencia` | revisión del caso de emergencia (stub de `tracer.9`) |

El contrato genera `SecuenciaMensaje` y `VersionReclamo` como **alias de `int64`**
(`type X = int64`). Esos alias son del wire. La contabilidad interna usa tipos
**nombrados** propios para que el compilador rechace mezclarlos; no hay ninguna
función de conversión entre ellos ni hacia el `seq` de transporte de `realtime`.
La no intercambiabilidad se prueba por **compilación** en `secuencias_arch_test.go`
(bloque de asignaciones cruzadas comentado, con el error que emitiría cada una).
No hay secuencia global de `mensajeria`.

## Esquema y rol

Esquema `mensajeria`, rol `svc_mensajeria` con `USAGE` solo sobre él (ningún otro
GRANT). 15 tablas: `conversacion`, `bandeja`, `pool_membresia`,
`ruta_conversacion`, `reclamo`, `mensaje`, `cursor_participante`,
`estado_atendido`, `adjunto`, `plantilla`, `trabajo_notificacion`,
`intento_entrega`, `auditoria_mensajeria`, **`outbox`** e `inbox_evento`. Cuatro
índices únicos parciales: una conversación activa por `(tenant_id, viaje_id)`; un
reclamo activo por conversación; una ruta activa por conversación; un mensaje por
`(conversacion_id, actor_real, client_message_id)`. Privilegio mínimo en `0002`
(sin DELETE; `mensaje`/auditoría/`intento_entrega` append-only).

## Las dos pruebas del prompt (comportamiento, contra Postgres)

- `internal/conversacion` — enviar escribe `mensaje` + su fila de `outbox` en la
  **misma transacción**; reintento por `client_message_id` devuelve el original,
  no crea un segundo mensaje ni evento.
- `internal/reclamos` — dos reclamos concurrentes con la misma versión esperada
  producen **un ganador y un conflicto de versión** para el perdedor.

Ambas se saltan sin `MENSAJERIA_TEST_DB`; en este entorno se ejecutaron contra
Postgres local y pasaron.

## Lo que queda como STUB por depender de `tracer.9` (DEC-089)

- **Emergencia (M9)** — `internal/emergencias/doc.go`: sin tablas, sin rutas, sin
  eventos. La familia versionada de emergencia no está en `tracer.7`.
  `RevisionEmergencia` ya existe como tipo no intercambiable.
- **Catch-up de bandeja (M3)** — `internal/bandejas/doc.go`: sin rutas de
  bootstrap/catch-up ni `409 stream_resync_required`.

Bloqueados por producto (no por `tracer.9`): `internal/pool` e
`internal/autorizacion` (mecánica del vínculo `Ejecutivo`↔`cliente`, DEC-045 #8 /
DEC-046 #2, sin definir); alcance de `adjuntos` (MSG-REC §13); política de
visibilidad de recibos (Addendum §8). Despacho externo sin worker ni destinatario
cliente (DEC-023).

## Brechas y contradicciones encontradas (reportadas, no resueltas)

1. **Tabla `outbox` vs `outbox_evento`.** El prompt lista la tabla de salida como
   `outbox_evento`, pero `pf-pkg-platform-go/outbox` (`OutboxDDL` y el `Relay`)
   leen una tabla llamada `outbox`. Para que el relay compartido funcione sin
   forkearlo, la migración la nombra `outbox`. Discrepancia de nombre con el
   prompt; la 15.ª tabla `inbox_evento` sí coincide.
2. **M6 presente, no ausente.** La tabla del prompt da por ausentes
   `sujeto.desactivado`/`rol.asignado`/`rol.revocado`; en `tracer.7` SÍ existen
   (`eventosv1`). `membresia.cambiada` también está en la familia restringida. El
   consumidor de identidad no se abre todavía por el pendiente de producto, no por
   el contrato.
3. **C-cat parcial.** `catalogos.yaml` de `tracer.7` SÍ expone la lectura por lote
   de clientes (`listarClientes?ids=`), que es lo que usa el puerto. NO expone
   "datos de contacto" por lote (no hay operación ni schema): la caché de contacto
   queda bloqueada; el puerto no la inventa.
4. **`INVENTARIO.md` vs `mensajeria.md` (g).** El inventario atribuye a
   `mensajeria` consumir `viaje.estado_cambiado/.asignado/.reasignado/.condicion_*`,
   que `(g)` no lista; y omite a `mensajeria` en `catalogo.actualizado`. No se crea
   efecto para los primeros; ambas quedan para reconciliar el inventario.
5. **Cliente TS `pf-web-frontend-client/.../conversaciones.ts`** tiene wire propio
   sin contrato (`seq`/`enviado_en`/`idempotencyKey`). No es fuente; deberá
   alinearse a `mensajeria.yaml`.
6. **Chat interno vertical (DEC-046 #3)** sin enmienda formal: no se construye (ni
   paquete, ni tabla, ni ruta, ni evento). Bloqueo de documentación.
7. **`mensajeria.md` (l) desactualizado** (fila ADR 23) y **`09-composicion...`**
   aún ubica `cmd/mensajeria` en `pf-backend`: prevalece DEC-050 #2 (vive aquí).

## Pendiente de ADR 32 (on-premise)

Adaptador real del broker y del almacenamiento de objetos (API S3). Mientras
tanto `cmd/mensajeria` arranca sobre el bus en memoria de la plataforma para que
el relay de outbox corra de verdad; el relay es agnóstico al nombre de tema.

## Arranque

- `cmd/api` — standalone (I5), sin `core` ni bus, sin Workers().
- `cmd/mensajeria` — raíz de producción (DEC-050 #2): mismo cableado HTTP +
  `Workers()` embebidos.

Variables: `DB_MENSAJERIA_CHAT_URL`, `DB_MENSAJERIA_DESPACHO_URL`,
`CATALOGOS_BASE_URL`, `HTTP_ADDR`, `SERVICE_NAME`.
