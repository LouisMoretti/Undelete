package outbox

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/LouisMoretti/Undelete/bot/internal/media"
	"github.com/LouisMoretti/Undelete/bot/internal/telegram"
	"github.com/LouisMoretti/Undelete/bot/internal/tenantexcl"
)

// verifyLeaseUpdate is the fencing check: only the holder of the current
// lease may acknowledge the row. A lost lease must surface ErrLeaseLost,
// never a silent success.
func TestVerifyLeaseUpdateContracts(t *testing.T) {
	if err := verifyLeaseUpdate(1, 7, nil); err != nil {
		t.Fatalf("verifyLeaseUpdate(1, nil) = %v, want nil", err)
	}
	for _, rows := range []int64{0, 2} {
		err := verifyLeaseUpdate(rows, 7, nil)
		if !errors.Is(err, ErrLeaseLost) {
			t.Fatalf("verifyLeaseUpdate(%d, nil) = %v, want ErrLeaseLost", rows, err)
		}
	}
	boom := errors.New("tx failed")
	err := verifyLeaseUpdate(1, 7, boom)
	if !errors.Is(err, boom) {
		t.Fatalf("verifyLeaseUpdate err = %v, want wrapped %v", err, boom)
	}
	if !strings.Contains(err.Error(), "outbox update") {
		t.Fatalf("wrapped error must name the update stage, got %v", err)
	}
}

// The lease token is the fencing value stored in PostgreSQL: it must be a
// non-empty hex token, unique per Claim.
func TestNewLeaseTokenIsHexAndUnique(t *testing.T) {
	a, err := newLeaseToken()
	if err != nil || a == "" {
		t.Fatalf("newLeaseToken() = %q, %v", a, err)
	}
	b, err := newLeaseToken()
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("two lease tokens must differ")
	}
	for _, tok := range []string{a, b} {
		raw, err := hex.DecodeString(tok)
		if err != nil || len(raw) != 16 {
			t.Fatalf("lease token %q is not 16 random bytes hex-encoded: %v", tok, err)
		}
	}
}

func TestNewRepositoryIsUsable(t *testing.T) {
	if NewRepository(nil) == nil {
		t.Fatal("NewRepository(nil) = nil")
	}
}

// Fast lane 10 attempts (~8.5 min of backoff cumulated), then failed + 6h
// slow-lane resweep. These numbers are the documented delivery contract:
// a short Telegram outage must not cost an alert, a long one must not spin.
func TestFastLaneBudgetAndSlowLaneDelay(t *testing.T) {
	if maxDeliveryAttempts != 10 {
		t.Fatalf("maxDeliveryAttempts = %d, want 10 (fast lane)", maxDeliveryAttempts)
	}
	if failedResweepDelay != 6*time.Hour {
		t.Fatalf("failedResweepDelay = %v, want 6h (slow lane)", failedResweepDelay)
	}
	want := []time.Duration{
		time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second,
		16 * time.Second, 32 * time.Second, 64 * time.Second, 128 * time.Second,
		256 * time.Second, 512 * time.Second,
	}
	for attempt, w := range want {
		if got := retryDelay(attempt); got != w {
			t.Fatalf("retryDelay(%d) = %v, want %v", attempt, got, w)
		}
	}
	var total time.Duration
	for attempt := 0; attempt < maxDeliveryAttempts-1; attempt++ {
		total += retryDelay(attempt)
	}
	if total != 511*time.Second {
		t.Fatalf("fast-lane backoff cumulated = %v, want 511s (~8.5min)", total)
	}
	if got := retryDelay(maxDeliveryAttempts); got != maxBackoff {
		t.Fatalf("retryDelay(%d) = %v, want capped %v", maxDeliveryAttempts, got, maxBackoff)
	}
}

// An empty queue is the normal idle case: no error, no send, no marking.
func TestProcessOneEmptyQueueIsQuiet(t *testing.T) {
	store := &fakeStore{}
	sender := &fakeSender{}
	worker := newTestWorker(store, sender, &bytes.Buffer{})
	processed, err := worker.ProcessOne(context.Background(), 11)
	if err != nil || processed {
		t.Fatalf("ProcessOne = (%t, %v), want (false, nil)", processed, err)
	}
	if len(sender.requests) != 0 || store.sent || store.failed {
		t.Fatalf("idle run must not send or mark: requests=%d sent=%t failed=%t",
			len(sender.requests), store.sent, store.failed)
	}
}

// Counterpart of TestProcessOneMarkFailedErrorIsWrapped on the exhaustion
// path: the fast lane is over, MarkFailed fails too -- the outcome is
// unknown and the error must name the exhaustion stage.
func TestProcessOneExhaustedMarkFailedErrorIsWrapped(t *testing.T) {
	store := &errStore{
		fakeStore: fakeStore{job: testJob(), attempts: maxDeliveryAttempts - 1},
		ackErr:    errors.New("tx failed"),
	}
	worker := newTestWorker(store, &fakeSender{err: errors.New("boom")}, &bytes.Buffer{})
	processed, err := worker.ProcessOne(context.Background(), 11)
	if err == nil || !processed {
		t.Fatalf("ProcessOne = (%t, %v), want (true, wrapped error)", processed, err)
	}
	if !strings.Contains(err.Error(), "outbox attempts exhausted") {
		t.Fatalf("error must name the exhaustion stage, got %v", err)
	}
}

// With a guard, a shutdown before the claim returns quietly without even
// reserving: the lease is untouched and the job replays after restart.
func TestProcessOneWithGuardShutdownIsQuiet(t *testing.T) {
	guard := tenantexcl.New()
	claimed := make(chan struct{})
	store := &claimSignallingStore{Store: &fakeStore{job: testJob()}, claimed: claimed}
	worker := NewWorker(store, &fakeSender{}, silentLogger(), guard)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	processed, err := worker.ProcessOne(ctx, 11)
	if err != nil || processed {
		t.Fatalf("ProcessOne = (%t, %v), want (false, nil) on shutdown", processed, err)
	}
	select {
	case <-claimed:
		t.Fatal("no Claim expected when the guard acquisition dies on shutdown")
	default:
	}
}

// 429 retry_after is STORED in next_attempt_at, never slept: even an hour
// must return immediately. If the worker ever slept the stored delay, this
// test would hang instead of passing.
func TestWorkerStoresRetryAfterWithoutSleeping(t *testing.T) {
	store := &fakeStore{job: testJob()}
	sender := &fakeSender{err: &telegram.APIError{Method: "sendMessage", Code: 429, RetryAfter: 3600}}
	worker := newTestWorker(store, sender, &bytes.Buffer{})
	start := time.Now()
	processed, err := worker.ProcessOne(context.Background(), 11)
	elapsed := time.Since(start)
	if err != nil || !processed {
		t.Fatalf("ProcessOne = (%t, %v), want (true, nil)", processed, err)
	}
	if store.retryIn != time.Hour {
		t.Fatalf("retryIn = %v, want the exact 1h retry_after", store.retryIn)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("ProcessOne took %v with a 1h retry_after: the delay must be stored, never slept", elapsed)
	}
}

func TestMediaErrorClassFullTaxonomy(t *testing.T) {
	cases := map[string]struct {
		err  error
		want string
	}{
		"generic":       {errors.New("boom"), "media_error"},
		"unsupported":   {errNoMediaDelivery, "media_unsupported"},
		"missing":       {telegram.ErrMediaUnavailable, "media_missing"},
		"too large":     {telegram.ErrMediaTooLarge, "media_too_large"},
		"unsafe path":   {media.ErrUnsafeRelativePath, "media_unsafe_path"},
		"telegram 400":  {&telegram.APIError{Method: "sendPhoto", Code: 400}, "telegram_400"},
		"telegram 429":  {&telegram.APIError{Method: "sendPhoto", Code: 429}, "telegram_429"},
		"telegram 502":  {&telegram.APIError{Method: "sendPhoto", Code: 502}, "telegram_502"},
		"wrapped miss":  {wrapForTaxonomyTest(telegram.ErrMediaUnavailable), "media_missing"},
		"wrapped 400":   {wrapForTaxonomyTest(&telegram.APIError{Method: "sendPhoto", Code: 400}), "telegram_400"},
		"wrapped large": {wrapForTaxonomyTest(telegram.ErrMediaTooLarge), "media_too_large"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := mediaErrorClass(tc.err); got != tc.want {
				t.Fatalf("mediaErrorClass = %q, want %q", got, tc.want)
			}
		})
	}
}

func wrapForTaxonomyTest(err error) error {
	return &taxonomyWrap{err: err}
}

type taxonomyWrap struct{ err error }

func (w *taxonomyWrap) Error() string { return "wrap: " + w.err.Error() }
func (w *taxonomyWrap) Unwrap() error { return w.err }

// Logs never contain message content: neither the retry reschedule nor the
// permanent failure may leak the text or the connection id, while the
// error class stays observable.
func TestWorkerLogsNeverContainContentOnRetryAndFailure(t *testing.T) {
	t.Run("retry", func(t *testing.T) {
		store := &fakeStore{job: testJob()}
		sender := &fakeSender{err: &telegram.APIError{Method: "sendMessage", Code: 503}}
		var logs bytes.Buffer
		worker := newTestWorker(store, sender, &logs)
		if _, err := worker.ProcessOne(context.Background(), 11); err != nil {
			t.Fatal(err)
		}
		out := logs.String()
		if strings.Contains(out, "private content") || strings.Contains(out, "bc-secret") {
			t.Fatalf("content or identifier leak in retry logs: %s", out)
		}
		if !strings.Contains(out, "telegram_503") {
			t.Fatalf("error class missing from retry logs: %s", out)
		}
	})
	t.Run("permanent failure", func(t *testing.T) {
		store := &fakeStore{job: testJob()}
		sender := &fakeSender{err: &telegram.APIError{Method: "sendMessage", Code: 400}}
		var logs bytes.Buffer
		worker := newTestWorker(store, sender, &logs)
		if _, err := worker.ProcessOne(context.Background(), 11); err != nil {
			t.Fatal(err)
		}
		out := logs.String()
		if strings.Contains(out, "private content") || strings.Contains(out, "bc-secret") {
			t.Fatalf("content or identifier leak in failure logs: %s", out)
		}
	})
}

// The media fallback is a plain bot message too: no business_connection_id
// on the wire, no content in the logs -- even though the job carries a
// media payload alongside its text.
func TestWorkerMediaFallbackHidesConnectionAndContent(t *testing.T) {
	job := testJob()
	job.Text = "SENTINEL-PRIVATE-FALLBACK"
	job.PayloadKind = PayloadKindMedia
	job.Media = nil // unreadable payload -> text fallback
	store := &fakeStore{job: job}
	sender := &fakeSender{}
	var logs bytes.Buffer
	worker := newTestWorker(store, sender, &logs)
	processed, err := worker.ProcessOne(context.Background(), 11)
	if err != nil || !processed {
		t.Fatalf("ProcessOne = (%t, %v), want (true, nil)", processed, err)
	}
	if len(sender.requests) != 1 {
		t.Fatalf("requests = %d, want 1 fallback text", len(sender.requests))
	}
	if sender.requests[0].ChatID != job.OwnerTelegramUserID {
		t.Fatalf("fallback addressed to %d, want owner %d", sender.requests[0].ChatID, job.OwnerTelegramUserID)
	}
	out := logs.String()
	if strings.Contains(out, "SENTINEL-PRIVATE-FALLBACK") || strings.Contains(out, "bc-secret") {
		t.Fatalf("content or identifier leak in fallback logs: %s", out)
	}
}
