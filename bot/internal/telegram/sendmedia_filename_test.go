package telegram

import (
	"strings"
	"testing"
	"unicode/utf8"
)

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
