# Testing Strategy

## The over-mocking problem

```go
mockRepo.On("CreateMut", mock.Anything).Return(&Mutation{}, nil)
```

This mock accepts any input and always succeeds. It encodes our *assumptions* about the database, not its behavior. Wrong SQL, broken serialization and violated constraints all pass. The fix is not "no mocks" but **clear boundaries**: mock at the level where the thing being tested does not depend on the real behavior of the mocked part, and test that real behavior separately against real systems.

## Test pyramid

| Test Level | What to Test | What to Mock |
|------------|--------------|--------------|
| **Unit** | Domain rules: transitions, validation, events, change tracking. Usecase orchestration: which branch runs on decline or failure, what is saved, what is returned. Observability decorators. | **Ports** (`OrderRepository`, `PaymentGateway`, `IDGenerator`) with simple hand-written fakes; **time** via `clock.Fake`. Domain is never mocked — it is pure and fast. |
| **Integration** | Each adapter against the real thing it talks to: SQL validity, constraints, transactions and rollback, NULL and time precision, JSON contract with the payment API, HTTP status mapping, timeouts, header propagation. | **Only what we don't own**: the payment provider (WireMock in a container). The database is **real** (Postgres via testcontainers, the same major version and migrations as production). |
| **E2E** | Critical user journeys through the public API: `POST /orders` → DB state + outbox + trace. A few tests, not all combinations. | Ideally nothing. External third parties we cannot run (the real payment provider) stay as WireMock. |

Proportions: many unit tests (milliseconds), a focused set of integration tests per adapter (seconds), very few E2E (minutes). Each level covers what the level below **cannot** see, rather than repeating it.

| Test | Level | Runs in |
|---|---|---|
| `domain/order_test.go` | Unit | ~ms, no Docker |
| `usecase/create_order_test.go` | Unit (fakes + `clock.Fake`) | ~ms, no Docker |
| `observability/observability_test.go` | Unit (in-memory span recorder, Prometheus registry) | ~ms, no Docker |
| `transport/httpapi/router_test.go` | Unit (`httptest`, fake interactor) | ~ms, no Docker |
| `testing/integration/order_repo_test.go` | Integration (Postgres container) | ~3 s |
| `testing/integration/payment_client_test.go` | Integration (WireMock container) | ~3 s |
| `testing/integration/outbox_relay_test.go` | Integration (Postgres container, fake broker) | ~3 s |

```bash
make test-unit          # no Docker
make test-integration   # requires Docker
go test -short ./...    # integration tests skip themselves with -short
```

## 1. Repository integration test (testcontainers)

`testing/setup/testcontainers.go` → `StartPostgres`: `postgres:16-alpine`, waits until it is ready, applies `migrations/*.up.sql` in order, and registers container cleanup. The tests assert **what is actually in the tables** via raw SQL, not what the repository returns.

| Subtest | Catches |
|---|---|
| `CreateMut generates valid SQL` | Syntax errors, wrong column names or order, argument count, `""` stored instead of `NULL`, microsecond time truncation, JSON payload of outbox events, `traceparent` in outbox, `Get` round trip |
| `UpdateMut updates only dirty fields` | Asserts the generated SQL (`SET status = $1, updated_at = $2`). A concurrent writer changes `customer_id` before our update, and the update must not overwrite it — a "save everything" implementation would. After saving, nothing is dirty. Outbox has `order.created, order.paid`. |
| `Update of missing order returns ErrOrderNotFound` | `RowsAffected == 0` handling |
| `DB constraints are enforced` | Duplicate id → `ErrOrderAlreadyExists` (unique violation mapping). `quantity = 0` and unknown `status` → `check_violation`, **with the order row rolled back** together with items (transaction atomicity). Domain validation is deliberately bypassed through `Rehydrate` to test the database's own protection. |

## 2. External service test (WireMock)

`StartWireMock` runs `wiremock/wiremock` and provides `Stub`, `Reset` (isolation between subtests) and `CountRequests`.

The stub matcher **is the contract**: method, path, `Content-Type`, `Idempotency-Key`, and `equalToJson` on the body. If the client serializes differently, WireMock does not match and returns 404, and the test fails.

| Subtest | Scenario |
|---|---|
| `success` | 200 → `payment_id` is parsed; exactly one request was made |
| `request contract mismatch is caught` | Wrong amount → no stub matches → error |
| `declined` | 402 → `usecase.ErrPaymentDeclined` (business outcome) |
| `server error` | 500 → error that is **not** a decline |
| `malformed response` | 200 with invalid JSON → decode error |
| `timeout` | `fixedDelayMilliseconds: 2000` with a 200 ms client timeout → `net.Error.Timeout()`, and the client returns without waiting for the slow response |
| `propagates traceparent` | The stub only matches if the `traceparent` header carries the caller's trace ID |

## 3. Outbox relay test (testcontainers)

`outbox/relay.go` is built to be tested without timing: `ProcessOnce` runs one batch synchronously; `Run` is only a ticker loop around it. The database is real, because locking and transactions are the point. The broker is a recording fake behind the `Publisher` port.

| Subtest | Checks |
|---|---|
| `publishes pending events in order and marks them processed` | id order, payload, `processed_at` from the injected clock; a second run publishes nothing |
| `failed publish keeps the event and the order for the next run` | Broker fails on event 2: event 1 is marked, 2 and 3 stay pending (3 is **not** published ahead of 2); the next run delivers them. At-least-once, no duplicates |
| `continues the trace of the request that wrote the event` | The publisher receives a ctx whose trace ID equals the one of the original request, although the relay was called with an empty ctx |
| `parallel relays never publish the same event twice` | Relay A locks events 1–5 and is blocked inside its transaction by channels; relay B, running meanwhile, gets 6–10. This is deterministic: no sleeps, and B has its own timeout so a regression fails in 5 s instead of hanging |
| `Run drains the outbox in the background` | The only test of the background loop: `require.Eventually` waits for a condition, not a fixed time |

## 4. HTTP handler tests

`transport/httpapi/router_test.go`, `httptest` with a fake interactor:

- status mapping: 201 / 400 (invalid JSON, domain validation) / 402 (declined, with the failed order in the body) / 500 without internal details;
- JSON → `CreateOrderRequest` mapping;
- routes, 405 and 404;
- the access log continues the caller's `traceparent`, logs 5xx as ERROR, and never contains the request body;
- `/healthz` and `/metrics` are not access-logged.

## Verifying that the tests can fail

A test that passed on the first run proves nothing until it has been seen failing. Two realistic bugs were introduced by hand:

| Bug | Unit test with fake payment | Integration test |
|---|---|---|
| JSON tag `amount_cents` → `amount` in `payment/client.go` | **passes** (the fake never serializes) | **fails**: WireMock returns 404 in `success`, `declined`, `malformed response`, `timeout` |
| `UpdateMut` also writes `customer_id` | not covered | **fails**: generated SQL differs, and the concurrent `customer_id` change is overwritten |
| Relay without `SKIP LOCKED` | not covered | **fails**: relay B blocks on A's rows (`relay B must not wait for rows locked by A`) |
| Relay continues the batch after a failed publish | not covered | **fails**: event 3 is published ahead of event 2 |

All bugs were reverted after the check. This is exactly the gap from the over-mocking example.

## Time abstraction

`clock/clock.go`: the `Clock` interface with `Real` (UTC) and `Fake` (`NewFake(t)`, `Advance(d)`). The domain never calls `time.Now()`: time comes in as a parameter (`NewOrder(..., now)`, `MarkPaid(now)`), and the usecase gets it from the injected clock. In tests, all times are exact and can be compared with `assert.Equal`. The integration tests also use a fixed time **with nanoseconds** on purpose, and assert against `Truncate(time.Microsecond)`, because that is what Postgres stores. See ANSWERS.md Q5.

## Not covered (yet)

- **E2E test** of `POST /orders` against the `docker-compose` environment. It was verified manually: 201, rows and outbox in Postgres, full trace in Jaeger including `outbox.publish` spans. It is not automated.
- **A real broker adapter.** The relay publishes through the `Publisher` port; locally `LogPublisher` logs the messages. A Kafka/NATS adapter would get its own integration test with a broker container, like the WireMock test for the payment client.
