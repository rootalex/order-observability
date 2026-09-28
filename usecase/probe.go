package usecase

import "context"

// Step is an order processing step. The set of values is bounded: it is also a metric label.
type Step string

const (
	StepValidation  Step = "validation"
	StepPayment     Step = "payment"
	StepFulfillment Step = "fulfillment"
)

// Probe is the observability port for usecase steps (Domain-Oriented Observability).
// The usecase reports what is happening; the implementation in observability decides
// how to turn it into spans and metrics. The usecase does not import OTel or Prometheus.
type Probe interface {
	// StepStarted begins a step. Pass the returned ctx to calls within the step
	// and call done with the step's error (or nil) when it ends.
	StepStarted(ctx context.Context, step Step) (context.Context, func(err error))
}

type noopProbe struct{}

func (noopProbe) StepStarted(ctx context.Context, _ Step) (context.Context, func(error)) {
	return ctx, func(error) {}
}
