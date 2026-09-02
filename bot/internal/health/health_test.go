package health

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/LouisMoretti/Undelete/bot/internal/metrics"
)

// fakePinger plays the database: it is the only way to test the degraded
// "database unreachable" state without PostgreSQL.
type fakePinger struct{ err error }

func (p fakePinger) Ping(context.Context) error { return p.err }

type fakePoller struct{ last time.Time }

func (p fakePoller) LastSuccessfulPoll() time.Time { return p.last }

// now is the tests' frozen clock: poller freshness is tested by arithmetic,
// never by a real wait.
var now = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func newTestHandler(dbErr error, lastPoll time.Time) *Handler {
	return NewHandler(fakePinger{err: dbErr}, fakePoller{last: lastPoll}, &metrics.Counters{}, func() time.Time { return now })
}

func do(t *testing.T, h *Handler, path string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.Mux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	body, err := io.ReadAll(rec.Result().Body)
	if err != nil {
		t.Fatalf("reading the body: %v", err)
	}
	return rec.Code, string(body)
}

func decodeChecks(t *testing.T, body string) (string, map[string]string) {
	t.Helper()
	var parsed struct {
		Status string            `json:"status"`
		Checks map[string]string `json:"checks"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("invalid JSON response %q: %v", body, err)
	}
	return parsed.Status, parsed.Checks
}

func TestLivezRespondsOKWithoutDependency(t *testing.T) {
	// Dead database and poller never started: liveness stays green, otherwise
	// the orchestrator would restart a perfectly alive process.
	code, body := do(t, newTestHandler(errors.New("connection refused"), time.Time{}), "/livez")

	if code != http.StatusOK {
		t.Fatalf("code = %d, expected %d", code, http.StatusOK)
	}
	status, _ := decodeChecks(t, body)
	if status != "ok" {
		t.Fatalf("status = %q, expected \"ok\"", status)
	}
}

func TestReadyzOKWhenDatabaseAndPollerHealthy(t *testing.T) {
	code, body := do(t, newTestHandler(nil, now.Add(-10*time.Second)), "/readyz")

	if code != http.StatusOK {
		t.Fatalf("code = %d, expected %d (body %s)", code, http.StatusOK, body)
	}
	status, checks := decodeChecks(t, body)
	if status != "ok" || checks["database"] != "ok" || checks["poller"] != "ok" {
		t.Fatalf("healthy readyz expected, got status=%q checks=%v", status, checks)
	}
}

func TestReadyzDegradedWhenDatabaseUnreachable(t *testing.T) {
	// The error carries a full DSN: the response must not take ANYTHING from it.
	dbErr := errors.New("dial postgres://undelete_app:s3cret@postgres:5432/undelete: connection refused")
	code, body := do(t, newTestHandler(dbErr, now.Add(-time.Second)), "/readyz")

	if code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, expected %d", code, http.StatusServiceUnavailable)
	}
	status, checks := decodeChecks(t, body)
	if status != "degraded" || checks["database"] != "unreachable" {
		t.Fatalf("status=%q checks=%v", status, checks)
	}
	if checks["poller"] != "ok" {
		t.Fatalf("the poller must remain healthy, checks=%v", checks)
	}
	for _, leak := range []string{"s3cret", "postgres://", "connection refused"} {
		if strings.Contains(body, leak) {
			t.Fatalf("leak of %q in the response: %s", leak, body)
		}
	}
}

func TestReadyzDegradedWhenPollerStale(t *testing.T) {
	code, body := do(t, newTestHandler(nil, now.Add(-pollerFreshnessThreshold-time.Second)), "/readyz")

	if code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, expected %d", code, http.StatusServiceUnavailable)
	}
	status, checks := decodeChecks(t, body)
	if status != "degraded" || checks["poller"] != "stale" {
		t.Fatalf("status=%q checks=%v", status, checks)
	}
	if checks["database"] != "ok" {
		t.Fatalf("the database must remain healthy, checks=%v", checks)
	}
}

// The threshold is a strict bound: a poll exactly at 90s remains acceptable, a
// 50s poll (the long polling timeout) must always pass.
func TestReadyzExactThresholdTolerance(t *testing.T) {
	code, _ := do(t, newTestHandler(nil, now.Add(-pollerFreshnessThreshold)), "/readyz")
	if code != http.StatusOK {
		t.Fatalf("poll exactly at threshold: code = %d, expected %d", code, http.StatusOK)
	}
}

func TestReadyzDegradedBeforeFirstSuccessfulPoll(t *testing.T) {
	code, body := do(t, newTestHandler(nil, time.Time{}), "/readyz")

	if code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, expected %d", code, http.StatusServiceUnavailable)
	}
	if _, checks := decodeChecks(t, body); checks["poller"] != "no_successful_poll_yet" {
		t.Fatalf("checks = %v", checks)
	}
}

func TestMetricsServesPrometheusExposition(t *testing.T) {
	counters := &metrics.Counters{}
	counters.AddUpdates(5)
	h := NewHandler(fakePinger{}, fakePoller{last: now}, counters, func() time.Time { return now })

	rec := httptest.NewRecorder()
	h.Mux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != metrics.ContentType {
		t.Fatalf("Content-Type = %q, expected %q", got, metrics.ContentType)
	}
	if !strings.Contains(rec.Body.String(), "undelete_updates_total 5") {
		t.Fatalf("unexpected exposition:\n%s", rec.Body.String())
	}
}

// Serve must return on context cancellation, otherwise the binary's clean
// shutdown would remain stuck on wg.Wait().
func TestServeStopsOnContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	done := make(chan error, 1)

	go func() { done <- Serve(ctx, "127.0.0.1:0", newTestHandler(nil, now), logger) }()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve() did not stop after context cancellation")
	}
}

func TestServeDisabledIfEmptyAddress(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	if err := Serve(context.Background(), "", newTestHandler(nil, now), logger); err != nil {
		t.Fatalf("Serve(\"\") error = %v, expected nil", err)
	}
}
