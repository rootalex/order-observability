// Package httpapi is the HTTP boundary of the Order Service: request parsing, status codes,
// trace context extraction and access logs.
package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/rootalex/order-observability/domain"
	"github.com/rootalex/order-observability/usecase"
)

func NewRouter(uc usecase.CreateOrderInteractor, log *slog.Logger, metrics http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("POST /orders", &createOrderHandler{uc: uc})
	mux.Handle("GET /metrics", metrics)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	// otelhttp is outermost: it extracts traceparent from API GW and starts the server span,
	// so the access log inside already has trace_id.
	return otelhttp.NewHandler(accessLog(log, mux), "http.server",
		otelhttp.WithFilter(func(r *http.Request) bool { return !isInfraPath(r.URL.Path) }),
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string { return r.Method + " " + r.URL.Path }),
	)
}

func isInfraPath(p string) bool { return p == "/metrics" || p == "/healthz" }

type createOrderRequest struct {
	CustomerID   string `json:"customer_id"`
	CustomerTier string `json:"customer_tier"`
	Email        string `json:"email"`
	Items        []struct {
		ProductID  string `json:"product_id"`
		Quantity   int    `json:"quantity"`
		PriceCents int64  `json:"price_cents"`
	} `json:"items"`
}

type createOrderResponse struct {
	OrderID   string `json:"order_id"`
	Status    string `json:"status"`
	PaymentID string `json:"payment_id,omitempty"`
}

type errorResponse struct {
	Error string `json:"error"`
}

type createOrderHandler struct {
	uc usecase.CreateOrderInteractor
}

func (h *createOrderHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var in createOrderRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid json"})
		return
	}

	req := &usecase.CreateOrderRequest{
		CustomerID: domain.CustomerID(in.CustomerID),
		Tier:       domain.CustomerTier(in.CustomerTier),
		Email:      in.Email,
	}
	for _, it := range in.Items {
		req.Items = append(req.Items, domain.Item{ProductID: it.ProductID, Quantity: it.Quantity, PriceCents: it.PriceCents})
	}

	resp, err := h.uc.Execute(r.Context(), req)
	switch {
	case errors.Is(err, domain.ErrEmptyOrder), errors.Is(err, domain.ErrInvalidQuantity):
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
	case errors.Is(err, usecase.ErrPaymentDeclined) && resp != nil:
		writeJSON(w, http.StatusPaymentRequired, toResponse(resp))
	case err != nil:
		// Error details go to logs and traces, never to the client.
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal error"})
	default:
		writeJSON(w, http.StatusCreated, toResponse(resp))
	}
}

func toResponse(r *usecase.CreateOrderResponse) createOrderResponse {
	return createOrderResponse{OrderID: string(r.OrderID), Status: string(r.Status), PaymentID: r.PaymentID}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// accessLog writes an operational log entry for every request at the HTTP boundary.
func accessLog(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isInfraPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		level := slog.LevelInfo
		if rec.status >= http.StatusInternalServerError {
			level = slog.LevelError
		}
		log.Log(r.Context(), level, "http request",
			slog.String("log_type", "operational"),
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", rec.status),
			slog.Int64("duration_ms", time.Since(start).Milliseconds()),
		)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}
