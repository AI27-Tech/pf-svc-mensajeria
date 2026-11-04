-- 0009_vinculo_ejecutivo_cliente.sql — vínculo pegajoso Ejecutivo↔cliente
-- (DEC-103 #2 y #3, DEC-128). Lo usa internal/vinculo.
--
--   1. dueno_cliente: el dueño vigente de cada empresa cliente. Es un hecho con
--      fecha: quién lo tomó (ejecutivo_id), desde cuándo (desde) y con qué
--      conversación (conversacion_id). Nace con el primer reclamo de una
--      conversación del cliente y cambia cuando otro Ejecutivo reclama una
--      conversación del cliente mientras el dueño no está disponible. Cada
--      cambio queda además en la auditoría del reclamo que lo produjo.
--   2. actividad_ejecutivo: la última actividad de cada Ejecutivo en mensajeria
--      (reclamar, enviar, marcar atendida, recibo). disponible_desde es el
--      inicio de la racha actual sin huecos mayores a la ventana N: la dirección
--      de una conversación sigue vigente solo si la racha del dueño empezó antes
--      de que la conversación naciera. Sin tenant: los internos no se acotan por
--      tenant (DEC-129).
--   3. ruta_conversacion.dirigida_a: el dueño al que iba dirigida la
--      conversación cuando nació, si estaba disponible. NULL = pool.
--
-- Aditivo e idempotente (IF NOT EXISTS). Grants mínimos como en 0002: SELECT,
-- INSERT y UPDATE (el dueño y la actividad avanzan), sin DELETE.

CREATE TABLE IF NOT EXISTS mensajeria.dueno_cliente (
  tenant_id        TEXT        NOT NULL,
  cliente_id       UUID        NOT NULL,
  ejecutivo_id     UUID        NOT NULL,
  conversacion_id  UUID        NOT NULL,   -- la conversación cuyo reclamo lo hizo dueño
  desde            TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, cliente_id)
);

CREATE TABLE IF NOT EXISTS mensajeria.actividad_ejecutivo (
  ejecutivo_id      UUID        PRIMARY KEY,
  ultima_en         TIMESTAMPTZ NOT NULL DEFAULT now(),
  disponible_desde  TIMESTAMPTZ NOT NULL DEFAULT now()
);

ALTER TABLE mensajeria.ruta_conversacion
  ADD COLUMN IF NOT EXISTS dirigida_a UUID;

GRANT SELECT, INSERT, UPDATE ON mensajeria.dueno_cliente TO svc_mensajeria;
GRANT SELECT, INSERT, UPDATE ON mensajeria.actividad_ejecutivo TO svc_mensajeria;
