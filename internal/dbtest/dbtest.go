// Package dbtest ofrece utilidades para las pruebas de integración de mensajeria
// que requieren Postgres. Replica la forma de pf-svc-reportes/internal/dbtest (el
// dbtest de pf-pkg-platform-go es internal y no se puede importar), acotada a lo
// que mensajeria necesita: aplicar sus migraciones y abrir un pool por rol para
// ejercer el envío idempotente y el reclamo concurrente.
//
// Toda prueba que lo usa se salta (t.Skip) cuando MENSAJERIA_TEST_DB no está
// definido, de modo que `go test ./...` sigue en verde en CI sin Postgres.
//
// MENSAJERIA_TEST_DB debe ser un DSN de una base EFÍMERA con privilegios para
// CREATE ROLE y CREATE SCHEMA (superusuario o equivalente): las pruebas crean y
// eliminan el esquema mensajeria y el rol svc_mensajeria.
//
// Ejemplo:
//
//	MENSAJERIA_TEST_DB="postgres://postgres@localhost:5432/postgres" go test ./...
package dbtest

import (
	"context"
	"io/fs"
	"os"
	"sort"
	"testing"

	"github.com/AI27-Tech/pf-svc-mensajeria/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// EnvDSN es la variable de entorno que activa las pruebas de integración de BD.
const EnvDSN = "MENSAJERIA_TEST_DB"

const (
	Schema = "mensajeria"
	Role   = "svc_mensajeria"
)

// DSN devuelve el DSN de administración, o salta la prueba si no está definido.
func DSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv(EnvDSN)
	if dsn == "" {
		t.Skipf("se salta la prueba de integración: %s no está definido", EnvDSN)
	}
	return dsn
}

// Serializar toma un lock de sesión (pg_advisory_lock) para que las pruebas de
// integración de distintos paquetes, que comparten una base, no se pisen el
// esquema o el rol. El lock se libera al cerrar la conexión en el Cleanup.
func Serializar(t *testing.T, ctx context.Context) {
	t.Helper()
	conn, err := pgx.Connect(ctx, DSN(t))
	if err != nil {
		t.Fatalf("dbtest: conectar para serializar: %v", err)
	}
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock(748227)"); err != nil {
		_ = conn.Close(ctx)
		t.Fatalf("dbtest: tomar lock de serialización: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(ctx) })
}

// AdminConn abre una conexión de administración para crear o eliminar roles y
// esquemas. El llamador la cierra con la función devuelta.
func AdminConn(t *testing.T, ctx context.Context) (*pgx.Conn, func()) {
	t.Helper()
	conn, err := pgx.Connect(ctx, DSN(t))
	if err != nil {
		t.Fatalf("dbtest: conectar como admin: %v", err)
	}
	return conn, func() { _ = conn.Close(ctx) }
}

// Exec ejecuta una sentencia con la conexión de administración y falla si hay error.
func Exec(t *testing.T, ctx context.Context, conn *pgx.Conn, sql string) {
	t.Helper()
	if _, err := conn.Exec(ctx, sql); err != nil {
		t.Fatalf("dbtest: exec %q: %v", sql, err)
	}
}

// Reiniciar deja la base limpia: elimina el esquema mensajeria y el rol
// svc_mensajeria si existen. Se llama al inicio y al final de cada prueba.
func Reiniciar(t *testing.T, ctx context.Context, admin *pgx.Conn) {
	t.Helper()
	Exec(t, ctx, admin, "DROP SCHEMA IF EXISTS "+pgx.Identifier{Schema}.Sanitize()+" CASCADE")
	Exec(t, ctx, admin, "DROP ROLE IF EXISTS "+pgx.Identifier{Role}.Sanitize())
}

// AplicarMigraciones aplica, como admin, todos los .sql del esquema en orden de
// nombre (0001 estructura, 0002 grants).
func AplicarMigraciones(t *testing.T, ctx context.Context, admin *pgx.Conn) {
	t.Helper()
	files, err := fs.Glob(migrations.FS, Schema+"/*.sql")
	if err != nil {
		t.Fatalf("dbtest: listar migraciones: %v", err)
	}
	sort.Strings(files)
	for _, f := range files {
		sql, err := migrations.FS.ReadFile(f)
		if err != nil {
			t.Fatalf("dbtest: leer %s: %v", f, err)
		}
		if _, err := admin.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("dbtest: aplicar %s: %v", f, err)
		}
	}
}

// PoolComoRol abre un pool que asume el rol svc_mensajeria en cada conexión (SET
// ROLE) y fija el search_path al esquema del módulo, como en producción: así los
// privilegios efectivos son los del rol y la tabla `outbox` (sin esquema) se
// resuelve a mensajeria.outbox.
func PoolComoRol(t *testing.T, ctx context.Context) (*pgxpool.Pool, func()) {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(DSN(t))
	if err != nil {
		t.Fatalf("dbtest: parsear DSN: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = Schema
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, "SET ROLE "+pgx.Identifier{Role}.Sanitize())
		return err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("dbtest: crear pool como %s: %v", Role, err)
	}
	return pool, pool.Close
}
