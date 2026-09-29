package observability

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/trace"

	"github.com/rootalex/order-observability/domain"
	"github.com/rootalex/order-observability/usecase"
)

// Metrics are the business metrics of order processing.
// All labels have a bounded set of values; order and customer IDs live in traces and logs.
type Metrics struct {
	ordersCreated   *prometheus.CounterVec
	stepDuration    *prometheus.HistogramVec
	pending         prometheus.Gauge
	paymentRequests *prometheus.CounterVec
}

func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		ordersCreated: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "orders_created_total",
			Help: "Orders processed by CreateOrder, by outcome (success|failure|pending) and customer tier.",
		}, []string{"status", "customer_tier"}),
		stepDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "order_processing_duration_seconds",
			Help:    "Duration of order processing steps.",
			Buckets: prometheus.DefBuckets,
		}, []string{"step"}),
		pending: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "orders_pending_count",
			Help: "Orders currently being processed.",
		}),
		paymentRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "payment_requests_total",
			Help: "Charge requests to the payment provider, by outcome (success|declined|rejected|timeout|error).",
		}, []string{"outcome"}),
	}
	reg.MustRegister(m.ordersCreated, m.stepDuration, m.pending, m.paymentRequests)
	return m
}

// MetricsHandler serves /metrics in OpenMetrics format, which exemplars require.
func MetricsHandler(g prometheus.Gatherer) http.Handler {
	return promhttp.HandlerFor(g, promhttp.HandlerOpts{EnableOpenMetrics: true})
}

func (m *Metrics) observeStep(ctx context.Context, step usecase.Step, d time.Duration) {
	obs := m.stepDuration.WithLabelValues(string(step))
	if eo, ok := obs.(prometheus.ExemplarObserver); ok {
		if ex := traceExemplar(ctx); ex != nil {
			eo.ObserveWithExemplar(d.Seconds(), ex)
			return
		}
	}
	obs.Observe(d.Seconds())
}

// MetricsInteractor counts orders by outcome and tracks orders in processing.
type MetricsInteractor struct {
	inner   usecase.CreateOrderInteractor
	metrics *Metrics
}

func NewMetricsInteractor(inner usecase.CreateOrderInteractor, m *Metrics) *MetricsInteractor {
	return &MetricsInteractor{inner: inner, metrics: m}
}

func (mi *MetricsInteractor) Execute(ctx context.Context, req *usecase.CreateOrderRequest) (*usecase.CreateOrderResponse, error) {
	mi.metrics.pending.Inc()
	defer mi.metrics.pending.Dec()

	resp, err := mi.inner.Execute(ctx, req)

	status := "success"
	switch {
	case errors.Is(err, usecase.ErrPaymentPending):
		status = "pending"
	case err != nil:
		status = "failure"
	}
	incWithExemplar(ctx, mi.metrics.ordersCreated.WithLabelValues(status, tierLabel(req.Tier)))
	return resp, err
}

// MetricsPaymentGateway decorates the PaymentGateway port and counts charge
// requests by outcome. Timeouts are counted separately: they are the "charged
// but shows as failed" risk from ANSWERS.md Q3 and have their own alert.
type MetricsPaymentGateway struct {
	inner   usecase.PaymentGateway
	metrics *Metrics
}

var _ usecase.PaymentGateway = (*MetricsPaymentGateway)(nil)

func NewMetricsPaymentGateway(inner usecase.PaymentGateway, m *Metrics) *MetricsPaymentGateway {
	return &MetricsPaymentGateway{inner: inner, metrics: m}
}

func (g *MetricsPaymentGateway) Charge(ctx context.Context, req usecase.ChargeRequest) (usecase.ChargeResult, error) {
	res, err := g.inner.Charge(ctx, req)
	incWithExemplar(ctx, g.metrics.paymentRequests.WithLabelValues(paymentOutcome(err)))
	return res, err
}

func paymentOutcome(err error) string {
	var netErr net.Error
	switch {
	case err == nil:
		return "success"
	case errors.Is(err, usecase.ErrPaymentDeclined):
		return "declined"
	case errors.Is(err, usecase.ErrPaymentRejected):
		return "rejected"
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return "timeout"
	default:
		return "error"
	}
}

func incWithExemplar(ctx context.Context, c prometheus.Counter) {
	if ea, ok := c.(prometheus.ExemplarAdder); ok {
		if ex := traceExemplar(ctx); ex != nil {
			ea.AddWithExemplar(1, ex)
			return
		}
	}
	c.Inc()
}

// tierLabel guards against cardinality explosion: unknown values from a request
// must not create new time series.
func tierLabel(t domain.CustomerTier) string {
	switch t {
	case domain.TierFree, domain.TierPremium:
		return string(t)
	default:
		return "unknown"
	}
}

// traceExemplar links a metric sample to a trace (Grafana: exemplar -> Jaeger).
func traceExemplar(ctx context.Context) prometheus.Labels {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsSampled() {
		return nil
	}
	return prometheus.Labels{"trace_id": sc.TraceID().String()}
}
