# Order Service: Observability & Testing

[![CI](https://github.com/rootalex/order-observability/actions/workflows/ci.yml/badge.svg)](https://github.com/rootalex/order-observability/actions/workflows/ci.yml)

Solution for the Senior Backend assessment (Task 3).

## Layout

```
cmd/orders/          process: config, logger, tracing, HTTP server, graceful shutdown
app/                 wiring shared by cmd/orders and the E2E tests (decorator chain, relay)
domain/              pure domain: Order, domain events (no ctx, no infra)
usecase/             CreateOrder interactor + ports (incl. Probe for step observability)
transport/httpapi/   HTTP boundary: POST /orders, /metrics, /healthz
outbox/              outbox relay: publishes domain events, continues the trace
idgen/               order ID generator
repo/postgres/       OrderRepository adapter
payment/             HTTP payment client
clock/               Clock abstraction (Real / Fake)
observability/       tracing, logging, metrics decorators
testing/integration/ integration tests (testcontainers, WireMock)
testing/e2e/         E2E tests through the HTTP API (real wiring, testcontainers)
testing/setup/       container helpers
migrations/          SQL schema
```

## Local environment

```bash
make up     # Postgres + migrations, WireMock, Jaeger, Prometheus, Grafana
make run    # order service on the host
make down   # stop (make reset — also drop DB volume)
```

| Service | URL |
|---|---|
| Postgres | `postgres://orders:orders@localhost:55432/orders` |
| Payment mock (WireMock) | http://localhost:8081 |
| Jaeger UI | http://localhost:16686 (OTLP: 4317 gRPC, 4318 HTTP) |
| Prometheus | http://localhost:9090 (scrapes `host:8080/metrics`) |
| Grafana | http://localhost:3000 (Prometheus + Jaeger, exemplars → traces) |

## Running tests

```bash
make test-unit         # domain, usecase (fakes + FakeClock), observability — no Docker
make test-integration  # Postgres + WireMock via testcontainers — requires Docker
make test-e2e          # full request through the HTTP API — requires Docker
make test              # all of the above
go test -short ./...   # integration and E2E tests are skipped with -short
```

CI (`.github/workflows/ci.yml`) runs the same targets on every push and pull request: `go mod tidy` check, `make lint`, `make build`, `make test-unit`, `make test-integration` and `make test-e2e` with testcontainers on the GitHub runner's Docker.

## Documents
- [TRACING.md](TRACING.md)
- [LOGGING.md](LOGGING.md)
- [METRICS.md](METRICS.md)
- [TESTING.md](TESTING.md)
- [ANSWERS.md](ANSWERS.md)
