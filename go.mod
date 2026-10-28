module github.com/AI27-Tech/pf-svc-mensajeria

go 1.27

// mensajeria es capa de soporte y vive en su propio deployable (00, tabla de
// topología; mensajeria.md (d), ADR 17). Depende SOLO de los paquetes publicados,
// nunca de otro pf-svc-* (00 regla 1). Sin replace a rutas locales ni go.work
// versionado (00 regla 7). Aristas salientes, ambas HTTP vía cliente generado:
// catalogos (lectura por lote de clientes) y la restringida a identidad para
// revocación (DEC-051); ninguna implica requerir esos módulos.
//
// Versiones fijadas por el prompt 07 (DEC-089): la conversación de mensajería
// entró en contracts v0.1.0-tracer.7; la emergencia espera tracer.9.
require (
	github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go v0.1.0-tracer.21
	github.com/AI27-Tech/pf-pkg-platform-go v0.1.6
	github.com/google/uuid v1.6.0
	github.com/jackc/pgx/v5 v5.11.0
)

require (
	github.com/apapsch/go-jsonmerge/v2 v2.0.0 // indirect
	github.com/cenkalti/backoff/v5 v5.0.3 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/felixge/httpsnoop v1.1.0 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/golang-jwt/jwt/v5 v5.3.1 // indirect
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.30.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/oapi-codegen/runtime v1.7.0 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.71.0 // indirect
	go.opentelemetry.io/otel v1.46.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp v1.46.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace v1.46.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp v1.46.0 // indirect
	go.opentelemetry.io/otel/metric v1.46.0 // indirect
	go.opentelemetry.io/otel/sdk v1.46.0 // indirect
	go.opentelemetry.io/otel/sdk/metric v1.46.0 // indirect
	go.opentelemetry.io/otel/trace v1.46.0 // indirect
	go.opentelemetry.io/proto/otlp v1.11.0 // indirect
	golang.org/x/net v0.58.0 // indirect
	golang.org/x/sync v0.23.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/text v0.42.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20260819154853-08b0e4226688 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260819154853-08b0e4226688 // indirect
	google.golang.org/grpc v1.83.1 // indirect
	google.golang.org/protobuf v1.36.12 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)
