-- 0002_grants.sql — privilegio mínimo por tabla del esquema mensajeria.
-- Fase de grants, separada de la de estructura (0001), igual que en
-- catalogos/operacion/reportes. ningún GRANT hacia este esquema fuera de
-- svc_mensajeria y ningún GRANT de svc_mensajeria sobre otro esquema (SPEC §A.5
-- R1, ADR 18, I1). Idempotente; la aplica el mismo rol que aplicó 0001.
--
-- Por qué existe: 0001 deja un ALTER DEFAULT PRIVILEGES genérico (SELECT/INSERT/
-- UPDATE/DELETE sobre toda tabla, sin privilegio sobre secuencias). Sin este
-- 0002, svc_mensajeria podría borrar mensajes y auditoría (contradice "sin
-- edición ni borrado en firme de mensajes en V1", DEC-030/041, y la auditoría
-- inmutable del Addendum §11) y no podría insertar en las tablas BIGSERIAL.
--
-- Criterio: SELECT sobre todo lo suyo; INSERT donde escribe; UPDATE solo donde
-- un registro avanza (conversacion.siguiente_secuencia/estado, reclamo.version/
-- cerrado_en, ruta/membresia/atendido/cursor/trabajo). DELETE en NINGUNA tabla:
-- mensaje, outbox, inbox, auditoría e intentos son append-only o permanentes.

-- --- Partir de cero: retirar lo genérico de 0001 ------------------------------
REVOKE ALL ON ALL TABLES    IN SCHEMA mensajeria FROM svc_mensajeria;
REVOKE ALL ON ALL SEQUENCES IN SCHEMA mensajeria FROM svc_mensajeria;
ALTER DEFAULT PRIVILEGES IN SCHEMA mensajeria
  REVOKE ALL ON TABLES FROM svc_mensajeria;

-- --- conversacion: se crea, avanza su secuencia/estado; no se borra -----------
GRANT SELECT, INSERT, UPDATE ON mensajeria.conversacion TO svc_mensajeria;

-- --- bandeja: se crea y su revisión avanza ------------------------------------
GRANT SELECT, INSERT, UPDATE ON mensajeria.bandeja TO svc_mensajeria;

-- --- pool_membresia: se proyecta y cambia (activa/version) --------------------
GRANT SELECT, INSERT, UPDATE ON mensajeria.pool_membresia TO svc_mensajeria;

-- --- ruta_conversacion: se abre y se desactiva (transferencia) ----------------
GRANT SELECT, INSERT, UPDATE ON mensajeria.ruta_conversacion TO svc_mensajeria;

-- --- reclamo: se abre, su version avanza y se cierra (cerrado_en) -------------
GRANT SELECT, INSERT, UPDATE ON mensajeria.reclamo TO svc_mensajeria;

-- --- mensaje: APPEND-ONLY. Sin UPDATE ni DELETE (DEC-030/041) -----------------
GRANT SELECT, INSERT ON mensajeria.mensaje TO svc_mensajeria;

-- --- cursor_participante: avanza con recibos ----------------------------------
GRANT SELECT, INSERT, UPDATE ON mensajeria.cursor_participante TO svc_mensajeria;

-- --- estado_atendido: se fija y cambia ----------------------------------------
GRANT SELECT, INSERT, UPDATE ON mensajeria.estado_atendido TO svc_mensajeria;

-- --- adjunto: metadatos; se crea; no se borra en firme ------------------------
GRANT SELECT, INSERT ON mensajeria.adjunto TO svc_mensajeria;

-- --- plantilla: administrable -------------------------------------------------
GRANT SELECT, INSERT, UPDATE ON mensajeria.plantilla TO svc_mensajeria;

-- --- trabajo_notificacion: la mampara encola y avanza estado/intentos ---------
GRANT SELECT, INSERT, UPDATE ON mensajeria.trabajo_notificacion TO svc_mensajeria;

-- --- intento_entrega: APPEND-ONLY (bitácora) ----------------------------------
GRANT SELECT, INSERT ON mensajeria.intento_entrega TO svc_mensajeria;
GRANT USAGE ON SEQUENCE mensajeria.intento_entrega_id_seq TO svc_mensajeria;

-- --- auditoria_mensajeria: INMUTABLE, append-only (Addendum §11) --------------
GRANT SELECT, INSERT ON mensajeria.auditoria_mensajeria TO svc_mensajeria;
GRANT USAGE ON SEQUENCE mensajeria.auditoria_mensajeria_id_seq TO svc_mensajeria;

-- --- outbox: se inserta en la tx del cambio; el relay marca publicado (UPDATE);
--     no se borra. (nombre `outbox` por pf-pkg-platform-go, ver 0001) ----------
GRANT SELECT, INSERT, UPDATE ON mensajeria.outbox TO svc_mensajeria;
GRANT USAGE ON SEQUENCE mensajeria.outbox_id_seq TO svc_mensajeria;

-- --- inbox_evento: dedup del consumidor; se registra, no se reescribe ni borra -
GRANT SELECT, INSERT ON mensajeria.inbox_evento TO svc_mensajeria;
