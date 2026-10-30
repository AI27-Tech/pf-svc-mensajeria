# CHANGELOG

## 2026-10-30 — E9-T8: lectura de la conversación y marcarAtendida

**Resumen:** se montan las tres operaciones de lectura/estado que el contrato ya definía pero el servicio no exponía: `GET /api/v1/conversaciones/{id}` (`obtenerConversacion`), `GET /api/v1/conversaciones/{id}/mensajes` (`listarMensajes`) y `POST /api/v1/conversaciones/{id}/atendida` (`marcarAtendida`). Antes `Montar()` solo registraba reclamo, transferencia y envío; ahora el `bff` puede proyectar al cliente el detalle de la conversación, su historia de mensajes y el indicador de "atendido" (DEC-014/Q113). El envío (`enviarMensaje`, E3-T2) no se rehace.

**Para quién:** para el `bff` y, a través de él, para el portal cliente (E9) y el panel interno. La lectura funciona en los tres estados del ciclo de vida (`abierta`/`solo_lectura`/`cerrada`); la escritura de envío sigue rechazando `solo_lectura`/`cerrada` con 409 `conversacion_solo_lectura` (guarda preexistente de E3-T2, verificada: sentinel `ErrConversacionSoloLectura` + `codigo` del contrato, DEC-030/DEC-041).

**Proyección por audiencia (contrato Conversacion/Mensaje; patrón de `emergencias`):**
- Un lector **Cliente** recibe la vista mínima: `Conversacion` sin `bandeja_id` ni `reclamo_activo`; cada `Mensaje` sin `actor_real` ni `rol_al_enviar`. Sí recibe `version_reclamo`, el estado atendido (`atendida_en`) y, en cada mensaje, `audiencia_autor` —el "de_cliente": quién escribió, lado Cliente o AI27—, derivado del `rol_al_enviar` (cliente ⇒ Cliente; resto ⇒ Interno), nunca de `actor_real`.
- Un lector **Interno** recibe la vista completa (bandeja de la ruta activa y reclamo activo hidratado).

**Autorización:** las lecturas y `marcarAtendida` pasan por la guarda de acceso existente (`autorizacion.PuedeEscribir`, DEC-097: cliente dueño o membresía efectiva de pool). En las lecturas, "sin acceso efectivo" se traduce a **404**, no 401: poseer el UUID no autoriza y el servidor no revela existencia por el código (contrato `obtenerConversacion`/`listarMensajes`, Addendum §7). `marcarAtendida` exige además, en el dominio, que el actor sea el **responsable del reclamo activo** con la versión esperada (concurrencia optimista, igual que el envío interno): sin reclamo propio → 404; versión equivocada → 409 `conflicto_version_reclamo`.

**Cambios:**
- `internal/conversacion/conversacion.go`: `ObtenerConversacion` (cabecera + reclamo activo + bandeja de ruta activa + marca de `estado_atendido`, en una tx coherente; `ConversacionProyectada` con todos los campos de dominio). `ListarMensajes` (`EntradaListado`/`ResultadoListado`: paginación por cursor `antes_de`/`despues_de` mutuamente excluyentes, `limite` 1..200; orden siempre por `secuencia`, nunca por `creado_en`; `hay_mas` por el truco de pedir `limite+1`). `MarcarAtendida` (guarda de responsable + versión, upsert idempotente de `estado_atendido`; sentinel `ErrNoEsResponsable` y error tipado `ErrConflictoVersionAtendida`).
- `adapters/http/conversacion/conversacion.go`: `Montar()` registra las tres rutas. Handlers `obtener`, `listarMensajes`, `marcarAtendida`; proyectores `proyectarConversacion` y `proyectarMensaje` por audiencia; helpers `secuenciaQuery` y `ocultarAccesoEnLectura` (no_autorizado → no_encontrado en lectura). Se actualizó el doc del paquete (estaba desfasado: afirmaba que el envío no se montaba).
- `adapters/http/conversacion/lectura_integration_test.go` (nuevo): `TestListarMensajesHTTP` (historia por secuencia con `audiencia_autor` correcto; Cliente no ve `actor_real`/`rol_al_enviar`, Interno sí), `TestObtenerConversacionProyeccionHTTP` (Cliente sin campos internos, Interno con ellos; aislamiento por tenant y por alcance → 404), `TestMarcarAtendidaHTTP` (responsable prende el indicador; sin reclamo propio → 404; versión equivocada → 409; idempotente; `obtenerConversacion` refleja `atendida_en`).

**Contrato:** el pin de `go.mod` ya estaba en `v0.1.0-tracer.21` en la rama base `pf-msg-d` (commit `0721f3c`), que ya trae `obtenerConversacion`, `listarMensajes` y `marcarAtendida`; no requirió subirlo. Entre `tracer.21` y el `tracer.23` vigente, `mensajeria.yaml` no cambió funcionalmente (solo `bff.yaml`/`operacion.yaml` de otras historias), así que se mantiene el pin.

**Verificación:** `go build ./...`, `go vet ./...`, `gofmt -l .` (limpio) y `arch-test: OK` verdes. Suite completa (`go test ./...`) contra Postgres 17 real (DSN local): las tres pruebas nuevas PASS y la suite previa (envío, reclamo, transferencia, guardas de estado/reclamo, emergencia, consumidores) intacta.

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
