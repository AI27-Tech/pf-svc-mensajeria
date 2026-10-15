// Command mensajeria es la RAÍZ DE PRODUCCIÓN del deployable `mensajeria` (ADR
// 17/29, DEC-050 #2): el mismo cableado HTTP que cmd/api MÁS los Workers()
// embebidos del módulo, arrancados aquí (cmd/api no los arranca). El readiness
// del proceso incluye la convergencia de los workers (p. ej. tombstones de
// revocación, ADR 26) vía health.Registry.
//
// Scaffolding: abre los dos pools de svc_mensajeria, cablea el adaptador HTTP a
// catalogos y arranca el módulo con un bus. El transporte real del broker queda
// EN ESPERA de ADR 32 (propuesta: NATS JetStream); mientras tanto se usa el bus
// en memoria de la plataforma (bus/memory) para que Workers() —hoy, el relay de
// outbox— tenga transporte y arranque de verdad. El almacenamiento de objetos
// real (adjuntos) también espera ADR 32. Sin replace ni adaptador de proveedor.
package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/AI27-Tech/pf-pkg-platform-go/bus"
	"github.com/AI27-Tech/pf-pkg-platform-go/bus/memory"
	"github.com/AI27-Tech/pf-pkg-platform-go/config"
	"github.com/AI27-Tech/pf-pkg-platform-go/observability"
	"github.com/AI27-Tech/pf-pkg-platform-go/pgxpool"
	"github.com/AI27-Tech/pf-pkg-platform-go/server"
	"github.com/AI27-Tech/pf-pkg-platform-go/worker"
	mensajeria "github.com/AI27-Tech/pf-svc-mensajeria"
	catalogoshttp "github.com/AI27-Tech/pf-svc-mensajeria/adapters/http/catalogos"
)

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
	logger.Info("arrancando mensajeria (raíz de producción)", slog.String("service", cfg.ServiceName))

	shutdownOtel, err := observability.Init(ctx, observability.Config{
		ServiceName: cfg.ServiceName,
		ExporterURL: cfg.OtelExporterURL,
	})
	if err != nil {
		logger.Warn("observabilidad no inicializada", slog.Any("error", err))
	} else {
		defer func() { _ = shutdownOtel(context.Background()) }()
	}

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

	// TODO(mensajeria): aplicar migrations.FS con el rol svc_mensajeria antes de
	// servir tráfico. Pendiente del aplicador de migraciones acordado.

	cat, err := catalogoshttp.NewCatalogosHTTP(os.Getenv(envCatalogosBase), nil)
	if err != nil {
		return err
	}

	// Bus en memoria mientras ADR 32 elige el broker. Es Publisher y Subscriber.
	busMem := memory.Nuevo(registroPermisivo{}, memory.Opciones{})

	mod, err := mensajeria.New(
		mensajeria.Pools{Chat: poolChat, Despacho: poolDespacho},
		mensajeria.Deps{Catalogos: cat, Publisher: busMem, Subscriber: busMem},
	)
	if err != nil {
		return err
	}

	srv := server.New(cfg.HTTPAddr, server.WithLogger(logger))
	mod.Routes(srv.Mux())
	srv.Registry().RegisterCheck("db_chat", poolChat.CheckFunc())
	srv.Registry().RegisterCheck("db_despacho", poolDespacho.CheckFunc())

	// Arrancar los Workers() embebidos (ADR 29). Corren hasta que se cancele el
	// contexto del grupo; su readiness se registra en el mismo health.Registry.
	workersCtx, cancelWorkers := context.WithCancel(ctx)
	defer cancelWorkers()
	workers := mod.Workers()
	go func() {
		if err := worker.RunGroup(workersCtx, srv.Registry(), workers...); err != nil {
			logger.Error("grupo de workers terminó con error", slog.Any("error", err))
		}
	}()
	logger.Info("workers embebidos arrancados", slog.Int("n", len(workers)))

	srv.MarkReady()
	return srv.Start(ctx) // bloquea hasta señal; el defer cancela los workers
}

// registroPermisivo es el bus.Registry que el bus en memoria exige. En este
// arranque acepta todo y ordena por agregado (la clave de orden de ADR 29). El
// registro REAL se genera desde el contrato y lo inyectará la raíz con el
// adaptador del broker real (en espera de ADR 32); aquí solo basta para arrancar
// el relay de outbox sobre el doble en memoria.
type registroPermisivo struct{}

func (registroPermisivo) Allowed(string, int) bool       { return true }
func (registroPermisivo) OrderingKey(s bus.Sobre) string { return s.AgregadoID }
