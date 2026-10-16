# CHANGELOG

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
