package store

import (
	"errors"
	"strings"
	"testing"
)

// FuzzValidateFilePath fuzzes the Telegram file_path guard: it must never
// panic, an acceptance must satisfy every documented rule, and a rejection
// must carry ErrPathTraversal (callers branch on errors.Is, not on wording).
// Run with: go test -fuzz=FuzzValidateFilePath -fuzztime=30s ./internal/media/store/
func FuzzValidateFilePath(f *testing.F) {
	seeds := []string{
		"photos/file_1.jpg",
		"documents/a-b_c.jpg",
		"a/b/c/d.webp",
		"f",
		"",
		"   ",
		"/absolute/path.jpg",
		"../escape.jpg",
		"a/../../b.jpg",
		`a\b.jpg`,
		"https://evil.example/x.jpg",
		"a://b",
		"a//b.jpg",
		"a/./b.jpg",
		"a/\x00b.jpg",
		"a/\x1fb.jpg",
		"a/\x7fb.jpg",
		".",
		"a/.",
		"a/..",
		"a/b ",
		"\xff\xfe",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, p string) {
		err := validateFilePath(p)
		if err == nil {
			if strings.TrimSpace(p) == "" {
				t.Fatalf("accepted blank file_path %q", p)
			}
			if strings.HasPrefix(p, "/") {
				t.Fatalf("accepted absolute file_path %q", p)
			}
			if strings.Contains(p, "..") || strings.Contains(p, "\\") || strings.Contains(p, "://") {
				t.Fatalf("accepted suspicious file_path %q", p)
			}
			for _, r := range p {
				if r < 0x20 || r == 0x7f {
					t.Fatalf("accepted control char %U in %q", r, p)
				}
			}
			for _, seg := range strings.Split(p, "/") {
				if seg == "" || seg == "." {
					t.Fatalf("accepted bad segment %q in %q", seg, p)
				}
			}
			return
		}
		if !errors.Is(err, ErrPathTraversal) {
			t.Fatalf("validateFilePath(%q) = %v, want ErrPathTraversal", p, err)
		}
	})
}
