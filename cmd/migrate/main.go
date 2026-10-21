// Command migrate es el BOOTSTRAP de base de datos del deployable `mensajeria`
// (DEC-096, E0-T7). Corre UNA vez en el despliegue, con credencial
// administrativa, ANTES de que arranque `cmd/mensajeria`. Crea el esquema, el
// rol y los grants aplicando las migraciones embebidas del módulo, y fija el
// search_path del rol para que el relay de outbox resuelva la tabla `outbox`.
//
// Por qué un binario aparte y no dentro de cmd/mensajeria (DEC-096): el 0001
// hace CREATE SCHEMA / CREATE ROLE, privilegio administrativo que el pool de app
// de mensajeria (rol svc_mensajeria, privilegio mínimo tras los REVOKE del 0002)
// no tiene y no debe tener. Meter esa credencial en el proceso del servicio la
// dejaría viva en cada arranque; este binario la confina al paso de despliegue y
// termina. `cmd/mensajeria` arranca después con su rol de app y encuentra todo
// hecho. Es el mismo patrón que cmd/migrate de core.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/AI27-Tech/pf-pkg-platform-go/migrate"
	"github.com/jackc/pgx/v5"

	mensajeriamig "github.com/AI27-Tech/pf-svc-mensajeria/migrations"
)

// envDBAdmin es la credencial ADMINISTRATIVA del bootstrap: un rol con permiso
// de CREATE SCHEMA y CREATE ROLE. No es el rol de app svc_mensajeria que el
// servicio abre como pool (DEC-096).
const envDBAdmin = "DB_ADMIN_URL"

func main() {
	if err := run(); err != nil {
		slog.Error("migrate (bootstrap de mensajeria) terminó con error", slog.Any("error", err))
		os.Exit(1)
	}
}

func run() error {
	ctx := context.Background()
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	dsn := os.Getenv(envDBAdmin)
	if dsn == "" {
		return fmt.Errorf("migrate: falta %s (credencial administrativa del bootstrap)", envDBAdmin)
	}

	// Una sola conexión administrativa aplica el esquema. El bootstrap no usa el
	// pool de app: el ResolverEjecutor devuelve esta conexión para el rol
	// svc_mensajeria porque es la credencial admin la que crea el esquema y el
	// rol (DEC-096). La pertenencia del esquema a su rol la fija el propio 0001.
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("migrate: conectar con la credencial admin: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	resolver := func(rol string) (migrate.Ejecutor, error) {
		return conn, nil
	}

	m := migrate.Modulo{
		Nombre: "mensajeria",
		Esquemas: []migrate.Esquema{
			{Nombre: "mensajeria", Rol: "svc_mensajeria", FS: mensajeriamig.FS, Dir: "mensajeria"},
		},
	}

	logger.Info("aplicando migraciones", slog.String("modulo", m.Nombre))
	if err := migrate.Aplicar(ctx, m, resolver); err != nil {
		return err
	}

	// search_path del rol: su relay de outbox consulta la tabla `outbox` sin
	// calificar el esquema (platform-go/outbox), así que svc_mensajeria debe
	// resolverla por search_path (DEC-096 nota 3). Tras aplicar, el rol ya existe.
	if _, err := conn.Exec(ctx, "ALTER ROLE svc_mensajeria SET search_path TO mensajeria"); err != nil {
		return fmt.Errorf("migrate: fijar search_path de svc_mensajeria: %w", err)
	}

	logger.Info("bootstrap de base de datos completo", slog.String("modulo", m.Nombre))
	return nil
}
