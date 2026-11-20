-- 0012_rol_sujeto.sql — rol vigente de cada sujeto, proyectado de los eventos de
-- identidad (rol.asignado, rol.revocado, sujeto.desactivado, sujeto.reactivado).
--
-- Lo usa la transferencia de una conversación `tipo=solicitud`: su destino debe
-- tener el rol `cotizacion` (DEC-148 #31; 409 `destino_no_es_cotizacion`).
-- mensajeria no llama a identidad en la ruta caliente, así que el rol del destino
-- se lee de esta proyección. Sin fila, o con otro rol, o desactivado: no es
-- destino válido (falla cerrado).
--
-- rol_en y activo_en son las marcas de tiempo de los eventos aplicados, y un
-- evento más viejo no pisa a uno más nuevo (reentregas y desorden convergen).
-- Aditiva e idempotente. Grants mínimos como en 0002: sin DELETE.

CREATE TABLE IF NOT EXISTS mensajeria.rol_sujeto (
  sujeto_id  TEXT        PRIMARY KEY,   -- identificador de identidad, no FK
  rol        TEXT,                      -- NULL = sin rol vigente
  rol_en     TIMESTAMPTZ,
  activo     BOOLEAN     NOT NULL DEFAULT true,
  activo_en  TIMESTAMPTZ
);

GRANT SELECT, INSERT, UPDATE ON mensajeria.rol_sujeto TO svc_mensajeria;
