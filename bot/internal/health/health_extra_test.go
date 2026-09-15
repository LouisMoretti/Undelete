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

// TestReadyzOKWhenPollerTimestampInFuture pins clock-skew tolerance: a poll
// timestamp slightly ahead of the probe clock is fresh, never "stale".
func TestReadyzOKWhenPollerTimestampInFuture(t *testing.T) {
	code, body := do(t, newTestHandler(nil, now.Add(time.Second)), "/readyz")

	if code != http.StatusOK {
		t.Fatalf("future poll: code = %d, want %d (body %s)", code, http.StatusOK, body)
	}
	if _, checks := decodeChecks(t, body); checks[checkNamePoller] != reasonOK {
		t.Fatalf("future poll: checks = %v, want poller %q", checks, reasonOK)
	}
}

// TestReadyzOKJustInsideFreshnessThreshold tightens the freshness boundary
// from below: threshold minus a second is fresh (the exact threshold and
// threshold-plus-a-second cases live in health_test.go).
func TestReadyzOKJustInsideFreshnessThreshold(t *testing.T) {
	h := newTestHandler(nil, now.Add(-pollerFreshnessThreshold+time.Second))
	code, body := do(t, h, "/readyz")

	if code != http.StatusOK {
		t.Fatalf("poll just inside threshold: code = %d, want %d (body %s)", code, http.StatusOK, body)
	}
	if _, checks := decodeChecks(t, body); checks[checkNamePoller] != reasonOK {
		t.Fatalf("poll just inside threshold: checks = %v, want poller %q", checks, reasonOK)
	}
}

// TestReadyzDegradedWhenDependenciesNil pins the fail-closed composition at
// the HTTP level: a nil database AND a nil poller answer 503 with both fixed
// reasons (the unit-level nil branches are pinned separately above).
func TestReadyzDegradedWhenDependenciesNil(t *testing.T) {
	h := NewHandler(nil, nil, &metrics.Counters{}, func() time.Time { return now })
	rec := httptest.NewRecorder()
	h.Mux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	resp := rec.Result()
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body %s)", resp.StatusCode, body)
	}
	_, checks := decodeChecks(t, string(body))
	if len(checks) != 2 {
		t.Fatalf("checks = %v, want exactly {database, poller}", checks)
	}
	if checks[checkNameDatabase] != reasonDBUnreachable || checks[checkNamePoller] != reasonPollerNoPoll {
		t.Fatalf("checks = %v, want database=%q poller=%q", checks, reasonDBUnreachable, reasonPollerNoPoll)
	}
}

// TestLivezEnvelopeIsClosedJSON pins the liveness envelope: JSON content,
// one single "status":"ok" key -- no per-check detail, no reason strings,
// nothing that could carry user content or a DSN fragment.
func TestLivezEnvelopeIsClosedJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	newTestHandler(nil, now).Mux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/livez", nil))
	resp := rec.Result()
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	var envelope map[string]any
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("invalid JSON %q: %v", body, err)
	}
	if len(envelope) != 1 || envelope["status"] != statusOK {
		t.Fatalf("envelope = %v, want exactly {status:ok}", envelope)
	}
}

// TestReadyzEnvelopeKeysAreClosed pins the readiness envelope shape: exactly
// "status" plus "checks", and checks holds exactly the two known probes.
// Any extra key would be a leak surface for error text (DSN included).
func TestReadyzEnvelopeKeysAreClosed(t *testing.T) {
	rec := httptest.NewRecorder()
	newTestHandler(nil, now.Add(-time.Second)).Mux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	resp := rec.Result()
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("invalid JSON %q: %v", body, err)
	}
	if len(envelope) != 2 {
		t.Fatalf("envelope has %d top-level keys, want exactly {status, checks}: %s", len(envelope), body)
	}
	var status string
	if err := json.Unmarshal(envelope["status"], &status); err != nil || status != statusOK {
		t.Fatalf("status = %s, want %q", envelope["status"], statusOK)
	}
	var checks map[string]string
	if err := json.Unmarshal(envelope["checks"], &checks); err != nil {
		t.Fatalf("invalid checks %s: %v", envelope["checks"], err)
	}
	if len(checks) != 2 || checks[checkNameDatabase] != reasonOK || checks[checkNamePoller] != reasonOK {
		t.Fatalf("checks = %v, want exactly {database:ok, poller:ok}", checks)
	}
}

// TestMuxUnknownPathsAreNotFound pins that only the three probes are
// exposed: anything else is a 404, never a probe-shaped answer.
func TestMuxUnknownPathsAreNotFound(t *testing.T) {
	h := newTestHandler(nil, now)
	for _, path := range []string{"/", "/unknown", "/readyz/"} {
		rec := httptest.NewRecorder()
		h.Mux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("GET %s: code = %d, want 404", path, rec.Code)
		}
	}
}

// TestCheckDatabaseCancelledContextFailsClosed pins the timeout plumbing
// without sleeping: an already-cancelled request context resolves to
// "unreachable" immediately instead of hanging until the 2s ping bound.
func TestCheckDatabaseCancelledContextFailsClosed(t *testing.T) {
	h := NewHandler(stubPinger{}, stubPoller{last: now}, &metrics.Counters{}, func() time.Time { return now })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	if got := h.checkDatabase(ctx); got != reasonDBUnreachable {
		t.Fatalf("checkDatabase(cancelled ctx) = %q, want unreachable", got)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("cancelled probe took %v, want immediate fail-closed", elapsed)
	}
}
