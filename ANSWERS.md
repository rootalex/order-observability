# Answers

## Q1. Tracing domain operations without passing context

`trace.SpanFromContext(ctx)` inside `Order.Complete` makes the domain depend on OTel and on `ctx`. The domain should report *what happened* and let the outer layers decide how to observe it.

**Approach 1 — wrap the call from the application layer.** The usecase (or a decorator) opens a span around the domain call. Duration and error of the domain operation are visible; the domain method stays `func (o *Order) Complete(now time.Time) error`. Here it is done through the `usecase.Probe` port: `CreateOrder.validation` / `CreateOrder.fulfillment` spans wrap `NewOrder`, `MarkPaid` and `Fail` (`usecase/create_order.go`, `observability/probe.go`).

**Approach 2 — domain events.** The aggregate records plain event values (`OrderCreated`, `OrderPaid`, `OrderFailed`), which carry no infrastructure. The application layer pulls them (`order.PullEvents()`) and turns them into:
- span events with the original timestamps (`TracedInteractor`);
- business logs (`LoggingInteractor`);
- outbox rows in the same DB transaction (`OrderRepo`).

This gives the most information: not just "the method took 3 µs", but "the order failed because `payment_declined`" — and traces, logs and published events all come from one source.

A variant of approach 1 is the **decorator/proxy around the aggregate**: hard to do cleanly for entities in Go, and it adds little over wrapping at the usecase level.

## Q2. Mocks: the right choice, when they hide bugs, when you need both

**1. Mocking is the right choice** — unit testing usecase *logic*, where the real behavior of the dependency is not the subject. Example: "if the payment is declined, the order is marked `failed` with reason `payment_declined`, the failure is persisted, and the caller still gets the order id". With a fake `PaymentGateway` returning `ErrPaymentDeclined`, this runs in microseconds and covers every branch (`TestCreateOrder_PaymentOutcomes`). Doing the same through a real payment API would be slow and flaky, and some failures could not be reproduced on demand at all. Mocking the **clock** is always right.

**2. Mocking hides bugs** — when the mocked dependency's *behavior* is what can be wrong:
- `mockRepo.On("CreateMut", mock.Anything).Return(&Mutation{}, nil)` hides a typo in a column name, a missing argument, a `NOT NULL` or `CHECK` violation (`quantity > 0`), a unique-key conflict, `""` written where `NULL` is expected, and time truncation to microseconds.
- A concrete bug checked in this repo: changing the JSON tag `amount_cents` to `amount` in the payment client. Every usecase unit test with a fake gateway **still passes**, while production would send requests the provider rejects. The WireMock contract test fails immediately (TESTING.md, "Verifying that the tests can fail").
- Another one: an `UpdateMut` that writes *all* columns. A mock returns success; the real database silently overwrites a concurrent change (caught by `UpdateMut updates only dirty fields`).

**3. You need both** — the payment client:
- a **mock** of the `PaymentGateway` port in usecase tests: how the business reacts to success, decline or error, including cases that are hard to create for real (charged but the DB update failed — `TestCreateOrder_ChargedButUpdateFailed`);
- an **integration** test of `payment.Client` against WireMock: the adapter really serializes the request correctly, maps 402 vs 5xx, times out, and propagates `traceparent`.

Neither replaces the other. The mock test cannot see the wire format; the WireMock test cannot efficiently cover every business branch. The same holds for the repository: fake repo in usecase tests, real Postgres in repository tests.

## Q3. "Order was charged but shows as failed"

### Likely causes

| # | What happened | How it looks |
|---|---|---|
| a | The payment request **timed out on our side**, but the provider completed the charge. We treat the timeout as a failure and mark the order `failed`. | This is the current behavior of `CreateOrder` (`payment_error` → `Fail`), and the most likely cause. |
| b | Charge succeeded, then **saving the status failed** (DB error or timeout). The order stays `pending`; the client got a 500 and shows "failed". | `update order` error right after a successful charge. |
| c | Client **retry** after (a) or (b) creates a second order, or a second charge if the idempotency key is not stable. | Two orders or payments for one checkout. |

### Debugging workflow

1. From the support ticket get `order_id` (or customer id + time). Every log line has `order_id` and `trace_id`.
2. Find the business logs: `order.created` → `order.failed` with `reason`, or `order.created` without `order.paid`/`order.failed` (case b).
3. Open the trace by `trace_id`. `CreateOrder.payment` → `HTTP POST` shows the duration against the client timeout, status and error; `CreateOrder.fulfillment` shows the SQL error.
4. Ask the provider for its view of the charge by **idempotency key = order id**. It tells you whether a charge exists.
5. Check for duplicates: other orders from the same customer in that time window.

### Logs that help

- Business events with `order_id`, `payment_id`, `reason` (already there).
- Payment client: request sent / response received with `order_id`, idempotency key, status and **duration**. A timeout is logged as a timeout, not as a generic error.
- ERROR with stack for the failed status update, **including `payment_id`** — this line is the evidence that money was taken.
- All of it tied together by `trace_id`.

### Metrics that would indicate it

Already available:
- `order_processing_duration_seconds{step="payment"}` — p99 approaching the client timeout.
- `orders_created_total{status="failure"}` — failure rate going up.

Needed in addition:
- `payment_requests_total{outcome="success|declined|error|timeout"}` — timeouts specifically (case a).
- `orders_status_update_failures_total{after="charge"}` — failed saves after a successful charge (case b).
- `orders_stuck_pending` — orders `pending` for longer than N minutes (gauge from the DB, see METRICS.md).
- `payment_reconciliation_mismatches_total` — from the reconciliation job below.

### Alerts

| Alert | Condition (example) | Why |
|---|---|---|
| Payment timeouts | `rate(payment_requests_total{outcome="timeout"}[5m]) / rate(payment_requests_total[5m]) > 0.02` for 5m | Case (a) is starting |
| Charged but not saved | `increase(orders_status_update_failures_total{after="charge"}[5m]) > 0` | Every occurrence means money without an order: page immediately |
| Stuck orders | `orders_stuck_pending > 0` for 10m | Case (b) that nobody noticed |
| Reconciliation mismatch | `increase(payment_reconciliation_mismatches_total[1h]) > 0` | Last line of defense |
| Payment latency | p99 of `step="payment"` > 80% of client timeout | Early warning, before the timeouts |

Alerts are based on symptoms that cost money, not on single error log lines. Expected declines are excluded; that is why declines are WARN, not ERROR.

### Fixing the root cause

- A **timeout is "unknown", not "failed"**. Mark the order `payment_pending` and resolve it by querying the provider with the idempotency key, or wait for its webhook.
- A stable **idempotency key** (the order id — already sent in the `Idempotency-Key` header) makes retries safe.
- Persist "payment requested" before calling the provider, and drive the rest through the **outbox / saga**, so a crash between the charge and the status update can be recovered.
- A **reconciliation job** that compares provider charges with order statuses and fixes or flags mismatches.

## Q4. Testing the outbox end-to-end without flaky timing

Flakiness comes from `time.Sleep` and waiting for a background goroutine. The fix is to **make the worker steppable** and to wait on **conditions**, not on time.

1. **Design the worker so that one iteration can be called directly:** `ProcessOnce(ctx) (processed int, err error)` reads a batch (`SELECT … WHERE processed_at IS NULL ORDER BY id FOR UPDATE SKIP LOCKED LIMIT n`), publishes, and marks rows processed. `Run(ctx)` is just a loop with a ticker around it and is not needed in most tests.
2. **Test with real infrastructure, in the test goroutine:**

   ```go
   func TestOutbox_EndToEnd(t *testing.T) {
       db := setup.StartPostgres(ctx, t)
       broker := setup.StartKafka(ctx, t)        // or RabbitMQ / NATS container
       clk := clock.NewFake(baseTime)

       // 1. aggregate + outbox in one transaction
       o := newOrder(t, clk, "ord-1")
       require.NoError(t, repo.Create(ctx, o, o.PullEvents()))

       // 2–4. one deterministic worker step: read, publish, mark processed
       n, err := worker.ProcessOnce(ctx)
       require.NoError(t, err)
       require.Equal(t, 1, n)

       // assert the message: consumer with a deadline, not a sleep
       msg := broker.Consume(t, "orders", 10*time.Second)
       assert.Equal(t, "order.created", msg.Type)
       assert.Equal(t, "ord-1", msg.Key)
       assert.Contains(t, msg.Headers["traceparent"], "00-")   // trace continues

       assertProcessed(t, db, "ord-1")
   }
   ```

3. **If the real background loop must be tested**, use `require.Eventually(t, cond, 10*time.Second, 50*time.Millisecond)`. It returns as soon as the condition is true, and the long timeout only matters when something is broken, so it is fast *and* stable.
4. **Scenarios that matter more than the happy path**, all deterministic with `ProcessOnce`:
   - **publish fails** (broker stopped, or a fake publisher returning an error) → the row stays unprocessed → the next `ProcessOnce` publishes it (**at-least-once**);
   - **crash after publish, before mark** → published twice → the consumer must deduplicate by event id (test the consumer's idempotency separately);
   - **two workers concurrently** → `SKIP LOCKED` → each event is published exactly once per successful run;
   - **ordering** per aggregate (`ORDER BY id`);
   - **atomicity**: if the aggregate insert fails, there is no outbox row (same transaction — already covered by the rollback test in `order_repo_test.go`).
5. **Time** in the worker (retry backoff, "retry after") comes from `clock.Clock`, so `clk.Advance(time.Minute)` replaces waiting a minute.

In this repo the write side is implemented and tested (outbox row in the same transaction, JSON payload, `traceparent`). The relay worker is not implemented; the test above is how it would be tested.

## Q5. Time-dependent tests

```go
order.CreatedAt = time.Now()
// ... later
assert.Equal(t, time.Now(), order.CreatedAt) // fails sometimes
```

**What's wrong.** The two `time.Now()` calls happen at different moments, so the values differ. Locally the gap is often below the clock resolution and the test "passes"; on a busy CI runner it is microseconds or milliseconds. Related traps in the same test:
- `time.Now()` contains a **monotonic clock reading**, so `==` / `assert.Equal` compares more than the wall time. Two values printed identically may still differ.
- **Database precision**: Postgres `timestamptz` keeps microseconds, so a value read back never equals the in-memory nanosecond value.
- **Time zones**: a local time and the same moment in UTC are not `Equal` by struct comparison; CI usually runs in UTC, laptops often don't.
- Tests around **midnight or month end** that compute "today" behave differently depending on when they run.

**Fix.** Make time an explicit dependency.

1. Inject a clock and never call `time.Now()` in domain or usecase code:

   ```go
   type Clock interface{ Now() time.Time }        // clock/clock.go
   uc := usecase.NewCreateOrder(repo, pay, ids, clock.Real{}, probe)          // production
   uc := usecase.NewCreateOrder(repo, pay, ids, clock.NewFake(fixedNow), nil) // tests
   ```

   The domain takes time as a parameter (`NewOrder(..., now)`, `MarkPaid(now)`), so it stays pure.
2. With a fake clock, assert the **exact** value: `assert.Equal(t, fixedNow, resp.Events[0].OccurredAt())` (`usecase/create_order_test.go`). Use `clk.Advance(d)` to move time forward instead of `time.Sleep`.
3. When comparing values that went through the database, normalize them: `.UTC()` and `.Truncate(time.Microsecond)`, as `order_repo_test.go` does with a base time that deliberately has nanoseconds.
4. Where real time cannot be avoided (e.g. a timeout test), assert a **range**: `assert.WithinDuration(t, want, got, time.Second)` or `assert.Less(t, elapsed, limit)` — never equality.
5. Compare `time.Time` values with `t1.Equal(t2)`, not `==`. `Clock.Real` returns UTC to avoid zone differences.
