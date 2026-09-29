// Package payment is the HTTP adapter for PaymentGateway. In tests, WireMock stands in for the API.
package payment

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/rootalex/order-observability/observability"
	"github.com/rootalex/order-observability/usecase"
)

type Client struct {
	baseURL string
	http    *http.Client
}

func NewClient(baseURL string, timeout time.Duration) *Client {
	// otelhttp.NewTransport creates a client span and propagates traceparent to the Payment Service.
	return &Client{baseURL: baseURL, http: &http.Client{
		Timeout:   timeout,
		Transport: otelhttp.NewTransport(http.DefaultTransport),
	}}
}

type chargeRequest struct {
	OrderID     string `json:"order_id"`
	AmountCents int64  `json:"amount_cents"`
	Currency    string `json:"currency"`
}

type chargeResponse struct {
	PaymentID string `json:"payment_id"`
}

func (c *Client) Charge(ctx context.Context, req usecase.ChargeRequest) (usecase.ChargeResult, error) {
	body, err := json.Marshal(chargeRequest{
		OrderID:     string(req.OrderID),
		AmountCents: req.AmountCents,
		Currency:    req.Currency,
	})
	if err != nil {
		return usecase.ChargeResult{}, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/charges", bytes.NewReader(body))
	if err != nil {
		return usecase.ChargeResult{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Idempotency-Key", req.IdempotencyKey)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return usecase.ChargeResult{}, observability.WithStack(fmt.Errorf("payment request: %w", err))
	}
	defer resp.Body.Close()

	// 402 and other 4xx are definitive: the charge did not happen. 5xx, timeouts,
	// network errors and unreadable responses are not: the usecase treats them as
	// an unknown outcome.
	switch {
	case resp.StatusCode == http.StatusPaymentRequired:
		return usecase.ChargeResult{}, usecase.ErrPaymentDeclined
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		return usecase.ChargeResult{}, observability.WithStack(fmt.Errorf("%w: status %d", usecase.ErrPaymentRejected, resp.StatusCode))
	case resp.StatusCode != http.StatusOK:
		return usecase.ChargeResult{}, observability.WithStack(fmt.Errorf("payment api: unexpected status %d", resp.StatusCode))
	}

	var out chargeResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return usecase.ChargeResult{}, observability.WithStack(fmt.Errorf("decode payment response: %w", err))
	}
	return usecase.ChargeResult{PaymentID: out.PaymentID}, nil
}
