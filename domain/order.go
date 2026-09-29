// Package domain contains the pure business logic of an order.
// No context.Context, loggers, tracers or metrics here.
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
	// StatusPaymentPending means the charge outcome is unknown (e.g. the payment
	// provider timed out). The order is neither paid nor failed until reconciled.
	StatusPaymentPending Status = "payment_pending"
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

// Field is a mutable field of the aggregate. The aggregate records what changed,
// and the repository updates only those fields (UpdateMut). This is not infrastructure:
// the domain knows nothing about SQL or columns.
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

// Rehydrate restores an order from storage: no pending events, no changed fields.
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

// NewOrder creates an order. Time is passed in explicitly (from a Clock) to keep tests deterministic.
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

// MarkPaid is allowed from pending, and from payment_pending once reconciliation
// confirms the charge.
func (o *Order) MarkPaid(now time.Time) error {
	if o.Status != StatusPending && o.Status != StatusPaymentPending {
		return ErrInvalidTransition
	}
	o.Status = StatusPaid
	o.UpdatedAt = now
	o.touch(FieldStatus, FieldUpdatedAt)
	o.record(OrderPaid{OrderID: o.ID, At: now})
	return nil
}

// MarkPaymentPending records that the charge was attempted but its outcome is
// unknown. The money may or may not have been taken, so the order must not be failed.
func (o *Order) MarkPaymentPending(now time.Time) error {
	if o.Status != StatusPending {
		return ErrInvalidTransition
	}
	o.Status = StatusPaymentPending
	o.UpdatedAt = now
	o.touch(FieldStatus, FieldUpdatedAt)
	o.record(OrderPaymentPending{OrderID: o.ID, At: now})
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

// PullEvents returns the recorded domain events and clears them.
// The application layer turns them into span events, business logs and outbox rows.
func (o *Order) PullEvents() []Event {
	ev := o.events
	o.events = nil
	return ev
}

// Changes returns the fields changed since creation, loading or the last save,
// in a stable order.
func (o *Order) Changes() []Field {
	var out []Field
	for _, f := range []Field{FieldStatus, FieldFailReason, FieldUpdatedAt} {
		if _, ok := o.changed[f]; ok {
			out = append(out, f)
		}
	}
	return out
}

// MarkPersisted clears the change list after a successful save.
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
