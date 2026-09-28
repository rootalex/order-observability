package integration

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/rootalex/order-observability/clock"
	"github.com/rootalex/order-observability/domain"
	"github.com/rootalex/order-observability/repo/postgres"
	"github.com/rootalex/order-observability/testing/setup"
	"github.com/rootalex/order-observability/usecase"
)

// Наносекунды в фиксированном времени — намеренно: Postgres хранит микросекунды.
var baseTime = time.Date(2026, 9, 28, 12, 0, 0, 123456789, time.UTC)

func newOrder(t *testing.T, clk clock.Clock, id domain.OrderID) *domain.Order {
	t.Helper()
	o, err := domain.NewOrder(id, "c-1", domain.TierPremium, []domain.Item{
		{ProductID: "p-1", Quantity: 2, PriceCents: 500},
		{ProductID: "p-2", Quantity: 1, PriceCents: 250},
	}, clk.Now())
	require.NoError(t, err)
	return o
}

func pgCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

func TestOrderRepo_Integration(t *testing.T) {
	ctx := context.Background()
	db := setup.StartPostgres(ctx, t)
	repo := postgres.NewOrderRepo(db)
	tracer := sdktrace.NewTracerProvider().Tracer("test")

	t.Run("CreateMut generates valid SQL", func(t *testing.T) {
		clk := clock.NewFake(baseTime)
		o := newOrder(t, clk, "ord-create")

		spanCtx, span := tracer.Start(ctx, "test")
		require.NoError(t, repo.Create(spanCtx, o, o.PullEvents()))
		span.End()

		// Проверяем то, что реально легло в таблицы, а не то, что вернул репозиторий.
		var (
			customerID, tier, status string
			failReason               sql.NullString
			createdAt                time.Time
		)
		require.NoError(t, db.QueryRowContext(ctx,
			`SELECT customer_id, tier, status, fail_reason, created_at FROM orders WHERE id = $1`, "ord-create").
			Scan(&customerID, &tier, &status, &failReason, &createdAt))
		assert.Equal(t, "c-1", customerID)
		assert.Equal(t, "premium", tier)
		assert.Equal(t, "pending", status)
		assert.False(t, failReason.Valid, "empty fail_reason must be stored as NULL")
		assert.Equal(t, baseTime.Truncate(time.Microsecond), createdAt.UTC(), "Postgres keeps microsecond precision")

		var itemsCount int
		var total int64
		require.NoError(t, db.QueryRowContext(ctx,
			`SELECT count(*), sum(quantity * price_cents) FROM order_items WHERE order_id = $1`, "ord-create").
			Scan(&itemsCount, &total))
		assert.Equal(t, 2, itemsCount)
		assert.Equal(t, o.TotalCents(), total)

		var eventType, payload, traceparent string
		require.NoError(t, db.QueryRowContext(ctx,
			`SELECT event_type, payload::text, traceparent FROM outbox WHERE aggregate_id = $1`, "ord-create").
			Scan(&eventType, &payload, &traceparent))
		assert.Equal(t, "order.created", eventType)
		assert.JSONEq(t, `{"order_id":"ord-create","occurred_at":"2026-09-28T12:00:00.123456789Z"}`, payload)
		assert.Contains(t, traceparent, span.SpanContext().TraceID().String(), "outbox must carry trace context")

		got, err := repo.Get(ctx, "ord-create")
		require.NoError(t, err)
		assert.Equal(t, o.Items, got.Items)
		assert.Equal(t, o.Status, got.Status)
		assert.Equal(t, o.CreatedAt.Truncate(time.Microsecond), got.CreatedAt)
		assert.Empty(t, got.Changes(), "rehydrated order has no pending changes")
	})

	t.Run("UpdateMut updates only dirty fields", func(t *testing.T) {
		clk := clock.NewFake(baseTime)
		o := newOrder(t, clk, "ord-update")
		require.NoError(t, repo.Create(ctx, o, o.PullEvents()))

		// Параллельный писатель меняет колонку, которую текущая операция не трогает.
		_, err := db.ExecContext(ctx, `UPDATE orders SET customer_id = 'changed-concurrently' WHERE id = $1`, "ord-update")
		require.NoError(t, err)

		clk.Advance(time.Minute)
		require.NoError(t, o.MarkPaid(clk.Now()))

		mut, err := repo.UpdateMut(o)
		require.NoError(t, err)
		assert.Equal(t, "UPDATE orders SET status = $1, updated_at = $2 WHERE id = $3", mut.SQL)

		require.NoError(t, repo.Update(ctx, o, o.PullEvents()))

		var customerID, status string
		var updatedAt time.Time
		require.NoError(t, db.QueryRowContext(ctx,
			`SELECT customer_id, status, updated_at FROM orders WHERE id = $1`, "ord-update").
			Scan(&customerID, &status, &updatedAt))
		assert.Equal(t, "changed-concurrently", customerID, "non-dirty column must not be overwritten")
		assert.Equal(t, "paid", status)
		assert.Equal(t, baseTime.Add(time.Minute).Truncate(time.Microsecond), updatedAt.UTC())

		mut, err = repo.UpdateMut(o)
		require.NoError(t, err)
		assert.Nil(t, mut, "nothing is dirty after a successful save")

		var events []string
		rows, err := db.QueryContext(ctx, `SELECT event_type FROM outbox WHERE aggregate_id = $1 ORDER BY id`, "ord-update")
		require.NoError(t, err)
		defer rows.Close()
		for rows.Next() {
			var e string
			require.NoError(t, rows.Scan(&e))
			events = append(events, e)
		}
		assert.Equal(t, []string{"order.created", "order.paid"}, events)
	})

	t.Run("Update of missing order returns ErrOrderNotFound", func(t *testing.T) {
		clk := clock.NewFake(baseTime)
		o := newOrder(t, clk, "ord-missing")
		require.NoError(t, o.MarkPaid(clk.Now()))

		err := repo.Update(ctx, o, o.PullEvents())
		assert.ErrorIs(t, err, usecase.ErrOrderNotFound)
	})

	t.Run("DB constraints are enforced", func(t *testing.T) {
		t.Run("duplicate id", func(t *testing.T) {
			clk := clock.NewFake(baseTime)
			require.NoError(t, repo.Create(ctx, newOrder(t, clk, "ord-dup"), nil))

			err := repo.Create(ctx, newOrder(t, clk, "ord-dup"), nil)
			assert.ErrorIs(t, err, usecase.ErrOrderAlreadyExists)
		})

		t.Run("non-positive quantity is rejected and transaction rolled back", func(t *testing.T) {
			// Обходим доменную валидацию: проверяем именно защиту на уровне БД.
			o := domain.Rehydrate("ord-bad-qty", "c-1", domain.TierFree,
				[]domain.Item{{ProductID: "p-1", Quantity: 0, PriceCents: 100}},
				domain.StatusPending, "", baseTime, baseTime)

			err := repo.Create(ctx, o, nil)
			assert.Equal(t, "23514", pgCode(err), "check_violation expected, got %v", err)

			var n int
			require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM orders WHERE id = $1`, "ord-bad-qty").Scan(&n))
			assert.Zero(t, n, "order row must be rolled back together with items")
		})

		t.Run("unknown status is rejected", func(t *testing.T) {
			o := domain.Rehydrate("ord-bad-status", "c-1", domain.TierFree,
				[]domain.Item{{ProductID: "p-1", Quantity: 1, PriceCents: 100}},
				"shipped", "", baseTime, baseTime)

			err := repo.Create(ctx, o, nil)
			assert.Equal(t, "23514", pgCode(err), "check_violation expected, got %v", err)
		})
	})
}
