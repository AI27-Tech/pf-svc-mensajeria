-- 0010_alcance.sql — proyección local de `alcance.cambiado` (DEC-149 #20 y
-- #22, tracer.42): los clientes efectivos de cada sujeto según identidad ("mis
-- clientes" más los de sus subordinados). Con ella mensajeria acota las
-- emergencias del viaje de monitorista, supervisor y coordinador por el
-- cliente del viaje (DEC-149 #6, DEC-053 #3). Lo usa internal/alcance.
--
-- Vive en Postgres y no en memoria porque el consumidor es durable y comparte
-- grupo entre réplicas: cada evento llega a UNA réplica.
--
-- Cada fila es el conjunto COMPLETO (snapshot, no delta); vacío = el sujeto no
-- opera sobre ninguna emergencia por alcance de monitoreo. cambiado_en ordena:
-- un alcance más viejo que el guardado no gana. Sin DELETE: quitar a un sujeto
-- es dejar su conjunto vacío, conservando cambiado_en para que un evento
-- atrasado no lo reviva.
--
-- La llave es idp_subject, el `sub` del token del sujeto (DEC-149 #22): la
-- guarda busca el alcance de quien hace la petición con su claims.Subject,
-- porque el `sub` es opaco y distinto del sujeto_id (DEC-072). TEXT porque el
-- subject del IdP no es un UUID.
--
-- Aditivo e idempotente (IF NOT EXISTS). Grants mínimos como en 0002.

CREATE TABLE IF NOT EXISTS mensajeria.alcance_sujeto (
  idp_subject  TEXT        PRIMARY KEY,
  clientes_ids UUID[]      NOT NULL DEFAULT '{}',
  cambiado_en  TIMESTAMPTZ NOT NULL
);

GRANT SELECT, INSERT, UPDATE ON mensajeria.alcance_sujeto TO svc_mensajeria;
