// Package outbox relays domain events from the outbox table to a message broker.
//
// Delivery is at-least-once: an event is marked processed only after it was
// published, so a crash between publish and mark re-publishes it. Consumers
// must deduplicate by Message.ID.
package outbox

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/rootalex/order-observability/clock"
)

// Message is one outbox row as handed to the broker.
type Message struct {
	ID          int64
	AggregateID string
	EventType   string
	Payload     []byte
	CreatedAt   time.Time
}

// Publisher is the port to the message broker. The ctx carries the span of the
// original request, so an instrumented adapter propagates it into message headers.
type Publisher interface {
	Publish(ctx context.Context, m Message) error
}

type Relay struct {
	db        *sql.DB
	publisher Publisher
	clock     clock.Clock
	tracer    trace.Tracer
	log       *slog.Logger
	batchSize int
}

func NewRelay(db *sql.DB, p Publisher, clk clock.Clock, tp trace.TracerProvider, log *slog.Logger, batchSize int) *Relay {
	return &Relay{
		db:        db,
		publisher: p,
		clock:     clk,
		tracer:    tp.Tracer("github.com/rootalex/order-observability/outbox"),
		log:       log,
		batchSize: batchSize,
	}
}

// ProcessOnce publishes one batch of pending events in id order and marks them
// processed. It is the unit that tests drive directly: no background goroutine,
// no sleeps.
//
// Rows are locked with FOR UPDATE SKIP LOCKED, so several relays can run in
// parallel without publishing the same event twice. On the first publish error
// the batch stops: events published so far are marked, the failed one and the
// rest stay pending and keep their order for the next run.
func (r *Relay) ProcessOnce(ctx context.Context) (int, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}

	msgs, parents, err := r.lockPending(ctx, tx)
	if err != nil {
		_ = tx.Rollback()
		return 0, err
	}

	var (
		processed  int
		publishErr error
	)
	for i, m := range msgs {
		if publishErr = r.publish(ctx, m, parents[i]); publishErr != nil {
			break
		}
		if _, err := tx.ExecContext(ctx, `UPDATE outbox SET processed_at = $1 WHERE id = $2`, r.clock.Now(), m.ID); err != nil {
			_ = tx.Rollback()
			return 0, fmt.Errorf("mark outbox %d processed: %w", m.ID, err)
		}
		processed++
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit outbox batch: %w", err)
	}
	if publishErr != nil {
		return processed, fmt.Errorf("publish: %w", publishErr)
	}
	return processed, nil
}

func (r *Relay) lockPending(ctx context.Context, tx *sql.Tx) ([]Message, []string, error) {
	rows, err := tx.QueryContext(ctx, `
SELECT id, aggregate_id, event_type, payload, created_at, COALESCE(traceparent, '')
FROM outbox
WHERE processed_at IS NULL
ORDER BY id
LIMIT $1
FOR UPDATE SKIP LOCKED`, r.batchSize)
	if err != nil {
		return nil, nil, fmt.Errorf("select outbox: %w", err)
	}
	defer rows.Close()

	var (
		msgs    []Message
		parents []string
	)
	for rows.Next() {
		var (
			m           Message
			traceparent string
		)
		if err := rows.Scan(&m.ID, &m.AggregateID, &m.EventType, &m.Payload, &m.CreatedAt, &traceparent); err != nil {
			return nil, nil, err
		}
		msgs = append(msgs, m)
		parents = append(parents, traceparent)
	}
	return msgs, parents, rows.Err()
}

// publish continues the trace of the request that wrote the event.
func (r *Relay) publish(ctx context.Context, m Message, traceparent string) error {
	if traceparent != "" {
		ctx = propagation.TraceContext{}.Extract(ctx, propagation.MapCarrier{"traceparent": traceparent})
	}
	ctx, span := r.tracer.Start(ctx, "outbox.publish "+m.EventType, trace.WithSpanKind(trace.SpanKindProducer))
	defer span.End()

	if err := r.publisher.Publish(ctx, m); err != nil {
		span.RecordError(err)
		r.log.ErrorContext(ctx, "outbox publish failed",
			slog.String("log_type", "operational"),
			slog.Int64("outbox_id", m.ID),
			slog.String("event_type", m.EventType),
			slog.String("error", err.Error()),
		)
		return err
	}
	return nil
}

// Run calls ProcessOnce every interval until ctx is done. A full batch is
// followed immediately by the next one, so a backlog drains without waiting.
func (r *Relay) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		for {
			n, err := r.ProcessOnce(ctx)
			if err != nil && ctx.Err() == nil {
				r.log.ErrorContext(ctx, "outbox relay iteration failed",
					slog.String("log_type", "operational"), slog.String("error", err.Error()))
			}
			if err != nil || n < r.batchSize {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// LogPublisher is a stand-in broker adapter for local runs: it logs messages.
// Replace with a Kafka/NATS/RabbitMQ adapter that injects traceparent into headers.
type LogPublisher struct {
	Log *slog.Logger
}

func (p LogPublisher) Publish(ctx context.Context, m Message) error {
	p.Log.InfoContext(ctx, "outbox message published",
		slog.String("log_type", "operational"),
		slog.Int64("outbox_id", m.ID),
		slog.String("aggregate_id", m.AggregateID),
		slog.String("event_type", m.EventType),
	)
	return nil
}
