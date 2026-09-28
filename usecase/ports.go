package usecase

import (
	"context"
	"errors"

	"github.com/rootalex/order-observability/domain"
)

var (
	ErrPaymentDeclined    = errors.New("payment declined")
	ErrOrderNotFound      = errors.New("order not found")
	ErrOrderAlreadyExists = errors.New("order already exists")
)

// OrderRepository is the storage port. ctx is needed for cancellation and trace propagation.
// events are written to the outbox in the same transaction as the order.
type OrderRepository interface {
	Create(ctx context.Context, o *domain.Order, events []domain.Event) error
	Update(ctx context.Context, o *domain.Order, events []domain.Event) error
	Get(ctx context.Context, id domain.OrderID) (*domain.Order, error)
}

type ChargeRequest struct {
	OrderID        domain.OrderID
	AmountCents    int64
	Currency       string
	IdempotencyKey string
}

type ChargeResult struct {
	PaymentID string
}

// PaymentGateway is the port to the external payment service.
type PaymentGateway interface {
	Charge(ctx context.Context, req ChargeRequest) (ChargeResult, error)
}

type IDGenerator interface {
	NewOrderID() domain.OrderID
}
