// Package observability provides decorators that add traces, logs and metrics
// around usecases without touching business code or the domain.
package observability

import (
	"context"
	"fmt"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/rootalex/order-observability/usecase"
)

const instrumentationName = "github.com/rootalex/order-observability/observability"

// InitTracerProvider sets up the global TracerProvider with OTLP/HTTP export
// to endpoint (a base URL as in OTEL_EXPORTER_OTLP_ENDPOINT: http://localhost:4318)
// and the W3C propagator (traceparent).
func InitTracerProvider(ctx context.Context, serviceName, endpoint string) (shutdown func(context.Context) error, err error) {
	exporter, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpointURL(strings.TrimRight(endpoint, "/")+"/v1/traces"))
	if err != nil {
		return nil, fmt.Errorf("otlp exporter: %w", err)
	}
	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(
		attribute.String("service.name", serviceName),
	))
	if err != nil {
		return nil, fmt.Errorf("resource: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	return tp.Shutdown, nil
}

// TracedInteractor creates a span around the usecase (option C) and passes it on via ctx (option A).
// Domain events from the response become span events: this is how the domain is traced without ctx.
type TracedInteractor struct {
	inner  usecase.CreateOrderInteractor
	tracer trace.Tracer
}

func NewTracedInteractor(inner usecase.CreateOrderInteractor, tp trace.TracerProvider) *TracedInteractor {
	return &TracedInteractor{inner: inner, tracer: tp.Tracer(instrumentationName)}
}

func (t *TracedInteractor) Execute(ctx context.Context, req *usecase.CreateOrderRequest) (*usecase.CreateOrderResponse, error) {
	ctx, span := t.tracer.Start(ctx, "CreateOrder", trace.WithAttributes(
		attribute.String("order.customer_tier", string(req.Tier)),
		attribute.Int("order.items_count", len(req.Items)),
	))
	defer span.End()

	resp, err := t.inner.Execute(ctx, req)
	if resp != nil {
		span.SetAttributes(
			attribute.String("order.id", string(resp.OrderID)),
			attribute.String("order.status", string(resp.Status)),
		)
		for _, e := range resp.Events {
			span.AddEvent(e.Name(), trace.WithTimestamp(e.OccurredAt()))
		}
	}
	if err != nil {
		span.RecordError(err)
		if !isExpected(err) {
			span.SetStatus(codes.Error, err.Error())
		}
	}
	return resp, err
}
