-- 0008_bandeja_unica.sql — bandeja única de conversaciones (DEC-127, DEC-103 #1).
--
-- Desde DEC-127 cada conversación nueva nace enrutada a la bandeja única de su
-- tenant, en la misma transacción que la crea (conversacion.CrearPorEvento). Las
-- conversaciones creadas antes quedaron sin ruta y no aparecían en ninguna
-- bandeja. Esta migración las enruta:
--
--   1. Crea la bandeja de chat del tenant si no tiene ninguna y tiene
--      conversaciones sin ruta activa.
--   2. Enruta cada conversación sin ruta activa a la bandeja de chat MÁS ANTIGUA
--      de su tenant (el mismo criterio que bandejas.ResolverMiBandeja), con una
--      revisión nueva por ruta, y avanza bandeja.revision a la última, para que
--      el catch-up las reporte como altas (0003_ruta_revision).
--
-- No toca las rutas activas existentes ni pool_membresia: la tabla se conserva,
-- aunque ya no es la fuente de la membresía de chat (DEC-127 #3). La bandeja de
-- Reacción (tipo 'emergencias') no cambia.
--
-- Idempotente: una segunda pasada no encuentra conversaciones sin ruta. Sin
-- grants nuevos (bandeja y ruta_conversacion ya tienen INSERT/UPDATE en 0002).

INSERT INTO mensajeria.bandeja (tenant_id, tipo, nombre)
SELECT DISTINCT c.tenant_id, 'chat', 'Conversaciones'
  FROM mensajeria.conversacion c
 WHERE NOT EXISTS (SELECT 1 FROM mensajeria.ruta_conversacion r
                    WHERE r.conversacion_id = c.id AND r.activa)
   AND NOT EXISTS (SELECT 1 FROM mensajeria.bandeja b
                    WHERE b.tenant_id = c.tenant_id AND b.tipo = 'chat');

WITH destino AS (
  SELECT DISTINCT ON (tenant_id) id, tenant_id, revision
    FROM mensajeria.bandeja
   WHERE tipo = 'chat'
   ORDER BY tenant_id, creado_en, id
), pendientes AS (
  SELECT c.id AS conversacion_id, c.tenant_id, d.id AS bandeja_id,
         d.revision + row_number() OVER (PARTITION BY d.id ORDER BY c.creado_en, c.id) AS revision
    FROM mensajeria.conversacion c
    JOIN destino d ON d.tenant_id = c.tenant_id
   WHERE NOT EXISTS (SELECT 1 FROM mensajeria.ruta_conversacion r
                      WHERE r.conversacion_id = c.id AND r.activa)
), rutas AS (
  INSERT INTO mensajeria.ruta_conversacion (conversacion_id, tenant_id, bandeja_id, revision)
  SELECT conversacion_id, tenant_id, bandeja_id, revision FROM pendientes
  RETURNING bandeja_id, revision
)
UPDATE mensajeria.bandeja b
   SET revision = m.revision
  FROM (SELECT bandeja_id, max(revision) AS revision FROM rutas GROUP BY bandeja_id) m
 WHERE b.id = m.bandeja_id;
