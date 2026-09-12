package outbox

import (
	"bytes"
	"context"
	"strings"
	"testing"
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
