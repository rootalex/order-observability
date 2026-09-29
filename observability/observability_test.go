package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/rootalex/order-observability/domain"
	"github.com/rootalex/order-observability/usecase"
)

var fixedTime = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

type fakeInteractor struct {
	resp *usecase.CreateOrderResponse
	err  error
}

func (f fakeInteractor) Execute(context.Context, *usecase.CreateOrderRequest) (*usecase.CreateOrderResponse, error) {
	return f.resp, f.err
}

func newRecorder() (*tracetest.SpanRecorder, *sdktrace.TracerProvider) {
	rec := tracetest.NewSpanRecorder()
	return rec, sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
}

func decodeLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("invalid json log line %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

func TestLogger_AddsTraceIDAndRedactsPII(t *testing.T) {
	_, tp := newRecorder()
	ctx, span := tp.Tracer("test").Start(context.Background(), "op")
	defer span.End()

	var buf bytes.Buffer
	log := NewLogger(&buf, nil)
	log.InfoContext(ctx, "hello", "email", "john@example.com", "customer_id", "c-1")

	line := decodeLines(t, &buf)[0]
	if got, want := line["trace_id"], span.SpanContext().TraceID().String(); got != want {
		t.Errorf("trace_id = %v, want %v", got, want)
	}
	if line["email"] != redacted {
		t.Errorf("email not redacted: %v", line["email"])
	}
	if line["customer_id"] != "c-1" {
		t.Errorf("customer_id = %v, want c-1", line["customer_id"])
	}
}

func TestLoggingInteractor_BoundaryAndBusinessLogs(t *testing.T) {
	var buf bytes.Buffer
	inner := fakeInteractor{
		resp: &usecase.CreateOrderResponse{
			OrderID: "ord-1",
			Status:  domain.StatusFailed,
			Events:  []domain.Event{domain.OrderFailed{OrderID: "ord-1", Reason: "payment_error", At: fixedTime}},
		},
		err: WithStack(errors.New("payment api: unexpected status 500")),
	}
	li := NewLoggingInteractor(inner, NewLogger(&buf, nil))

	_, _ = li.Execute(context.Background(), &usecase.CreateOrderRequest{
		CustomerID: "c-1", Tier: domain.TierFree, Email: "john@example.com",
		Items: []domain.Item{{ProductID: "p-1", Quantity: 1, PriceCents: 100}},
	})

	if strings.Contains(buf.String(), "john@example.com") {
		t.Fatal("email leaked into logs")
	}

	lines := decodeLines(t, &buf)
	byMsg := map[string]map[string]any{}
	for _, l := range lines {
		byMsg[l["msg"].(string)] = l
	}

	if l, ok := byMsg["order.failed"]; !ok || l["log_type"] != business || l["reason"] != "payment_error" {
		t.Errorf("business log for order.failed missing or wrong: %v", l)
	}
	failed, ok := byMsg["create order failed"]
	if !ok || failed["log_type"] != operational {
		t.Fatalf("operational error log missing: %v", failed)
	}
	errGroup := failed["error"].(map[string]any)
	if !strings.Contains(errGroup["stack"].(string), "TestLoggingInteractor_BoundaryAndBusinessLogs") {
		t.Errorf("stack should point to where the error was created, got:\n%s", errGroup["stack"])
	}
}

func TestTracedInteractor_DomainEventsAndErrorStatus(t *testing.T) {
	rec, tp := newRecorder()
	inner := fakeInteractor{
		resp: &usecase.CreateOrderResponse{
			OrderID: "ord-1",
			Status:  domain.StatusFailed,
			Events: []domain.Event{
				domain.OrderCreated{OrderID: "ord-1", At: fixedTime},
				domain.OrderFailed{OrderID: "ord-1", At: fixedTime},
			},
		},
		err: errors.New("charge failed"),
	}

	_, _ = NewTracedInteractor(inner, tp).Execute(context.Background(), &usecase.CreateOrderRequest{Tier: domain.TierPremium})

	spans := rec.Ended()
	if len(spans) != 1 || spans[0].Name() != "CreateOrder" {
		t.Fatalf("want 1 span CreateOrder, got %d", len(spans))
	}
	s := spans[0]
	if s.Status().Code != codes.Error {
		t.Errorf("span status = %v, want Error", s.Status().Code)
	}
	var names []string
	for _, e := range s.Events() {
		names = append(names, e.Name)
	}
	if got := strings.Join(names, ","); got != "order.created,order.failed,exception" {
		t.Errorf("span events = %s", got)
	}
}

func TestProbe_ChildSpanAndStepHistogram(t *testing.T) {
	rec, tp := newRecorder()
	reg := prometheus.NewRegistry()
	p := NewProbe(tp, NewMetrics(reg))

	ctx, root := tp.Tracer("test").Start(context.Background(), "root")
	_, done := p.StepStarted(ctx, usecase.StepPayment)
	done(nil)
	root.End()

	spans := rec.Ended()
	if spans[0].Name() != "CreateOrder.payment" || spans[0].Parent().SpanID() != root.SpanContext().SpanID() {
		t.Errorf("step span must be a child of the usecase span, got %q", spans[0].Name())
	}
	if n := testutil.CollectAndCount(reg, "order_processing_duration_seconds"); n != 1 {
		t.Errorf("want 1 histogram series, got %d", n)
	}
}

func TestMetricsInteractor_BoundedLabels(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	ok := NewMetricsInteractor(fakeInteractor{}, m)
	fail := NewMetricsInteractor(fakeInteractor{err: errors.New("boom")}, m)

	_, _ = ok.Execute(context.Background(), &usecase.CreateOrderRequest{Tier: domain.TierPremium})
	_, _ = fail.Execute(context.Background(), &usecase.CreateOrderRequest{Tier: "gold-vip-123"})

	if v := testutil.ToFloat64(m.ordersCreated.WithLabelValues("success", "premium")); v != 1 {
		t.Errorf("success/premium = %v, want 1", v)
	}
	if v := testutil.ToFloat64(m.ordersCreated.WithLabelValues("failure", "unknown")); v != 1 {
		t.Errorf("unknown tier must map to 'unknown', got %v", v)
	}
	if v := testutil.ToFloat64(m.pending); v != 0 {
		t.Errorf("pending gauge = %v, want 0 after completion", v)
	}
}

func TestExpectedErrors_WarnWithoutStackAndNoSpanError(t *testing.T) {
	var buf bytes.Buffer
	rec, tp := newRecorder()
	inner := fakeInteractor{err: fmt.Errorf("new order: %w", domain.ErrEmptyOrder)}
	uc := NewTracedInteractor(NewLoggingInteractor(inner, NewLogger(&buf, nil)), tp)

	_, _ = uc.Execute(context.Background(), &usecase.CreateOrderRequest{})

	last := decodeLines(t, &buf)[1]
	if last["level"] != "WARN" || last["error"] != nil {
		t.Errorf("expected WARN without error/stack, got %v", last)
	}
	if s := rec.Ended()[0]; s.Status().Code == codes.Error {
		t.Error("expected business error must not mark span as Error")
	}
}

func TestMetricsInteractor_PendingIsNotFailure(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	inner := fakeInteractor{err: fmt.Errorf("%w: timeout", usecase.ErrPaymentPending)}

	_, _ = NewMetricsInteractor(inner, m).Execute(context.Background(), &usecase.CreateOrderRequest{Tier: domain.TierFree})

	assert.Equal(t, 1.0, testutil.ToFloat64(m.ordersCreated.WithLabelValues("pending", "free")))
	assert.Equal(t, 0.0, testutil.ToFloat64(m.ordersCreated.WithLabelValues("failure", "free")))
}

type fakeGateway struct{ err error }

func (g fakeGateway) Charge(context.Context, usecase.ChargeRequest) (usecase.ChargeResult, error) {
	return usecase.ChargeResult{}, g.err
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestMetricsPaymentGateway_Outcomes(t *testing.T) {
	tests := []struct {
		err  error
		want string
	}{
		{nil, "success"},
		{usecase.ErrPaymentDeclined, "declined"},
		{fmt.Errorf("%w: status 422", usecase.ErrPaymentRejected), "rejected"},
		{fmt.Errorf("payment request: %w", context.DeadlineExceeded), "timeout"},
		{fmt.Errorf("payment request: %w", timeoutErr{}), "timeout"}, // http.Client.Timeout
		{errors.New("payment api: unexpected status 503"), "error"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			m := NewMetrics(prometheus.NewRegistry())
			_, err := NewMetricsPaymentGateway(fakeGateway{err: tt.err}, m).Charge(context.Background(), usecase.ChargeRequest{})

			assert.Equal(t, tt.err, err, "decorator must return the inner error unchanged")
			assert.Equal(t, 1.0, testutil.ToFloat64(m.paymentRequests.WithLabelValues(tt.want)))
		})
	}
}
