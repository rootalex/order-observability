package integration

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/rootalex/order-observability/payment"
	"github.com/rootalex/order-observability/testing/setup"
	"github.com/rootalex/order-observability/usecase"
)

type obj = map[string]any

// chargeRequest is the contract with the Payment API. If the client serializes differently,
// WireMock finds no stub, answers 404 and the test fails. A port mock cannot catch this.
var chargeRequest = obj{
	"method":  "POST",
	"urlPath": "/v1/charges",
	"headers": obj{
		"Content-Type":    obj{"equalTo": "application/json"},
		"Idempotency-Key": obj{"equalTo": "ord-1"},
	},
	"bodyPatterns": []obj{
		{"equalToJson": obj{"order_id": "ord-1", "amount_cents": 1500, "currency": "USD"}},
	},
}

func TestPaymentClient_ExternalAPI(t *testing.T) {
	ctx := context.Background()
	wm := setup.StartWireMock(ctx, t)

	otel.SetTextMapPropagator(propagation.TraceContext{})
	req := usecase.ChargeRequest{OrderID: "ord-1", AmountCents: 1500, Currency: "USD", IdempotencyKey: "ord-1"}
	client := payment.NewClient(wm.BaseURL, 2*time.Second)

	stub := func(t *testing.T, request, response obj) {
		wm.Reset(t)
		wm.Stub(t, obj{"request": request, "response": response})
	}

	t.Run("success", func(t *testing.T) {
		stub(t, chargeRequest, obj{"status": 200, "jsonBody": obj{"payment_id": "pay-123"}})

		res, err := client.Charge(ctx, req)
		require.NoError(t, err)
		assert.Equal(t, "pay-123", res.PaymentID)
		assert.Equal(t, 1, wm.CountRequests(t, chargeRequest))
	})

	t.Run("request contract mismatch is caught", func(t *testing.T) {
		stub(t, chargeRequest, obj{"status": 200, "jsonBody": obj{"payment_id": "pay-123"}})

		wrong := req
		wrong.AmountCents = 1499
		_, err := client.Charge(ctx, wrong)
		assert.ErrorContains(t, err, "unexpected status 404")
	})

	t.Run("declined", func(t *testing.T) {
		stub(t, chargeRequest, obj{"status": 402, "jsonBody": obj{"error": "insufficient_funds"}})

		_, err := client.Charge(ctx, req)
		assert.ErrorIs(t, err, usecase.ErrPaymentDeclined)
	})

	t.Run("server error", func(t *testing.T) {
		stub(t, chargeRequest, obj{"status": 500})

		_, err := client.Charge(ctx, req)
		require.Error(t, err)
		assert.NotErrorIs(t, err, usecase.ErrPaymentDeclined, "5xx is not a business decline")
	})

	t.Run("malformed response", func(t *testing.T) {
		stub(t, chargeRequest, obj{"status": 200, "body": "not json"})

		_, err := client.Charge(ctx, req)
		assert.ErrorContains(t, err, "decode payment response")
	})

	t.Run("timeout", func(t *testing.T) {
		stub(t, chargeRequest, obj{"status": 200, "jsonBody": obj{"payment_id": "late"}, "fixedDelayMilliseconds": 2000})

		fast := payment.NewClient(wm.BaseURL, 200*time.Millisecond)
		start := time.Now()
		_, err := fast.Charge(ctx, req)

		var netErr net.Error
		require.True(t, errors.As(err, &netErr) && netErr.Timeout(), "want timeout error, got %v", err)
		assert.Less(t, time.Since(start), 2*time.Second, "client must not wait for the slow response")
	})

	t.Run("propagates traceparent", func(t *testing.T) {
		spanCtx, span := sdktrace.NewTracerProvider().Tracer("test").Start(ctx, "charge")
		defer span.End()

		withTrace := obj{}
		for k, v := range chargeRequest {
			withTrace[k] = v
		}
		withTrace["headers"] = obj{
			"traceparent": obj{"matches": "00-" + span.SpanContext().TraceID().String() + "-[0-9a-f]{16}-01"},
		}
		stub(t, withTrace, obj{"status": 200, "jsonBody": obj{"payment_id": "pay-traced"}})

		res, err := client.Charge(spanCtx, req)
		require.NoError(t, err)
		assert.Equal(t, "pay-traced", res.PaymentID)
	})
}
