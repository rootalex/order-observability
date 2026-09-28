// Package setup поднимает зависимости для интеграционных тестов (testcontainers-go).
package setup

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/rootalex/order-observability/repo/postgres"
)

// SkipIfShort — интеграционные тесты не запускаются с `go test -short`.
func SkipIfShort(t testing.TB) {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test: requires Docker")
	}
}

// StartPostgres поднимает Postgres в контейнере, накатывает migrations/*.up.sql
// и возвращает пул соединений. Контейнер удаляется по окончании теста.
func StartPostgres(ctx context.Context, t testing.TB) *sql.DB {
	t.Helper()
	SkipIfShort(t)

	ctr, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("orders"),
		tcpostgres.WithUsername("orders"),
		tcpostgres.WithPassword("orders"),
		tcpostgres.BasicWaitStrategies(),
	)
	testcontainers.CleanupContainer(t, ctr)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("postgres dsn: %v", err)
	}
	db, err := postgres.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	applyMigrations(ctx, t, db)
	return db
}

func applyMigrations(ctx context.Context, t testing.TB, db *sql.DB) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(projectRoot(), "migrations", "*.up.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no migrations found: %v", err)
	}
	sort.Strings(files)
	for _, f := range files {
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if _, err := db.ExecContext(ctx, string(body)); err != nil {
			t.Fatalf("apply %s: %v", filepath.Base(f), err)
		}
	}
}

func projectRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..")
}

// WireMock — контейнер WireMock с управлением стабами через admin API.
type WireMock struct {
	BaseURL string
}

func StartWireMock(ctx context.Context, t testing.TB) *WireMock {
	t.Helper()
	SkipIfShort(t)

	ctr, err := testcontainers.Run(ctx, "wiremock/wiremock:3.9.1",
		testcontainers.WithExposedPorts("8080/tcp"),
		testcontainers.WithWaitStrategy(
			wait.ForHTTP("/__admin/mappings").WithPort("8080/tcp").WithStartupTimeout(60*time.Second),
		),
	)
	testcontainers.CleanupContainer(t, ctr)
	if err != nil {
		t.Fatalf("start wiremock: %v", err)
	}

	endpoint, err := ctr.PortEndpoint(ctx, "8080/tcp", "http")
	if err != nil {
		t.Fatalf("wiremock endpoint: %v", err)
	}
	return &WireMock{BaseURL: endpoint}
}

// Stub регистрирует маппинг (формат WireMock JSON: request/response).
func (w *WireMock) Stub(t testing.TB, mapping any) {
	t.Helper()
	w.admin(t, http.MethodPost, "/__admin/mappings", mapping, http.StatusCreated, nil)
}

// Reset удаляет все стабы и журнал запросов — изоляция между подтестами.
func (w *WireMock) Reset(t testing.TB) {
	t.Helper()
	w.admin(t, http.MethodPost, "/__admin/reset", nil, http.StatusOK, nil)
}

// CountRequests возвращает число запросов, совпавших с паттерном.
func (w *WireMock) CountRequests(t testing.TB, pattern any) int {
	t.Helper()
	var out struct {
		Count int `json:"count"`
	}
	w.admin(t, http.MethodPost, "/__admin/requests/count", pattern, http.StatusOK, &out)
	return out.Count
}

func (w *WireMock) admin(t testing.TB, method, path string, body any, wantStatus int, out any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode wiremock request: %v", err)
		}
	}
	req, err := http.NewRequest(method, w.BaseURL+path, &buf)
	if err != nil {
		t.Fatalf("wiremock request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("wiremock %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != wantStatus {
		t.Fatalf("wiremock %s %s: status %d, want %d", method, path, resp.StatusCode, wantStatus)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("decode wiremock response: %v", err)
		}
	}
}
