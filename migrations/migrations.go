// Package migrations expone las migraciones SQL del esquema mensajeria como un
// embed.FS. La raíz de producción (cmd/mensajeria) y el arranque standalone
// (cmd/api) las aplican con el rol dueño del esquema (svc_mensajeria).
// platform-go no administra schemas: cada módulo trae los suyos (00 "Forma de un
// módulo").
package migrations

import "embed"

// FS contiene los archivos .sql de migración, ordenados por nombre.
//
//go:embed mensajeria/*.sql
var FS embed.FS
