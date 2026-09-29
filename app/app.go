// Package app wires the order service: adapters, usecase, observability
// decorators and the outbox relay. cmd/orders and the E2E tests use the same
// wiring, so E2E tests exercise the real decorator chain.
package app

import (
	"database/sql"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/trace"

	"github.com/rootalex/order-observability/clock"
	"github.com/rootalex/order-observability/idgen"
	"github.com/rootalex/order-observability/observability"
	"github.com/rootalex/order-observability/outbox"
	"github.com/rootalex/order-observability/payment"
	"github.com/rootalex/order-observability/repo/postgres"
	"github.com/rootalex/order-observability/transport/httpapi"
	"github.com/rootalex/order-observability/usecase"
)

// Deps are the process-level resources the application is built from.
// Their lifecycle (open, shutdown) belongs to the caller.
type Deps struct {
	DB             *sql.DB
	PaymentURL     string
	PaymentTimeout time.Duration
	Log            *slog.Logger
	// TracerProvider must also be registered globally (otel.SetTracerProvider):
	// the otelhttp and otelsql instrumentation use the global provider.
	TracerProvider trace.TracerProvider
	Registry       *prometheus.Registry
	Clock          clock.Clock
	Publisher      outbox.Publisher
	RelayBatchSize int
}

type App struct {
	Handler http.Handler
	Relay   *outbox.Relay
}

func New(d Deps) *App {
	metrics := observability.NewMetrics(d.Registry)

	repo := postgres.NewOrderRepo(d.DB)
	pay := observability.NewMetricsPaymentGateway(payment.NewClient(d.PaymentURL, d.PaymentTimeout), metrics)

	var uc usecase.CreateOrderInteractor = usecase.NewCreateOrder(
		repo, pay, idgen.Random{}, d.Clock, observability.NewProbe(d.TracerProvider, metrics),
	)
	// Tracing is outermost so that logging and metrics see the span in ctx (trace_id, exemplars).
	uc = observability.NewMetricsInteractor(uc, metrics)
	uc = observability.NewLoggingInteractor(uc, d.Log)
	uc = observability.NewTracedInteractor(uc, d.TracerProvider)

	return &App{
		Handler: httpapi.NewRouter(uc, d.Log, observability.MetricsHandler(d.Registry)),
		Relay:   outbox.NewRelay(d.DB, d.Publisher, d.Clock, d.TracerProvider, d.Log, d.RelayBatchSize),
	}
}
