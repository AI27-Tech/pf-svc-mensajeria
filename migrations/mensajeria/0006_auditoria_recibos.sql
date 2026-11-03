-- 0006_auditoria_recibos.sql — auditoría legible por contrato y cursor
-- "leído por AI27" de la conversación (E3-T6).
--
-- 1) auditoria_mensajeria.registro_id: el contrato expone RegistroAuditoria.id
--    como UUID ("nunca un correlativo expuesto", _comunes.yaml Id). El id
--    BIGSERIAL de 0001 se queda como clave interna y orden de paginación; el
--    UUID es lo que sale por listarAuditoriaConversacion.
-- 2) Índices de lectura de la auditoría (E3-T6): por conversación y por actor,
--    ambos en orden de ocurrencia.
-- 3) conversacion.leido_ai27_hasta: el "leído por AI27" visible al cliente, el
--    quinto de los seis estados de entrega. Solo lo avanza el recibo de lectura
--    del RESPONSABLE ACTIVO (P23). Se guarda aparte del cursor personal
--    (cursor_participante) porque no se puede derivar después: quien hoy es
--    responsable pudo leer antes como miembro no responsable, y esa vista
--    incidental no debe contar.
--
-- Aditivo e idempotente (IF NOT EXISTS). Hereda los grants de tabla de 0002:
-- auditoria_mensajeria sigue en SELECT, INSERT (sin UPDATE ni DELETE) y
-- conversacion en SELECT, INSERT, UPDATE. No requiere grant nuevo.

ALTER TABLE mensajeria.auditoria_mensajeria
  ADD COLUMN IF NOT EXISTS registro_id UUID NOT NULL DEFAULT gen_random_uuid();

CREATE UNIQUE INDEX IF NOT EXISTS auditoria_registro_id_uidx
  ON mensajeria.auditoria_mensajeria (registro_id);

CREATE INDEX IF NOT EXISTS auditoria_conversacion_ts_idx
  ON mensajeria.auditoria_mensajeria (conversacion_id, ocurrido_en);

CREATE INDEX IF NOT EXISTS auditoria_actor_ts_idx
  ON mensajeria.auditoria_mensajeria (actor_real, ocurrido_en);

ALTER TABLE mensajeria.conversacion
  ADD COLUMN IF NOT EXISTS leido_ai27_hasta BIGINT NOT NULL DEFAULT 0;
