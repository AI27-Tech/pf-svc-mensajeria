# CHANGELOG

## 2026-11-03 13:00 — Conversación por empresa cliente (DEC-124) y bandeja única con membresía por rol (DEC-127)

**Para quién:** el `bff`, `pf-svc-core` en proceso, los dos portales y la siembra de la demo. Cambia lo que observan: cualquier usuario del portal de la empresa cliente abre y escribe la conversación de su viaje; la conversación nueva aparece sola en la bandeja de todo Ejecutivo; la demo ya no siembra bandejas, rutas ni `pool_membresia` para conversaciones.

**Resumen:** dos defectos medidos en el smoke de core del 2026-11-03. El cliente solo abría su conversación si el `sub` del token era igual a `conversacion.cliente_id`; con Keycloak el `sub` es la persona y el tenant es la empresa, así que nunca coincidían (404). Y la conversación creada por `viaje.mensajeria_habilitada` no quedaba en ninguna bandeja, y el Ejecutivo solo era miembro con una fila sembrada en `pool_membresia`.

**Comportamiento:**
- Audiencia Cliente: autoriza si el tenant del token es el `cliente_id` de la conversación (DEC-124, DEC-112). El `sub` ya no decide. Aplica a todas las rutas de cliente, HTTP y `ConversacionesAPI`, incluida `obtenerConversacionDeViaje`. Toda consulta sigue acotada por tenant: un cliente de otro tenant recibe `404`.
- La conversación nace enrutada a la bandeja única de conversaciones de su tenant, en la misma transacción que la crea. Si el tenant no tiene bandeja de chat, se crea (`Conversaciones`). La ruta entra con revisión nueva, así que el catch-up la reporta como alta. Un lock transaccional por tenant evita dos bandejas cuando llegan varias conversaciones a la vez.
- Membresía por rol (DEC-127): todo usuario con rol `ejecutivo` es miembro de la bandeja de chat sin filas en `pool_membresia`. Aplica a leer, enviar, reclamar, liberar, transferir, atendida y recibos (`internoPuede`), a `listarTrabajoMiBandeja` (HTTP y en proceso) y a `listarTrabajoBandeja` sobre una bandeja de chat.
- Otro rol interno (monitorista, supervisor, coordinador…) recibe la misma denegación que antes sin membresía: `401` sobre la conversación, `404` en la bandeja del sujeto. Una fila sembrada en `pool_membresia` ya no da acceso a conversaciones.
- Si un tenant tiene más de una bandeja de chat (siembra anterior), la única es la más antigua. Desaparece `ErrMembresiaAmbigua`.
- Sin cambio: la bandeja de Reacción y los casos de emergencia siguen exigiendo membresía en `pool_membresia` (DEC-028). Las tablas `pool_membresia` se conservan.

**Migración:** `0008_bandeja_unica.sql`. Crea la bandeja de chat del tenant que tiene conversaciones sin ruta y no tiene bandeja de chat, y enruta cada conversación sin ruta activa a la bandeja de chat más antigua de su tenant, con revisiones nuevas y `bandeja.revision` al día. No toca rutas existentes. Idempotente: `cmd/migrate` (el de este repo y el de core, que embebe `migrations.FS`) la reaplica sin efecto.

**Cambios:**
- `internal/autorizacion`: `clientePuede` compara tenant contra `cliente_id`; `internoPuede` exige rol `ejecutivo` y ruta activa a una bandeja de chat; `PuedeVerBandeja` decide por rol en la de chat y por membresía en las demás; nuevo `EsMiembroBandejaChat`.
- `internal/bandejas`: `ResolverMiBandeja(ctx, tenantID)` devuelve la bandeja de chat más antigua del tenant; nuevo `EnrutarABandejaUnica` (crea bandeja si falta, avanza revisión, inserta ruta) en la tx del llamador.
- `internal/conversacion`: `CrearPorEvento` enruta la conversación nueva en su transacción.
- `adapters/http/bandejas`, `conversaciones_api.go`: la bandeja del sujeto exige rol `ejecutivo`; mensaje del 404 compartido (`SinBandejaChat`).
- `adapters/http/conversacion`: comentarios.
- Pruebas: las de conversación ya no siembran `pool_membresia` y usan tenant = cliente; los actores sin acceso pasan a ser monitorista. Nuevas: evento → bandeja del Ejecutivo → reclamo → persona de la empresa lee y escribe, sin filas sembradas; migración 0008 sobre datos previos y reaplicada; ocho creaciones concurrentes de un tenant nuevo dan una sola bandeja y revisiones 1..8.

**Archivos modificados:**
- `internal/autorizacion/autorizacion.go`, `internal/autorizacion/autorizacion_integration_test.go`
- `internal/bandejas/bandejas.go`, `internal/bandejas/bandejas_integration_test.go`, `internal/bandejas/bandeja_unica_integration_test.go` (nuevo)
- `internal/conversacion/conversacion.go`, `internal/conversacion/ciclo_vida_test.go`
- `adapters/http/bandejas/bandejas.go`, `adapters/http/bandejas/bandejas_test.go`, `adapters/http/bandejas/bandejas_integration_test.go`
- `adapters/http/conversacion/conversacion.go` y sus pruebas `atencion_`, `conversacion_`, `enviar_`, `lectura_`, `reclamo_perdido_integration_test.go`
- `conversaciones_api.go`, `conversaciones_api_integration_test.go`, `atencion_api_integration_test.go`, `conversacion_de_viaje_integration_test.go`, `bandeja_unica_integration_test.go` (nuevo)
- `migrations/mensajeria/0008_bandeja_unica.sql` (nuevo)
- `CHANGELOG.md`

**Verificación:** `gofmt` limpio, `go vet ./...`, `go build ./...`, `go build ./cmd/...`, `go test -race -p 1 ./...` con `MENSAJERIA_TEST_DB` (Postgres.app local, base desechable: 71 pruebas de primer nivel PASS, 0 SKIP) y sin él, `arch-test@v0.1.1` OK. Sin el lock por tenant, la prueba concurrente falla (5 y 6 bandejas en lugar de 1).

## 2026-11-03 12:55 — Emergencia: reclamar solo desde el pool (DEC-125) y quién libera o transfiere (DEC-126)

**Para quién:** el `bff`, `pf-svc-core` en proceso y el panel interno (bandeja de Reacción). Cambia lo que observan: reclamar un caso que ya tiene responsable responde `409` aunque mande la versión vigente; liberar o transferir el caso de otro responde `403 prohibido`, salvo para Supervisor y Coordinador.

**Resumen:** dos hallazgos del PR #44. Un miembro de Reacción con la versión vigente se quedaba con un caso ajeno (el reclamo pasaba a él), y cualquier miembro de Reacción liberaba o transfería cualquier caso. Sustituye la línea "Emergencia, sin cambio" de la entrada anterior.

**Comportamiento:**
- `reclamarEmergencia` (HTTP y `EmergenciasAPI`) solo procede si el caso no tiene reclamo activo (DEC-125). Con reclamo activo responde `409 conflicto`, también para el propio responsable. El caso sigue con su responsable.
- Cada intento de reclamo de emergencia queda en `auditoria_mensajeria`: `emergencia.reclamo.intento_ganado` en la tx del reclamo y `emergencia.reclamo.intento_perdido` en una tx propia, con `conversacion_id` NULL y el caso en `detalle.caso_emergencia_id`. Antes el reclamo de emergencia no se auditaba. La carrera desde el pool no cambia: uno `201`, otro `409`, los dos auditados.
- `liberarReclamoEmergencia` y `transferirEmergencia` (HTTP y `EmergenciasAPI`): los hace el responsable activo, o cualquier usuario con rol `supervisor` o `coordinador` sin verificar el mando (DEC-126, atajo de DEC-099 hasta DT-06). Otro miembro de Reacción recibe `403 prohibido`. Orden de validación: sin reclamo `404`, versión no vigente `409`, no responsable `403`.
- Supervisor y Coordinador no necesitan membresía en el pool de Reacción para liberar o transferir: esas dos operaciones saltan `PuedeAtenderEmergencia` para ellos. Para todo lo demás (reclamar, resolver, y liberar/transferir de cualquier otro rol) la guarda de membresía no cambia: sin fila activa en `pool_membresia` de la bandeja del caso, `401`.

**Cambios:**
- `internal/emergencias`: `Reclamar` abre solo desde el pool (se borra el `UPDATE` que reasignaba) y audita cada intento; `tomarActivo` (bloquea el reclamo activo y valida versión y responsable); `Liberar` y `Transferir` reciben `mando`; `ErrNoEsResponsable`; `TieneMando`.
- `internal/auditoria`: `AccionEmergenciaReclamoGanado`/`Perdido`; `Registrar` guarda `conversacion_id` NULL cuando viene vacío.
- `adapters/http/emergencias` y `emergencias_api.go`: `conMando`; liberar y transferir saltan la guarda de membresía con mando; `ErrNoEsResponsable` → `403 prohibido`.
- Pruebas: B (Reacción) reclama el caso de A con la versión vigente (`409` por HTTP y en proceso, el caso sigue con A en versión 1, tres filas de auditoría literales); B libera y transfiere el caso de A (`403 prohibido` por HTTP y en proceso); Monitorista sin membresía libera (`401`); Supervisor y Coordinador, sin membresía, liberan y transfieren el caso ajeno (HTTP y en proceso); el responsable transfiere y libera el suyo; el caso termina `abierta` en versión 6. La prueba de concurrencia afirma la auditoría de los dos intentos.

**Archivos modificados:**
- `internal/emergencias/emergencias.go`, `internal/emergencias/emergencias_integration_test.go`
- `internal/auditoria/auditoria.go`
- `adapters/http/emergencias/emergencias.go`
- `adapters/http/emergencias/emergencias_integration_test.go` (nuevo)
- `emergencias_api.go`, `emergencias_api_test.go`
- `CHANGELOG.md`

**Verificación:** `gofmt` limpio, `go vet ./...`, `go build ./... ./cmd/...`, `go test -race -p 1 ./...` con `MENSAJERIA_TEST_DB` (Postgres desechable: 69 pruebas de primer nivel PASS, 0 SKIP) y sin él, `arch-test@v0.1.1` OK. Mutaciones: sin la guarda de responsable, B libera con `204`; sin el salto de membresía, el Supervisor recibe `401`; sin el chequeo de reclamo activo, la auditoría del intento perdido cambia de versión actual.


## 2026-11-03 11:56 — Reclamar solo desde el pool (DEC-123) y versión monotónica del reclamo de emergencia

**Para quién:** el `bff`, `pf-svc-core` en proceso y el panel interno (bandeja de conversaciones y de Reacción). Cambia lo que observan: `POST /reclamos` sobre una conversación que ya tiene responsable responde `409` aunque mande la versión vigente; y el reclamo de un caso de emergencia ya no reabre en versión 1 al transferir.

**Resumen:** dos defectos reportados en el PR #41. Otro miembro del pool con la versión vigente se quedaba con una conversación ajena (medido: `201`, el reclamo pasaba a B). `emergencias.Transferir` reabría el reclamo del caso en versión 1, el mismo defecto que el PR #41 corrigió para conversaciones (medido: tras A→B, la transferencia B→A con versión 2 daba conflicto con versión actual 1).

**Comportamiento:**
- `reclamarConversacion` (HTTP y `ConversacionesAPI`) solo procede si la conversación está en el pool (DEC-123). Con reclamo activo responde `409 conflicto`, también para el propio responsable que re-reclama. El intento queda auditado como `reclamo.intento_perdido`. Para cambiar de responsable, el responsable libera o transfiere (DEC-118, DEC-119).
- La carrera de dos reclamos desde el pool no cambia: uno `201`, otro `409`, los dos auditados.
- Emergencia: la versión del reclamo es monotónica POR CASO. Reclamar desde el pool y transferir abren en la versión más alta del caso + 1; tras A→B→A las versiones son 1, 2, 3 y una versión previa da conflicto en transferir, liberar, resolver y reclamar. La `version_reclamo` del caso (`listarEmergencias`) refleja la versión nueva tras transferir y la vigente tras liberar. Del pool, una versión distinta de la vigente da conflicto (antes solo se aceptaba `0`).
- `emergencia.transferida` lleva `version_reclamo` y `reclamo_anterior_id`, que el contrato ya declaraba y el payload omitía.
- Emergencia, sin cambio: liberar y transferir siguen sin exigir que quien llama sea el responsable (ADR 21 §5, DEC-099: Supervisor y Coordinador pueden liberar o transferir el reclamo de sus miembros de Reacción). Reclamar un caso con responsable y la versión vigente sigue reasignándolo (fuera del alcance de DEC-123).

**Cambios:**
- `internal/reclamos`: `reclamar` ya no avanza un reclamo activo; solo abre desde el pool. Se borra el `UPDATE` que reasignaba.
- `internal/emergencias`: `versionMaxima`; `abrirReclamo` recibe la versión, devuelve el id y traduce la violación de unicidad a `ErrConflictoVersion`; `Transferir` abre en la versión siguiente y la refleja en la fila del caso; `Liberar` deja el caso en la versión vigente.
- `adapters/http/conversacion`: comentario de `reclamar`.
- Pruebas: B reclama la conversación de A con la versión vigente (`409` por HTTP y en proceso, el reclamo sigue con A, intentos auditados) y B reclama del pool tras liberar (`201`, versión 2); emergencia A→B→A con versiones `[1 2 3]`, conflictos con la versión previa y reclamo del pool en versión 4. La prueba de concurrencia de `internal/reclamos` sembraba un reclamo activo y esperaba que uno de los dos lo tomara: ahora la carrera es desde el pool y afirma la auditoría de los dos intentos.

**Archivos modificados:**
- `internal/reclamos/reclamos.go`, `internal/reclamos/reclamos_integration_test.go`
- `internal/emergencias/emergencias.go`, `internal/emergencias/emergencias_integration_test.go`
- `adapters/http/conversacion/conversacion.go`
- `adapters/http/conversacion/reclamar_pool_integration_test.go` (nuevo)
- `CHANGELOG.md`

**Verificación:** `gofmt` limpio, `go vet ./...`, `go build ./...`, `go build ./cmd/...`, `go test -race -p 1 ./...` con `MENSAJERIA_TEST_DB` (Postgres local, base desechable: 67 pruebas de primer nivel PASS, 0 SKIP) y sin él, `arch-test@v0.1.1` OK. Antes del arreglo, la prueba de DEC-123 fallaba (B recibía `201`) y la de emergencia también (`B→A: esperada 2, actual 1`).

## 2026-11-03 10:46 — Versión de reclamo monotónica, solo el responsable libera o transfiere y 403 real

**Para quién:** el `bff`, `pf-svc-core` en proceso y los dos portales. Cambia lo que observan: un Ejecutivo que no tiene el reclamo ya no puede liberar ni transferir; las denegaciones por facultad pasan de `401` a `403`; y la `version_reclamo` de una conversación en el pool deja de ser `0` tras el primer reclamo.

**Resumen:** corrige los defectos medidos el 2026-11-02. `Transferir` reabría el reclamo en versión 1, así que tras A→B→A un envío de A con la versión previa entraba (`201`) y un reclamar con ella reasignaba en silencio (`201`, versión 2). `Liberar` y `Transferir` no verificaban que quien llama tuviera el reclamo (DEC-118). Las denegaciones por facultad salían como `401` aunque el contrato declara `403`.

**Comportamiento:**
- La versión del reclamo es monotónica POR CONVERSACIÓN (contrato `VersionReclamo`): reclamar desde el pool y transferir abren en la versión más alta de la conversación + 1. Liberar no cambia la versión. Una versión previa a una transferencia nunca vuelve a ser vigente: enviar, reclamar, liberar, transferir o marcar atendida con ella dan `409`.
- Sin reclamo activo, `version_reclamo` (en `obtenerConversacion` y en la bandeja) es la versión vigente de la conversación, no `0`; `0` solo si nunca se reclamó. Reclamar desde el pool exige esa versión: tras una liberación, `version_esperada = 0` da `409`.
- Liberar y transferir: sin reclamo activo `404`; versión no vigente `409`; quien llama no es el responsable activo `403 prohibido` (DEC-118, incluido el Supervisor). La versión se valida antes que el responsable, también en `marcarAtendida`: una vista obsoleta recibe `409` y recarga.
- `403 prohibido` (pf-pkg-platform-go `v0.1.8`, DEC-115) para: marcar atendida sin ser el responsable, liberar o transferir sin ser el responsable, leer la auditoría sin ser `administrador`. Siguen en `401`: falta de claims y la guarda de acceso (`PuedeEscribir`, sin membresía o cliente ajeno).

**Cambios:**
- `go.mod`: `pf-pkg-platform-go` de `v0.1.6` a `v0.1.8`.
- `internal/reclamos`: `versionConversacion` y `tomarActivo` (bloquea el reclamo activo y valida versión y responsable); `abrir` recibe la versión; `ErrNoEsResponsable`. Se borra `versionActiva`.
- `internal/conversacion`: `ObtenerConversacion` devuelve la versión vigente sin reclamo activo; `MarcarAtendida` valida versión antes que responsable.
- `internal/bandejas`: misma `version_reclamo` en la bandeja.
- `adapters/http/conversacion`: `TraducirReclamo`, compartida por los handlers y `ConversacionesAPI` (reemplaza el mapeo repetido y `traducirConflictoReclamo`); `TraducirAtencion` y la auditoría usan `NewProhibido`.
- Migración `0007_reclamo_version_por_conversacion.sql`: índice `(conversacion_id, version)` para leer la versión vigente. No es único: los datos previos pueden repetir versiones.
- Pruebas: P20 A→B→A con versión obsoleta (`409` en envío y en reclamo); `403` al liberar y transferir sin ser el responsable; versión conservada tras liberar en obtener y en la bandeja. Se actualizan las que fijaban `401` o la versión 1 tras transferir (PR #36, PR #37, E3-T3).

**Archivos modificados:**
- `go.mod`, `go.sum`
- `internal/reclamos/reclamos.go`, `internal/reclamos/reclamos_integration_test.go`
- `internal/conversacion/conversacion.go`, `internal/conversacion/atencion.go`
- `internal/bandejas/bandejas.go`
- `adapters/http/conversacion/conversacion.go`, `adapters/http/conversacion/atencion.go`
- `adapters/http/conversacion/reclamo_perdido_integration_test.go`, `adapters/http/conversacion/atencion_integration_test.go`
- `conversaciones_api.go`, `conversaciones_api_integration_test.go`, `atencion_api_integration_test.go`
- `migrations/mensajeria/0007_reclamo_version_por_conversacion.sql` (nuevo)
- `CHANGELOG.md`

**Verificación:** `gofmt` limpio, `go vet ./...`, `go build ./...`, `go build ./cmd/api ./cmd/mensajeria ./cmd/migrate`, `go test -race ./...` con `MENSAJERIA_TEST_DB` (Postgres local, base desechable: 63 pruebas de primer nivel PASS, 0 SKIP) y sin él, `arch-test@v0.1.1` OK. Antes del arreglo, las dos pruebas nuevas y la de P20 actualizada fallaban (versiones `[1 1 1]`, transferir abría en 1, liberar ajeno `204`).

## 2026-11-03 10:39 — Conversación por viaje y mensaje del cliente que desmarca atendida (E9-T8, DEC-122 #1)

**Para quién:** el `bff` y `pf-svc-core` en proceso, que solo conocen el `viaje_id` y necesitan el `id` de la conversación para listar y enviar; y la bandeja interna, que veía como atendida una conversación con un mensaje del cliente sin contestar.

**Resumen:** mensajeria implementa `obtenerConversacionDeViaje` (`GET /api/v1/viajes/{id}/conversacion`, contrato `tracer.33`) por HTTP y en `ConversacionesAPI`, y un mensaje nuevo del cliente desmarca atendida en la misma tx del envío.

**Comportamiento:**
- `{id}` es el `viaje_id`. Devuelve la conversación no cerrada del viaje; si todas están cerradas, la más reciente por `creado_en` (la tabla no guarda la hora de cierre). Misma forma y misma guarda que `obtenerConversacion`. Sin conversación, sin acceso (otro cliente, interno sin membresía, otro tenant) o `{id}` que no es UUID: `404`, como pide el contrato. Sin claims: `401`.
- Mensaje nuevo del cliente: `estado_atendido.atendida = false` en la tx del mensaje y su outbox (DEC-122 #1). El envío interno no la toca (DEC-117). El reintento idempotente no es mensaje nuevo y no desmarca. No se publica evento propio: el registro del contrato no tiene uno para desmarcar.

**Cambios:**
- `go.mod`: `api-client-go` de `tracer.31` a `tracer.33`. Sin renombres de enum.
- `internal/conversacion`: `ConversacionDeViaje`; `Enviar` desmarca atendida si el autor es el cliente.
- `adapters/http/conversacion`: ruta nueva y `Handlers.ObtenerDeViaje`, el camino que comparten HTTP y la superficie en proceso.
- `ConversacionesAPI`: `ObtenerConversacionDeViaje(ctx, viajeID msgclient.Id) (*msgclient.Conversacion, error)`.

**Archivos modificados:**
- `go.mod`, `go.sum`
- `internal/conversacion/conversacion.go`
- `adapters/http/conversacion/conversacion.go`
- `conversaciones_api.go`
- `conversacion_de_viaje_integration_test.go` (nuevo)
- `adapters/http/conversacion/atencion_integration_test.go`
- `CHANGELOG.md`

**Verificación:** `gofmt` limpio, `go vet ./...`, `go build ./...`, `go build ./cmd/api ./cmd/mensajeria ./cmd/migrate`, `go test -race ./...` con `MENSAJERIA_TEST_DB` (Postgres local, base desechable: 63 pruebas de primer nivel PASS, 0 SKIP) y sin él, `arch-test@v0.1.1` OK.

## 2026-11-02 18:03 — Atendida, recibos con P23 y auditoría inmutable (E3-T6)

**Para quién:** el `bff` (y `pf-svc-core` en proceso), que ya montan `/atendida` y `/recibos/*` y hoy reciben 404 de mensajeria; y el Administrador, que podrá leer la auditoría de una conversación.

**Resumen:** mensajeria implementa `marcarAtendida`, `registrarReciboEntregado`, `registrarReciboLeido` y `listarAuditoriaConversacion` (contrato `tracer.31`, sin subir pin), por HTTP y en `ConversacionesAPI` sobre las mismas instancias de casos de uso.

**Comportamiento:**
- `marcarAtendida` es acción explícita del responsable activo con versión esperada (DEC-117); enviar no la marca. No responsable → `no_autorizado` (401; el contrato dice 403 pero la taxonomía de la plataforma no tiene prohibido). Sin reclamo activo o versión obsoleta → 409. Marca, auditoría y `conversacion.atendida` en una tx.
- Recibos: avanzan el cursor personal (`cursor_participante`), nunca retroceden y se recortan a la última secuencia; leer implica entregado. El "leído por AI27" visible al cliente (`conversacion.leido_ai27_hasta`) solo lo mueve el recibo de lectura del responsable activo (P23). Leer la historia no escribe cursores. Publica `recibo.entregado`/`recibo.leido` solo si el cursor personal avanzó.
- Auditoría inmutable: intento de reclamo ganado (en la tx del reclamo), intento perdido (en tx propia tras el conflicto), liberación, transferencia y atendida. El rol de app no tiene `UPDATE`/`DELETE` sobre la tabla y no hay ruta que edite o borre. Lectura solo para `administrador` (DEC-036), paginada de 50 en 50.
- Corrección: en la carrera del PRIMER reclamo (dos `version_esperada = 0` sobre una conversación sin reclamo), el perdedor recibía 502 por la violación de índice único sin mapear. Ahora recibe 409 (P19).

**Cambios:**
- Migración `0006_auditoria_recibos.sql`: `auditoria_mensajeria.registro_id` (UUID del contrato), índices `(conversacion_id, ocurrido_en)` y `(actor_real, ocurrido_en)`, columna `conversacion.leido_ai27_hasta`. Aditiva e idempotente; sin grants nuevos.
- `internal/auditoria`: deja de ser stub (`Registrar` en la tx del llamador, `Listar`).
- `internal/reclamos`: audita cada intento, la liberación y la transferencia; mapea 23505 a conflicto.
- `internal/conversacion`: `MarcarAtendida`, `RegistrarRecibo`, `ErrNoEsResponsable`.
- `adapters/http/conversacion`: cuatro rutas nuevas y `TraducirAtencion`, que comparten HTTP y la superficie en proceso.
- `ConversacionesAPI`: `MarcarAtendida`, `RegistrarReciboEntregado`, `RegistrarReciboLeido`, `ListarAuditoriaConversacion`.

**Archivos modificados:**
- `migrations/mensajeria/0006_auditoria_recibos.sql` (nuevo)
- `internal/auditoria/auditoria.go` (nuevo; reemplaza `doc.go`)
- `internal/conversacion/atencion.go` (nuevo)
- `internal/reclamos/reclamos.go`
- `adapters/http/conversacion/atencion.go` (nuevo), `adapters/http/conversacion/conversacion.go`
- `conversaciones_api.go`, `mensajeria.go`
- `adapters/http/conversacion/atencion_integration_test.go` (nuevo), `atencion_api_integration_test.go` (nuevo)
- `CHANGELOG.md`

**Verificación:** `gofmt` limpio, `go vet ./...`, `go build ./...`, `go build ./cmd/api ./cmd/mensajeria`, `go test ./...` con `MENSAJERIA_TEST_DB` (Postgres 18 local, 55 pruebas PASS, 0 SKIP) y sin él, `-race` en los paquetes tocados, `arch-test@v0.1.1` OK. Migraciones aplicadas dos veces seguidas sin error.
## 2026-11-02 17:59 — Pruebas E3-T3: versión previa a la transferencia y lectura sin envío

**Para quién:** quien cierra la definición de terminado de E3-T3. Faltaban dos pruebas: P20 (una escritura en vuelo con la versión previa a una transferencia se rechaza) y lectura-sin-envío tras perder el reclamo. Solo pruebas; no cambia código de producción.

**Resumen:** prueba de integración HTTP contra Postgres, como llama el bff.
- `TestVersionAnteriorATransferenciaHTTP`: tras transferir, el responsable anterior envía y libera con la versión previa: ambos `409 conflicto`, no queda mensaje, y el destino envía con la versión que le dio la transferencia (`201`).
- `TestLecturaSinEnvioTrasPerderReclamoHTTP` (por liberación y por transferencia): el ejecutivo anterior, con membresía vigente, obtiene la conversación y lista mensajes (`200`, ve el mensaje que envió con el reclamo) y su envío da `409 conflicto`.

**Hallazgo, no corregido aquí:** `Transferir` reabre el reclamo del destino en versión 1 en lugar de avanzarla (la historia pide "incrementa `version`"). En el escenario de una transferencia el `409` lo produce la guarda de actor, no la versión. Medido: tras A→B→A, un envío de A con la versión previa entra (`201`), y un reclamar con la versión previa reasigna el reclamo (`201`, versión 2). P20 queda abierta en esa variante hasta corregir `Transferir`.

**Cambios:**
- `adapters/http/conversacion/reclamo_perdido_integration_test.go` (nuevo).
- `CHANGELOG.md`.

**Verificación:** `gofmt` limpio, `go vet ./...`, `go build ./...`, `go build ./cmd/api ./cmd/mensajeria`, `go test ./...` con y sin `MENSAJERIA_TEST_DB` (Postgres 18 local, base desechable), `arch-test@v0.1.1` OK.

## 2026-11-02 17:55 — No filtrar el error interno en la respuesta HTTP (seguridad)

**Para quién:** cualquier cliente de las rutas HTTP de mensajeria (hoy el bff). Medido: un token con sujeto que no es UUID respondía `502` con el texto crudo de Postgres en el cuerpo (`"mensajeria: error interno: bandejas: iterar membresía: ERROR: invalid input syntax for type uuid: \"no-es-uuid\" (SQLSTATE 22P02)"`). Ahora responde `401 {"codigo":"no_autorizado","mensaje":"sin claims"}` sin llegar a SQL.

**Causa raíz:** los tres escritores de error (`conversacion`, `bandejas`, `emergencias`) emitían `err.Error()`, y `PlatformError.Error()` de pf-pkg-platform-go concatena la causa envuelta (`"%s: %v"`). Todo `NewRotoErr` (de `traducir` y de `internal/autorizacion`) llevaba su causa al cliente.

**Arreglo, en un solo lugar:** nuevo paquete `adapters/http/boundary`.
- `WriteError` emite solo `PlatformError.Message` (el error más externo de la taxonomía); un error fuera de la taxonomía sale como `"error interno"`. Status y código no cambian. Los tres `escribirError` delegan en él.
- `Claims` devuelve los claims solo si el sujeto es un UUID canónico; si no, `nil`, y cada handler responde el mismo `401 no_autorizado` que ya daba ante la ausencia de claims (un sujeto que no es UUID no identifica a ningún actor). Lo usan los handlers HTTP y `claimsDe` de la superficie en proceso. El sujeto es el único claim que llega a columnas UUID (`usuario_id`, `actor_real`); `tenant_id` es TEXT.

**Fuera de alcance:** no se agrega log de la causa: los adaptadores HTTP no usan logger y esta historia no añade infraestructura de logging. En la ruta HTTP la causa de un `roto` ya no queda visible en ningún lado; en proceso sigue disponible por `errors.Unwrap`. Un `{id}` de ruta que no es UUID sigue respondiendo `502 roto` (ya sin texto de Postgres); debería ser `404`, va en su propio cambio.

**Cambios:**
- `adapters/http/boundary/boundary.go` (nuevo) y su prueba.
- `adapters/http/conversacion/conversacion.go`, `adapters/http/bandejas/bandejas.go`, `adapters/http/emergencias/emergencias.go`: `boundary.Claims` y `escribirError` → `boundary.WriteError`.
- `emergencias_api.go`: `claimsDe` usa `boundary.Claims`.
- `adapters/http/bandejas/bandejas_test.go` (nuevo): reproduce el caso medido con un caso de uso que falla como Postgres; afirma 401, cuerpo literal y que no se llamó al caso de uso.
- `emergencias_api_test.go`: el sujeto de prueba pasa a UUID; nueva prueba de sujeto no UUID en proceso.

**Verificación:** `gofmt` limpio, `go vet ./...`, `go build ./...`, `go build ./cmd/api ./cmd/mensajeria`, `go test ./...` con y sin `MENSAJERIA_TEST_DB` (Postgres 18 local, base desechable), `arch-test@v0.1.1` OK. La prueba nueva de bandejas falla sobre `pf-msg-d` (502 con el texto de Postgres) y pasa con el arreglo.


## 2026:11:02 16:57 — Superficie en proceso de conversación y bandeja (`ConversacionesAPI`)

**Para quién:** `pf-svc-core`, que compone bff y mensajeria en un solo binario. Al subir core a bff v0.1.12 su adaptador `BFFMensajeriaEnProceso` ya no cumple `agregacion.MensajeriaPort`, porque el puerto pide conversación y bandeja y mensajeria solo exponía `EmergenciasAPI` en proceso. Con `Module.Conversaciones()` el adaptador de core queda en reenviar y descartar el bearer.

**Resumen:** nueva interfaz pública `ConversacionesAPI` (mismo patrón que `EmergenciasAPI`: solo tipos de contrato, claims leídos del ctx, taxonomía de error de la plataforma). Cubre `ObtenerConversacion`, `ListarMensajes`, `EnviarMensaje`, `ListarTrabajoBandeja` (la ruta sin `{id}`, DEC-103), `ReclamarConversacion`, `LiberarReclamo` y `TransferirConversacion`. Cada método repite el orden y el mapeo de error de su handler HTTP sobre las mismas instancias de casos de uso: un solo cableado (`handlersConversacion`/`handlersBandeja`) lo usan `Routes` y `Conversaciones()`, y la regla del alias comercial se reutiliza del handler (`ResolverAlias`), no se copia.

**Fuera de alcance:** `MarcarAtendida` y los recibos entregado/leído no se exponen: mensajeria no tiene esos casos de uso ni sus rutas HTTP. Por HTTP hoy responden 404 del mux.

**Pin de contrato:** `api-client-go` sube de `tracer.21` a `tracer.31` (trae `ListarTrabajoMiBandejaParams`). Compiló sin renombres de constantes de enum.

**Cambios:**
- Superficie en proceso `ConversacionesAPI` y accesor `Module.Conversaciones()`.
- `Routes` usa los mismos helpers de cableado que la superficie en proceso; el comportamiento HTTP no cambia.
- `resolverAlias` del handler de conversación pasa a exportado (`ResolverAlias`), sin cambio de lógica.
- Prueba de integración contra Postgres que llama la superficie como core: sin claims, bandeja (bootstrap, catch-up, sin pool → no_encontrado), lectura, denegación sin membresía (paridad con el 401 HTTP), reclamo y conflicto de versión (paridad con el 409 HTTP), envío idempotente, ventana de escritura (solo_lectura → conflicto), cursores excluyentes → validación, transferencia y liberación.

**Archivos modificados:**
- `conversaciones_api.go` (nuevo)
- `conversaciones_api_integration_test.go` (nuevo)
- `mensajeria.go`
- `adapters/http/conversacion/conversacion.go`
- `go.mod`, `go.sum`
- `CHANGELOG.md`

**Verificación:** `gofmt` limpio, `go vet ./...`, `go build ./...`, `go build ./cmd/api ./cmd/mensajeria`, `go test ./...` con y sin `MENSAJERIA_TEST_DB` (Postgres 18 local), `arch-test@v0.1.1` OK.

## 2026-10-30 — Bandeja del pool sin {id}: resolución por membresía (E8-T15, DEC-103)

**Para quién:** el bff (y a través de él, el portal interno), que ya expone `GET /api/v1/bandeja/trabajo` sin `{id}`. mensajeria ahora sirve esa ruta: resuelve la bandeja de chat del sujeto por su membresía efectiva del pool, sin que el llamante nombre la bandeja (cola única e implícita, DEC-103).

**Resumen:** cierra el hueco de E8-T15 (opción A) del lado de mensajeria. Monta `GET /api/v1/bandeja/trabajo` (hermana sin `{id}` de `listarTrabajoBandeja`). El handler resuelve la bandeja de chat del sujeto y comparte el resto de la lógica (bootstrap/catch-up) con la ruta por `{id}`, que se conserva para uso administrativo.

**Resolución y autorización:** `ResolverMiBandeja` consulta `pool_membresia ⋈ bandeja` filtrando `tipo = 'chat'` (no Reacción). Resolver la bandeja ES la autorización: si hay bandeja, el sujeto es miembro activo por construcción, así que no se añade una guarda `PuedeVerBandeja` aparte. Sin membresía de chat → 404 (como bandeja inexistente), incluida la membresía solo en una bandeja de otro tipo. Membresía activa en más de una bandeja de chat → error interno (`ErrMembresiaAmbigua`): es un dato inconsistente con la cola única y no se elige una en silencio.

**Cambios:**
- `internal/bandejas/bandejas.go`: `ResolverMiBandeja`, `ErrMembresiaAmbigua`.
- `adapters/http/bandejas/bandejas.go`: handler `miTrabajo`, ruta `GET /api/v1/bandeja/trabajo`, helper común `servirTrabajo` extraído de `trabajo`.
- `internal/bandejas/bandejas_integration_test.go`: `TestResolverMiBandeja` (resuelve, sin pool → 404, solo emergencias → 404, dos chat → ambigua).

**Verificación:** `gofmt`, `go vet ./...`, `go build ./...`, `go test ./...` (integración contra Postgres real, incluido el test nuevo), `arch-test` en verde.

**Pin de contrato:** sigue en `tracer.21`. El handler monta la ruta a mano y usa tipos de dominio que ya existen, así que no necesita el pin de `tracer.31` (que añade el operationId al spec). El pin se sube cuando `tracer.31` llegue a `-m` y tenga tag, para no fijar una pseudo-version.

## 2026-10-29 — E9-T8 (B.0): lectura de la conversación del viaje (obtenerConversacion, listarMensajes)

**Resumen:** monta los dos GET de lectura de conversación que el contrato (tracer.21) ya definía pero el servicio no servía: `GET /api/v1/conversaciones/{id}` (`obtenerConversacion`) y `GET /api/v1/conversaciones/{id}/mensajes` (`listarMensajes`). Es el pre-requisito de servicio (paso B.0) para exponer la conversación del viaje al portal cliente (E9-T8) a través del `bff`.

**Para quién:** desbloquea la Ola B. Cuando el `bff` proyecte la superficie de cliente, podrá leer la conversación y su historia reenviando a estos GET. La escritura (`enviarMensaje`) ya existía.

**Cambios:**
- `internal/conversacion`: `ObtenerConversacion` hidrata el agregado `Conversacion` (conversación + `estado_atendido` + ruta activa + reclamo activo en una lectura); `ultima_secuencia` = `siguiente_secuencia` (la última secuencia asignada; 0 sin mensajes); `version_reclamo` 0 si no hay reclamo. `ListarMensajes` pagina por secuencia canónica (`antes_de`/`despues_de`/`limite`, mutuamente excluyentes), orden de salida siempre ascendente, `hay_mas` por `limite+1`. Se extrae `mapearMensaje` (reusada por `MensajePorID`). `audiencia_autor` se deriva del rol al leer, como ya hacía el envío.
- `adapters/http/conversacion`: handlers `obtener` y `listarMensajes` + sus rutas; autorización de ACCESO reusando `PuedeEscribir` (cliente dueño o membresía de pool).

**Alcance (recorte ratificado):** B.0 hace solo los dos GET. `marcarAtendida` se difiere: es acción interna del Ejecutivo (superficie E3), no del portal cliente, y arrastra una regla de producto sin levantar (si exige el reclamo activo del propio actor). El campo "atendido" ya se expone en la lectura (`atendida_en`), null mientras nadie lo marque.

**Nota de seguridad (hallazgo):** el aislamiento del cliente sigue el criterio preexistente de `autorizacion`: 404 si la conversación no existe, 401 si existe pero el cliente no es el dueño. Difiere del 404-para-ajeno que usa `operacion` (E9-T7); unificar el criterio entre superficies de cliente es una decisión aparte, registrada en el plan de olas.

**Devuelve el agregado completo; el `bff` recorta por audiencia** para su schema de cliente (no expone `bandeja_id` ni `reclamo_activo`). El servicio no recorta.

**Verificación:** `gofmt`, `go vet ./...`, `go build ./...`, `go test ./...` en verde contra Postgres 16. Tests de dominio (hidratación, clamp de secuencia, orden/paginación/`hay_mas`, vacío, inexistente) y de handler (dueño ve 200, cliente ajeno 401).

## 2026-10-29 — E3-T3: eventos de reclamo publicados por outbox

**Resumen:** cada mutación del reclamo de una conversación ahora publica su evento de dominio en el outbox DENTRO de su misma transacción: `Reclamar` emite `conversacion.reclamada`, `Liberar` emite `conversacion.liberada`, `Transferir` emite `conversacion.transferida`. Antes `internal/reclamos` solo escribía el reclamo (y auditoría en la transferencia), sin dejar rastro en el bus. Completa el criterio de E3-T3 "Eventos conversacion.reclamada/liberada/transferida publicados por outbox".

**Para quién:** para `realtime` (y `reportes` en la transferencia), que el contrato tracer.21 declara consumidores durables de estos tres eventos. Es el puente hacia E3-T5: sin estos eventos, realtime no puede reflejar quién responde la conversación. El camino HTTP y la semántica de concurrencia no cambian; solo se añade la publicación.

**Cambios:**
- `internal/reclamos/reclamos.go`: `Servicio` gana un `Publicador` (interfaz mínima de un método declarada por el consumidor, satisfecha por `*outbox.Publisher`); `NewServicio(pool, pub)`. `Reclamar`, `Liberar` y `Transferir` publican su evento en la tx existente, con `sensibilidad=restringida`, `agregado_id=conversacion_id` y el payload que define el contrato (`conversacion_id`, `reclamo_id`/`reclamo_anterior_id`, `responsable_id`, `version_reclamo`). `abrir` y el cierre de transferencia ahora devuelven el `id` del reclamo vía `RETURNING id` para el payload. `Reclamar` emite el evento en sus dos caminos (abrir el primer reclamo y reasignar/avanzar uno activo), porque ambos cambian el responsable que realtime muestra.
- `mensajeria.go`: cablea el `Publisher` existente en `reclamos.NewServicio` (mismo `pub` que ya usa `conversacion`).
- `internal/reclamos/reclamos_integration_test.go`: los tres tests existentes migran al nuevo constructor. Nuevo `TestEventosDeReclamoSeEscribenEnOutbox`: tras reclamar/transferir/liberar afirma exactamente una fila del tipo esperado en `mensajeria.outbox`, restringida, con el payload del contrato (responsable y versión).

**Fuera de alcance (no es trabajo del Lead):** el override auditado de supervisor sigue cerrado (`ErrOverrideNoImplementado`, DEC-107): requiere la proyección de alcance jerárquico que mensajeria no tiene (DEC-053/DEC-045). Es bloqueo de producto, no de este slice.

**Verificación:** `gofmt`, `go build ./...`, `go vet ./...` verdes. Suite completa contra Postgres 16 real (Docker efímero): `TestEventosDeReclamoSeEscribenEnOutbox` PASS y las pruebas de concurrencia/liberar/transferir/HTTP intactas.

## 2026-10-28 — E3-T1: ciclo de vida de la conversación por evento del viaje

**Resumen:** la conversación de un viaje ahora nace y fija su ventana de escritura por EVENTO, no por petición. Dos consumidores durables nuevos sobre la superficie amplia del bus: `viaje.mensajeria_habilitada` crea la conversación (idempotente); `viaje.cerrado` fija `escritura_hasta` (3 días sin incidencia, 6 meses con incidencia, DEC-030/DEC-041). Más la columna `legal_hold` que faltaba en la tabla. No existe endpoint para que el cliente cree una conversación: solo el evento la crea.

**Para quién:** cierra el ciclo que da sustento al envío de E3-T2: antes la conversación se sembraba a mano en los tests; ahora la crea el evento real de operacion (viaje.mensajeria_habilitada, que operacion emite desde este mismo sprint).

**Cambios:**
- `migrations/mensajeria/0005_conversacion_legal_hold.sql` (nuevo): columna `legal_hold BOOLEAN NOT NULL DEFAULT false`. Solo la forma; la política de retención/legal hold sigue pendiente (DEC-016 #3).
- `internal/conversacion/conversacion.go`: `CrearPorEvento` (INSERT idempotente por el índice único parcial `una_conversacion_activa_por_viaje`, `ON CONFLICT DO NOTHING`; tenant_id = cliente_id, DEC-049) y `CerrarVentana` (fija `escritura_hasta` según motivo_cierre, acotado por viaje_id, convergente). No usan inbox_evento: el invariante de unicidad del negocio es el árbitro (DEC-067, igual que el envío).
- `internal/consumidores/conversacion.go` (nuevo): `ConsumidorHabilitada` y `ConsumidorCerrado`, puertos estrechos, idempotencia durable. `viaje.acceso_revocado` NO se implementa (su productor no existe en operacion; E3-T1 lo marca bloqueado).
- `mensajeria.go`: cablea los dos consumidores en `New()` y los devuelve en `Workers()`, replicando el patrón del consumidor de catálogo.
- `internal/conversacion/conversacion.go` (`Enviar`): la guarda de envío ahora también rechaza si `escritura_hasta` ya venció (no solo si el estado no es `abierta`), con `ErrConversacionSoloLectura`. Cierra el hueco entre el vencimiento de la ventana y el barrido que materializará `solo_lectura`: el estado se deriva de `escritura_hasta` en el momento del envío. La condición viaja en el mismo `UPDATE` (atómica, sin lectura previa).

**Verificación:** `go build/vet ./...`, `gofmt`, `arch-test` verdes. `go test ./internal/conversacion/` contra Postgres 16 real: `TestCrearPorEventoEsIdempotente` (reentrega no duplica, tenant=cliente) y `TestCerrarVentanaFijaEscrituraHasta` (3d + 6m) PASS, suite de E3-T2 intacta.

## 2026-10-28 — E3-T2: exigir reclamo activo en el envío interno; override cerrado en V1

**Resumen:** completa la autorización de envío de E3-T2. Un actor interno ahora debe tener un reclamo activo propio en la versión que afirma (`version_reclamo_esperada`) para enviar; sin él recibe 409. Antes el handler ignoraba ese campo del contrato y bastaba la membresía de pool. El override de supervisor (`motivo_override`) queda **cerrado en V1**: se rechaza con 400 hasta que exista la proyección de alcance jerárquico que lo autorizaría (DEC-107).

**Para quién:** para el panel interno y auditoría. Responder una conversación exige el reclamo, no solo pertenecer al pool. El cliente no cambia (no reclama). El override no se abre sin verificación de rol, por seguridad.

**Cambios:**
- `internal/conversacion/conversacion.go`: `EntradaEnvio` gana `VersionReclamoEsperada *int64` y `MotivoOverride string`. Sentinels `ErrSinReclamoActivo` y `ErrOverrideNoImplementado`. `Enviar` rechaza `motivo_override` antes de abrir la tx (override cerrado, DEC-107); para audiencia interna, `exigirReclamoActivo` verifica en la misma tx —antes de tocar la secuencia— que hay reclamo activo del propio actor en la versión esperada. Rechazo sin efecto. Helpers `esInterno` y `exigirReclamoActivo`.
- `adapters/http/conversacion/conversacion.go`: el handler pasa `version_reclamo_esperada` y `motivo_override` del body; mapea `ErrSinReclamoActivo` a 409 `conflicto_version_reclamo` y `ErrOverrideNoImplementado` a 400. (Códigos de máquina: la plataforma solo emite el código de categoría, limitación transversal preexistente.)
- Test `internal/conversacion/guarda_reclamo_test.go`: 6 casos (interno sin versión, versión equivocada, reclamo de otro actor, reclamo correcto, cliente sin reclamo, override cerrado rechazado sin efecto).

**Decidido en esta entrega:** DEC-107 (override cerrado en V1); DEC-108 abierta (condición "pendiente de respuesta", bloqueada por contrato).

**Verificación:** `go build/vet ./...`, `gofmt`, `arch-test` verdes. Tests de integración contra Postgres 16 real: `TestEnviarGuardaReclamoInterno` (6 subcasos) PASS, y la suite previa de conversación (envío, reclamo, transferencia, guarda de estado) sigue verde — `TestEnviarMensajeHTTP` ahora ejercita la guarda de reclamo que antes ignoraba.

## 2026-10-28 — E3-T2: guarda de estado en el envío (rechaza conversación no `abierta`)

**Resumen:** cierra un hueco de seguridad del envío: hasta ahora un mensaje sobre una conversación `solo_lectura` o `cerrada` se aceptaba e insertaba. Ahora `Enviar` rechaza cualquier conversación que no esté `abierta`, sin crear mensaje, sin incrementar `siguiente_secuencia` y sin publicar `mensaje.creado`. Es parte del criterio de autorización de envío de E3-T2 (AI27-61); la guarda de reclamo activo + versión esperada (interno) sigue pendiente en su propia unidad.

**Para quién:** para el cliente y el panel interno, que al intentar escribir en una conversación cuya ventana operativa venció (DEC-030/DEC-041) reciben 409, no un mensaje fantasma. Para auditoría: ya no entran mensajes a conversaciones cerradas.

**Cambios:**
- `internal/conversacion/conversacion.go`: nuevo sentinel `ErrConversacionSoloLectura`. La guarda viaja en el mismo `UPDATE ... WHERE estado = 'abierta'` que toma el lock de fila y asigna la secuencia (atómico, sin lectura previa que abriría una carrera). Si no toca fila, `noAbiertaOInexistente` desambigua en la misma tx inexistente (no_encontrado) vs no-abierta (`ErrConversacionSoloLectura`), solo en el camino de rechazo.
- `adapters/http/conversacion/conversacion.go`: el handler `enviar` mapea `ErrConversacionSoloLectura` a 409. Nota: el `codigo` de máquina del contrato (`conversacion_solo_lectura`) no se emite literal; la taxonomía de la plataforma solo expone el código de categoría (`conflicto`), limitación transversal preexistente (igual que `conflicto_version_reclamo` en el handler de reclamo).
- Tests (integración contra Postgres): `internal/conversacion/guarda_estado_test.go` (dominio, casos `solo_lectura` y `cerrada`, afirma efectos nulos); `adapters/http/conversacion/enviar_integration_test.go` suma `TestEnviarMensajeHTTPConversacionSoloLectura` (409 por HTTP).

**Vocabulario del contrato (no inventado):** `EstadoConversacion` enum `[abierta, solo_lectura, cerrada]` y el código `conversacion_solo_lectura` salen de `mensajeria.yaml` (tracer.21).

**Verificación:** `go build/vet ./...`, `gofmt`, `arch-test` verdes. Tests de integración ejecutados contra Postgres 16 real: `TestEnviarRechazaConversacionNoAbierta` (solo_lectura + cerrada), `TestEnviarMensajeHTTPConversacionSoloLectura`, y los dos del camino feliz ya existentes, todos PASS.

## 2026-10-28 — tracer.21: subir el pin de contrato

**Resumen:** se sube el pin de `pf-pkg-contracts` de `v0.1.0-tracer.17` a `v0.1.0-tracer.21` (+4 olas: .18 a .21). Aditivo para `mensajeria`; su superficie (conversación, bandeja, emergencia) no cambia entre .17 y .21. Solo `go.mod`/`go.sum`.

**Para quién:** alineación de versión de contrato con el resto del backend.

**Cambios:**
- `go.mod`/`go.sum`: `api-client-go` `v0.1.0-tracer.17` → `v0.1.0-tracer.21`.

**Verificación:** `go build/vet ./...`, `go test ./...` (10 paquetes ok), `gofmt` limpio, `arch-test` (v0.1.1) PASS. Sin renombres de enum (§8): el build quedó verde sin tocar constantes, pese al salto de 4 olas.

## 2026-10-27 — Superficie en proceso del carril de emergencia (E7, para core)

**Para quién:** para `pf-svc-core`, que al co-ubicar `mensajeria` dentro del binario compuesto necesita resolver la arista `bff→mensajeria` EN PROCESO (hasta ahora el diseño la daba por SIEMPRE HTTP). Sin esta superficie, componer `bff` v0.1.4 en core no compilaba: el puerto `bff.MensajeriaPort` no tenía a quién delegar en proceso.

**Qué cambia:** `mensajeria` expone `EmergenciasAPI` (archivo `emergencias_api.go`, paquete raíz), tipada con `msgclient.*`, con la misma forma que `bff.MensajeriaPort` (los 8 métodos de emergencia). Se construye con `(*Module).Emergencias()` sobre los casos de uso que el `Module` ya tiene (`m.emergencias`, `m.autz`); no toca `New` ni abre recursos. Es el análogo de `operacion.ConsultasAPI` y `catalogos.ConsultasAPI` para la arista entrante en proceso. El adaptador en proceso vive en `pf-svc-core`.

**Dónde vive la lógica de contrato:** `EmergenciasAPI` encapsula autorizar (`PuedeAtenderEmergencia`), proyectar por audiencia (ADR 21 §6: el cliente no ve origen ni actor_real), traducir errores de dominio a `PlatformError`, y orquestar `resolver = Resolver + AgregarNota(es_resolucion)`. Lee los claims del ctx (`auth.ClaimsFromContext`), igual que el handler HTTP; el `bearer` del puerto del `bff` lo consume el adaptador HTTP del `bff`, no esta superficie.

**Cambios:**
- `emergencias_api.go` (nuevo): interfaz `EmergenciasAPI`, impl `emergenciasAPI` sobre interfaces de consumidor `casoDeEmergencia`/`autorizador` (satisfechas por `*emergencias.Servicio` y `*autorizacion.Servicio`, comprobado en compilación), y las funciones puras de proyección/traducción.
- `emergencias_api_test.go` (nuevo): comportamiento de la proyección por audiencia (cliente oculta origen/actor_real, interno los ve) y rechazo sin claims. Sin BD.

**Shortcuts registrados (con su condición de upgrade, en el código):**
1. `ReclamarEmergencia`/`TransferirEmergencia` devuelven un `msgclient.Reclamo` con solo `Version` poblada (reclamar) o vacío (transferir): el dominio no construye un Reclamo completo y el carril HTTP tampoco lo devolvía (201 sin cuerpo). Upgrade si el panel llega a consumir id/responsable/timestamps.
2. `proyectar`/`traducir`/etc. quedan duplicadas con `adapters/http/emergencias` (funciones puras idénticas). No se dedupe porque el handler vive en un paquete que el root importa (ciclo), y extraerlas metería `auth` en el dominio y reescribiría un carril de seguridad sin tests de forma. Upgrade: extraer a `internal/emergencias` si la regla de audiencia cambia.

**Verificación:** `go build/vet ./...`, `go test ./...` (unit de proyección verde, integración de dominio intacta), `arch-test` (sigue hoja: catalogos/identidad), `go build ./cmd/api ./cmd/mensajeria`.

## 2026-10-21 — Catch-up de la colección de bandeja (M3, contrato tracer.14)

**Para quién:** para el panel interno que lista el trabajo de una bandeja y lo mantiene vivo. `listarTrabajoBandeja` deja de ser un listado plano: ahora el cliente obtiene el trabajo con la `revision` de la colección (bootstrap) y pide con `desde_revision` solo lo que cambió (catch-up), el protocolo que ADR 28 §3 exige y que la conversación ya tenía.

**Pin de contrato:** `pf-pkg-contracts` de `tracer.7` a `tracer.14` (trae `TrabajoBandeja`, `RevisionBandeja`, el parámetro `desde_revision` y el `409 stream_resync_required` de bandeja).

**Modelo de la revisión (Opción 1):** `ruta_conversacion` gana una columna `revision` (migración `0003`). Cada cambio de la colección avanza `bandeja.revision` y estampa esa revisión en la fila de ruta afectada, en la misma tx. El catch-up es `rutas con revision > desde`, incluidas las inactivas (bajas), para que el cliente retire lo que salió. Una baja no borra la fila: la deja `activa=false` con su revisión de cambio. Sin tabla de deltas aparte (la Opción 2 se descartó por maquinaria que M3 en V1 no pide).

**Cambios:**
- `migrations/mensajeria/0003_ruta_revision.sql` (nuevo): columna `revision` en `ruta_conversacion` + índice `(bandeja_id, revision)` para el camino caliente del catch-up. Aditivo, hereda los grants de `0002`.
- `internal/bandejas/bandejas.go` (reemplaza el stub): `Trabajo` (bootstrap: activas + revisión de colección) y `CambiosDesde` (catch-up: rutas con `revision > desde`, incluidas bajas). Hidrata el `Conversacion` del contrato uniendo `ruta_conversacion` → `conversacion` + LEFT JOIN al reclamo activo para `version_reclamo`. Sin N+1.
- `internal/autorizacion/autorizacion.go`: `PuedeVerBandeja` (membresía activa en ESA bandeja; el cliente no lista bandejas; bandeja inexistente → no_encontrado).
- `adapters/http/bandejas/bandejas.go` (nuevo): `GET /api/v1/bandejas/{id}/trabajo`, autoriza, decide bootstrap vs catch-up por `desde_revision`, devuelve `TrabajoBandeja`.
- `mensajeria.go`: `New` construye el servicio de bandejas; `Routes` monta la ruta.

**Nota sobre el 409:** en V1, con revisión por fila durable y sin purga, el catch-up siempre reconstruye desde cualquier revisión, así que `fueraDeVentana` es false y el `409 stream_resync_required` no se dispara. Queda como capacidad del contrato para cuando se introduzca una ventana de retención con purga (ADR 28 §5).

**Verificado:** `go build`/`vet` verde, `gofmt` limpio, `arch-test` OK, suite completa verde con `MENSAJERIA_TEST_DB`. Tests de integración contra Postgres: dominio (`TestTrabajoYCatchUp`: bootstrap, catch-up incremental, baja reportada) y handler (`TestListarTrabajoBandejaHTTP`: 401 sin membresía, 200 bootstrap con revisión, catch-up por `desde_revision`).

## 2026-10-21 — CI: workflows de promoción d→q y q→m

**Para quién:** para quien promueve mensajería por el pipeline. Antes el repo solo tenía `ci.yml`; a diferencia de core y reportes, no abría el PR de promoción `-d → -q` automático, así que los cambios en `pf-msg-d` no generaban su PR de promoción y el tramo quedaba manual y fácil de olvidar.

**Cambios:**
- `.github/workflows/promotion-pr-dq.yml` (nuevo): en cada push a `pf-msg-d` mantiene abierto el PR `pf-msg-d → pf-msg-q` y reescribe su cuerpo con los feature-PRs pendientes de promover. No fusiona; la promoción se clickea a mano.
- `.github/workflows/promotion-pr-qm.yml` (nuevo): equivalente para `pf-msg-q → pf-msg-m`.

Copiados sin cambios de `pf-svc-core` (idénticos a los de `pf-svc-reportes`): derivan las ramas del sufijo, sin prefijos hardcodeados, así que funcionan en mensajería tal cual.
## 2026-10-19 — Ciclo de vida del reclamo por HTTP: Liberar + Transferir

**Para quién:** para el ejecutivo que suelta o traspasa una conversación desde el panel. Completa el ciclo del reclamo iniciado con ReclamarConversacion: reclamar, liberar (devolver al pool) y transferir (pasar la responsabilidad a otro miembro sin perder historia).

**Cambios:**
- `internal/reclamos/reclamos.go`: `Liberar` cierra el reclamo activo exigiendo su rid y versión (concurrencia optimista); `ErrConflictoVersion` si la versión no coincide, `ErrSinReclamo` si no hay activo. `Transferir` cierra el reclamo del origen y abre uno para el destino en versión 1, en una sola transacción (addendum §4 punto 6 y §6), y audita el hecho en `auditoria_mensajeria` (addendum §11). El índice único `un_reclamo_activo_por_conversacion` garantiza a lo más uno activo; el test afirma exactamente 1 tras transferir.
- `adapters/http/conversacion/conversacion.go`: `DELETE /api/v1/conversaciones/{id}/reclamos/{rid}?version_esperada=N` (liberar, 204) y `POST /api/v1/conversaciones/{id}/transferencias` (transferir, 201 con el nuevo Reclamo). Ambos detrás de la guarda de autorización (DEC-097). Conflicto de versión → 409, sin reclamo → 404.
- `mensajeria.go`: las dos rutas quedan montadas por `Routes` (la interfaz `Reclamador` del adaptador gana `Liberar` y `Transferir`).

**Verificado:** `go build`/`vet` verde, `gofmt` limpio, `arch-test` OK, suite completa verde con `MENSAJERIA_TEST_DB`. Tests de integración nuevos contra Postgres: dominio (`TestLiberarCierraElReclamoYDevuelveAlPool`, `TestTransferirCierraYAbreEnUnaTx`) y handler (liberar 204/404 en `TestReclamarConversacionHTTP`, `TestTransferirConversacionHTTP` 201 con responsable = destino).

**Alcance:** el override auditado de Supervisor/Coordinador (enviar/transferir saltándose el reclamo) no se implementa aquí; la transferencia normal exige versión esperada. Emergencia (M9) y catch-up de bandeja (M3) siguen esperando tracer.9.

## 2026-10-19 — Capa HTTP de conversación: EnviarMensaje con alias por cliente (DEC-097)

**Para quién:** para el flujo del ejecutivo que responde al cliente desde el panel. Con el reclamo activo, un ejecutivo envía un mensaje por HTTP y el cliente lo ve firmado con el nombre comercial de su cuenta. Completa el par con ReclamarConversacion del cambio anterior.

**Alias comercial por cliente (DEC-097 forma 1, Opción 2):** el `alias_mostrado` del mensaje es el nombre del cliente de la conversación, resuelto en el handler desde `catalogos` (puerto `CatalogosLector` ya existente, tracer.7). El dominio no llama a catalogos en la ruta caliente; la frontera compone el alias y lo pasa a `Enviar`, que lo persiste (inmutable, addendum §6).

**audiencia_autor derivada, no persistida:** el contrato exige `audiencia_autor`, pero el esquema no tiene esa columna y el addendum §6 solo lista tres campos inmutables (actor real, rol al enviar, alias mostrado). Se deriva del `rol_al_enviar` al leer (`cliente` ⇒ Cliente, resto ⇒ Interno): es estructural, da el mismo valor que persistir. Sin columna nueva, sin migración.

**Cambios:**
- `internal/conversacion/conversacion.go`: `EntradaEnvio` gana `AliasMostrado` y `RolAlEnviar`; el INSERT de `Enviar` los persiste (`NULLIF` para vacío ⇒ NULL). El camino caliente (idempotencia + outbox en una tx) no cambia. Nuevos lectores de hidratación `MensajePorID` (mapea al `Mensaje` del contrato, deriva la audiencia) y `ClienteDeConversacion` (da el `cliente_id` para resolver el alias).
- `adapters/http/conversacion/conversacion.go`: handler de `POST /api/v1/conversaciones/{id}/mensajes`. Autoriza (DEC-097), resuelve el alias por cliente, delega en el dominio, hidrata el `Mensaje` y devuelve 201. Si catalogos cae o no hay cliente, el alias queda vacío y el envío sigue (degradación, mensajeria.md (f)).
- `mensajeria.go`: `Routes` monta también EnviarMensaje, inyectando `m.conversacion` y el puerto `m.deps.Catalogos` (la misma instancia cacheada que arma `New`).

**Verificado:** `go build`/`vet` verde, `gofmt` limpio, `arch-test` OK, suite completa verde con `MENSAJERIA_TEST_DB`. `TestEnviarMensajeHTTP` (nuevo) contra Postgres real: envío con reclamo activo devuelve 201 con `alias_mostrado` resuelto desde catalogos y `audiencia_autor` derivada; reintento idempotente por `client_message_id` devuelve el mismo mensaje. El test de dominio original de envío idempotente sigue verde.

## 2026-10-19 — Capa HTTP de conversación: ReclamarConversacion + autorización (DEC-097)

**Para quién:** para el flujo interno de tomar una conversación desde el panel. Un Ejecutivo con membresía en el pool de la bandeja de una conversación puede reclamarla por HTTP; el bff ya tiene una ruta real que montar. Antes `Routes()` no montaba ninguna ruta de negocio y `internal/autorizacion` era un stub.

**Qué desbloqueó (DEC-097):** Producto cerró la mecánica de autorización de mensajería. El vínculo Ejecutivo↔cliente se administra a mano en `pool_membresia` (no se deriva de otro servicio, sin arista de contrato nueva). El cliente es multiusuario y la conversación se acota por `cliente_id`. El esquema ya tenía la forma; el stub pasó a implementación.

**Cambios:**
- `internal/autorizacion/autorizacion.go` (reemplaza el stub `doc.go`): `Servicio.PuedeEscribir` decide el acceso a una conversación desde la proyección local. Rama Cliente: el `cliente_id` de la conversación iguala el `sub` del token. Rama Interno: membresía activa en el pool de la bandeja a la que la conversación está enrutada (`ruta_conversacion` activa → `pool_membresia` activa). Conversación inexistente es `no_encontrado`, sin membresía es `no_autorizado` (addendum §7: poseer el UUID no autoriza).
- `adapters/http/conversacion/conversacion.go` (nuevo): handler de `POST /api/v1/conversaciones/{id}/reclamos`. Toma claims del contexto (los pone `auth.Middleware` en el bff), autoriza, reclama con la versión esperada e hidrata el `Reclamo` del contrato desde la fila. El conflicto de versión del dominio se traduce a 409. Helpers `escribirJSON`/`escribirError` con la forma `Error` del contrato, mismo patrón que reportes.
- `internal/reclamos/reclamos.go`: `ReclamoActivo` lee el reclamo activo y lo mapea al `Reclamo` del contrato (hidratación para el handler; el camino caliente de `Reclamar` no cambia). `ErrSinReclamo` nuevo.
- `mensajeria.go`: `New` construye el autorizador sobre el pool de chat; `Routes` monta el handler. EnviarMensaje NO se monta aún: su respuesta exige `alias_mostrado` (identidad comercial por cliente, DEC-097 forma 1), que se persiste en el camino de escritura; va en su propio slice. Emergencia (M9) y catch-up de bandeja (M3) siguen esperando tracer.9.
- `go.mod`/`go.sum`: se agrega `golang-jwt/jwt/v5` (transitiva de `platform-go/auth`).

**Verificado:** `go build`/`vet` en verde, `gofmt` limpio, `arch-test` OK, suite completa en verde con `MENSAJERIA_TEST_DB`. Tests de integración nuevos contra Postgres real: `TestPuedeEscribir` (5 ramas de autorización) y `TestReclamarConversacionHTTP` (401 sin membresía, 201 con `Reclamo` hidratado, 409 por conflicto de versión).

## 2026-10-20 — cmd/migrate: bootstrap de base de datos (E0-T7, DEC-096)

**Resumen:** Nuevo binario `cmd/migrate`: el bootstrap de esquema, rol y grants de `mensajeria`. Corre una vez en el despliegue con credencial administrativa (`DB_ADMIN_URL`), antes de `cmd/mensajeria`. Aplica las migraciones embebidas (`migrations.FS`) con el paquete `migrate` de platform-go (v0.1.6) y fija el `search_path` de `svc_mensajeria` para que el relay de outbox resuelva la tabla `outbox`.

**Por qué un binario aparte (DEC-096):** el `0001` hace `CREATE SCHEMA`/`CREATE ROLE`, privilegio que el pool de app de `mensajeria` (rol `svc_mensajeria`, privilegio mínimo) no tiene ni debe tener (I1). `cmd/migrate` lo confina al paso de despliegue y termina; el servicio arranca después con su rol de app y asume la base migrada. Mismo patrón que `cmd/migrate` de core.

**Cambios:**
- `cmd/migrate/main.go` (nuevo): bootstrap de un esquema (`mensajeria` → `svc_mensajeria`) con una conexión admin; `ALTER ROLE ... search_path` tras aplicar (DEC-096 nota 3).
- `cmd/mensajeria/main.go` y `cmd/api/main.go`: se retira el TODO de migraciones; se documenta que el bootstrap es externo y que el servicio asume la base migrada.
- `go.mod`/`go.sum`: `pf-pkg-platform-go` a `v0.1.6` (trae el paquete `migrate`).

**Verificado:** `go build`/`vet` en verde, `gofmt` limpio, `arch-test` OK. Bootstrap probado end-to-end contra Postgres efímero: crea el esquema y el rol, la tabla `outbox` queda con INSERT/UPDATE para el relay, el `search_path` fijado, e idempotente al reaplicar.

## 2026-10-15

**Resumen:** Lado consumidor de E5-T5 (AI27-77) en `mensajeria`: caché en proceso de clientes detrás del puerto `CatalogosLector`, invalidada por el evento `catalogo.actualizado` que publica `catalogos`. El productor ya emitía el evento (E5-T1..T4); esto cierra la mitad consumidora para `mensajeria`.

**Para quién:** para la ruta de bandeja/conversación que resuelve datos de cliente por lote. Antes cada lectura pegaba HTTP a `catalogos` (adaptador `CatalogosHTTP`) sin caché ni invalidación. Ahora, cuando hay bus, esas lecturas se sirven de una caché en proceso con techo de obsolescencia por TTL e invalidación por evento.

**Cambios:**
- `internal/catalogoscache/clientes.go` (nuevo): `CacheClientes` decora `CatalogosLector`, cacheando cada `Cliente` por su `id`. `ListarClientes` combina los aciertos vivos de caché con UN solo viaje a `catalogos` por los faltantes/vencidos (nunca N+1), preservando el orden pedido. TTL corto (default 30 s), reloj inyectable. Degradación (mensajeria.md (f)): si `catalogos` cae, sirve lo que haya —incluso obsoleto— y solo propaga el error degradado cuando no hay nada que servir; nunca inventa un cliente. Interfaz del puerto declarada en el propio paquete para no importar la raíz (evita ciclo).
- `internal/consumidores/catalogo.go` (nuevo): `ConsumidorCatalogo` procesa `catalogo.actualizado`, deduplica por `evento_id` en memoria (ADR 34 / DEC-067: efecto en memoria, sin tabla `inbox_evento`) e invalida la entrada de `cliente`. Payload ilegible o incompleto se acusa sin invalidar.
- `mensajeria.go`: con `Subscriber` y `Catalogos` presentes, `New` envuelve el puerto con la caché (misma instancia que invalida el consumidor) y abre el consumidor durable `mensajeria-catalogos` (superficie amplia, filtro `catalogo.actualizado`); `Workers()` lo expone. Sin `Subscriber` no se cachea.
- Tests: unit de caché (hit/TTL/combina lote/degradación/invalidación/idempotencia), unit de consumidor, e2e con el bus en memoria real, y cableado del worker en `New()`/`Workers()` con y sin `Subscriber`. Los 2 tests de `Workers()` previos siguen verdes.

**Alcance V1:** solo se cachea la entidad `cliente` (contrato/contacto están bloqueados por contrato, brecha C-cat: `catalogos.yaml` no los expone). Un `catalogo.actualizado` de otra entidad es no-op.

**Brecha C10:** el evento no lleva campo `version` en el proto; la versión viaja en el sobre (`bus.Sobre.Version`). El consumidor invalida de forma incondicional y no la necesita.

**Verificado:** `go build ./...`, `go vet ./...`, `go test ./...` (todo verde).

## 2026-10-14

**Resumen:** Scaffold inicial del módulo `mensajeria` sobre el contrato `v0.1.0-tracer.7` (conversación de negocio; DEC-089). Es estructura, no comportamiento: los servicios de dominio devuelven "no implementado" salvo los dos caminos que las pruebas ejercen.

**Cambios:**
- Superficie pública en `mensajeria.go`: `New(Pools{Chat,Despacho}, Deps)`, `Routes`, `Workers` (sin `Consultas`: no es destino de arista síncrona en proceso).
- Cuatro contabilidades como tipos nombrados distintos y no convertibles en `internal/tipos` (`SecuenciaMensaje`, `VersionReclamo`, `RevisionBandeja`, `RevisionEmergencia`), con prueba de arquitectura por compilación.
- Migración `mensajeria`: esquema, rol `svc_mensajeria`, 15 tablas, 4 índices únicos parciales. Privilegio mínimo por tabla en `0002_grants.sql`: `mensaje`, `adjunto`, `intento_entrega`, `inbox_evento` y `auditoria` son append-only, sin DELETE en ninguna tabla (DEC-030/041).
- Dos raíces: `cmd/api` (standalone, sin workers) y `cmd/mensajeria` (producción, arranca `Workers()`: hoy solo el relay de outbox).
- Mampara de despacho externo en `internal/despacho` sin adaptadores ni worker activo ni destinatario cliente (DEC-023).
- Adaptador HTTP a `catalogos` para la lectura por lote de clientes; arista a `identidad` solo revocación.
- Dos pruebas de comportamiento contra Postgres real: envío idempotente por `client_message_id` (mensaje + outbox en una transacción), y reclamo concurrente con ganador y conflicto de versión.
- Stubs declarados que esperan `tracer.9`: `internal/emergencias` (familia de emergencia) e `internal/bandejas` (catch-up). `internal/pool` e `internal/autorizacion` esperan el vínculo Ejecutivo↔cliente (DEC-045).

**Verificación:** `gofmt -l` vacío; `go build`, `go vet`, `arch-test@v0.1.1` y `go test ./...` en verde, con las dos pruebas de integración pasando contra Postgres. Comprobado contra Postgres que `svc_mensajeria` no puede borrar un mensaje (0 tablas con DELETE).

## 2026:10:06 12:55

**Resumen:** Se agregó la plantilla de Pull Request del backend, adaptada al stack Go de AI27.

**Cambios:**
- Se agregó `.github/pull_request_template.md` con secciones de invariantes de arquitectura (I1-I5, contrato primero), base de datos, eventos/bus, verificación (go build/vet/test, arch-test, gofmt), CHANGELOG y checklist de convenciones.

**Archivos modificados:**
- `.github/pull_request_template.md`

## 2026:10:06 12:48

**Resumen:** Se agregó la regla de steering de CHANGELOG y este archivo, antes de que el repo tenga código, para que la convención esté vigente desde el inicio.

**Cambios:**
- Se agregó `.kiro/steering/changelog.md` (`inclusion: always`): toda modificación del repo debe registrarse aquí, en español, con marca de tiempo, resumen, cambios y archivos modificados.
- Se creó `CHANGELOG.md`.

**Archivos modificados:**
- `.kiro/steering/changelog.md`
- `CHANGELOG.md`
