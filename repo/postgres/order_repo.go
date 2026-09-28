// Package postgres — адаптер OrderRepository поверх PostgreSQL.
package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/XSAM/otelsql"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib" // драйвер "pgx" для database/sql
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"

	"github.com/rootalex/order-observability/domain"
	"github.com/rootalex/order-observability/usecase"
)

const pgUniqueViolation = "23505"

// Open открывает пул соединений с otel-инструментацией: каждый запрос — span
// внутри трейса usecase (ctx пробрасывается в методы репозитория, вариант A).
func Open(ctx context.Context, dsn string) (*sql.DB, error) {
	db, err := otelsql.Open("pgx", dsn,
		otelsql.WithAttributes(attribute.String("db.system", "postgresql")),
		otelsql.WithSpanOptions(otelsql.SpanOptions{OmitConnResetSession: true}),
	)
	if err != nil {
		return nil, err
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return db, nil
}

// Mutation — SQL-запрос с аргументами, который генерирует репозиторий.
// Интеграционные тесты проверяют, что он валиден на настоящей БД.
type Mutation struct {
	SQL  string
	Args []any
}

type OrderRepo struct {
	db *sql.DB
}

var _ usecase.OrderRepository = (*OrderRepo)(nil)

func NewOrderRepo(db *sql.DB) *OrderRepo { return &OrderRepo{db: db} }

// CreateMut строит INSERT заказа и его позиций.
func (r *OrderRepo) CreateMut(o *domain.Order) []Mutation {
	muts := []Mutation{{
		SQL: `INSERT INTO orders (id, customer_id, tier, status, fail_reason, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		Args: []any{string(o.ID), string(o.CustomerID), string(o.Tier), string(o.Status),
			nullString(o.FailReason), o.CreatedAt, o.UpdatedAt},
	}}
	if len(o.Items) == 0 {
		return muts
	}

	var b strings.Builder
	b.WriteString("INSERT INTO order_items (order_id, product_id, quantity, price_cents) VALUES ")
	args := make([]any, 0, len(o.Items)*4)
	for i, it := range o.Items {
		if i > 0 {
			b.WriteString(", ")
		}
		n := i * 4
		fmt.Fprintf(&b, "($%d, $%d, $%d, $%d)", n+1, n+2, n+3, n+4)
		args = append(args, string(o.ID), it.ProductID, it.Quantity, it.PriceCents)
	}
	return append(muts, Mutation{SQL: b.String(), Args: args})
}

var columnByField = map[domain.Field]string{
	domain.FieldStatus:     "status",
	domain.FieldFailReason: "fail_reason",
	domain.FieldUpdatedAt:  "updated_at",
}

// UpdateMut строит UPDATE только по изменённым (dirty) полям агрегата.
// Если ничего не менялось, возвращает nil: запрос в БД не нужен.
func (r *OrderRepo) UpdateMut(o *domain.Order) (*Mutation, error) {
	changes := o.Changes()
	if len(changes) == 0 {
		return nil, nil
	}

	sets := make([]string, 0, len(changes))
	args := make([]any, 0, len(changes)+1)
	for i, f := range changes {
		col, ok := columnByField[f]
		if !ok {
			return nil, fmt.Errorf("unmapped field %q", f)
		}
		sets = append(sets, fmt.Sprintf("%s = $%d", col, i+1))
		args = append(args, fieldValue(o, f))
	}
	args = append(args, string(o.ID))

	return &Mutation{
		SQL:  fmt.Sprintf("UPDATE orders SET %s WHERE id = $%d", strings.Join(sets, ", "), len(args)),
		Args: args,
	}, nil
}

func fieldValue(o *domain.Order, f domain.Field) any {
	switch f {
	case domain.FieldStatus:
		return string(o.Status)
	case domain.FieldFailReason:
		return nullString(o.FailReason)
	case domain.FieldUpdatedAt:
		return o.UpdatedAt
	}
	return nil
}

// outboxMut пишет доменные события в outbox вместе с traceparent текущего span.
func outboxMut(ctx context.Context, aggregateID domain.OrderID, events []domain.Event) (*Mutation, error) {
	if len(events) == 0 {
		return nil, nil
	}
	carrier := propagation.MapCarrier{}
	propagation.TraceContext{}.Inject(ctx, carrier)
	traceparent := nullString(carrier.Get("traceparent"))

	var b strings.Builder
	b.WriteString("INSERT INTO outbox (aggregate_id, event_type, payload, created_at, traceparent) VALUES ")
	args := make([]any, 0, len(events)*5)
	for i, e := range events {
		payload, err := json.Marshal(e)
		if err != nil {
			return nil, fmt.Errorf("marshal event %s: %w", e.Name(), err)
		}
		if i > 0 {
			b.WriteString(", ")
		}
		n := i * 5
		fmt.Fprintf(&b, "($%d, $%d, $%d, $%d, $%d)", n+1, n+2, n+3, n+4, n+5)
		args = append(args, string(aggregateID), e.Name(), string(payload), e.OccurredAt(), traceparent)
	}
	return &Mutation{SQL: b.String(), Args: args}, nil
}

func (r *OrderRepo) Create(ctx context.Context, o *domain.Order, events []domain.Event) error {
	outbox, err := outboxMut(ctx, o.ID, events)
	if err != nil {
		return err
	}
	err = r.inTx(ctx, func(tx *sql.Tx) error {
		for _, m := range r.CreateMut(o) {
			if _, err := tx.ExecContext(ctx, m.SQL, m.Args...); err != nil {
				return mapErr(err)
			}
		}
		return execOptional(ctx, tx, outbox)
	})
	if err != nil {
		return err
	}
	o.MarkPersisted()
	return nil
}

func (r *OrderRepo) Update(ctx context.Context, o *domain.Order, events []domain.Event) error {
	mut, err := r.UpdateMut(o)
	if err != nil {
		return err
	}
	outbox, err := outboxMut(ctx, o.ID, events)
	if err != nil {
		return err
	}
	err = r.inTx(ctx, func(tx *sql.Tx) error {
		if mut != nil {
			res, err := tx.ExecContext(ctx, mut.SQL, mut.Args...)
			if err != nil {
				return mapErr(err)
			}
			if n, err := res.RowsAffected(); err == nil && n == 0 {
				return usecase.ErrOrderNotFound
			}
		}
		return execOptional(ctx, tx, outbox)
	})
	if err != nil {
		return err
	}
	o.MarkPersisted()
	return nil
}

func (r *OrderRepo) Get(ctx context.Context, id domain.OrderID) (*domain.Order, error) {
	var (
		customerID, tier, status, failReason string
		o                                    domain.Order
	)
	err := r.db.QueryRowContext(ctx, `
SELECT customer_id, tier, status, COALESCE(fail_reason, ''), created_at, updated_at
FROM orders WHERE id = $1`, string(id)).
		Scan(&customerID, &tier, &status, &failReason, &o.CreatedAt, &o.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, usecase.ErrOrderNotFound
	}
	if err != nil {
		return nil, err
	}

	rows, err := r.db.QueryContext(ctx, `
SELECT product_id, quantity, price_cents FROM order_items WHERE order_id = $1 ORDER BY product_id`, string(id))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []domain.Item
	for rows.Next() {
		var it domain.Item
		if err := rows.Scan(&it.ProductID, &it.Quantity, &it.PriceCents); err != nil {
			return nil, err
		}
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return domain.Rehydrate(id, domain.CustomerID(customerID), domain.CustomerTier(tier), items,
		domain.Status(status), failReason, o.CreatedAt.UTC(), o.UpdatedAt.UTC()), nil
}

func (r *OrderRepo) inTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func execOptional(ctx context.Context, tx *sql.Tx, m *Mutation) error {
	if m == nil {
		return nil
	}
	_, err := tx.ExecContext(ctx, m.SQL, m.Args...)
	return mapErr(err)
}

func mapErr(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
		return fmt.Errorf("%w: %w", usecase.ErrOrderAlreadyExists, err)
	}
	return err
}

func nullString(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }
