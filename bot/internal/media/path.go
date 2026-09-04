package media

import (
	"errors"
	"fmt"
	"strings"
)

// ErrUnsafeRelativePath is returned (wrapped) by ValidateRelativePath for any
// path that could escape the media directory. Exported sentinel so callers and
// tests identify the failure via errors.Is rather than by the message text.
var ErrUnsafeRelativePath = errors.New("unsafe media relative path")

// ErrInvalidSHA256 is returned (wrapped) by ValidateSHA256 for a hash the
// sha256 CHECK of migration 0004 would refuse. Exported sentinel, same contract
// as ErrUnsafeRelativePath.
var ErrInvalidSHA256 = errors.New("invalid sha256")

// ValidateRelativePath rejects any path that must never be joined to the media
// base directory.
//
// This duplicates the CHECK constraints of migration 0004 on purpose. The
// database constraint is the last line of defence, but it only fires once the
// row is written: by then the file may already have been created on disk under
// the very path we are refusing. Validating here lets the caller refuse BEFORE
// touching the filesystem.
//
// The rule is deliberately stricter than filepath.Clean-based checks: we do not
// normalise a suspicious path into an acceptable one, we reject it. A path is
// valid only if it is relative, non-empty, made of non-empty components, and
// contains no '.', '..' or backslash. Backslashes are rejected even on Linux:
// they are a separator on other platforms and a classic way to smuggle one past
// a naive check.
//
// Paths are generated on our side (never derived from a Telegram file name,
// which the sender chooses), so a rejection here means a bug in the generator,
// not a hostile input that must be sanitised.
func ValidateRelativePath(p string) error {
	if p == "" {
		return fmt.Errorf("%w: empty", ErrUnsafeRelativePath)
	}
	if strings.HasPrefix(p, "/") {
		return fmt.Errorf("%w: absolute path %q", ErrUnsafeRelativePath, p)
	}
	if strings.Contains(p, `\`) {
		return fmt.Errorf("%w: backslash in %q", ErrUnsafeRelativePath, p)
	}
	if strings.ContainsRune(p, 0) {
		return fmt.Errorf("%w: NUL byte in %q", ErrUnsafeRelativePath, p)
	}
	for _, component := range strings.Split(p, "/") {
		switch component {
		case "":
			// Covers both a trailing slash and a '//' in the middle: an empty
			// component designates no file.
			return fmt.Errorf("%w: empty component in %q", ErrUnsafeRelativePath, p)
		case ".", "..":
			return fmt.Errorf("%w: %q component in %q", ErrUnsafeRelativePath, component, p)
		}
	}
	return nil
}

// ValidateSHA256 mirrors the sha256 CHECK of migration 0004: 64 lowercase hex
// characters.
//
// Same reason to duplicate the constraint as ValidateRelativePath, and the same
// ordering problem: MarkStored runs AFTER the blob was written under ./media, so
// letting a malformed hash reach the server means the UPDATE fails, the row
// stays pending and the file stays on disk with nothing pointing at it. Refusing
// here names the offending field instead of returning an opaque 23514.
//
// Uppercase hex is rejected rather than lowercased: two spellings of one hash
// would break any later deduplication by content.
func ValidateSHA256(h string) error {
	if len(h) != 64 {
		return fmt.Errorf("%w: %d characters, want 64", ErrInvalidSHA256, len(h))
	}
	for _, c := range h {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return fmt.Errorf("%w: %q is not lowercase hex", ErrInvalidSHA256, h)
		}
	}
	return nil
}
