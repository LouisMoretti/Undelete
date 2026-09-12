package purge

// This file is the TOCTOU-safe half of the purge: every deletion and every
// inspection of a stored path goes through directory file descriptors, never
// through a resolved-then-used path string.
//
// Why descriptors: ValidateRelativePath only constrains the COMPONENTS of a
// stored path, and os.Lstat only verifies the FINAL one. A symlink planted
// as an intermediate component (media/42/linked -> /elsewhere, catalogue row
// 42/linked/victim) passes both: the lexical checks accept the string, and
// the kernel silently traverses the parent symlink on the way to the final
// Lstat -- which then reports a genuine regular file, elsewhere on the host,
// that os.Remove dutifully unlinks. The fix is to never give the kernel a
// whole path to resolve: each component is opened in turn, relative to the
// file descriptor of its parent, with O_NOFOLLOW, so a symlink at ANY level
// fails the open (ELOOP) instead of being traversed. There is no
// resolve-then-use window because nothing is ever resolved as a string.
//
// Every function here is Linux-specific in the same way the deployment is:
// the bot ships as a Linux container, and these are raw openat/fstatat
// semantics. The stdlib syscall package carries them; no new dependency.

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/LouisMoretti/Undelete/bot/internal/media"
)

// openDirNofollow opens an existing directory and returns it, refusing to
// follow a symlink at that final component.
func openDirNofollow(path string) (*os.File, error) {
	fd, err := syscall.Open(path,
		syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

// openChildDir opens name, known to be a direct child of the open directory
// parent, refusing symlinks. The lookup is relative to parent's file
// descriptor: nothing outside that directory can be named, whatever the
// string contains.
func openChildDir(parent *os.File, name string) (*os.File, error) {
	fd, err := syscall.Openat(int(parent.Fd()), name,
		syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

// statChild stats a direct child of an open directory without following a
// final symlink (which is then visible as S_IFLNK rather than its target).
func statChild(dir *os.File, name string) (syscall.Stat_t, error) {
	var st syscall.Stat_t
	if err := syscall.Fstatat(int(dir.Fd()), name, &st, atSymlinkNofollow); err != nil {
		return syscall.Stat_t{}, err
	}
	return st, nil
}

// atSymlinkNofollow is AT_SYMLINK_NOFOLLOW, which the stdlib syscall package
// only carries as the private _AT_SYMLINK_NOFOLLOW. Stable Linux ABI, 0x100
// on every architecture; defined here rather than pulled in with a new
// dependency for one constant.
const atSymlinkNofollow = 0x100

// isRegular reports a stat that is a plain regular file.
func isRegular(st syscall.Stat_t) bool {
	return st.Mode&syscall.S_IFMT == syscall.S_IFREG
}

// isDir reports a stat that is a directory.
func isDir(st syscall.Stat_t) bool {
	return st.Mode&syscall.S_IFMT == syscall.S_IFDIR
}

// openParent walks the intermediate components of a tenant-owned relative
// path, each relative to its parent's descriptor, and returns the open
// parent directory with the final base name. A symlink at any level, or a
// non-directory, fails with an error wrapping ErrUnsafeTarget (ELOOP,
// ENOTDIR) so the caller refuses rather than follows.
//
// A missing component maps to os.ErrNotExist: the file (or its directory) is
// already gone, which for a deletion is the outcome wanted, and for an
// inspection means "absent".
func (p *Purger) openParent(ownerUserID int64, rel string) (dir *os.File, base string, err error) {
	if err := media.ValidateRelativePath(rel); err != nil {
		return nil, "", err
	}
	head, _, found := strings.Cut(rel, "/")
	if !found || head != strconv.FormatInt(ownerUserID, 10) {
		return nil, "", fmt.Errorf("%w: %q is outside the storage subtree of tenant %d",
			ErrUnsafeTarget, rel, ownerUserID)
	}
	components := strings.Split(rel, "/")
	if len(components) < 2 {
		return nil, "", fmt.Errorf("%w: %q is outside the storage subtree of tenant %d",
			ErrUnsafeTarget, rel, ownerUserID)
	}

	root, err := openDirNofollow(p.root)
	if err != nil {
		return nil, "", fmt.Errorf("opening the media root: %w", err)
	}
	dir = root
	// Every component but the last is descended through with
	// O_NOFOLLOW|O_DIRECTORY -- the tenant's own directory first of all, so
	// a tenant subtree that is a symlink is refused instead of descended
	// through.
	for _, component := range components[:len(components)-1] {
		child, err := openChildDir(dir, component)
		dir.Close()
		if err != nil {
			return nil, "", openComponentError(rel, component, err)
		}
		dir = child
	}
	return dir, components[len(components)-1], nil
}

// openComponentError classifies a failed intermediate open: absent means the
// subtree is (partly) gone, anything else is a refusal.
func openComponentError(rel, component string, err error) error {
	if errors.Is(err, syscall.ENOENT) {
		return fmt.Errorf("media subtree %q: %w", rel, os.ErrNotExist)
	}
	if errors.Is(err, syscall.ELOOP) {
		return fmt.Errorf("%w: symlink in the path of %q at %q", ErrUnsafeTarget, rel, component)
	}
	return fmt.Errorf("%w: cannot descend to %q in %q: %v", ErrUnsafeTarget, component, rel, err)
}

// removeRel unlinks the file designated by rel, owned by the given tenant,
// and reports whether there was anything to unlink.
//
// Every component is verified relative to a directory descriptor (see the
// file comment): a symlink anywhere in the chain, a non-regular target, or
// a path outside the tenant's own subtree is refused with ErrUnsafeTarget
// instead of deleted. An already absent file is a success, not an error:
// that is what makes the whole purge replayable after a crash.
func (p *Purger) removeRel(ownerUserID int64, rel, reason string, dryRun bool) (bool, error) {
	dir, base, err := p.openParent(ownerUserID, rel)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// The file or one of its parent directories is already gone:
			// the outcome is the one wanted.
			return false, nil
		}
		return false, err
	}
	defer dir.Close()

	st, err := statChild(dir, base)
	switch {
	case errors.Is(err, syscall.ENOENT):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("inspecting media file: %w", err)
	case !isRegular(st):
		return false, fmt.Errorf("%w: %s is not a regular file", ErrUnsafeTarget, rel)
	}

	if dryRun {
		p.cfg.Logger.Info("media purge: dry run, would delete a file",
			slog.String("reason", reason),
			slog.Int64("bytes", st.Size))
		p.cfg.Logger.Debug("media purge: dry run target", slog.String("relative_path", rel))
		return true, nil
	}
	if err := syscall.Unlinkat(int(dir.Fd()), base); err != nil {
		if errors.Is(err, syscall.ENOENT) {
			// Lost a race with a concurrent purge or an operator: the
			// outcome is the one we wanted.
			return false, nil
		}
		return false, fmt.Errorf("deleting media file: %w", err)
	}
	p.cfg.Logger.Debug("media purge: file deleted",
		slog.String("reason", reason), slog.String("relative_path", rel))
	return true, nil
}

// statRel inspects a tenant-owned relative path without following anything:
// symlinks are reported as what they are (S_IFLNK), never traversed. Absence
// at any level maps to os.ErrNotExist. Used by the reconciliation to decide
// "stored row, file present or not" without ever resolving a string path.
func (p *Purger) statRel(ownerUserID int64, rel string) (syscall.Stat_t, error) {
	dir, base, err := p.openParent(ownerUserID, rel)
	if err != nil {
		return syscall.Stat_t{}, err
	}
	defer dir.Close()
	st, err := statChild(dir, base)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) {
			return syscall.Stat_t{}, fmt.Errorf("media file %q: %w", rel, os.ErrNotExist)
		}
		return syscall.Stat_t{}, fmt.Errorf("inspecting media file: %w", err)
	}
	return st, nil
}
