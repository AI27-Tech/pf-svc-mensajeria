// Command api es el arranque standalone de mensajeria (invariante I5): compila y
// sirve /health y /ready SIN core ni bus, cableando el adaptador HTTP hacia
// catalogos. NO arranca Workers() (eso es de cmd/mensajeria). Existe para
// levantar y probar el módulo de forma aislada.
//
// Scaffolding: abre los DOS pools de svc_mensajeria (chat y despacho; mismo rol,
// presupuestos separados, mensajeria.md (d)), construye el módulo con el
// adaptador HTTP de catalogos y sirve health/ready. Aún no monta rutas de
// negocio.
package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/AI27-Tech/pf-pkg-platform-go/config"
	"github.com/AI27-Tech/pf-pkg-platform-go/observability"
	"github.com/AI27-Tech/pf-pkg-platform-go/pgxpool"
	"github.com/AI27-Tech/pf-pkg-platform-go/server"
	mensajeria "github.com/AI27-Tech/pf-svc-mensajeria"
	catalogoshttp "github.com/AI27-Tech/pf-svc-mensajeria/adapters/http/catalogos"
)

// Claves de entorno propias de mensajeria. Un esquema, un rol (svc_mensajeria),
// DOS pools de ese rol con presupuestos separados: chat y despacho.
const (
	envDBChat        = "DB_MENSAJERIA_CHAT_URL"
	envDBDespacho    = "DB_MENSAJERIA_DESPACHO_URL"
	envCatalogosBase = "CATALOGOS_BASE_URL"
)

func main() {
	if err := run(); err != nil {
		slog.Error("mensajeria terminó con error", slog.Any("error", err))
		os.Exit(1)
	}
}

func run() error {
	ctx := context.Background()

	cfg, err := config.Load(envDBChat, envDBDespacho, envCatalogosBase)
	if err != nil {
		return err
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	logger.Info("arrancando mensajeria (standalone)", slog.String("service", cfg.ServiceName))

	shutdownOtel, err := observability.Init(ctx, observability.Config{
		ServiceName: cfg.ServiceName,
		ExporterURL: cfg.OtelExporterURL,
	})
	if err != nil {
		logger.Warn("observabilidad no inicializada", slog.Any("error", err))
	} else {
		defer func() { _ = shutdownOtel(context.Background()) }()
	}

	// Dos pools del MISMO rol svc_mensajeria, presupuestos separados (la mampara,
	// mensajeria.md (d), (k)). Ninguno alcanza otro esquema (I1).
	poolChat, err := pgxpool.New(ctx, "svc_mensajeria", os.Getenv(envDBChat))
	if err != nil {
		return err
	}
	defer poolChat.Close()

	poolDespacho, err := pgxpool.New(ctx, "svc_mensajeria", os.Getenv(envDBDespacho))
	if err != nil {
		return err
	}
	defer poolDespacho.Close()

	// Las migraciones NO se aplican aquí (DEC-096): el bootstrap de esquema, rol
	// y grants corre como un paso de despliegue aparte, en `cmd/migrate`, con
	// credencial administrativa. mensajeria arranca con su rol de app de
	// privilegio mínimo y asume la base migrada (I1: el proceso del servicio
	// nunca porta CREATE SCHEMA/CREATE ROLE). Los pools hacen ping arriba; una
	// base sin migrar falla ahí de forma observable.

	// Adaptador HTTP hacia catalogos (siempre HTTP; no hay adaptador en proceso).
	cat, err := catalogoshttp.NewCatalogosHTTP(os.Getenv(envCatalogosBase), nil)
	if err != nil {
		return err
	}

	mod, err := mensajeria.New(
		mensajeria.Pools{Chat: poolChat, Despacho: poolDespacho},
		mensajeria.Deps{Catalogos: cat}, // sin bus: standalone no arranca Workers()
	)
	if err != nil {
		return err
	}

	srv := server.New(cfg.HTTPAddr, server.WithLogger(logger))
	mod.Routes(srv.Mux())
	srv.Registry().RegisterCheck("db_chat", poolChat.CheckFunc())
	srv.Registry().RegisterCheck("db_despacho", poolDespacho.CheckFunc())

	srv.MarkReady()
	return srv.Start(ctx)
}
