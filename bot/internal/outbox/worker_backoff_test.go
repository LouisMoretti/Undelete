package outbox

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/LouisMoretti/Undelete/bot/internal/telegram"
)

func TestWorkerCapsHugeRetryAfter(t *testing.T) {
	store := &fakeStore{job: testJob()}
	sender := &fakeSender{err: &telegram.APIError{Method: "sendMessage", Code: 429, RetryAfter: 7200}}
	worker := newTestWorker(store, sender, &bytes.Buffer{})

	if _, err := worker.ProcessOne(context.Background(), 11); err != nil {
		t.Fatalf("ProcessOne: %v", err)
	}
	if store.retryIn != maxBackoff {
		t.Fatalf("retryIn=%v, want cap %v", store.retryIn, maxBackoff)
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
