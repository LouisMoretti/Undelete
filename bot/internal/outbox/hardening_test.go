package outbox

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/LouisMoretti/Undelete/bot/internal/telegram"
)

func textUnits(s string) int {
	n := 0
	for _, r := range s {
		if u := utf16.RuneLen(r); u > 0 {
			n += u
		} else {
			n++
		}
	}
	return n
}

func TestSendTextWithNoteNeverExceedsLimit(t *testing.T) {
	huge := strings.Repeat("y", 4096)
	job := testJob()
	job.Text = huge
	job.PayloadKind = PayloadKindMedia
	job.Media = nil // forces text fallback path via deliver

	store := &fakeStore{job: job}
	sender := &fakeSender{}
	worker := newTestWorker(store, sender, &bytes.Buffer{})

	// Deliver through the media-fallback path: no media sender configured,
	// so sendMedia fails hopeless and sendText(withNote=true) runs.
	if err := worker.deliver(context.Background(), job); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if len(sender.requests) != 1 {
		t.Fatalf("requests=%d, want 1", len(sender.requests))
	}
	got := sender.requests[0].Text
	if u := textUnits(got); u > 4096 {
		t.Fatalf("fallback text is %d units, limit is 4096", u)
	}
	if !strings.HasSuffix(got, telegram.MediaUnavailableNote) {
		t.Fatalf("note must be present even after truncation")
	}
}

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

func TestDecodeCorruptPayloadFallsBackToText(t *testing.T) {
	payload, err := decodeMediaPayload([]byte(`{"items":[{invalid`))
	if err == nil {
		t.Fatalf("expected decode error for corrupt JSON")
	}
	if payload != nil {
		t.Fatalf("corrupt payload must decode to nil (text fallback), got %+v", payload)
	}
	empty, err := decodeMediaPayload(nil)
	if err != nil || empty != nil {
		t.Fatalf("empty payload must be (nil,nil), got (%v,%v)", empty, err)
	}
}
