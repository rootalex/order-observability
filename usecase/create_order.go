package usecase

import (
	"context"
	"errors"
	"fmt"

	"github.com/rootalex/order-observability/clock"
	"github.com/rootalex/order-observability/domain"
)

type CreateOrderRequest struct {
	CustomerID domain.CustomerID
	Tier       domain.CustomerTier
	Items      []domain.Item
	Email      string // PII: must never reach the logs
}

type CreateOrderResponse struct {
	OrderID   domain.OrderID
	Status    domain.Status
	PaymentID string
	Events    []domain.Event
}

// CreateOrderInteractor is the interface wrapped by the observability decorators
// (TracedInteractor, LoggingInteractor, MetricsInteractor).
type CreateOrderInteractor interface {
	Execute(ctx context.Context, req *CreateOrderRequest) (*CreateOrderResponse, error)
}

type createOrder struct {
	repo    OrderRepository
	payment PaymentGateway
	ids     IDGenerator
	clock   clock.Clock
	probe   Probe
}

func NewCreateOrder(repo OrderRepository, payment PaymentGateway, ids IDGenerator, clk clock.Clock, probe Probe) CreateOrderInteractor {
	if probe == nil {
		probe = noopProbe{}
	}
	return &createOrder{repo: repo, payment: payment, ids: ids, clock: clk, probe: probe}
}

func (uc *createOrder) Execute(ctx context.Context, req *CreateOrderRequest) (*CreateOrderResponse, error) {
	order, events, err := uc.validate(ctx, req)
	if err != nil {
		return nil, err
	}

	paymentID, chargeErr := uc.charge(ctx, order)

	finalEvents, err := uc.finalize(ctx, order, chargeErr)
	if err != nil {
		return nil, err
	}

	resp := &CreateOrderResponse{
		OrderID:   order.ID,
		Status:    order.Status,
		PaymentID: paymentID,
		Events:    append(events, finalEvents...),
	}
	switch {
	case chargeErr == nil:
		return resp, nil
	case order.Status == domain.StatusPaymentPending:
		return resp, fmt.Errorf("%w: %w", ErrPaymentPending, chargeErr)
	default:
		return resp, fmt.Errorf("charge: %w", chargeErr)
	}
}

func (uc *createOrder) validate(ctx context.Context, req *CreateOrderRequest) (order *domain.Order, events []domain.Event, err error) {
	ctx, done := uc.probe.StepStarted(ctx, StepValidation)
	defer func() { done(err) }()

	order, err = domain.NewOrder(uc.ids.NewOrderID(), req.CustomerID, req.Tier, req.Items, uc.clock.Now())
	if err != nil {
		return nil, nil, fmt.Errorf("new order: %w", err)
	}
	events = order.PullEvents()
	if err = uc.repo.Create(ctx, order, events); err != nil {
		return nil, nil, fmt.Errorf("save order: %w", err)
	}
	return order, events, nil
}

func (uc *createOrder) charge(ctx context.Context, order *domain.Order) (paymentID string, err error) {
	ctx, done := uc.probe.StepStarted(ctx, StepPayment)
	defer func() { done(err) }()

	res, err := uc.payment.Charge(ctx, ChargeRequest{
		OrderID:        order.ID,
		AmountCents:    order.TotalCents(),
		Currency:       "USD",
		IdempotencyKey: string(order.ID),
	})
	return res.PaymentID, err
}

// finalize persists the final order status. Inventory Service reservation belongs here as well.
func (uc *createOrder) finalize(ctx context.Context, order *domain.Order, chargeErr error) (events []domain.Event, err error) {
	ctx, done := uc.probe.StepStarted(ctx, StepFulfillment)
	defer func() { done(err) }()

	now := uc.clock.Now()
	switch {
	case chargeErr == nil:
		err = order.MarkPaid(now)
	case errors.Is(chargeErr, ErrPaymentDeclined):
		err = order.Fail("payment_declined", now)
	case errors.Is(chargeErr, ErrPaymentRejected):
		err = order.Fail("payment_rejected", now)
	default:
		// Timeout, network error, 5xx: the provider may have charged the customer.
		// Failing the order here is exactly the "charged but shows as failed" bug
		// (ANSWERS.md Q3). Leave it pending; reconciliation resolves it by the
		// idempotency key, which is the order id.
		err = order.MarkPaymentPending(now)
	}
	if err != nil {
		return nil, err
	}
	events = order.PullEvents()
	if err = uc.repo.Update(ctx, order, events); err != nil {
		return nil, fmt.Errorf("update order: %w", err)
	}
	return events, nil
}
