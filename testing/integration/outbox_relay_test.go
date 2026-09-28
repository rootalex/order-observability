package integration

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/rootalex/order-observability/clock"
	"github.com/rootalex/order-observability/domain"
	"github.com/rootalex/order-observability/outbox"
	"github.com/rootalex/order-observability/repo/postgres"
	"github.com/rootalex/order-observability/testing/setup"
)

// recordingPublisher is the broker stand-in: the database is real, the broker
// is not the subject of these tests.
type recordingPublisher struct {
	mu        sync.Mutex
	published []outbox.Message
	traceIDs  []string
	// hook runs before a message is accepted; a non-nil error fails the publish.
	hook func(m outbox.Message) error
}

func (p *recordingPublisher) Publish(ctx context.Context, m outbox.Message) error {
	if p.hook != nil {
		if err := p.hook(m); err != nil {
			return err
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.published = append(p.published, m)
	p.traceIDs = append(p.traceIDs, trace.SpanContextFromContext(ctx).TraceID().String())
	return nil
}

func (p *recordingPublisher) ids() []int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]int64, 0, len(p.published))
	for _, m := range p.published {
		out = append(out, m.ID)
	}
	return out
}

func TestOutboxRelay_Integration(t *testing.T) {
	ctx := context.Background()
	db := setup.StartPostgres(ctx, t)
	repo := postgres.NewOrderRepo(db)
	clk := clock.NewFake(baseTime)
	tp := sdktrace.NewTracerProvider()
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))

	newRelay := func(p outbox.Publisher, batch int) *outbox.Relay {
		return outbox.NewRelay(db, p, clk, tp, discard, batch)
	}

	// Each subtest starts from an empty outbox with ids from 1.
	reset := func(t *testing.T) {
		t.Helper()
		_, err := db.ExecContext(ctx, `TRUNCATE outbox, order_items, orders RESTART IDENTITY`)
		require.NoError(t, err)
	}

	// createOrders writes n orders; each one adds a single order.created event.
	createOrders := func(t *testing.T, ctx context.Context, n int) {
		t.Helper()
		for i := 1; i <= n; i++ {
			o := newOrder(t, clk, domain.OrderID(fmt.Sprintf("ord-%02d", i)))
			require.NoError(t, repo.Create(ctx, o, o.PullEvents()))
		}
	}

	pending := func(t *testing.T) []int64 {
		t.Helper()
		rows, err := db.QueryContext(ctx, `SELECT id FROM outbox WHERE processed_at IS NULL ORDER BY id`)
		require.NoError(t, err)
		defer rows.Close()
		var ids []int64
		for rows.Next() {
			var id int64
			require.NoError(t, rows.Scan(&id))
			ids = append(ids, id)
		}
		return ids
	}

	t.Run("publishes pending events in order and marks them processed", func(t *testing.T) {
		reset(t)
		createOrders(t, ctx, 3)
		pub := &recordingPublisher{}
		relay := newRelay(pub, 10)

		n, err := relay.ProcessOnce(ctx)
		require.NoError(t, err)
		assert.Equal(t, 3, n)
		assert.Equal(t, []int64{1, 2, 3}, pub.ids())
		assert.Equal(t, "ord-01", pub.published[0].AggregateID)
		assert.Equal(t, "order.created", pub.published[0].EventType)
		assert.JSONEq(t, `{"order_id":"ord-01","occurred_at":"2026-09-28T12:00:00.123456789Z"}`, string(pub.published[0].Payload))
		assert.Empty(t, pending(t))

		var processedAt time.Time
		require.NoError(t, db.QueryRowContext(ctx, `SELECT processed_at FROM outbox WHERE id = 1`).Scan(&processedAt))
		assert.Equal(t, clk.Now().Truncate(time.Microsecond), processedAt.UTC(), "processed_at comes from the injected clock")

		n, err = relay.ProcessOnce(ctx)
		require.NoError(t, err)
		assert.Zero(t, n, "second run finds nothing")
		assert.Len(t, pub.published, 3, "no event is published twice")
	})

	t.Run("failed publish keeps the event and the order for the next run", func(t *testing.T) {
		reset(t)
		createOrders(t, ctx, 3)
		brokerDown := true
		pub := &recordingPublisher{hook: func(m outbox.Message) error {
			if m.ID == 2 && brokerDown {
				return errors.New("broker unavailable")
			}
			return nil
		}}
		relay := newRelay(pub, 10)

		n, err := relay.ProcessOnce(ctx)
		assert.ErrorContains(t, err, "broker unavailable")
		assert.Equal(t, 1, n, "event 1 was published before the failure and is marked")
		assert.Equal(t, []int64{2, 3}, pending(t), "event 3 is not published ahead of event 2")

		brokerDown = false
		n, err = relay.ProcessOnce(ctx)
		require.NoError(t, err)
		assert.Equal(t, 2, n)
		assert.Equal(t, []int64{1, 2, 3}, pub.ids(), "at-least-once, in order, no duplicates of 1")
		assert.Empty(t, pending(t))
	})

	t.Run("continues the trace of the request that wrote the event", func(t *testing.T) {
		reset(t)
		reqCtx, reqSpan := tp.Tracer("test").Start(ctx, "POST /orders")
		createOrders(t, reqCtx, 1)
		reqSpan.End()

		pub := &recordingPublisher{}
		_, err := newRelay(pub, 10).ProcessOnce(ctx) // note: a fresh ctx without any span
		require.NoError(t, err)

		require.Len(t, pub.traceIDs, 1)
		assert.Equal(t, reqSpan.SpanContext().TraceID().String(), pub.traceIDs[0])
	})

	t.Run("parallel relays never publish the same event twice", func(t *testing.T) {
		reset(t)
		createOrders(t, ctx, 10)

		// Relay A locks events 1..5 and blocks inside its transaction.
		started, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		pubA := &recordingPublisher{hook: func(outbox.Message) error {
			once.Do(func() { close(started); <-release })
			return nil
		}}
		type result struct {
			n   int
			err error
		}
		doneA := make(chan result, 1)
		go func() {
			n, err := newRelay(pubA, 5).ProcessOnce(ctx)
			doneA <- result{n, err}
		}()

		select {
		case <-started:
		case <-time.After(10 * time.Second):
			t.Fatal("relay A did not start publishing")
		}

		// Relay B runs while A holds its locks: SKIP LOCKED gives it the next rows.
		// Without SKIP LOCKED, B would block on A's rows: bound it instead of hanging.
		ctxB, cancelB := context.WithTimeout(ctx, 5*time.Second)
		defer cancelB()
		pubB := &recordingPublisher{}
		n, err := newRelay(pubB, 5).ProcessOnce(ctxB)
		require.NoError(t, err, "relay B must not wait for rows locked by A")
		assert.Equal(t, 5, n)
		assert.Equal(t, []int64{6, 7, 8, 9, 10}, pubB.ids())

		close(release)
		var resA result
		select {
		case resA = <-doneA:
		case <-time.After(10 * time.Second):
			t.Fatal("relay A did not finish")
		}
		require.NoError(t, resA.err)
		assert.Equal(t, []int64{1, 2, 3, 4, 5}, pubA.ids())
		assert.Empty(t, pending(t))
	})

	t.Run("Run drains the outbox in the background", func(t *testing.T) {
		reset(t)
		pub := &recordingPublisher{}
		runCtx, cancel := context.WithCancel(ctx)
		stopped := make(chan struct{})
		go func() {
			newRelay(pub, 2).Run(runCtx, 20*time.Millisecond)
			close(stopped)
		}()

		createOrders(t, ctx, 5)

		// Wait for a condition, not for a fixed time: returns as soon as it holds.
		require.Eventually(t, func() bool { return len(pub.ids()) == 5 }, 10*time.Second, 20*time.Millisecond)
		assert.Equal(t, []int64{1, 2, 3, 4, 5}, pub.ids())

		cancel()
		select {
		case <-stopped:
		case <-time.After(5 * time.Second):
			t.Fatal("Run did not stop after context cancellation")
		}
	})
}
