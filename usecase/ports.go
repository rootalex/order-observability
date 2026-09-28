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

// OrderRepository — порт хранилища. ctx нужен для отмены запросов и проброса трейса.
// events сохраняются в outbox в той же транзакции, что и заказ.
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

// PaymentGateway — порт внешнего платёжного сервиса.
type PaymentGateway interface {
	Charge(ctx context.Context, req ChargeRequest) (ChargeResult, error)
}

type IDGenerator interface {
	NewOrderID() domain.OrderID
}
