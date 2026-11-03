-- 0007_reclamo_version_por_conversacion.sql — índice para la versión del
-- reclamo por conversación (P20).
--
-- La versión del reclamo es monotónica POR CONVERSACIÓN (contrato
-- VersionReclamo): un reclamo nuevo, desde el pool o por transferencia, abre en
-- la versión más alta de la conversación + 1. La bandeja y el bootstrap leen esa
-- versión cuando la conversación no tiene reclamo activo. Este índice sirve ese
-- max(version) sin recorrer la tabla; el índice parcial de 0001 solo cubre el
-- reclamo activo.
--
-- No es UNIQUE a propósito: antes de esta corrección Transferir reabría en
-- versión 1, así que puede haber versiones repetidas entre reclamos cerrados de
-- una conversación.
--
-- Aditivo e idempotente (IF NOT EXISTS). Sin grants nuevos.

CREATE INDEX IF NOT EXISTS reclamo_version_por_conversacion
  ON mensajeria.reclamo (conversacion_id, version);
