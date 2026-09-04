package media_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/LouisMoretti/Undelete/bot/internal/media"
)

func TestValidateRelativePath(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		safe bool
	}{
		{name: "nested generated path", path: "42/bc-1/77/1/0.jpg", safe: true},
		{name: "single component", path: "file.bin", safe: true},
		{name: "hex name", path: "42/9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08", safe: true},
		{name: "hidden-looking but not a dot component", path: "42/.hidden.jpg", safe: true},

		{name: "empty", path: "", safe: false},
		{name: "absolute", path: "/etc/passwd", safe: false},
		{name: "absolute inside media", path: "/media/42/0.jpg", safe: false},
		{name: "parent traversal", path: "../../etc/passwd", safe: false},
		{name: "parent traversal in the middle", path: "42/../../etc/passwd", safe: false},
		{name: "trailing parent", path: "42/..", safe: false},
		{name: "current directory component", path: "./42/0.jpg", safe: false},
		{name: "current directory in the middle", path: "42/./0.jpg", safe: false},
		{name: "lone dot", path: ".", safe: false},
		{name: "backslash separator", path: `42\0.jpg`, safe: false},
		{name: "backslash traversal", path: `..\..\etc\passwd`, safe: false},
		{name: "empty component", path: "42//0.jpg", safe: false},
		{name: "trailing slash", path: "42/", safe: false},
		{name: "NUL byte truncation", path: "42/0.jpg\x00.png", safe: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := media.ValidateRelativePath(tc.path)
			if tc.safe && err != nil {
				t.Fatalf("ValidateRelativePath(%q) = %v, want nil", tc.path, err)
			}
			if !tc.safe {
				if err == nil {
					t.Fatalf("ValidateRelativePath(%q) = nil, want a rejection", tc.path)
				}
				// The sentinel, not the wording: rephrasing the message must not
				// turn this guard into a test that passes for the wrong reason.
				if !errors.Is(err, media.ErrUnsafeRelativePath) {
					t.Fatalf("ValidateRelativePath(%q) = %v, want media.ErrUnsafeRelativePath", tc.path, err)
				}
			}
		})
	}
}

func TestValidateSHA256(t *testing.T) {
	const valid = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
	for _, tc := range []struct {
		name string
		hash string
		ok   bool
	}{
		{name: "lowercase hex", hash: valid, ok: true},
		{name: "all digits", hash: strings.Repeat("0", 64), ok: true},

		{name: "empty", hash: "", ok: false},
		{name: "truncated", hash: "deadbeef", ok: false},
		// The CHECK of migration 0004 anchors both ends: a longer string is not
		// a hash with a suffix, it is not a hash.
		{name: "too long", hash: valid + "00", ok: false},
		{name: "uppercase", hash: strings.ToUpper(valid), ok: false},
		{name: "non hex letter", hash: strings.Repeat("g", 64), ok: false},
		{name: "leading space", hash: " " + valid[1:], ok: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := media.ValidateSHA256(tc.hash)
			if tc.ok && err != nil {
				t.Fatalf("ValidateSHA256(%q) = %v, want nil", tc.hash, err)
			}
			if !tc.ok {
				if err == nil {
					t.Fatalf("ValidateSHA256(%q) = nil, want a rejection", tc.hash)
				}
				if !errors.Is(err, media.ErrInvalidSHA256) {
					t.Fatalf("ValidateSHA256(%q) = %v, want media.ErrInvalidSHA256", tc.hash, err)
				}
			}
		})
	}
}
