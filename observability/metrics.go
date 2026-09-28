package observability

import (
	"context"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/trace"

	"github.com/rootalex/order-observability/domain"
	"github.com/rootalex/order-observability/usecase"
)

// Metrics — бизнес-метрики обработки заказов.
// Все лейблы имеют ограниченный набор значений; ID заказов/клиентов живут в трейсах и логах.
type Metrics struct {
	ordersCreated *prometheus.CounterVec
	stepDuration  *prometheus.HistogramVec
	pending       prometheus.Gauge
}

func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		ordersCreated: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "orders_created_total",
			Help: "Orders processed by CreateOrder, by outcome and customer tier.",
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
	}
	reg.MustRegister(m.ordersCreated, m.stepDuration, m.pending)
	return m
}

// MetricsHandler отдаёт /metrics в формате OpenMetrics — он нужен для exemplars.
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

// MetricsInteractor считает заказы по результату и держит gauge заказов в обработке.
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
	if err != nil {
		status = "failure"
	}
	c := mi.metrics.ordersCreated.WithLabelValues(status, tierLabel(req.Tier))
	if ea, ok := c.(prometheus.ExemplarAdder); ok {
		if ex := traceExemplar(ctx); ex != nil {
			ea.AddWithExemplar(1, ex)
			return resp, err
		}
	}
	c.Inc()
	return resp, err
}

// tierLabel защищает от взрыва кардинальности: неизвестные значения из запроса
// не должны порождать новые time series.
func tierLabel(t domain.CustomerTier) string {
	switch t {
	case domain.TierFree, domain.TierPremium:
		return string(t)
	default:
		return "unknown"
	}
}

// traceExemplar связывает точку метрики с трейсом (Grafana: exemplar -> Jaeger).
func traceExemplar(ctx context.Context) prometheus.Labels {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsSampled() {
		return nil
	}
	return prometheus.Labels{"trace_id": sc.TraceID().String()}
}
