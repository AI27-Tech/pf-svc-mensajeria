-- 0004_emergencia.sql — el caso de emergencia V1 como caso de mensajeria en la
-- bandeja de Reacción (ADR 21, DEC-027/028/034/099; contrato tracer.17).
--
-- La emergencia es un CASO PROPIO vinculado al viaje, no una conversación ni una
-- incidencia (ADR 21 §1). Reutiliza la mecánica de bandeja/pool/reclamo del
-- esquema (bandeja.tipo = 'emergencias', pool_membresia, el mismo patrón de
-- concurrencia optimista del reclamo), pero con sus propias tablas: su agregado
-- es el caso (emergencia_id), no la conversación. No se generaliza la tabla
-- reclamo de conversación (DEC-100 punto 2): se duplica su forma aquí contra
-- emergencia_id, para no tocar el camino caliente ya probado de conversación.
--
-- Notas UNIDIRECCIONALES y append-only (ADR 21 §4): el cliente/operador agregan
-- notas hacia Reacción; no hay conversación bidireccional. Cada nota se copia al
-- histórico del viaje por el evento emergencia.nota_agregada (DEC-100 punto 1),
-- producido por el outbox en la misma tx.
--
-- Estructura + grants en este mismo archivo (hay tablas nuevas). Mismo criterio
-- que 0002: SELECT sobre lo suyo; INSERT donde escribe; UPDATE solo donde un
-- registro avanza; DELETE en NINGUNA (notas y auditoría son append-only).
-- Idempotente. Ningún GRANT fuera de svc_mensajeria ni hacia otro esquema (I1).

-- ===========================================================================
-- emergencia — el caso. Ciclo abierta → en_atencion → resuelta → cerrada
-- (ADR 21 §5); resuelta y cerrada son irreversibles. Idempotencia durable de la
-- activación por (tenant_id, viaje_id, activacion_id): reentregar la misma
-- activación devuelve el mismo caso; una pulsación nueva (otro activacion_id)
-- crea otro caso aunque ya exista uno abierto (ADR 21 §2). origen y actor_real
-- se registran siempre; solo se entregan a audiencia Interno (ADR 21 §3, §6).
-- ===========================================================================
CREATE TABLE IF NOT EXISTS mensajeria.emergencia (
  id             UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id      TEXT        NOT NULL,
  viaje_id       UUID        NOT NULL,   -- identificador, no FK
  cliente_id     UUID        NOT NULL,   -- alcance de la vista mínima del cliente
  bandeja_id     UUID        NOT NULL,   -- bandeja de Reacción (tipo 'emergencias')
  estado         TEXT        NOT NULL DEFAULT 'abierta',
  origen         TEXT        NOT NULL,   -- 'cliente' | 'operacion'
  actor_real     UUID        NOT NULL,   -- quién activó
  activacion_id  UUID        NOT NULL,   -- clave de idempotencia de la activación
  version_reclamo BIGINT     NOT NULL DEFAULT 0,  -- VersionReclamo del caso
  activada_en    TIMESTAMPTZ NOT NULL DEFAULT now(),
  resuelta_en    TIMESTAMPTZ,
  cerrada_en     TIMESTAMPTZ
);
-- Invariante: idempotencia durable de la activación (ADR 21 §2). Reentregar la
-- misma activacion_id no crea un segundo caso; es el árbitro, como el índice de
-- idempotencia del mensaje.
CREATE UNIQUE INDEX IF NOT EXISTS una_emergencia_por_activacion
  ON mensajeria.emergencia (tenant_id, viaje_id, activacion_id);
-- Lectura de los casos de un viaje por audiencia (listarEmergencias).
CREATE INDEX IF NOT EXISTS emergencia_viaje_idx
  ON mensajeria.emergencia (tenant_id, viaje_id);
-- Casos de una bandeja de Reacción, para la cola de atención.
CREATE INDEX IF NOT EXISTS emergencia_bandeja_idx
  ON mensajeria.emergencia (bandeja_id, tenant_id);

-- ===========================================================================
-- nota_emergencia — notas append-only del caso, unidireccionales hacia Reacción
-- (ADR 21 §4). audiencia_autor y actor_real separados e inmutables; es_resolucion
-- marca la nota que acompañó la resolución (obligatoria, ADR 21 §5). Sin edición
-- ni borrado (append-only, como mensaje).
-- ===========================================================================
CREATE TABLE IF NOT EXISTS mensajeria.nota_emergencia (
  id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  emergencia_id   UUID        NOT NULL,
  tenant_id       TEXT        NOT NULL,
  audiencia_autor TEXT        NOT NULL,   -- 'Cliente' | 'Interno' (DEC-054)
  actor_real      UUID        NOT NULL,
  cuerpo          TEXT        NOT NULL,
  es_resolucion   BOOLEAN     NOT NULL DEFAULT false,
  creado_en       TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS nota_emergencia_caso_idx
  ON mensajeria.nota_emergencia (emergencia_id, creado_en);

-- ===========================================================================
-- reclamo_emergencia — el reclamo que autoriza a atender el caso. A lo más uno
-- ACTIVO por emergencia; concurrencia optimista por `version`, misma forma que
-- mensajeria.reclamo pero con agregado emergencia_id (DEC-100 punto 2, no se
-- generaliza la tabla de conversación). cerrado_en NULL = activo.
-- ===========================================================================
CREATE TABLE IF NOT EXISTS mensajeria.reclamo_emergencia (
  id             UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  emergencia_id  UUID        NOT NULL,
  tenant_id      TEXT        NOT NULL,
  actor_real     UUID        NOT NULL,       -- responsable de Reacción
  version        BIGINT      NOT NULL DEFAULT 1,  -- VersionReclamo
  abierto_en     TIMESTAMPTZ NOT NULL DEFAULT now(),
  cerrado_en     TIMESTAMPTZ
);
-- Invariante: a lo más un reclamo ACTIVO por emergencia.
CREATE UNIQUE INDEX IF NOT EXISTS un_reclamo_emergencia_activo
  ON mensajeria.reclamo_emergencia (emergencia_id)
  WHERE cerrado_en IS NULL;

-- --- grants de privilegio mínimo (criterio de 0002) ---------------------------
-- emergencia: se crea; estado/version_reclamo/resuelta_en/cerrada_en avanzan.
GRANT SELECT, INSERT, UPDATE ON mensajeria.emergencia TO svc_mensajeria;
-- nota_emergencia: APPEND-ONLY. Sin UPDATE ni DELETE (ADR 21 §4).
GRANT SELECT, INSERT ON mensajeria.nota_emergencia TO svc_mensajeria;
-- reclamo_emergencia: se abre, su version avanza y se cierra (cerrado_en).
GRANT SELECT, INSERT, UPDATE ON mensajeria.reclamo_emergencia TO svc_mensajeria;
