package domain

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var now = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

func newTestOrder(t *testing.T) *Order {
	t.Helper()
	o, err := NewOrder("ord-1", "c-1", TierFree, []Item{{ProductID: "p-1", Quantity: 2, PriceCents: 150}}, now)
	require.NoError(t, err)
	return o
}

func TestNewOrder_Validation(t *testing.T) {
	_, err := NewOrder("ord-1", "c-1", TierFree, nil, now)
	assert.ErrorIs(t, err, ErrEmptyOrder)

	_, err = NewOrder("ord-1", "c-1", TierFree, []Item{{ProductID: "p-1", Quantity: 0}}, now)
	assert.ErrorIs(t, err, ErrInvalidQuantity)
}

func TestOrder_Transitions(t *testing.T) {
	later := now.Add(time.Minute)

	t.Run("pending -> paid -> completed", func(t *testing.T) {
		o := newTestOrder(t)
		require.NoError(t, o.MarkPaid(later))
		require.NoError(t, o.Complete(later))
		assert.Equal(t, StatusCompleted, o.Status)
		assert.Equal(t, later, o.UpdatedAt)
	})

	t.Run("cannot complete unpaid order", func(t *testing.T) {
		assert.ErrorIs(t, newTestOrder(t).Complete(later), ErrInvalidTransition)
	})

	t.Run("cannot fail completed order", func(t *testing.T) {
		o := newTestOrder(t)
		require.NoError(t, o.MarkPaid(later))
		require.NoError(t, o.Complete(later))
		assert.ErrorIs(t, o.Fail("late", later), ErrInvalidTransition)
	})
}

func TestOrder_EventsAndChanges(t *testing.T) {
	o := newTestOrder(t)
	assert.Equal(t, int64(300), o.TotalCents())
	assert.Empty(t, o.Changes(), "new order is inserted as a whole, no field-level changes")

	require.NoError(t, o.Fail("payment_error", now))
	assert.Equal(t, []Field{FieldStatus, FieldFailReason, FieldUpdatedAt}, o.Changes())

	events := o.PullEvents()
	require.Len(t, events, 2)
	assert.Equal(t, "order.created", events[0].Name())
	assert.Equal(t, OrderFailed{OrderID: "ord-1", Reason: "payment_error", At: now}, events[1])
	assert.Empty(t, o.PullEvents(), "events are pulled once")

	o.MarkPersisted()
	assert.Empty(t, o.Changes())
}
