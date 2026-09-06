package outbox

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/LouisMoretti/Undelete/bot/internal/telegram"
)

func silentLogger() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

// errStoreFails every Store method with a configured error, to pin the
// worker's error wrapping on each acknowledgement path.
type errStore struct {
	fakeStore
	claimErr error
	ackErr   error
}

func (s *errStore) Claim(ctx context.Context, owner int64, lease time.Duration) (*Job, error) {
	if s.claimErr != nil {
		return nil, s.claimErr
	}
	return s.fakeStore.Claim(ctx, owner, lease)
}

func (s *errStore) MarkSent(context.Context, int64, int64, string) error { return s.ackErr }
func (s *errStore) MarkRetry(context.Context, int64, int64, string, time.Duration, string) error {
	return s.ackErr
}
func (s *errStore) MarkFailed(context.Context, int64, int64, string, string) error {
	return s.ackErr
}

// TestProcessOneClaimErrorIsWrapped pins the reservation failure: a live
// context plus a Claim error must surface a wrapped error (the poller-side
// loop logs it), never a silent skip.
func TestProcessOneClaimErrorIsWrapped(t *testing.T) {
	store := &errStore{claimErr: errors.New("connection refused")}
	worker := newTestWorker(store, &fakeSender{}, &bytes.Buffer{})
	processed, err := worker.ProcessOne(context.Background(), 11)
	if err == nil || processed {
		t.Fatalf("ProcessOne = (%t, %v), want (false, wrapped error)", processed, err)
	}
	if !strings.Contains(err.Error(), "outbox reservation") {
		t.Fatalf("error must name the reservation stage, got %v", err)
	}
}

// TestProcessOneClaimShutdownReturnsQuietly pins the shutdown path: a Claim
// failing BECAUSE the context died returns (false, nil) -- the lease will
// expire and the job will be replayed, no error to log.
func TestProcessOneClaimShutdownReturnsQuietly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	store := &errStore{claimErr: context.Canceled}
	worker := newTestWorker(store, &fakeSender{}, &bytes.Buffer{})
	processed, err := worker.ProcessOne(ctx, 11)
	if err != nil || processed {
		t.Fatalf("ProcessOne = (%t, %v), want (false, nil) on shutdown", processed, err)
	}
}

// TestProcessOneMarkSentErrorIsWrapped pins the worst acknowledgement case:
// the alert went out but MarkSent failed -- the error must surface (the job
// stays leased and may duplicate, which the logs must show).
func TestProcessOneMarkSentErrorIsWrapped(t *testing.T) {
	store := &errStore{fakeStore: fakeStore{job: testJob()}, ackErr: errors.New("tx failed")}
	worker := newTestWorker(store, &fakeSender{}, &bytes.Buffer{})
	processed, err := worker.ProcessOne(context.Background(), 11)
	if err == nil || !processed {
		t.Fatalf("ProcessOne = (%t, %v), want (true, wrapped error)", processed, err)
	}
	if !strings.Contains(err.Error(), "outbox acknowledgement") {
		t.Fatalf("error must name the acknowledgement stage, got %v", err)
	}
}

// TestProcessOneMarkSentShutdownStaysQuiet pins the shutdown-after-send path:
// the alert is out, the context is dead, MarkSent cannot succeed -- return
// (false, nil) and let the lease replay it, rather than erroring.
func TestProcessOneMarkSentShutdownStaysQuiet(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the send happened, the shutdown lands during acknowledgement
	store := &errStore{fakeStore: fakeStore{job: testJob()}, ackErr: context.Canceled}
	sender := &fakeSender{}
	worker := newTestWorker(store, sender, &bytes.Buffer{})
	processed, err := worker.ProcessOne(ctx, 11)
	if err != nil || processed {
		t.Fatalf("ProcessOne = (%t, %v), want (false, nil) on shutdown", processed, err)
	}
	if len(sender.requests) != 1 {
		t.Fatal("the alert must still have been sent before the shutdown")
	}
}

// TestProcessOneMarkRetryErrorIsWrapped pins the retry-scheduling failure:
// the send failed AND the reschedule failed -- the job outcome is unknown,
// the error must surface.
func TestProcessOneMarkRetryErrorIsWrapped(t *testing.T) {
	job := testJob()
	store := &errStore{fakeStore: fakeStore{job: job}, ackErr: errors.New("tx failed")}
	worker := newTestWorker(store, &fakeSender{err: errors.New("boom")}, &bytes.Buffer{})
	processed, err := worker.ProcessOne(context.Background(), 11)
	if err == nil || !processed {
		t.Fatalf("ProcessOne = (%t, %v), want (true, wrapped error)", processed, err)
	}
	if !strings.Contains(err.Error(), "outbox retry scheduling") {
		t.Fatalf("error must name the retry stage, got %v", err)
	}
}

// TestProcessOneMarkFailedErrorIsWrapped pins the terminal-acknowledgement
// failure on the 4xx path.
func TestProcessOneMarkFailedErrorIsWrapped(t *testing.T) {
	store := &errStore{fakeStore: fakeStore{job: testJob()}, ackErr: errors.New("tx failed")}
	sender := &fakeSender{err: &telegram.APIError{Method: "sendMessage", Code: 400}}
	worker := newTestWorker(store, sender, &bytes.Buffer{})
	processed, err := worker.ProcessOne(context.Background(), 11)
	if err == nil || !processed {
		t.Fatalf("ProcessOne = (%t, %v), want (true, wrapped error)", processed, err)
	}
	if !strings.Contains(err.Error(), "outbox permanent failure") {
		t.Fatalf("error must name the permanent-failure stage, got %v", err)
	}
}

// TestDeliverUnknownPayloadKindFallsBackToText pins the normalisation: any
// PayloadKind that is not "media" -- including a value from a future schema
// -- delivers as text rather than erroring or dropping the alert.
func TestDeliverUnknownPayloadKindFallsBackToText(t *testing.T) {
	store := &fakeStore{job: testJob()}
	store.job.PayloadKind = "video"
	sender := &fakeSender{}
	worker := newTestWorker(store, sender, &bytes.Buffer{})
	processed, err := worker.ProcessOne(context.Background(), 11)
	if err != nil || !processed {
		t.Fatalf("ProcessOne = (%t, %v), want (true, nil)", processed, err)
	}
	if len(sender.requests) != 1 || sender.requests[0].Text != "private content" {
		t.Fatalf("unknown kind must deliver the text unchanged: %+v", sender.requests)
	}
	if store.retryIn != 0 || store.failed {
		t.Fatal("unknown kind must succeed, not retry or fail")
	}
}

// TestSendTextTrimsTrailingNewlinesBeforeNote pins the fallback formatting:
// trailing blank lines are collapsed before the unavailability note, so the
// alert never shows a hole of empty lines.
func TestSendTextTrimsTrailingNewlinesBeforeNote(t *testing.T) {
	job := testJob()
	job.PayloadKind = PayloadKindMedia
	job.Media = &MediaPayload{}
	store := &fakeStore{job: job}
	job.Text = "caption here\n\n\n"
	// Plain text sender (no media support): sendMedia short-circuits to
	// errNoMediaDelivery without a call, then the text fallback goes out.
	sender := &fakeSender{}
	worker := newTestWorker(store, sender, &bytes.Buffer{})
	if err := worker.deliver(context.Background(), store.job); err != nil {
		t.Fatalf("deliver must fall back to text, got %v", err)
	}
	want := "caption here\n\n" + telegram.MediaUnavailableNote
	if sender.requests[0].Text != want {
		t.Fatalf("fallback text = %q, want %q", sender.requests[0].Text, want)
	}
}

// TestMediaIsHopelessShutdownNeverFallsBack pins the shutdown guard: when the
// delivery failed BECAUSE the context died, the media is NOT hopeless -- the
// job must keep its backoff and replay later, never degrade to text on a
// shutdown. (A definitive Telegram error received while the context also
// happens to be dead stays hopeless: the fallback is attempted, its send
// fails on the dead context, and ProcessOne still returns quietly.)
func TestMediaIsHopelessShutdownNeverFallsBack(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if mediaIsHopeless(ctx, context.Canceled) {
		t.Fatal("a context-cancellation error must never be hopeless, even on shutdown")
	}
	// A definitive Telegram error stays hopeless even when the context also
	// happens to be dead: the fallback is attempted, its send fails on the
	// dead context, and ProcessOne still returns quietly via isShutdown.
	if !mediaIsHopeless(ctx, telegram.ErrMediaUnavailable) {
		t.Fatal("a definitive media error stays hopeless; shutdown only guards context errors")
	}
	if isShutdown(context.Background(), context.DeadlineExceeded) {
		t.Fatal("a live context with a network timeout value is not a shutdown")
	}
	if !isShutdown(ctx, context.Canceled) {
		t.Fatal("a dead context with a cancellation error is a shutdown")
	}
}

// TestWorker429WithoutRetryAfterUsesBackoff pins the 429-without-retry_after
// corner: IsRateLimited() is false, so the media path must NOT fall back to
// text (429 is excluded from the 4xx-hopeless range) and the job must take
// the standard backoff instead.
func TestWorker429WithoutRetryAfterUsesBackoff(t *testing.T) {
	job := testJob()
	job.PayloadKind = PayloadKindMedia
	job.Media = &MediaPayload{Items: []MediaItem{{MediaType: "photo", RelativePath: "2026/01/01/u1/photo"}}}
	store := &fakeStore{job: job}
	sender := &fakeMediaSender{mediaErr: &telegram.APIError{Method: "sendPhoto", Code: 429}}
	worker := NewWorker(store, sender, silentLogger(), WithMediaDir(t.TempDir()))
	processed, err := worker.ProcessOne(context.Background(), 11)
	if err != nil || !processed {
		t.Fatalf("ProcessOne = (%t, %v), want (true, nil)", processed, err)
	}
	if store.failed {
		t.Fatal("a bare 429 must retry, never fail or fall back to text")
	}
	if store.retryIn != time.Second {
		t.Fatalf("retry delay = %v, want 1s standard backoff (attempts=0)", store.retryIn)
	}
}

// TestWorker408UsesBackoff pins the timeout corner on the media path: a 408
// is retryable, never hopeless.
func TestWorker408UsesBackoff(t *testing.T) {
	job := testJob()
	job.PayloadKind = PayloadKindMedia
	job.Media = &MediaPayload{Items: []MediaItem{{MediaType: "photo", RelativePath: "2026/01/01/u1/photo"}}}
	store := &fakeStore{job: job}
	sender := &fakeMediaSender{mediaErr: &telegram.APIError{Method: "sendPhoto", Code: 408}}
	worker := NewWorker(store, sender, silentLogger(), WithMediaDir(t.TempDir()))
	processed, err := worker.ProcessOne(context.Background(), 11)
	if err != nil || !processed {
		t.Fatalf("ProcessOne = (%t, %v), want (true, nil)", processed, err)
	}
	if store.failed {
		t.Fatal("a 408 must retry, never fail")
	}
}

// TestMediaErrorClassDefaults pins the log taxonomy: an unclassified media
// error is "media_error", a Telegram failure names its code.
func TestMediaErrorClassDefaults(t *testing.T) {
	if got := mediaErrorClass(errors.New("boom")); got != "media_error" {
		t.Fatalf("mediaErrorClass(generic) = %q, want media_error", got)
	}
	if got := mediaErrorClass(&telegram.APIError{Method: "sendPhoto", Code: 502}); got != "telegram_502" {
		t.Fatalf("mediaErrorClass(502) = %q, want telegram_502", got)
	}
	if got := mediaErrorClass(errNoMediaDelivery); got != "media_unsupported" {
		t.Fatalf("mediaErrorClass(no delivery) = %q, want media_unsupported", got)
	}
}

// TestPayloadKindNormalisesEmptyToText pins the legacy-row path: jobs read
// from rows written before the kind column (or built by tests) deliver as
// text.
func TestPayloadKindNormalisesEmptyToText(t *testing.T) {
	if got := payloadKind(&Job{}); got != PayloadKindText {
		t.Fatalf("payloadKind(empty) = %q, want text", got)
	}
	if got := payloadKind(&Job{PayloadKind: "future-kind"}); got != PayloadKindText {
		t.Fatalf("payloadKind(unknown) = %q, want text", got)
	}
	if got := payloadKind(&Job{PayloadKind: PayloadKindMedia}); got != PayloadKindMedia {
		t.Fatalf("payloadKind(media) = %q, want media", got)
	}
}
