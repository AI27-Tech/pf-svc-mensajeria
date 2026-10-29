-- 0005_conversacion_legal_hold.sql — columna legal_hold de la conversación (E3-T1).
--
-- legal_hold marca que la conversación está bajo retención legal y NO debe
-- purgarse al vencer su ventana operativa. Es un concepto DISTINTO de
-- escritura_hasta: escritura_hasta es el fin de la ventana de escritura (tras él,
-- solo_lectura; DEC-030/DEC-041), mientras legal_hold gobierna la purga/retención.
-- La POLÍTICA de retención y de legal hold sigue pendiente de Legal/Producto
-- (DEC-016 #3); esta migración solo fija la FORMA (la columna), no la política:
-- nada en V1 la activa todavía.
--
-- Aditivo: columna nueva con DEFAULT false, sin reescribir filas existentes de
-- forma incompatible. Hereda los grants de tabla de 0002, no requiere grant nuevo.

ALTER TABLE mensajeria.conversacion
  ADD COLUMN IF NOT EXISTS legal_hold BOOLEAN NOT NULL DEFAULT false;
