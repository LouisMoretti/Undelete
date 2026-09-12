package media_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/LouisMoretti/Undelete/bot/internal/media"
)

// FuzzValidateRelativePath fuzzes the guard that runs BEFORE any file is
// created under ./media: it must never panic, and every verdict must agree
// with the documented rule (relative, non-empty components, no ".", ".." or
// backslash, no NUL). Run with: go test -fuzz=FuzzValidateRelativePath -fuzztime=30s ./internal/media/
func FuzzValidateRelativePath(f *testing.F) {
	seeds := []string{
		"42/bc-1/77/1/0.jpg",
		"file.bin",
		"",
		"/etc/passwd",
		"../../etc/passwd",
		"42/../../etc/passwd",
		"./42/0.jpg",
		".",
		`42\0.jpg`,
		"42//0.jpg",
		"42/",
		"42/0.jpg\x00.png",
		"42/.hidden.jpg",
		"a/b/c/d.webp",
		"../escape",
		"a/../b",
		"a/./b",
		"a ",
		" a/b",
		"\xff\xfe/evil",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, p string) {
		err := media.ValidateRelativePath(p)
		if err == nil {
			// Accepted => the rule holds verbatim.
			if p == "" {
				t.Fatalf("accepted empty path")
			}
			if strings.HasPrefix(p, "/") {
				t.Fatalf("accepted absolute path %q", p)
			}
			if strings.Contains(p, `\`) {
				t.Fatalf("accepted backslash path %q", p)
			}
			if strings.ContainsRune(p, 0) {
				t.Fatalf("accepted NUL path %q", p)
			}
			for _, c := range strings.Split(p, "/") {
				if c == "" || c == "." || c == ".." {
					t.Fatalf("accepted bad component %q in %q", c, p)
				}
			}
			return
		}
		// Rejected => the sentinel must survive wrapping (callers branch on
		// errors.Is, never on the message wording).
		if !errors.Is(err, media.ErrUnsafeRelativePath) {
			t.Fatalf("ValidateRelativePath(%q) = %v, want media.ErrUnsafeRelativePath", p, err)
		}
	})
}

// FuzzValidateSHA256 fuzzes the mirror of the migration 0004 CHECK: an
// acceptance means exactly 64 lowercase hex chars, a rejection means the
// sentinel. Run with: go test -fuzz=FuzzValidateSHA256 -fuzztime=30s ./internal/media/
func FuzzValidateSHA256(f *testing.F) {
	seeds := []string{
		"9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
		strings.Repeat("0", 64),
		"",
		"deadbeef",
		strings.Repeat("0", 65),
		strings.Repeat("G", 64),
		strings.Repeat("A", 64),
		" 9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a0",
		"xyz",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, h string) {
		err := media.ValidateSHA256(h)
		if err == nil {
			if len(h) != 64 {
				t.Fatalf("accepted hash of length %d: %q", len(h), h)
			}
			for _, c := range h {
				if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
					t.Fatalf("accepted non-lowercase-hex hash %q", h)
				}
			}
			return
		}
		if !errors.Is(err, media.ErrInvalidSHA256) {
			t.Fatalf("ValidateSHA256(%q) = %v, want media.ErrInvalidSHA256", h, err)
		}
	})
}
