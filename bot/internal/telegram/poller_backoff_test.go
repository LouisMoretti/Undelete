package telegram

import (
	"testing"
	"time"
)

func TestPollWaitCapsRetryAfter(t *testing.T) {
	huge := &APIError{Code: 429, RetryAfter: 3600}
	if got := pollWait(time.Second, huge); got != maxBackoff {
		t.Fatalf("pollWait(3600s) = %v, want %v", got, maxBackoff)
	}
	small := &APIError{Code: 429, RetryAfter: 3}
	if got := pollWait(time.Second, small); got != 3*time.Second {
		t.Fatalf("pollWait(3s) = %v, want 3s", got)
	}
	// 429 WITHOUT retry_after is not rate-limited: falls back to backoff.
	naked := &APIError{Code: 429}
	if !naked.IsRateLimited() && pollWait(2*time.Second, naked) != 2*time.Second {
		t.Fatalf("naked 429 must use backoff")
	}
}
