// Package domain содержит чистую бизнес-логику заказа.
// Здесь нет context.Context, логгеров, трейсеров и метрик.
package domain

import (
	"errors"
	"time"
)

type (
	OrderID      string
	CustomerID   string
	Status       string
	CustomerTier string
)

const (
	StatusPending   Status = "pending"
	StatusPaid      Status = "paid"
	StatusCompleted Status = "completed"
	StatusFailed    Status = "failed"
)

const (
	TierFree    CustomerTier = "free"
	TierPremium CustomerTier = "premium"
)

var (
	ErrEmptyOrder        = errors.New("order has no items")
	ErrInvalidQuantity   = errors.New("item quantity must be positive")
	ErrInvalidTransition = errors.New("invalid order status transition")
)

// Field — изменяемое поле агрегата. Агрегат сам отмечает, что изменилось,
// а репозиторий обновляет только эти поля (UpdateMut). Это не инфраструктура:
// домен не знает ни про SQL, ни про колонки.
type Field string

const (
	FieldStatus     Field = "status"
	FieldFailReason Field = "fail_reason"
	FieldUpdatedAt  Field = "updated_at"
)

type Item struct {
	ProductID  string
	Quantity   int
	PriceCents int64
}

type Order struct {
	ID         OrderID
	CustomerID CustomerID
	Tier       CustomerTier
	Items      []Item
	Status     Status
	FailReason string
	CreatedAt  time.Time
	UpdatedAt  time.Time

	events  []Event
	changed map[Field]struct{}
}

// Rehydrate восстанавливает заказ из хранилища: без событий и без изменённых полей.
func Rehydrate(id OrderID, customerID CustomerID, tier CustomerTier, items []Item, status Status,
	failReason string, createdAt, updatedAt time.Time) *Order {
	return &Order{
		ID:         id,
		CustomerID: customerID,
		Tier:       tier,
		Items:      items,
		Status:     status,
		FailReason: failReason,
		CreatedAt:  createdAt,
		UpdatedAt:  updatedAt,
	}
}

// NewOrder создаёт заказ. Время передаётся явно (из Clock), чтобы тесты были детерминированными.
func NewOrder(id OrderID, customerID CustomerID, tier CustomerTier, items []Item, now time.Time) (*Order, error) {
	if len(items) == 0 {
		return nil, ErrEmptyOrder
	}
	for _, it := range items {
		if it.Quantity <= 0 {
			return nil, ErrInvalidQuantity
		}
	}
	o := &Order{
		ID:         id,
		CustomerID: customerID,
		Tier:       tier,
		Items:      items,
		Status:     StatusPending,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	o.record(OrderCreated{OrderID: id, At: now})
	return o, nil
}

func (o *Order) TotalCents() int64 {
	var total int64
	for _, it := range o.Items {
		total += it.PriceCents * int64(it.Quantity)
	}
	return total
}

func (o *Order) MarkPaid(now time.Time) error {
	if o.Status != StatusPending {
		return ErrInvalidTransition
	}
	o.Status = StatusPaid
	o.UpdatedAt = now
	o.touch(FieldStatus, FieldUpdatedAt)
	o.record(OrderPaid{OrderID: o.ID, At: now})
	return nil
}

func (o *Order) Complete(now time.Time) error {
	if o.Status != StatusPaid {
		return ErrInvalidTransition
	}
	o.Status = StatusCompleted
	o.UpdatedAt = now
	o.touch(FieldStatus, FieldUpdatedAt)
	o.record(OrderCompleted{OrderID: o.ID, At: now})
	return nil
}

func (o *Order) Fail(reason string, now time.Time) error {
	if o.Status == StatusCompleted || o.Status == StatusFailed {
		return ErrInvalidTransition
	}
	o.Status = StatusFailed
	o.FailReason = reason
	o.UpdatedAt = now
	o.touch(FieldStatus, FieldFailReason, FieldUpdatedAt)
	o.record(OrderFailed{OrderID: o.ID, Reason: reason, At: now})
	return nil
}

// PullEvents отдаёт накопленные доменные события и очищает их.
// Слой приложения превращает их в span events, бизнес-логи и outbox-записи.
func (o *Order) PullEvents() []Event {
	ev := o.events
	o.events = nil
	return ev
}

// Changes возвращает поля, изменённые с момента создания/загрузки/последнего сохранения,
// в стабильном порядке.
func (o *Order) Changes() []Field {
	var out []Field
	for _, f := range []Field{FieldStatus, FieldFailReason, FieldUpdatedAt} {
		if _, ok := o.changed[f]; ok {
			out = append(out, f)
		}
	}
	return out
}

// MarkPersisted сбрасывает список изменений после успешного сохранения.
func (o *Order) MarkPersisted() { o.changed = nil }

func (o *Order) record(e Event) { o.events = append(o.events, e) }

func (o *Order) touch(fields ...Field) {
	if o.changed == nil {
		o.changed = make(map[Field]struct{}, len(fields))
	}
	for _, f := range fields {
		o.changed[f] = struct{}{}
	}
}
