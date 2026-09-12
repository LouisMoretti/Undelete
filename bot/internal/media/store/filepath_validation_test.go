package store

import "testing"

func TestValidateFilePathRejectsHostile(t *testing.T) {
	hostile := []string{
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
	}
	for _, p := range hostile {
		if err := validateFilePath(p); err == nil {
			t.Errorf("validateFilePath(%q) = nil, want error", p)
		}
	}
	legit := []string{
		"photos/file_1.jpg",
		"documents/a-b_c.jpg",
		"a/b/c/d.webp",
		"f",
	}
	for _, p := range legit {
		if err := validateFilePath(p); err != nil {
			t.Errorf("validateFilePath(%q) = %v, want nil", p, err)
		}
	}
}
