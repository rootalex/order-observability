package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"go.opentelemetry.io/otel"

	"github.com/rootalex/order-observability/clock"
	"github.com/rootalex/order-observability/idgen"
	"github.com/rootalex/order-observability/observability"
	"github.com/rootalex/order-observability/outbox"
	"github.com/rootalex/order-observability/payment"
	"github.com/rootalex/order-observability/repo/postgres"
	"github.com/rootalex/order-observability/transport/httpapi"
	"github.com/rootalex/order-observability/usecase"
)

const serviceName = "order-service"

func main() {
	if err := run(); err != nil {
		slog.Error("service stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log := observability.NewLogger(os.Stdout, slog.LevelInfo)
	slog.SetDefault(log)

	// Trace export errors must not get lost among INFO logs.
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		log.Warn("opentelemetry error", "error", err)
	}))
	shutdownTracing, err := observability.InitTracerProvider(ctx, serviceName,
		env("OTEL_EXPORTER_OTLP_ENDPOINT", "http://localhost:4318"))
	if err != nil {
		return fmt.Errorf("init tracing: %w", err)
	}
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdownTracing(sctx)
	}()
	tp := otel.GetTracerProvider()

	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	metrics := observability.NewMetrics(reg)

	db, err := postgres.Open(ctx, env("DATABASE_URL", "postgres://orders:orders@localhost:55432/orders?sslmode=disable"))
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer db.Close()

	repo := postgres.NewOrderRepo(db)
	pay := payment.NewClient(env("PAYMENT_URL", "http://localhost:8081"), 5*time.Second)

	var uc usecase.CreateOrderInteractor = usecase.NewCreateOrder(
		repo, pay, idgen.Random{}, clock.Real{}, observability.NewProbe(tp, metrics),
	)
	// Tracing is outermost so that logging and metrics see the span in ctx (trace_id, exemplars).
	uc = observability.NewMetricsInteractor(uc, metrics)
	uc = observability.NewLoggingInteractor(uc, log)
	uc = observability.NewTracedInteractor(uc, tp)

	// Outbox relay: publishes domain events written by the repository.
	// LogPublisher stands in for a real broker adapter.
	relay := outbox.NewRelay(db, outbox.LogPublisher{Log: log}, clock.Real{}, tp, log, 100)
	go relay.Run(ctx, time.Second)

	srv := &http.Server{
		Addr:              env("HTTP_ADDR", ":8080"),
		Handler:           httpapi.NewRouter(uc, log, observability.MetricsHandler(reg)),
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("http server started", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	log.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(sctx)
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
