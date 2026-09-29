// Package e2e drives the whole service through its public HTTP API: the real
// wiring from package app, a real Postgres, and WireMock in place of the
// payment provider (the only third party). The broker is a recording fake
// behind the outbox.Publisher port.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/rootalex/order-observability/app"
	"github.com/rootalex/order-observability/clock"
	"github.com/rootalex/order-observability/observability"
	"github.com/rootalex/order-observability/outbox"
	"github.com/rootalex/order-observability/testing/setup"
)

type obj = map[string]any

const paymentTimeout = 300 * time.Millisecond

// syncBuffer collects logs written from server goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type recordingPublisher struct {
	mu       sync.Mutex
	events   []string
	traceIDs []string
}

func (p *recordingPublisher) Publish(ctx context.Context, m outbox.Message) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, m.EventType)
	p.traceIDs = append(p.traceIDs, trace.SpanContextFromContext(ctx).TraceID().String())
	return nil
}

func (p *recordingPublisher) reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events, p.traceIDs = nil, nil
}

func TestCreateOrder_E2E(t *testing.T) {
	ctx := context.Background()
	db := setup.StartPostgres(ctx, t)
	wm := setup.StartWireMock(ctx, t)

	// Same as cmd/orders: the provider is global, because otelhttp (server and
	// payment client) and otelsql use the global one.
	otel.SetTextMapPropagator(propagation.TraceContext{})
	spans := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))
	prevTP := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(prevTP) })
	logs := &syncBuffer{}
	pub := &recordingPublisher{}

	application := app.New(app.Deps{
		DB:             db,
		PaymentURL:     wm.BaseURL,
		PaymentTimeout: paymentTimeout,
		Log:            observability.NewLogger(logs, nil),
		TracerProvider: tp,
		Registry:       prometheus.NewRegistry(),
		Clock:          clock.Real{},
		Publisher:      pub,
		RelayBatchSize: 100,
	})
	srv := httptest.NewServer(application.Handler)
	t.Cleanup(srv.Close)

	stubCharge := func(t *testing.T, response obj) {
		t.Helper()
		wm.Reset(t)
		wm.Stub(t, obj{
			"request": obj{
				"method":  "POST",
				"urlPath": "/v1/charges",
				"headers": obj{"Idempotency-Key": obj{"matches": "ord_[0-9a-f]{32}"}},
			},
			"response": response,
		})
	}

	postOrder := func(t *testing.T, traceID, body string) (int, map[string]any) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/orders", strings.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("traceparent", "00-"+traceID+"-00f067aa0ba902b7-01")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		var out map[string]any
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
		return resp.StatusCode, out
	}

	orderStatus := func(t *testing.T, id string) (status, failReason string) {
		t.Helper()
		require.NoError(t, db.QueryRowContext(ctx,
			`SELECT status, COALESCE(fail_reason, '') FROM orders WHERE id = $1`, id).Scan(&status, &failReason))
		return status, failReason
	}

	outboxEvents := func(t *testing.T, id string) []string {
		t.Helper()
		rows, err := db.QueryContext(ctx, `SELECT event_type FROM outbox WHERE aggregate_id = $1 ORDER BY id`, id)
		require.NoError(t, err)
		defer rows.Close()
		var out []string
		for rows.Next() {
			var e string
			require.NoError(t, rows.Scan(&e))
			out = append(out, e)
		}
		return out
	}

	// metric scrapes /metrics over HTTP, like Prometheus does, and returns one sample.
	metric := func(t *testing.T, series string) float64 {
		t.Helper()
		resp, err := http.Get(srv.URL + "/metrics")
		require.NoError(t, err)
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		m := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(series) + ` (\S+)`).FindSubmatch(body)
		if m == nil {
			return 0
		}
		v, err := strconv.ParseFloat(string(m[1]), 64)
		require.NoError(t, err)
		return v
	}

	// traceSpanNames waits until the server span has ended: the response can reach
	// the client a moment before otelhttp finishes the span.
	traceSpanNames := func(t *testing.T, traceID string, mustHave string) []string {
		t.Helper()
		var names []string
		require.Eventually(t, func() bool {
			names = names[:0]
			for _, s := range spans.Ended() {
				if s.SpanContext().TraceID().String() == traceID {
					names = append(names, s.Name())
				}
			}
			for _, n := range names {
				if n == mustHave {
					return true
				}
			}
			return false
		}, 5*time.Second, 10*time.Millisecond)
		return names
	}

	body := func(email string) string {
		return fmt.Sprintf(`{"customer_id":"c-1","customer_tier":"premium","email":%q,
"items":[{"product_id":"p-1","quantity":2,"price_cents":500}]}`, email)
	}

	t.Run("paid order: HTTP -> DB -> payment -> outbox -> relay, in one trace", func(t *testing.T) {
		pub.reset()
		stubCharge(t, obj{"status": 200, "jsonBody": obj{"payment_id": "pay-e2e"}})
		traceID := "4bf92f3577b34da6a3ce929d0e0e4736"

		code, resp := postOrder(t, traceID, body("john@example.com"))

		require.Equal(t, http.StatusCreated, code, "response: %v", resp)
		assert.Equal(t, "paid", resp["status"])
		assert.Equal(t, "pay-e2e", resp["payment_id"])
		orderID := resp["order_id"].(string)

		status, _ := orderStatus(t, orderID)
		assert.Equal(t, "paid", status)
		assert.Equal(t, []string{"order.created", "order.paid"}, outboxEvents(t, orderID))

		// The provider got exactly one charge, keyed by the order id.
		assert.Equal(t, 1, wm.CountRequests(t, obj{
			"method": "POST", "urlPath": "/v1/charges",
			"headers": obj{"Idempotency-Key": obj{"equalTo": orderID}},
		}))

		// The relay publishes the events and continues the caller's trace.
		n, err := application.Relay.ProcessOnce(ctx)
		require.NoError(t, err)
		assert.Equal(t, 2, n)
		assert.Equal(t, []string{"order.created", "order.paid"}, pub.events)
		assert.Equal(t, []string{traceID, traceID}, pub.traceIDs)

		// One trace from the API gateway's traceparent down to SQL, the payment call and the relay.
		names := traceSpanNames(t, traceID, "POST /orders")
		for _, want := range []string{
			"POST /orders", "CreateOrder",
			"CreateOrder.validation", "CreateOrder.payment", "CreateOrder.fulfillment",
			"HTTP POST", "sql.conn.exec",
			"outbox.publish order.created", "outbox.publish order.paid",
		} {
			assert.Contains(t, names, want)
		}

		assert.Equal(t, 1.0, metric(t, `orders_created_total{customer_tier="premium",status="success"}`))
		assert.Equal(t, 1.0, metric(t, `payment_requests_total{outcome="success"}`))

		assert.Contains(t, logs.String(), `"trace_id":"`+traceID+`"`)
		assert.NotContains(t, logs.String(), "john@example.com", "PII must not reach the logs")
	})

	t.Run("payment timeout: accepted as payment_pending, not failed", func(t *testing.T) {
		stubCharge(t, obj{"status": 200, "jsonBody": obj{"payment_id": "too-late"}, "fixedDelayMilliseconds": 1500})
		traceID := "5bf92f3577b34da6a3ce929d0e0e4736"

		code, resp := postOrder(t, traceID, body("jane@example.com"))

		require.Equal(t, http.StatusAccepted, code, "response: %v", resp)
		assert.Equal(t, "payment_pending", resp["status"])
		orderID := resp["order_id"].(string)

		status, failReason := orderStatus(t, orderID)
		assert.Equal(t, "payment_pending", status, "the charge may have happened: the order must not be failed")
		assert.Empty(t, failReason)
		assert.Equal(t, []string{"order.created", "order.payment_pending"}, outboxEvents(t, orderID))

		assert.Equal(t, 1.0, metric(t, `payment_requests_total{outcome="timeout"}`))
		assert.Equal(t, 1.0, metric(t, `orders_created_total{customer_tier="premium",status="pending"}`))
		assert.Contains(t, logs.String(), "create order pending payment confirmation")
	})

	t.Run("declined: 402, order failed with a reason", func(t *testing.T) {
		stubCharge(t, obj{"status": 402, "jsonBody": obj{"error": "insufficient_funds"}})

		code, resp := postOrder(t, "6bf92f3577b34da6a3ce929d0e0e4736", body("max@example.com"))

		require.Equal(t, http.StatusPaymentRequired, code, "response: %v", resp)
		assert.Equal(t, "failed", resp["status"])
		status, failReason := orderStatus(t, resp["order_id"].(string))
		assert.Equal(t, "failed", status)
		assert.Equal(t, "payment_declined", failReason)
		assert.Equal(t, 1.0, metric(t, `payment_requests_total{outcome="declined"}`))
	})

	t.Run("invalid order: 400, nothing persisted, payment not called", func(t *testing.T) {
		stubCharge(t, obj{"status": 200, "jsonBody": obj{"payment_id": "never"}})
		var before int
		require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM orders`).Scan(&before))

		code, resp := postOrder(t, "7bf92f3577b34da6a3ce929d0e0e4736", `{"customer_id":"c-1","customer_tier":"free","items":[]}`)

		assert.Equal(t, http.StatusBadRequest, code)
		assert.Contains(t, resp["error"], "order has no items")
		var after int
		require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM orders`).Scan(&after))
		assert.Equal(t, before, after)
		assert.Zero(t, wm.CountRequests(t, obj{"method": "POST", "urlPath": "/v1/charges"}))
	})
}
