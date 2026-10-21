-- 0003_ruta_revision.sql — revisión por fila para el catch-up de la colección
-- de bandeja (M3, ADR 28 §3; contrato tracer.14).
--
-- El catch-up incremental (`listarTrabajoBandeja?desde_revision=N`) necesita
-- responder "qué cambió en la colección después de N". Para eso, cada cambio de
-- la colección (alta, baja por desenrutar, reenrutamiento) avanza
-- bandeja.revision y estampa esa revisión en la fila de ruta_conversacion
-- afectada, en la MISMA transacción (mensajeria.md §237: cada mutación que
-- produce un delta actualiza su revisión). El catch-up es entonces
-- `SELECT ... WHERE bandeja_id = ? AND revision > N`.
--
-- Una BAJA no borra la fila: la deja activa = false con su revisión de cambio,
-- para que el catch-up la reporte como "salió de la colección". El índice único
-- parcial una_ruta_activa_por_conversacion (sobre activa) ya permite varias
-- filas inactivas y a lo más una activa por conversación.
--
-- Aditivo: columna nueva con DEFAULT 0, sin reescribir filas existentes de forma
-- incompatible. Hereda los grants de tabla de 0002 (SELECT/INSERT/UPDATE), no
-- requiere grant nuevo.

ALTER TABLE mensajeria.ruta_conversacion
  ADD COLUMN IF NOT EXISTS revision BIGINT NOT NULL DEFAULT 0;

-- Índice del camino caliente del catch-up: cambios de una bandeja posteriores a
-- una revisión, en orden de revisión.
CREATE INDEX IF NOT EXISTS ruta_bandeja_revision_idx
  ON mensajeria.ruta_conversacion (bandeja_id, revision);
