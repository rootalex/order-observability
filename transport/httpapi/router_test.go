package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"

	"github.com/rootalex/order-observability/domain"
	"github.com/rootalex/order-observability/observability"
	"github.com/rootalex/order-observability/usecase"
)

type fakeInteractor struct {
	got  *usecase.CreateOrderRequest
	resp *usecase.CreateOrderResponse
	err  error
}

func (f *fakeInteractor) Execute(_ context.Context, req *usecase.CreateOrderRequest) (*usecase.CreateOrderResponse, error) {
	f.got = req
	return f.resp, f.err
}

const validBody = `{"customer_id":"c-1","customer_tier":"premium","email":"john@example.com",
"items":[{"product_id":"p-1","quantity":2,"price_cents":500}]}`

func newTestRouter(uc usecase.CreateOrderInteractor, logs io.Writer) http.Handler {
	metrics := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "# metrics") })
	return NewRouter(uc, observability.NewLogger(logs, nil), metrics)
}

func do(t *testing.T, h http.Handler, method, path, body string, header http.Header) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range header {
		req.Header[k] = v
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestCreateOrder_StatusMapping(t *testing.T) {
	paidResp := &usecase.CreateOrderResponse{OrderID: "ord-1", Status: domain.StatusPaid, PaymentID: "pay-1"}
	failedResp := &usecase.CreateOrderResponse{OrderID: "ord-1", Status: domain.StatusFailed}

	tests := []struct {
		name     string
		body     string
		uc       *fakeInteractor
		wantCode int
		wantBody string
	}{
		{
			name:     "created",
			body:     validBody,
			uc:       &fakeInteractor{resp: paidResp},
			wantCode: http.StatusCreated,
			wantBody: `{"order_id":"ord-1","status":"paid","payment_id":"pay-1"}`,
		},
		{
			name:     "invalid json",
			body:     `{"customer_id":`,
			uc:       &fakeInteractor{},
			wantCode: http.StatusBadRequest,
			wantBody: `{"error":"invalid json"}`,
		},
		{
			name:     "domain validation error",
			body:     `{"customer_id":"c-1","items":[]}`,
			uc:       &fakeInteractor{err: fmt.Errorf("new order: %w", domain.ErrEmptyOrder)},
			wantCode: http.StatusBadRequest,
			wantBody: `{"error":"new order: order has no items"}`,
		},
		{
			name:     "payment declined returns the failed order",
			body:     validBody,
			uc:       &fakeInteractor{resp: failedResp, err: fmt.Errorf("charge: %w", usecase.ErrPaymentDeclined)},
			wantCode: http.StatusPaymentRequired,
			wantBody: `{"order_id":"ord-1","status":"failed"}`,
		},
		{
			name:     "internal error does not leak details",
			body:     validBody,
			uc:       &fakeInteractor{err: errors.New("update order: pq: connection refused to 10.0.0.5")},
			wantCode: http.StatusInternalServerError,
			wantBody: `{"error":"internal error"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := do(t, newTestRouter(tt.uc, io.Discard), http.MethodPost, "/orders", tt.body, nil)

			assert.Equal(t, tt.wantCode, rec.Code)
			assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
			assert.JSONEq(t, tt.wantBody, rec.Body.String())
		})
	}
}

func TestCreateOrder_MapsRequestToUsecase(t *testing.T) {
	uc := &fakeInteractor{resp: &usecase.CreateOrderResponse{OrderID: "ord-1", Status: domain.StatusPaid}}

	do(t, newTestRouter(uc, io.Discard), http.MethodPost, "/orders", validBody, nil)

	require.NotNil(t, uc.got)
	assert.Equal(t, &usecase.CreateOrderRequest{
		CustomerID: "c-1",
		Tier:       domain.TierPremium,
		Email:      "john@example.com",
		Items:      []domain.Item{{ProductID: "p-1", Quantity: 2, PriceCents: 500}},
	}, uc.got)
}

func TestRouter_Routes(t *testing.T) {
	h := newTestRouter(&fakeInteractor{}, io.Discard)

	assert.Equal(t, http.StatusOK, do(t, h, http.MethodGet, "/healthz", "", nil).Code)
	assert.Equal(t, "# metrics", do(t, h, http.MethodGet, "/metrics", "", nil).Body.String())
	assert.Equal(t, http.StatusMethodNotAllowed, do(t, h, http.MethodGet, "/orders", "", nil).Code)
	assert.Equal(t, http.StatusNotFound, do(t, h, http.MethodPost, "/unknown", "", nil).Code)
}

func TestAccessLog_TraceIDFromCallerAndNoPII(t *testing.T) {
	otel.SetTextMapPropagator(propagation.TraceContext{})
	var logs bytes.Buffer
	uc := &fakeInteractor{err: errors.New("db down")}
	header := http.Header{"Traceparent": {"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"}}

	do(t, newTestRouter(uc, &logs), http.MethodPost, "/orders", validBody, header)

	var entry map[string]any
	require.NoError(t, json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &entry))
	assert.Equal(t, "http request", entry["msg"])
	assert.Equal(t, slog.LevelError.String(), entry["level"], "5xx is logged as ERROR")
	assert.EqualValues(t, http.StatusInternalServerError, entry["status"])
	assert.Equal(t, "4bf92f3577b34da6a3ce929d0e0e4736", entry["trace_id"], "trace continues from API GW")
	assert.NotContains(t, logs.String(), "john@example.com", "request body must not be logged")
}

func TestAccessLog_SkipsInfraEndpoints(t *testing.T) {
	var logs bytes.Buffer
	h := newTestRouter(&fakeInteractor{}, &logs)

	do(t, h, http.MethodGet, "/healthz", "", nil)
	do(t, h, http.MethodGet, "/metrics", "", nil)

	assert.Empty(t, logs.String(), "probes and scrapes would flood the logs")
}
