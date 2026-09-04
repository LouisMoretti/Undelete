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
