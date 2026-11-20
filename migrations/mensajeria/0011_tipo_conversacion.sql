-- 0011_tipo_conversacion.sql — tipo de la conversación: `viaje` o `solicitud`
-- (contrato tracer.45, DEC-148 #3, #29).
--
-- El viaje hereda el id de la solicitud, así que ahora pueden existir DOS
-- conversaciones con el mismo (tenant_id, viaje_id): la de la solicitud
-- (cliente ↔ pool de `cotizacion`) y la del viaje (cliente ↔ `ejecutivo`). Las
-- distingue `tipo`. Las existentes son de viaje.
--
--   1. conversacion.tipo, con CHECK; las filas previas valen 'viaje'.
--   2. conversacion.ventana_aplicada_en: cambiado_en del último
--      solicitud.estado_cambiado aplicado a la ventana. Una reentrega fuera de
--      orden (más vieja) no la pisa.
--   3. La unicidad "una conversación activa" pasa a ser por (tenant, viaje, tipo).
--
-- Idempotente. cmd/migrate reaplica 0001 en cada arranque, y 0001 hace
-- CREATE UNIQUE INDEX IF NOT EXISTS sobre el nombre del índice viejo. Si esta
-- migración le cambiara el nombre, 0001 lo recrearía con la definición vieja y
-- fallaría en cuanto hubiera dos conversaciones del mismo viaje_id. Por eso el
-- índice CONSERVA su nombre y solo cambia de definición: el IF NOT EXISTS de 0001
-- queda como no-op.

ALTER TABLE mensajeria.conversacion
  ADD COLUMN IF NOT EXISTS tipo TEXT NOT NULL DEFAULT 'viaje';

ALTER TABLE mensajeria.conversacion
  ADD COLUMN IF NOT EXISTS ventana_aplicada_en TIMESTAMPTZ;

DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint
                  WHERE conname = 'conversacion_tipo_valido'
                    AND conrelid = 'mensajeria.conversacion'::regclass) THEN
    ALTER TABLE mensajeria.conversacion
      ADD CONSTRAINT conversacion_tipo_valido CHECK (tipo IN ('viaje', 'solicitud'));
  END IF;

  IF EXISTS (SELECT 1 FROM pg_indexes
              WHERE schemaname = 'mensajeria'
                AND indexname = 'una_conversacion_activa_por_viaje'
                AND indexdef NOT LIKE '%tipo%') THEN
    DROP INDEX mensajeria.una_conversacion_activa_por_viaje;
  END IF;
END
$$;

CREATE UNIQUE INDEX IF NOT EXISTS una_conversacion_activa_por_viaje
  ON mensajeria.conversacion (tenant_id, viaje_id, tipo)
  WHERE estado <> 'cerrada';
