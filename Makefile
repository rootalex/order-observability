GO           ?= go
PKGS         := $(shell $(GO) list ./...)
UNIT_PKGS    := $(filter-out %/testing/integration %/testing/e2e,$(PKGS))
INTEGRATION  := ./testing/integration/...
E2E          := ./testing/e2e/...
BIN          := bin/orders
COMPOSE      ?= docker compose

export DATABASE_URL ?= postgres://orders:orders@localhost:55432/orders?sslmode=disable
export PAYMENT_URL  ?= http://localhost:8081
export OTEL_EXPORTER_OTLP_ENDPOINT ?= http://localhost:4318

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show available targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Build the order service binary
	$(GO) build -o $(BIN) ./cmd/orders

.PHONY: run
run: ## Run the order service (start env first: make up)
	$(GO) run ./cmd/orders

.PHONY: test
test: test-unit test-integration test-e2e ## Run all tests

.PHONY: test-unit
test-unit: ## Run unit tests (no Docker)
	$(GO) test -race -count=1 $(UNIT_PKGS)

.PHONY: test-integration
test-integration: ## Run integration tests (requires Docker)
	$(GO) test -race -count=1 -v $(INTEGRATION)

.PHONY: test-e2e
test-e2e: ## Run E2E tests through the HTTP API (requires Docker)
	$(GO) test -race -count=1 -v $(E2E)

.PHONY: cover
cover: ## Run all tests with coverage report
	$(GO) test -count=1 -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -1

.PHONY: fmt
fmt: ## Format code
	gofmt -w .

.PHONY: lint
lint: ## Check formatting and run go vet
	@test -z "$$(gofmt -l .)" || (echo "gofmt needed:"; gofmt -l .; exit 1)
	$(GO) vet ./...

.PHONY: tidy
tidy: ## Tidy go.mod
	$(GO) mod tidy

.PHONY: check
check: lint test-unit ## Lint + unit tests (quick pre-push check)

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf bin coverage.out

.PHONY: up
up: ## Start local env (Postgres, migrations, WireMock, Jaeger, Prometheus, Grafana)
	$(COMPOSE) up -d
	@echo "Jaeger:     http://localhost:16686"
	@echo "Prometheus: http://localhost:9090"
	@echo "Grafana:    http://localhost:3000"
	@echo "WireMock:   http://localhost:8081/__admin"

.PHONY: down
down: ## Stop local env
	$(COMPOSE) down

.PHONY: reset
reset: ## Stop local env and delete DB volume
	$(COMPOSE) down -v

.PHONY: migrate
migrate: ## Apply migrations to local Postgres
	$(COMPOSE) run --rm migrate

.PHONY: ps
ps: ## Show local env containers
	$(COMPOSE) ps

.PHONY: logs
logs: ## Follow local env logs (make logs s=postgres)
	$(COMPOSE) logs -f $(s)
