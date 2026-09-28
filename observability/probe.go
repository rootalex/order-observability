package observability

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/rootalex/order-observability/usecase"
)

// Probe implements usecase.Probe: every usecase step is a child span
// and an observation in order_processing_duration_seconds{step}.
type Probe struct {
	tracer  trace.Tracer
	metrics *Metrics
}

var _ usecase.Probe = (*Probe)(nil)

func NewProbe(tp trace.TracerProvider, m *Metrics) *Probe {
	return &Probe{tracer: tp.Tracer(instrumentationName), metrics: m}
}

func (p *Probe) StepStarted(ctx context.Context, step usecase.Step) (context.Context, func(error)) {
	ctx, span := p.tracer.Start(ctx, "CreateOrder."+string(step))
	start := time.Now()

	return ctx, func(err error) {
		if err != nil {
			span.RecordError(err)
			if !isExpected(err) {
				span.SetStatus(codes.Error, err.Error())
			}
		}
		span.End()
		if p.metrics != nil {
			p.metrics.observeStep(ctx, step, time.Since(start))
		}
	}
}
