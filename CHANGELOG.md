# CHANGELOG

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
