# CHANGELOG

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
