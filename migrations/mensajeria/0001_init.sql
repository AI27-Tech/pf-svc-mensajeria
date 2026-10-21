-- 0001_init.sql — esquema, rol y tablas iniciales de mensajeria.
-- Un solo esquema `mensajeria` y un solo rol `svc_mensajeria` con USAGE
-- ÚNICAMENTE sobre él (00 regla 4, ADR 18 §1; mensajeria.md (c), (e)). NINGÚN
-- GRANT hacia esquemas de operacion, identidad, catalogos ni realtime. Sin FK
-- hacia otro esquema (SPEC §A.5 R1, I1): viaje_id, usuario_id, cliente_id y la
-- clave de object storage son solo IDENTIFICADORES. mensajeria es el sistema de
-- registro durable COMPLETO de la conversación (mensajeria.md (c), (e)).
--
-- Fase de estructura; los GRANT de privilegio mínimo por tabla van en 0002,
-- igual que en catalogos/operacion/reportes.
--
-- Dos pools del mismo rol svc_mensajeria (chat y despacho) comparten estas
-- tablas; la separación es de PRESUPUESTO de conexiones, no de esquema
-- (mensajeria.md (d), (k)). Ningún pool alcanza otro esquema (I1).

CREATE SCHEMA IF NOT EXISTS mensajeria;

-- Rol dueño del esquema. LOGIN para que el pool se conecte con él.
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'svc_mensajeria') THEN
    CREATE ROLE svc_mensajeria LOGIN;
  END IF;
END
$$;

GRANT USAGE ON SCHEMA mensajeria TO svc_mensajeria;
-- Los privilegios por tabla se otorgan UNO A UNO en 0002_grants.sql (privilegio
-- mínimo: mensaje/adjunto/intento_entrega/inbox/auditoria son append-only, sin
-- DELETE, DEC-030/041). NO se usa ALTER DEFAULT PRIVILEGES con DELETE amplio:
-- anularía ese mínimo y dejaría los mensajes borrables (SPEC §A.5 R1).

-- ===========================================================================
-- conversacion — la conversación de negocio entre el cliente y AI27 (una por
-- viaje y tenant). Ciclo de vida (estado) SEPARADO del estado operativo de cola
-- (en ruta_conversacion): dos columnas, no una (mensajeria.md (e)).
-- siguiente_secuencia es el contador de la secuencia canónica de mensajes de
-- ESTA conversación; se incrementa bajo el bloqueo de fila en la tx del envío
-- (MSG-REC §7.2). No es una secuencia global.
-- ===========================================================================
CREATE TABLE IF NOT EXISTS mensajeria.conversacion (
  id                   UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id            TEXT        NOT NULL,
  viaje_id            UUID        NOT NULL,   -- identificador, no FK
  cliente_id          UUID,                   -- alcance de la conversación (DEC-045 #8)
  estado               TEXT        NOT NULL DEFAULT 'abierta',  -- ciclo de vida
  siguiente_secuencia  BIGINT      NOT NULL DEFAULT 0,          -- orden canónico de mensaje
  escritura_hasta      TIMESTAMPTZ,            -- tras esto, solo lectura (DEC-030/041)
  creado_en            TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- Invariante: una conversacion ACTIVA por (tenant_id, viaje_id) (Addendum §6).
CREATE UNIQUE INDEX IF NOT EXISTS una_conversacion_activa_por_viaje
  ON mensajeria.conversacion (tenant_id, viaje_id)
  WHERE estado <> 'cerrada';

-- ===========================================================================
-- bandeja — bandeja compartida atendida por un pool (chat / emergencias). La
-- revisión monotónica de la COLECCIÓN (RevisionBandeja) avanza con cada cambio
-- de la bandeja como colección (ADR 28).
-- ===========================================================================
CREATE TABLE IF NOT EXISTS mensajeria.bandeja (
  id         UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id  TEXT        NOT NULL,
  tipo       TEXT        NOT NULL,            -- p. ej. 'chat' (emergencias espera M9)
  nombre     TEXT        NOT NULL,
  revision   BIGINT      NOT NULL DEFAULT 0,  -- RevisionBandeja (colección)
  creado_en  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ===========================================================================
-- pool_membresia — membresía efectiva y versionada del pool de Ejecutivos que
-- atiende una bandeja (DEC-046). Proyectada desde eventos de identidad; perder
-- el reclamo NO es perder la membresía (Addendum §4 puntos 7 y 8).
-- STUB de forma: el cálculo depende del vínculo Ejecutivo↔cliente (pendiente de
-- Producto); la tabla existe para fijar la forma, no para derivar reglas.
-- ===========================================================================
CREATE TABLE IF NOT EXISTS mensajeria.pool_membresia (
  bandeja_id   UUID        NOT NULL,
  usuario_id   UUID        NOT NULL,          -- identificador de identidad, no FK
  tenant_id    TEXT        NOT NULL,
  rol          TEXT        NOT NULL,
  version      BIGINT      NOT NULL DEFAULT 1,
  activa       BOOLEAN     NOT NULL DEFAULT true,
  actualizado_en TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (bandeja_id, usuario_id)
);

-- ===========================================================================
-- ruta_conversacion — enrutamiento de una conversación a una bandeja/pool. El
-- estado OPERATIVO de cola vive aquí, separado del ciclo de vida de
-- conversacion.estado (mensajeria.md (e)).
-- ===========================================================================
CREATE TABLE IF NOT EXISTS mensajeria.ruta_conversacion (
  id               UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  conversacion_id  UUID        NOT NULL,
  tenant_id        TEXT        NOT NULL,
  bandeja_id       UUID        NOT NULL,
  activa           BOOLEAN     NOT NULL DEFAULT true,
  creado_en        TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- Invariante: una ruta ACTIVA por conversacion (Addendum §6).
CREATE UNIQUE INDEX IF NOT EXISTS una_ruta_activa_por_conversacion
  ON mensajeria.ruta_conversacion (conversacion_id)
  WHERE activa;

-- ===========================================================================
-- reclamo — el reclamo que autoriza a responder. A lo más uno ACTIVO por
-- conversacion; concurrencia optimista por `version` (VersionReclamo). El índice
-- único parcial produce el ganador y el conflicto de versión (Addendum §4
-- punto 5). cerrado_en NULL = activo.
-- ===========================================================================
CREATE TABLE IF NOT EXISTS mensajeria.reclamo (
  id               UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  conversacion_id  UUID        NOT NULL,
  tenant_id        TEXT        NOT NULL,
  actor_real       UUID        NOT NULL,       -- quién tiene la autoridad de envío
  version          BIGINT      NOT NULL DEFAULT 1,  -- VersionReclamo
  abierto_en       TIMESTAMPTZ NOT NULL DEFAULT now(),
  cerrado_en       TIMESTAMPTZ
);
-- Invariante: a lo más un reclamo ACTIVO por conversacion (Addendum §6).
CREATE UNIQUE INDEX IF NOT EXISTS un_reclamo_activo_por_conversacion
  ON mensajeria.reclamo (conversacion_id)
  WHERE cerrado_en IS NULL;

-- ===========================================================================
-- mensaje — el mensaje con su secuencia canónica por conversación. actor_real,
-- rol_al_enviar y alias_mostrado son columnas SEPARADAS e inmutables
-- (mensajeria.md (e)). Idempotencia durable por (conversacion_id, actor_real,
-- client_message_id). Sin edición ni borrado en firme en V1 (DEC-030/041).
-- ===========================================================================
CREATE TABLE IF NOT EXISTS mensajeria.mensaje (
  id                 UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  conversacion_id    UUID        NOT NULL,
  tenant_id          TEXT        NOT NULL,
  secuencia          BIGINT      NOT NULL,     -- SecuenciaMensaje (por conversacion)
  actor_real         UUID        NOT NULL,     -- quién lo envió de verdad
  rol_al_enviar      TEXT,                     -- rol en el momento del envío (inmutable)
  alias_mostrado     TEXT,                     -- lo que ve el cliente (inmutable)
  client_message_id  UUID        NOT NULL,     -- clave de idempotencia del cliente
  cuerpo             TEXT        NOT NULL,
  creado_en          TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- Invariante: idempotencia durable del envío (MSG-REC §7.1).
CREATE UNIQUE INDEX IF NOT EXISTS un_mensaje_por_clave_de_cliente
  ON mensajeria.mensaje (conversacion_id, actor_real, client_message_id);
-- Orden de lectura por conversación y secuencia (antes_de/despues_de del contrato).
CREATE INDEX IF NOT EXISTS mensaje_conversacion_secuencia_idx
  ON mensajeria.mensaje (conversacion_id, secuencia);

-- ===========================================================================
-- cursor_participante — hasta qué secuencia leyó/recibió cada participante de
-- una conversación (recibos; mensajeria.md (b) fanout). La política de
-- visibilidad de recibos (Addendum §8) está pendiente de contrato: tabla por
-- forma.
-- ===========================================================================
CREATE TABLE IF NOT EXISTS mensajeria.cursor_participante (
  conversacion_id   UUID        NOT NULL,
  participante_id   UUID        NOT NULL,
  tenant_id         TEXT        NOT NULL,
  leido_hasta       BIGINT      NOT NULL DEFAULT 0,  -- SecuenciaMensaje
  entregado_hasta   BIGINT      NOT NULL DEFAULT 0,  -- SecuenciaMensaje
  actualizado_en    TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (conversacion_id, participante_id)
);

-- ===========================================================================
-- estado_atendido — marca de "atendida" de una conversación, separada del ciclo
-- de vida y del reclamo (mensajeria.md (e)).
-- ===========================================================================
CREATE TABLE IF NOT EXISTS mensajeria.estado_atendido (
  conversacion_id  UUID        PRIMARY KEY,
  tenant_id        TEXT        NOT NULL,
  atendida         BOOLEAN     NOT NULL DEFAULT false,
  atendida_por     UUID,
  atendida_en      TIMESTAMPTZ
);

-- ===========================================================================
-- adjunto — metadatos del adjunto; los BYTES NO pasan por el servicio
-- (mensajeria.md (d)): van a object storage con API S3, alcanzado por tenant.
-- Sin límites de tamaño/tipo inventados (MSG-REC §13 pendiente de Producto).
-- ===========================================================================
CREATE TABLE IF NOT EXISTS mensajeria.adjunto (
  id               UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  conversacion_id  UUID        NOT NULL,
  tenant_id        TEXT        NOT NULL,
  clave_objeto     TEXT        NOT NULL,       -- clave en object storage (por tenant)
  content_type     TEXT,
  tamano_bytes     BIGINT,
  creado_en        TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ===========================================================================
-- plantilla — plantillas de mensaje (operacion de plantillas del contrato). Por
-- forma; el contenido lo administra el dominio.
-- ===========================================================================
CREATE TABLE IF NOT EXISTS mensajeria.plantilla (
  id         UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id  TEXT        NOT NULL,
  clave      TEXT        NOT NULL,
  cuerpo     TEXT        NOT NULL,
  creado_en  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ===========================================================================
-- trabajo_notificacion — cola de la MAMPARA de despacho externo (mensajeria.md
-- (d), (k)). Presupuesto, pool y cortacircuitos propios (en internal/despacho).
-- En V1 SIN worker ni destinatario cliente (DEC-023): la tabla es por FORMA; su
-- configuración (subconjunto de guardia/escalamiento, ventanas) está bloqueada
-- en configuración, no en forma (mensajeria.md (l)).
-- ===========================================================================
CREATE TABLE IF NOT EXISTS mensajeria.trabajo_notificacion (
  id             UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id      TEXT        NOT NULL,
  destinatario_id UUID       NOT NULL,         -- interno (guardia/escalamiento); nunca cliente
  estado         TEXT        NOT NULL DEFAULT 'pendiente',
  intentos       INT         NOT NULL DEFAULT 0,
  payload        JSONB       NOT NULL,
  creado_en      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ===========================================================================
-- intento_entrega — bitácora de intentos de un trabajo_notificacion (reintentos
-- de la mampara). Append-only.
-- ===========================================================================
CREATE TABLE IF NOT EXISTS mensajeria.intento_entrega (
  id            BIGSERIAL   PRIMARY KEY,
  trabajo_id    UUID        NOT NULL,
  tenant_id     TEXT        NOT NULL,
  resultado     TEXT        NOT NULL,          -- 'exitosa' | 'fallida'
  detalle       TEXT,
  intentado_en  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ===========================================================================
-- auditoria_mensajeria — hechos auditables INMUTABLES (Addendum §11): reclamo,
-- liberación, transferencia, override. Append-only.
-- ===========================================================================
CREATE TABLE IF NOT EXISTS mensajeria.auditoria_mensajeria (
  id               BIGSERIAL   PRIMARY KEY,
  tenant_id        TEXT        NOT NULL,
  conversacion_id  UUID,
  actor_real       UUID        NOT NULL,
  accion           TEXT        NOT NULL,
  detalle          JSONB,
  ocurrido_en      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ===========================================================================
-- outbox — publicación transaccional de eventos (ADR 29; P17). La estructura y
-- el NOMBRE los fija pf-pkg-platform-go/outbox: OutboxDDL y el Relay leen una
-- tabla llamada `outbox` (sin esquema, resuelta por search_path al esquema del
-- módulo). El prompt la lista como `outbox_evento`; se usa `outbox` para que el
-- Relay compartido funcione sin forkearlo (discrepancia de nombre reportada en
-- el README). Refleja la capa de DOMINIO del sobre (contracts/events); la capa
-- viva restringida (SobreVivo) la agrega el productor del tema, no el outbox.
-- ===========================================================================
CREATE TABLE IF NOT EXISTS mensajeria.outbox (
  id            BIGSERIAL   PRIMARY KEY,
  evento_id     UUID        NOT NULL UNIQUE,
  tipo          TEXT        NOT NULL,
  version       INT         NOT NULL DEFAULT 1,
  agregado_id   TEXT        NOT NULL,          -- clave de orden (ADR 29); no global
  sensibilidad  TEXT        NOT NULL DEFAULT 'amplia',
  ocurrido_en   TIMESTAMPTZ NOT NULL,
  payload       JSONB       NOT NULL,
  trace_context JSONB       NOT NULL DEFAULT '{}'::jsonb,
  publicado     BOOLEAN     NOT NULL DEFAULT false,
  publicado_en  TIMESTAMPTZ,
  creado_en     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS outbox_pendientes_idx
  ON mensajeria.outbox (creado_en) WHERE publicado = false;

-- ===========================================================================
-- inbox_evento — deduplicación idempotente del consumidor por evento_id del
-- Sobre (P13; refleja inbox.InboxDDL de pf-pkg-platform-go). La inserción aquí y
-- el efecto se confirman en la MISMA transacción; el acuse al broker ocurre
-- después (al menos una vez). Un consumidor por suscripción durable contratada.
-- ===========================================================================
CREATE TABLE IF NOT EXISTS mensajeria.inbox_evento (
  evento_id    UUID        PRIMARY KEY,
  tipo         TEXT        NOT NULL,
  consumidor   TEXT        NOT NULL,
  procesado_en TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ===========================================================================
-- AUSENTE A PROPÓSITO — ESPERA tracer.9 (DEC-089):
--   * emergencia (M9): sin tablas; la familia versionada de emergencia no está
--     en el contrato (internal/emergencias es stub). No se crea aquí.
--   * catch-up de bandeja (M3): sin rutas; no requiere tabla nueva.
-- ===========================================================================
