package telegram

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

func utf16Units(s string) int {
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

func TestSanitizeFileNameStripsCRLFInjection(t *testing.T) {
	got := sanitizeFileName("a\r\nBcc: x\u0000\u001f")
	if strings.ContainsAny(got, "\r\n\x00") {
		t.Fatalf("CRLF/controls survived sanitization: %q", got)
	}
	// Separators become underscores, never path components.
	if got2 := sanitizeFileName(`../a/b\c"d`); strings.ContainsAny(got2, `/\"`) {
		t.Fatalf("separator survived: %q", got2)
	}
	if sanitizeFileName(".") != "" || sanitizeFileName("..") != "" {
		t.Fatalf("dot names must sanitize to empty")
	}
}

func TestSanitizeFileNameUTF8SafeTruncation(t *testing.T) {
	long := strings.Repeat("é", 100) // 200 bytes, multibyte
	got := sanitizeFileName(long)
	if len(got) > 128 {
		t.Fatalf("len=%d, want <=128", len(got))
	}
	if !utf8.ValidString(got) {
		t.Fatalf("truncation produced invalid UTF-8: %q", got)
	}
	// Pure ASCII still caps at exactly 128.
	ascii := sanitizeFileName(strings.Repeat("a", 200))
	if len(ascii) != 128 {
		t.Fatalf("ascii len=%d, want 128", len(ascii))
	}
}

func TestWithMediaUnavailableNoteRespectsLimit(t *testing.T) {
	// Short text: note appended verbatim.
	short := WithMediaUnavailableNote("hello")
	if !strings.HasSuffix(short, MediaUnavailableNote) {
		t.Fatalf("note missing: %q", short)
	}
	// Text already at the limit: result must still fit, note preserved.
	full := strings.Repeat("x", telegramTextLimit)
	got := WithMediaUnavailableNote(full)
	if u := utf16Units(got); u > telegramTextLimit {
		t.Fatalf("overflow: %d units > %d", u, telegramTextLimit)
	}
	if !strings.HasSuffix(got, MediaUnavailableNote) {
		t.Fatalf("note must survive truncation: %q", got[len(got)-40:])
	}
	// Emoji (surrogate pairs) counted in UTF-16 units, not runes.
	emoji := strings.Repeat("😀", 2048) // 4096 units exactly
	got2 := WithMediaUnavailableNote(emoji)
	if u := utf16Units(got2); u > telegramTextLimit {
		t.Fatalf("emoji overflow: %d units", u)
	}
	if !strings.HasSuffix(got2, MediaUnavailableNote) {
		t.Fatalf("note missing on emoji text")
	}
}

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

func TestClientRefusesOversizedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true,"result":`))
		// Stream more than the limit without holding it all in memory.
		chunk := strings.Repeat("x", 1<<20)
		for i := 0; i < 12; i++ {
			_, _ = w.Write([]byte(chunk))
		}
		_, _ = w.Write([]byte(`}`))
	}))
	defer srv.Close()

	c := NewClient("token", 5*time.Second, WithBaseURL(srv.URL+"/bot"))
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/bot"+"token/getUpdates", nil)
	if err := c.do(req, "getUpdates", nil); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("expected oversize error, got %v", err)
	}
}
