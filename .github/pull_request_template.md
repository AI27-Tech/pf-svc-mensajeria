<!--
Plantilla de Pull Request de portal-cliente (pf-web-frontend-client).
Rellena las secciones que apliquen y borra las que no. El objetivo es que
quien revisa entienda el porqué del cambio y pueda verificarlo sin adivinar.
-->

## Resumen

<!-- Qué hace este PR y por qué. Una o dos frases. -->

## Contexto

<!--
Enlaza el issue, la decisión abierta o el documento de arquitectura relevante.
La autoridad vive en docs/arquitectura/ del workspace padre; no la reexpliques,
enlázala.
-->

- Issue / spec relacionado:
- Decisión de arquitectura afectada (ADR / decisión abierta del README):

## Tipo de cambio

- [ ] Feature
- [ ] Bugfix
- [ ] Refactor (sin cambio de comportamiento)
- [ ] Estilos / UI
- [ ] Documentación
- [ ] Tooling / build / CI
- [ ] Otro:

## Cambios

<!-- Lista concreta de lo que cambió. Agrupa por dominio o por capa si ayuda. -->

-

## Invariantes del cliente

<!--
Marca solo lo que este PR toca. Si tocas alguna, explica cómo se mantiene.
Ver README > "Reglas del cliente, y cómo están hechas cumplir".
-->

- [ ] No afecta ninguna invariante documentada.
- [ ] **P4** — un solo parser de tracking para pull y push (`lib/tracking/parseTrackingPayload.ts`).
- [ ] **P5** — orden y dedupe por secuencia de dominio, no por el `seq` del sobre.
- [ ] **P10** — el navegador no selecciona canales arbitrarios; el filtrado por audiencia es del servidor.
- [ ] La ausencia por degradación no se colapsa a `null` (`Fresco<T>` conserva sus tres estados).
- [ ] El fetch se mantiene en la frontera de la ruta; las `*Section` siguen siendo presentacionales.

## Decisiones abiertas

<!--
¿Este PR toca alguna de las decisiones abiertas del README? ¿Introduce un supuesto
nuevo sobre el contrato del backend, la custodia del token, el stream, etc.?
Si es así, dilo aquí y deja el TODO en su sitio en lugar de resolverlo en silencio.
-->

-

## Cómo se probó

<!-- Comandos ejecutados y su resultado. No basta con que compile. -->

- [ ] `pnpm run typecheck`
- [ ] `pnpm run lint`
- [ ] `pnpm run format:check`
- [ ] `pnpm run test`
- [ ] `pnpm run test:e2e`
- [ ] Verificado a mano en `pnpm run dev:mock` (cliente@demo.ai27 / demo1234)

Notas de la verificación:

## Capturas / evidencia

<!-- Para cambios de UI: antes / después. Borra si no aplica. -->

## CHANGELOG

<!-- El repo exige registrar cada cambio relevante. Ver .kiro/steering/changelog.md -->

- [ ] Agregué la entrada correspondiente en `CHANGELOG.md` (resumen + archivos modificados, entrada más reciente al inicio).

## Checklist

- [ ] El cambio se limita a la audiencia cliente (nada de `panel-admin` / `Audiencia: Interno`).
- [ ] No introduje librerías prohibidas (zustand, redux, jotai, swr, framer-motion, next-auth, aws-amplify, Radix, MUI, Chakra).
- [ ] No importo `lib/api/generated/*` fuera de `src/lib/`.
- [ ] Comentarios y docstrings en español; sustantivos de dominio en su forma canónica (`viaje`, `bitacora`, `conversacion`, `Audiencia`, `Fresco`, `Sobre`).
- [ ] No commiteé artefactos generados (`src/routeTree.gen.ts`, `src/lib/api/generated/`).
