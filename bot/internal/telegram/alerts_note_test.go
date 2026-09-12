package telegram

import (
	"strings"
	"testing"
	"unicode/utf16"
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
