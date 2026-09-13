package outbox

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/LouisMoretti/Undelete/bot/internal/telegram"
)

func TestWorkerHonoursLongRetryAfterExactly(t *testing.T) {
	store := &fakeStore{job: testJob()}
	sender := &fakeSender{err: &telegram.APIError{Method: "sendMessage", Code: 429, RetryAfter: 7200}}
	worker := newTestWorker(store, sender, &bytes.Buffer{})

	if _, err := worker.ProcessOne(context.Background(), 11); err != nil {
		t.Fatalf("ProcessOne: %v", err)
	}
	// The server's recovery timeline is stored as-is: the wait is a deadline
	// in next_attempt_at, never a sleep, so even hours block nothing.
	if store.retryIn != 2*time.Hour {
		t.Fatalf("retryIn=%v, want the exact 2h retry_after", store.retryIn)
	}
}

func TestWorkerCapsAbsurdRetryAfter(t *testing.T) {
	store := &fakeStore{job: testJob()}
	sender := &fakeSender{err: &telegram.APIError{Method: "sendMessage", Code: 429, RetryAfter: 30 * 24 * 3600}}
	worker := newTestWorker(store, sender, &bytes.Buffer{})

	if _, err := worker.ProcessOne(context.Background(), 11); err != nil {
		t.Fatalf("ProcessOne: %v", err)
	}
	if store.retryIn != maxThrottleDelay {
		t.Fatalf("retryIn=%v, want the %v sanity cap", store.retryIn, maxThrottleDelay)
	}
}

func TestRetryDelayCapsAtMaxBackoff(t *testing.T) {
	if got := retryDelay(100); got != maxBackoff {
		t.Fatalf("retryDelay(100)=%v, want %v", got, maxBackoff)
	}
	if got := retryDelay(-3); got != time.Second {
		t.Fatalf("retryDelay(-3)=%v, want 1s", got)
	}
}
