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
	Email      string // PII: не должен попадать в логи
}

type CreateOrderResponse struct {
	OrderID   domain.OrderID
	Status    domain.Status
	PaymentID string
	Events    []domain.Event
}

// CreateOrderInteractor — интерфейс, который оборачивают декораторы
// observability (TracedInteractor, LoggingInteractor, MetricsInteractor).
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

	// TODO(outbox): списание и смена статуса должны быть согласованы (idempotency key + outbox/saga).
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
	if chargeErr != nil {
		return resp, fmt.Errorf("charge: %w", chargeErr)
	}
	return resp, nil
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

// finalize фиксирует итоговый статус заказа. Здесь же будет резервирование в Inventory Service.
func (uc *createOrder) finalize(ctx context.Context, order *domain.Order, chargeErr error) (events []domain.Event, err error) {
	ctx, done := uc.probe.StepStarted(ctx, StepFulfillment)
	defer func() { done(err) }()

	now := uc.clock.Now()
	switch {
	case errors.Is(chargeErr, ErrPaymentDeclined):
		err = order.Fail("payment_declined", now)
	case chargeErr != nil:
		err = order.Fail("payment_error", now)
	default:
		err = order.MarkPaid(now)
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
