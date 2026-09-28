// Package observability содержит декораторы, которые добавляют трейсы, логи и метрики
// вокруг usecase, не затрагивая бизнес-код и домен.
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

// InitTracerProvider настраивает глобальный TracerProvider с OTLP/HTTP-экспортом
// на endpoint (базовый адрес, как в OTEL_EXPORTER_OTLP_ENDPOINT: http://localhost:4318)
// и W3C-пропагатором (traceparent).
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

// TracedInteractor создаёт span вокруг usecase (вариант C) и передаёт его дальше через ctx (вариант A).
// Доменные события из ответа становятся span events — так трассируется домен без ctx.
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
