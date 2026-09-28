# Tracing Design

## Summary

| Layer | Option | How the span is created |
|---|---|---|
| HTTP boundary | A | `otelhttp.NewHandler` extracts `traceparent` from API GW and starts the server span (`transport/httpapi/router.go`) |
| Usecase | **A + C** | `ctx` carries the trace; `TracedInteractor` decorator creates the `CreateOrder` span (`observability/tracing.go`) |
| Usecase steps | A + port | `usecase.Probe` port; `observability.Probe` creates `CreateOrder.<step>` child spans (`observability/probe.go`) |
| Domain | **none** | Domain has no `ctx` and no tracer. Traced from the outside (see Q4) |
| Repository | **A** | `ctx` is passed to every method; `otelsql` creates SQL spans automatically (`repo/postgres/order_repo.go`) |
| Outgoing HTTP | A | `otelhttp.NewTransport` creates the client span and injects `traceparent` (`payment/client.go`) |
| Async (outbox) | stored context | `traceparent` column in `outbox`, written in the same transaction as the aggregate; the relay continues the trace with an `outbox.publish <event>` producer span (`outbox/relay.go`) |

A real trace of `POST /orders` in Jaeger (local run against `docker-compose`):

```
POST /orders                         otelhttp server span (parent = API GW traceparent)
└── CreateOrder                      TracedInteractor; events: order.created, order.paid
    ├── CreateOrder.validation       Probe
    │   ├── sql.conn.begin_tx        otelsql
    │   ├── sql.conn.exec ×3         INSERT orders / order_items / outbox
    │   └── sql.tx.commit
    ├── CreateOrder.payment          Probe
    │   └── HTTP POST                otelhttp transport -> Payment Service
    └── CreateOrder.fulfillment      Probe
        ├── sql.conn.begin_tx
        ├── sql.conn.exec ×2         UPDATE orders (dirty fields) / outbox
        └── sql.tx.commit
```

## 1. Which option for usecases? Why?

**A + C: the trace lives in `context.Context`, and a decorator creates the span.**

- `ctx` already goes through every call because of cancellation and deadlines. OpenTelemetry for Go is built around it (`trace.SpanFromContext`). Any instrumented library (`otelsql`, `otelhttp`) picks the span up from `ctx` with no extra code.
- **Option B** (explicit `TraceContext` parameter) duplicates what `ctx` already carries, changes every signature and interface, and does not reach third-party libraries. It also puts the concept of "trace" into the usecase API.
- **Option C** keeps span creation out of business code. The usecase knows nothing about OTel, and tracing can be turned on, off or replaced at wiring time (`cmd/orders/main.go`):

```go
uc = observability.NewMetricsInteractor(uc, metrics)
uc = observability.NewLoggingInteractor(uc, log)
uc = observability.NewTracedInteractor(uc, tp) // outermost: logs and metrics see the span in ctx
```

A decorator only sees the start and end of the usecase. For per-step spans (`validation`, `payment`, `fulfillment`), the usecase depends on a small port that it owns (*Domain-Oriented Observability*):

```go
// usecase/probe.go — no OTel / Prometheus imports
type Probe interface {
    StepStarted(ctx context.Context, step Step) (context.Context, func(err error))
}
```

`observability.Probe` implements it with a child span plus a histogram observation. The usecase states *what* happens; the adapter decides *how* to record it.

## 2. Which option for domain methods? Why?

**None of them.** Domain methods take neither `ctx` nor a tracer:

```go
func (o *Order) MarkPaid(now time.Time) error
func (o *Order) Fail(reason string, now time.Time) error
```

- The domain must be pure: deterministic, with no infrastructure imports, testable without any setup. `trace.SpanFromContext` inside `Order.Complete` makes the domain depend on OTel and on a runtime context it has no reason to know about.
- Domain operations are in-memory and take microseconds. A separate span for each of them only adds noise. What is interesting is *what happened* (an event), not *how long it took*.
- Even the time is passed in (`now time.Time`, taken from `clock.Clock`) rather than read inside, for the same reason.

## 3. Which option for repository methods?

**A.** A repository needs `ctx` anyway: for query cancellation, deadlines and transactions. The spans come from the instrumented driver, so repository code contains no tracing at all:

```go
db, err := otelsql.Open("pgx", dsn,
    otelsql.WithAttributes(attribute.String("db.system", "postgresql")),
    otelsql.WithSpanOptions(otelsql.SpanOptions{OmitConnResetSession: true}),
)
```

A decorator (`TracedOrderRepo`) would also work, but it duplicates what the driver already provides and cannot see the individual SQL statements.

The repository also **persists** the trace context: `outboxMut` injects the current `traceparent` into each outbox row. The relay extracts it and starts `outbox.publish <event>` as a child span, so publishing appears in the **same trace** as the original `POST /orders`, even though it happens later and possibly in another process (verified in Jaeger and in `TestOutboxRelay_Integration`).

## 4. How do you trace a domain method without passing context to it?

Three complementary techniques, all used here:

1. **Wrap the call from the outside.** The usecase calls the domain inside a step span (`CreateOrder.validation`, `CreateOrder.fulfillment`). Duration and errors of the domain operation are visible without the domain knowing about it.
2. **Domain events become span events.** The aggregate records what happened (`OrderCreated`, `OrderPaid`, `OrderFailed`). The usecase returns the events and `TracedInteractor` attaches them to the span with their own timestamps:

   ```go
   for _, e := range resp.Events {
       span.AddEvent(e.Name(), trace.WithTimestamp(e.OccurredAt()))
   }
   ```

   The same events feed business logs and the outbox, so all three stay consistent.
3. **Return values become attributes.** The resulting `order.id`, `order.status` and errors are set on the span by the decorator.

Tested in `observability/observability_test.go`: `TestTracedInteractor_DomainEventsAndErrorStatus` and `TestProbe_ChildSpanAndStepHistogram`.

## Conventions

- **No PII in span attributes.** IDs (`order.id`) are fine in traces, and that is where they belong (see METRICS.md). Emails, phones and card data are never recorded.
- **Expected business outcomes are not span errors.** An invalid order or a declined payment is recorded with `RecordError`, but the span status is set to `Error` only for real failures (`isExpected` in `observability/logging.go`). Otherwise "error rate" in the tracing backend would mostly measure customers with insufficient funds.
- **Propagation:** W3C `traceparent` + baggage (`InitTracerProvider`). Inbound — `otelhttp.NewHandler`, outbound — `otelhttp.NewTransport`, async — the `outbox.traceparent` column.
- **Exporter:** OTLP/HTTP to `OTEL_EXPORTER_OTLP_ENDPOINT` (default `http://localhost:4318`, Jaeger in `docker-compose.yml`). Export errors go through `otel.SetErrorHandler` and are logged as WARN, so a broken exporter does not go unnoticed.
