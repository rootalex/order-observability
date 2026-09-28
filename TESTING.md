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
| `testing/integration/order_repo_test.go` | Integration (Postgres container) | ~3 s |
| `testing/integration/payment_client_test.go` | Integration (WireMock container) | ~3 s |

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

## Verifying that the tests can fail

A test that passed on the first run proves nothing until it has been seen failing. Two realistic bugs were introduced by hand:

| Bug | Unit test with fake payment | Integration test |
|---|---|---|
| JSON tag `amount_cents` → `amount` in `payment/client.go` | **passes** (the fake never serializes) | **fails**: WireMock returns 404 in `success`, `declined`, `malformed response`, `timeout` |
| `UpdateMut` also writes `customer_id` | not covered | **fails**: generated SQL differs, and the concurrent `customer_id` change is overwritten |

Both bugs were reverted after the check. This is exactly the gap from the over-mocking example.

## Time abstraction

`clock/clock.go`: the `Clock` interface with `Real` (UTC) and `Fake` (`NewFake(t)`, `Advance(d)`). The domain never calls `time.Now()`: time comes in as a parameter (`NewOrder(..., now)`, `MarkPaid(now)`), and the usecase gets it from the injected clock. In tests, all times are exact and can be compared with `assert.Equal`. The integration tests also use a fixed time **with nanoseconds** on purpose, and assert against `Truncate(time.Microsecond)`, because that is what Postgres stores. See ANSWERS.md Q5.

## Not covered (yet)

- **E2E test** of `POST /orders` against the `docker-compose` environment. It was verified manually: 201, rows and outbox in Postgres, full trace in Jaeger. It is not automated.
- **Outbox worker.** The outbox is written, but there is no relay yet. The testing approach is described in ANSWERS.md Q4.
- **HTTP handler tests** (`transport/httpapi`) — status code mapping; these would be cheap `httptest` unit tests.
