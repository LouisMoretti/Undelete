package health

import (
	"context"
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

type stubPinger struct{ err error }

func (s stubPinger) Ping(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(time.Hour):
		return s.err
	}
}

type stubPoller struct{ last time.Time }

func (s stubPoller) LastSuccessfulPoll() time.Time { return s.last }

// TestNewHandlerDefaultsNilDependencies pins the defensive construction: a
// nil clock means the system clock, a nil counter set means the default
// instance -- never a nil dereference later.
func TestNewHandlerDefaultsNilDependencies(t *testing.T) {
	h := NewHandler(stubPinger{}, stubPoller{last: time.Now()}, nil, nil)
	if h.now == nil || h.counters == nil {
		t.Fatal("NewHandler must default nil clock and counters")
	}
	if h.now().IsZero() {
		t.Fatal("default clock must return the system time")
	}
}

// TestCheckDatabaseWithNilPool pins the fail-closed probe: no pool means
// "unreachable", not a panic.
func TestCheckDatabaseWithNilPool(t *testing.T) {
	h := NewHandler(nil, stubPoller{last: time.Now()}, metrics.Default(), time.Now)
	if got := h.checkDatabase(context.Background()); got != reasonDBUnreachable {
		t.Fatalf("checkDatabase(nil pool) = %q, want unreachable", got)
	}
}

// TestCheckDatabaseSlowPingerTimesOut pins the 2s probe bound without
// sleeping 2s: a pinger that only returns when the context dies must resolve
// to "unreachable" as soon as the probe's own timeout fires.
func TestCheckDatabaseSlowPingerTimesOut(t *testing.T) {
	h := NewHandler(stubPinger{}, stubPoller{last: time.Now()}, metrics.Default(), time.Now)
	start := time.Now()
	if got := h.checkDatabase(context.Background()); got != reasonDBUnreachable {
		t.Fatalf("checkDatabase(slow pinger) = %q, want unreachable", got)
	}
	if elapsed := time.Since(start); elapsed < 1500*time.Millisecond || elapsed > 30*time.Second {
		t.Fatalf("probe must honor the ~2s ping bound, took %v", elapsed)
	}
}

// TestCheckPollerWithNilSource pins the pre-start probe: no poller means "no
// successful poll yet", the same closed reason as a zero timestamp.
func TestCheckPollerWithNilSource(t *testing.T) {
	h := NewHandler(stubPinger{}, nil, metrics.Default(), time.Now)
	if got := h.checkPoller(); got != reasonPollerNoPoll {
		t.Fatalf("checkPoller(nil) = %q, want %q", got, reasonPollerNoPoll)
	}
}

// TestReadyzDoubleDegradation pins the combined failure: database AND poller
// down still answer 503 with BOTH fixed reasons -- and no error text (which
// would carry the DSN).
func TestReadyzDoubleDegradation(t *testing.T) {
	secret := "postgres://undelete_app:s3cret-db-password@db/undelete"
	h := NewHandler(fakePinger{err: errors.New("dial " + secret + ": connection refused")}, fakePoller{}, metrics.Default(), time.Now)
	rec := httptest.NewRecorder()
	h.Mux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	resp := rec.Result()
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	for _, want := range []string{reasonDBUnreachable, reasonPollerNoPoll} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("body misses reason %q: %s", want, body)
		}
	}
	if strings.Contains(string(body), "s3cret-db-password") {
		t.Fatalf("DSN leaked into the probe response: %s", body)
	}
}

// TestWriteJSONSetsContentTypeAndCode pins the envelope all probes share.
func TestWriteJSONSetsContentTypeAndCode(t *testing.T) {
	rec := httptest.NewRecorder()
	writeJSON(rec, http.StatusTeapot, map[string]any{"status": "ok"})
	resp := rec.Result()
	resp.Body.Close()
	if resp.StatusCode != http.StatusTeapot {
		t.Fatalf("status = %d, want 418", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
}

// TestServeRejectsInvalidAddress pins fail-fast startup: an unparsable
// address returns an error instead of serving nothing while the bot runs.
func TestServeRejectsInvalidAddress(t *testing.T) {
	h := NewHandler(stubPinger{}, stubPoller{last: time.Now()}, metrics.Default(), time.Now)
	if err := Serve(context.Background(), "://no-such-address", h, slog.New(slog.NewJSONHandler(io.Discard, nil))); err == nil {
		t.Fatal("Serve() accepted an invalid address")
	}
}
