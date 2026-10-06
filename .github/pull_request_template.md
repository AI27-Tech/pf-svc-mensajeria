<!--
Plantilla de Pull Request del backend de AI27 (repos Go: pf-svc-* y pf-pkg-*).
Rellena las secciones que apliquen y borra las que no. El objetivo es que quien
revisa entienda el porqué del cambio y pueda verificarlo sin adivinar. La
autoridad de arquitectura vive en docs/ del workspace de producto (ADR,
decision-log, 00-indice del scaffold); no la reexpliques, enlázala.
-->

## Resumen

<!-- Qué hace este PR y por qué. Una o dos frases. -->

## Contexto

<!-- Enlaza el issue/spec, el ADR o la decisión (DEC-xxx) que gobierna el cambio. -->

- Issue / spec / prompt de scaffolding:
- ADR / decisión afectada (DEC-xxx):

## Tipo de cambio

- [ ] Feature
- [ ] Bugfix
- [ ] Refactor (sin cambio de comportamiento)
- [ ] Migración de base de datos
- [ ] Contrato (consumo de una nueva versión de pf-pkg-contracts)
- [ ] Documentación
- [ ] Tooling / build / CI
- [ ] Otro:

## Cambios

<!-- Lista concreta de lo que cambió. Agrupa por paquete o por capa si ayuda. -->

-

## Invariantes de arquitectura

<!--
Marca solo lo que este PR toca y explica cómo se mantiene. Ver 00-indice.md
(reglas 1-10) y los ADR 17/29. Si no aplica, marca la primera casilla.
-->

- [ ] No afecta ninguna invariante documentada.
- [ ] **I1 — un pool por rol de BD**, nunca compartido entre roles (ni dentro de `core`).
- [ ] **I2 — ninguna arista no declarada** en `tools/arch-test/reglas.yaml`; no se sustituye por una llamada directa "porque están en el mismo binario".
- [ ] **I3 — paridad de adaptadores** (en proceso y HTTP comparten presupuesto de tiempo, cancelación y taxonomía de error).
- [ ] **I4 — no se importa `internal/` de otro módulo**; solo su superficie pública (`New`/`Routes`/`Workers`/`Consultas`) y sus `ports.go`.
- [ ] **I5 — `cmd/api` arranca solo** y sirve `/health` y `/ready` sin `core`.
- [ ] **Contrato primero** — ningún tipo de wire, ruta ni evento escrito a mano; lo que falte en la versión fijada de `pf-pkg-contracts` se reporta, no se inventa.
- [ ] **Sin aristas salientes nuevas** fuera de las declaradas (y de la restringida a `identidad` para revocación, DEC-051/058).

## Decisiones abiertas / brechas

<!--
¿El PR topa con una decisión no cerrada o una brecha de contrato? Dilo aquí y
deja el TODO en su sitio en lugar de resolverlo en silencio. Lo que queda en
espera de ADR 32 (broker, IdP, orquestador) también va aquí.
-->

-

## Base de datos

<!-- Borra si el PR no toca el esquema. -->

- [ ] Las migraciones se aplican con el rol dueño del esquema; no agregan ningún `GRANT` fuera de ese rol.
- [ ] Los datos se versionan por vigencia donde corresponde; sin `UPDATE` que pise historia ni `DELETE` en firme (P8).

## Eventos / bus

<!-- Borra si el PR no toca eventos. -->

- [ ] El evento se escribe en la `outbox` en la **misma transacción** que el cambio de dominio (P17).
- [ ] El tipo/versión está en el registro del contrato; el consumidor es idempotente por `evento_id` (`inbox`).

## Cómo se probó

<!-- Comandos ejecutados y su resultado. No basta con que compile. -->

- [ ] `go build ./...`
- [ ] `go vet ./...`
- [ ] `go test ./...` (o `-race` donde aplique)
- [ ] `go run github.com/AI27-Tech/pf-pkg-platform-go/tools/arch-test@<version> .`
- [ ] `gofmt` limpio

Notas de la verificación:

## CHANGELOG

<!-- El repo exige registrar cada cambio. Ver .kiro/steering/changelog.md -->

- [ ] Agregué la entrada en `CHANGELOG.md` (fecha+hora, resumen, cambios y archivos modificados; la más reciente al inicio).

## Checklist

- [ ] Identificadores en inglés salvo sustantivos de dominio en español (`viaje`, `catalogos`, `operacion`, `bitacora`, `asignacion`, `custodio`, `Audiencia`, …); comentarios, README y commits en español (México).
- [ ] `go.mod` fija versiones **exactas**; sin `replace` a rutas locales ni `go.work` versionado.
- [ ] Ningún `pf-svc-*` importa otro `pf-svc-*` (excepto `pf-svc-core`).
- [ ] No commiteé binarios ni artefactos generados.
- [ ] Sin secretos, DSN ni URLs hardcodeadas.
