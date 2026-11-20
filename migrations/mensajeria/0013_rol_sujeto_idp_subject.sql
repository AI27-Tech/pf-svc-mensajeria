-- 0013_rol_sujeto_idp_subject.sql — `idp_subject` (el `sub` del token del IdP) en
-- rol_sujeto. El reclamo guarda el `sub`, no el sujeto_id; la transferencia de una
-- solicitud valida su destino buscando por esta columna (DEC-148 #31, tracer.46).
--
-- Backfill: sin script. NULL = aún no se conoce y el destino falla cerrado. La
-- columna se llena sola cuando llega cualquier evento de identidad del sujeto
-- (rol.asignado, rol.revocado, sujeto.desactivado, sujeto.reactivado) con
-- idp_subject; los eventos ya publicados lo traen vacío y no lo borran.
-- Aditiva e idempotente. Sin cambio de grants (0012 ya da INSERT/UPDATE).

ALTER TABLE mensajeria.rol_sujeto ADD COLUMN IF NOT EXISTS idp_subject TEXT;
CREATE INDEX IF NOT EXISTS rol_sujeto_idp_subject_idx ON mensajeria.rol_sujeto (idp_subject);
