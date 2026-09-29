# Logging Strategy

## 1. Which option maintains Clean Architecture?

**Option C, the decorator**, plus an access-log middleware at the HTTP boundary.

- **Option A** (logging inside the usecase) makes the usecase depend on a logger and mixes business rules with reporting. Every usecase ends up with its own "started/completed" boilerplate that drifts over time. Tests have to supply a logger to test business logic.
- **Option B** (logging in the service layer) is in the right *place*, since it is a boundary. But as written it logs `"request", req` — the whole request, including the customer's email. It also has to be repeated in every handler.
- **Option C** gives the placement of B without repetition: `LoggingInteractor` wraps any `CreateOrderInteractor` and logs the request and response in one place, with an explicit allowlist of fields. The usecase stays free of logging code.

Two boundaries are logged, each with its own responsibility:

| Boundary | Component | Logs |
|---|---|---|
| HTTP | `accessLog` in `transport/httpapi/router.go` | method, path, status, duration |
| Usecase | `LoggingInteractor` in `observability/logging.go` | request (allowlisted fields), result, error with stack, domain events |

The domain and the repository do not log. The repository returns errors, and the decorator logs them once, at the boundary. Logging and returning the same error at every layer produces duplicate entries for a single failure.

## 2. Business logs vs operational logs

| | Business logs | Operational logs |
|---|---|---|
| Answers | *What happened in the business?* | *How is the system behaving?* |
| Examples | `order.created`, `order.paid`, `order.failed` (reason) | request started/completed, duration, HTTP status, DB/timeout errors |
| Source | Domain events | Boundaries: HTTP, usecase decorator, adapters |
| Reader | Support, audit, product | On-call engineers, SRE |
| Retention | Longer (audit) | Shorter |
| Level | INFO | INFO / WARN / ERROR |

Every record carries `log_type=business|operational`, so they can be routed and retained separately. Business logs are generated from **domain events**, the same ones that become span events and outbox rows, so a log line cannot contradict what was actually persisted.

Actual output from a local run (email was in the request; it is not in the logs):

```json
{"level":"INFO","msg":"create order started","log_type":"operational","request":{"customer_id":"c-1","customer_tier":"premium","items_count":1},"trace_id":"4bf92f3577b34da6a3ce929d0e0e4736","span_id":"bf0da8ae448a5fba"}
{"level":"INFO","msg":"order.paid","log_type":"business","order_id":"ord_2330…","occurred_at":"2026-09-28T10:57:44.872137142Z","payment_id":"pay_7gbniznaqtmn","trace_id":"11112222…"}
{"level":"WARN","msg":"create order rejected","log_type":"operational","duration_ms":0,"reason":"new order: order has no items","trace_id":"358956b2…"}
```

### Levels

- **ERROR** — a failure that needs attention (DB down, payment API 5xx or timeout). Always includes a stack trace.
- **WARN** — an expected business outcome: invalid order, payment declined, or payment pending confirmation (unknown charge outcome, order kept in `payment_pending`). No stack; it is not a bug. Mixing these into ERROR makes error-based alerts useless.
- **INFO** — boundaries and business events.

## 3. How do you avoid logging sensitive data?

Defense in depth, from most to least important:

1. **Allowlist, never whole objects.** Decorators log explicitly chosen fields (`customer_id`, `customer_tier`, `items_count`), never `req` or raw request bodies. A field added to the request later does not end up in the logs by accident.
2. **Redaction as a safety net.** `RedactPII` is installed as `slog.HandlerOptions.ReplaceAttr` and replaces values of sensitive keys (`email`, `phone`, `card`, `card_number`, `cvv`, `password`, `token`, `authorization`, `address`) with `[REDACTED]` at any nesting level.
3. **Same rules for traces.** Span attributes contain IDs and enums only.
4. **Errors are not echoed to clients.** A 500 response says `internal error`; the details go to logs and traces only.
5. **Tests.** `TestLoggingInteractor_BoundaryAndBusinessLogs` sends a request with an email and fails if the email appears anywhere in the output.

Further steps for production: typed PII fields (e.g. `type Email string` with a `LogValue()` that masks it) and log-pipeline scanning for patterns such as card numbers.

## Implementation

`observability/logging.go`, JSON via `log/slog`:

- [x] **Trace ID in every log** — `TraceHandler` wraps any `slog.Handler` and adds `trace_id` / `span_id` from `ctx`. Logs must be written with `*Context` methods (`InfoContext`, `ErrorContext`); `NewLogger` builds the whole chain.
- [x] **Request/response logging at boundaries** — `LoggingInteractor` (usecase) and `accessLog` (HTTP).
- [x] **Error logging with stack traces** — `WithStack(err)` captures the stack where the error originated. Adapters use it, e.g. `payment/client.go`. `ErrorAttr(err)` logs `error.message` and `error.stack`; if the error carries no stack, the stack of the logging point is used.
- [x] **PII redaction** — allowlist + `RedactPII`.

Tests: `TestLogger_AddsTraceIDAndRedactsPII`, `TestLoggingInteractor_BoundaryAndBusinessLogs`, `TestExpectedErrors_WarnWithoutStackAndNoSpanError`.
