// Package outbox documenta el uso del outbox transaccional de mensajeria. La
// mecánica real NO se reimplementa aquí: la aporta pf-pkg-platform-go/outbox
// (00 regla "no inventar"), que ofrece el Publisher (inserta la fila de evento
// en la MISMA transacción de la mutación, P17; agnóstico al nombre de tema) y el
// Relay (drena las filas no publicadas y marca publicado tras el acuse del
// broker, ADR 29).
//
// Dónde se usan:
//   - El Publisher lo usa internal/conversacion dentro de la tx del envío para
//     escribir mensaje + mensaje.creado juntos.
//   - El Relay lo construye el paquete raíz (mensajeria.New) sobre el pool/rol
//     svc_mensajeria y lo devuelve en Workers() para que la raíz de producción
//     cmd/mensajeria lo arranque. El Relay publica el sobre de DOMINIO; la capa
//     viva restringida (audience_version, target_sujeto_ids del SobreVivo) la
//     agrega el productor del tema restringido, no el outbox.
//
// La tabla física que la plataforma lee se llama `outbox` (outbox.OutboxDDL y
// outbox.Relay la nombran así, sin esquema, resuelta por search_path al esquema
// del módulo). La migración de mensajeria la crea con ese nombre. Discrepancia
// de nombre con la lista del prompt ("outbox_evento") reportada en el README.
package outbox
