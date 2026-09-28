package domain

import "time"

type Event interface {
	Name() string
	OccurredAt() time.Time
}

type OrderCreated struct {
	OrderID OrderID   `json:"order_id"`
	At      time.Time `json:"occurred_at"`
}

type OrderPaid struct {
	OrderID OrderID   `json:"order_id"`
	At      time.Time `json:"occurred_at"`
}

type OrderCompleted struct {
	OrderID OrderID   `json:"order_id"`
	At      time.Time `json:"occurred_at"`
}

type OrderFailed struct {
	OrderID OrderID   `json:"order_id"`
	Reason  string    `json:"reason"`
	At      time.Time `json:"occurred_at"`
}

func (e OrderCreated) Name() string            { return "order.created" }
func (e OrderCreated) OccurredAt() time.Time   { return e.At }
func (e OrderPaid) Name() string               { return "order.paid" }
func (e OrderPaid) OccurredAt() time.Time      { return e.At }
func (e OrderCompleted) Name() string          { return "order.completed" }
func (e OrderCompleted) OccurredAt() time.Time { return e.At }
func (e OrderFailed) Name() string             { return "order.failed" }
func (e OrderFailed) OccurredAt() time.Time    { return e.At }
