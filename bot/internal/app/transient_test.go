package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/LouisMoretti/Undelete/bot/internal/business"
	"github.com/LouisMoretti/Undelete/bot/internal/metrics"
	"github.com/LouisMoretti/Undelete/bot/internal/telegram"
)

func TestIsTransientClassifiesEveryFailureFamily(t *testing.T) {
	connRefused := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain error", errors.New("boom"), false},
		{"shutdown", context.Canceled, false},
		{"deadline", fmt.Errorf("resolve: %w", context.DeadlineExceeded), false},
		{"telegram 429", &telegram.APIError{Code: 429, RetryAfter: 3}, true},
		{"telegram 502 wrapped", fmt.Errorf("getBusinessConnection bc: %w", &telegram.APIError{Code: 502}), true},
		{"telegram 400", &telegram.APIError{Code: 400}, false},
		{"telegram 403", &telegram.APIError{Code: 403}, false},
		{"pg connection failure", &pgconn.PgError{Code: "08006"}, true},
		{"pg serialization", &pgconn.PgError{Code: "40001"}, true},
		{"pg deadlock", &pgconn.PgError{Code: "40P01"}, true},
		{"pg admin shutdown", fmt.Errorf("message save: %w", &pgconn.PgError{Code: "57P01"}), true},
		{"pg too many connections", &pgconn.PgError{Code: "53300"}, true},
		{"pg unique violation", &pgconn.PgError{Code: "23505"}, false},
		{"pg undefined table", &pgconn.PgError{Code: "42P01"}, false},
		{"network refused", fmt.Errorf("calling getBusinessConnection: %w", connRefused), true},
		{"network timeout", &net.OpError{Op: "read", Err: timeoutErr{}}, false},
		{"connection cut mid-reply", fmt.Errorf("reading: %w", io.ErrUnexpectedEOF), true},
		{"refusal", business.ErrOwnerNotAllowed, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTransient(tc.err); got != tc.want {
				t.Fatalf("isTransient(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

// fakeClock drives transientRetry without real sleeps: every wait is recorded
// and advances the clock.
type fakeClock struct {
	now   time.Time
	waits []time.Duration
}

func newTestRetry(clock *fakeClock) *transientRetry {
	r := newTransientRetry()
	r.now = func() time.Time { return clock.now }
	r.sleep = func(ctx context.Context, d time.Duration) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		clock.waits = append(clock.waits, d)
		clock.now = clock.now.Add(d)
		return nil
	}
	return r
}

var errDBDown = &pgconn.PgError{Code: "08006"}

func failing(n int, err error, calls *int) func() error {
	return func() error {
		*calls++
		if *calls <= n {
			return err
		}
		return nil
	}
}

func TestTransientRetryRecoversFromABlip(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_800_000_000, 0)}
	r := newTestRetry(clock)
	retriesBefore := metricValue(t, "undelete_update_retries_total")

	calls := 0
	if err := r.run(context.Background(), failing(1, errDBDown, &calls)); err != nil {
		t.Fatalf("run = %v, want the second attempt's success", err)
	}
	if calls != 2 {
		t.Fatalf("attempts = %d, want 2", calls)
	}
	if got := metricValue(t, "undelete_update_retries_total") - retriesBefore; got != 1 {
		t.Fatalf("retries counted = %d, want 1", got)
	}
	// Jittered around the base delay: half of it at least, never above it.
	if len(clock.waits) != 1 || clock.waits[0] < transientBaseDelay/2 || clock.waits[0] > transientBaseDelay {
		t.Fatalf("waits = %v, want one jittered wait within [%v, %v]", clock.waits, transientBaseDelay/2, transientBaseDelay)
	}
}

func TestTransientRetryNeverRetriesAPermanentFailure(t *testing.T) {
	r := newTestRetry(&fakeClock{now: time.Unix(1_800_000_000, 0)})
	calls := 0
	permanent := &pgconn.PgError{Code: "23505"}
	if err := r.run(context.Background(), failing(5, permanent, &calls)); !errors.Is(err, permanent) {
		t.Fatalf("run = %v, want the permanent error", err)
	}
	if calls != 1 {
		t.Fatalf("attempts = %d, want 1: a constraint violation does not heal", calls)
	}
}

func TestTransientRetryStopsOnAPermanentFailureMidway(t *testing.T) {
	r := newTestRetry(&fakeClock{now: time.Unix(1_800_000_000, 0)})
	calls := 0
	permanent := errors.New("constraint")
	err := r.run(context.Background(), func() error {
		calls++
		if calls == 1 {
			return errDBDown
		}
		return permanent
	})
	if !errors.Is(err, permanent) || calls != 2 {
		t.Fatalf("run = %v after %d attempts, want the permanent error after 2", err, calls)
	}
}

// TestTransientRetryDegradesAfterAnExhaustedBudget pins the outage bound: once
// one update spent its whole budget, the following ones get a single attempt
// until the cooldown elapses or anything succeeds.
func TestTransientRetryDegradesAfterAnExhaustedBudget(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_800_000_000, 0)}
	r := newTestRetry(clock)
	droppedBefore := metricValue(t, "undelete_updates_dropped_transient_total")

	calls := 0
	if err := r.run(context.Background(), failing(100, errDBDown, &calls)); !errors.Is(err, errDBDown) {
		t.Fatalf("run = %v, want the transient error once the budget is spent", err)
	}
	if calls != transientAttempts {
		t.Fatalf("attempts = %d, want %d", calls, transientAttempts)
	}
	if got := metricValue(t, "undelete_updates_dropped_transient_total") - droppedBefore; got != 1 {
		t.Fatalf("dropped counted = %d, want 1", got)
	}

	calls = 0
	_ = r.run(context.Background(), failing(100, errDBDown, &calls))
	if calls != 1 {
		t.Fatalf("attempts while degraded = %d, want 1", calls)
	}

	clock.now = clock.now.Add(transientCooldown + time.Second)
	calls = 0
	_ = r.run(context.Background(), failing(100, errDBDown, &calls))
	if calls != transientAttempts {
		t.Fatalf("attempts after the cooldown = %d, want the full %d", calls, transientAttempts)
	}

	// Still degraded (the last run exhausted again); one success heals it.
	calls = 0
	if err := r.run(context.Background(), failing(0, nil, &calls)); err != nil {
		t.Fatalf("healthy run = %v", err)
	}
	calls = 0
	_ = r.run(context.Background(), failing(100, errDBDown, &calls))
	if calls != transientAttempts {
		t.Fatalf("attempts after a success = %d, want the full %d", calls, transientAttempts)
	}
}

func TestTransientRetryHonoursRetryAfterWithinItsCap(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_800_000_000, 0)}
	r := newTestRetry(clock)

	calls := 0
	_ = r.run(context.Background(), failing(1, &telegram.APIError{Code: 429, RetryAfter: 2}, &calls))
	calls = 0
	_ = r.run(context.Background(), failing(1, &telegram.APIError{Code: 429, RetryAfter: 3600}, &calls))

	want := []time.Duration{2 * time.Second, transientMaxWait}
	if len(clock.waits) != 2 || clock.waits[0] != want[0] || clock.waits[1] != want[1] {
		t.Fatalf("waits = %v, want %v (retry_after, then capped)", clock.waits, want)
	}
}

func TestTransientRetryStopsOnShutdown(t *testing.T) {
	r := newTestRetry(&fakeClock{now: time.Unix(1_800_000_000, 0)})
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	err := r.run(ctx, func() error {
		calls++
		cancel()
		return errDBDown
	})
	if !errors.Is(err, errDBDown) || calls != 1 {
		t.Fatalf("run = %v after %d attempts, want the first error and no retry once shutdown began", err, calls)
	}
}

// flakyBusiness fails the first resolutions with a transient error.
type flakyBusiness struct {
	fakeBusiness
	failures int
	calls    int
}

func (f *flakyBusiness) Resolve(ctx context.Context, id string) (*business.Connection, error) {
	f.calls++
	if f.calls <= f.failures {
		return nil, fmt.Errorf("getBusinessConnection %s: %w", id, &telegram.APIError{Code: 502})
	}
	return f.fakeBusiness.Resolve(ctx, id)
}

// TestHandleUpdateRetriesATransientResolution is the end-to-end half: a
// message whose connection resolution hits a Telegram 5xx is still captured,
// once, instead of being skipped by the poller.
func TestHandleUpdateRetriesATransientResolution(t *testing.T) {
	biz := &flakyBusiness{
		fakeBusiness: fakeBusiness{connections: map[string]*business.Connection{"bc-1": enabledConn()}},
		failures:     1,
	}
	msgs := &fakeMessages{}
	h := NewHandler(biz, msgs, nil, testLogger())
	h.retry = newTestRetry(&fakeClock{now: time.Unix(1_800_000_000, 0)})

	if err := h.HandleUpdate(context.Background(), telegram.Update{UpdateID: 1, BusinessMessage: testMessage()}); err != nil {
		t.Fatalf("HandleUpdate = %v, want the retried capture to succeed", err)
	}
	if len(msgs.saved) != 1 {
		t.Fatalf("saved = %d, want exactly 1", len(msgs.saved))
	}
}

func metricValue(t *testing.T, name string) int64 {
	t.Helper()
	for _, line := range strings.Split(metrics.Default().RenderPrometheus(), "\n") {
		if value, ok := strings.CutPrefix(line, name+" "); ok {
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				t.Fatalf("series %s: %v", name, err)
			}
			return n
		}
	}
	t.Fatalf("series %s not exposed", name)
	return 0
}
