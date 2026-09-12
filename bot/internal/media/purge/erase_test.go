package purge

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LouisMoretti/Undelete/bot/internal/media"
	"github.com/LouisMoretti/Undelete/bot/internal/media/store"
)

// TestEraseTenantRemovesEveryBlobAndRow: /delete_my_data does not care about
// status, retention or grace. A pending row whose download landed a second ago,
// a stored one, a purged one that still remembers a thumbnail, a fresh orphan
// and a temporary file of a download in flight all go — the tenant's connections
// were disabled before this ran, so there is nothing in flight to protect.
func TestEraseTenantRemovesEveryBlobAndRow(t *testing.T) {
	root := t.TempDir()
	cat := newCatalogue(fixedNow)

	stored := writeMedia(t, root, "42/2026-03/01/stored", 0)
	thumb := writeMedia(t, root, "42/2026-03/01/stored-thumb", 0)
	pending := writeMedia(t, root, "42/2026-03/01/pending", 0)
	orphan := writeMedia(t, root, "42/2026-03/01/orphan", 0)
	temp := writeMedia(t, root, "42/2026-03/01/"+store.TempPrefix+"inflight", 0)

	row := cat.add(1, media.StatusStored, stored, fixedNow())
	row.file.ThumbnailRelativePath = thumb
	cat.add(2, media.StatusPending, pending, fixedNow())
	// A purged row keeps no path: nothing to unlink, and the row still goes.
	cat.add(3, media.StatusPurged, "", fixedNow())

	p := newPurger(t, root, cat, false)
	files, rows, err := p.EraseTenant(context.Background(), testOwner)
	if err != nil {
		t.Fatalf("EraseTenant: %v", err)
	}
	if rows != 3 {
		t.Fatalf("rows deleted = %d, want 3", rows)
	}
	// stored + thumbnail + pending from the catalogue, orphan + temp from the
	// sweep.
	if files != 5 {
		t.Fatalf("files deleted = %d, want 5", files)
	}
	if len(cat.rows) != 0 {
		t.Fatalf("%d catalogue rows survived", len(cat.rows))
	}
	for _, rel := range []string{stored, thumb, pending, orphan, temp} {
		if _, err := os.Lstat(filepath.Join(root, rel)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s is still on disk: %v", rel, err)
		}
	}
	// The subtree itself goes too: an empty <root>/42 left behind would be the
	// last trace that this tenant ever stored anything.
	if _, err := os.Stat(filepath.Join(root, "42")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the tenant subtree survived: %v", err)
	}
}

// TestEraseTenantTouchesNoOtherTenant is the cross-tenant guarantee at the
// filesystem level: the walk is rooted at the tenant's own subtree, so another
// owner's files are not merely spared, they are never even visited.
func TestEraseTenantTouchesNoOtherTenant(t *testing.T) {
	root := t.TempDir()
	cat := newCatalogue(fixedNow)

	mine := writeMedia(t, root, "42/2026-03/01/mine", 0)
	theirs := writeMedia(t, root, "99/2026-03/01/theirs", 0)
	cat.add(1, media.StatusStored, mine, fixedNow())

	p := newPurger(t, root, cat, false)
	if _, _, err := p.EraseTenant(context.Background(), testOwner); err != nil {
		t.Fatalf("EraseTenant: %v", err)
	}

	if _, err := os.Stat(filepath.Join(root, theirs)); err != nil {
		t.Fatalf("another tenant's file was touched: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, mine)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the erased tenant's file survived: %v", err)
	}
}

// TestEraseTenantIgnoresTheDryRun: MEDIA_PURGE_DRY_RUN holds back the deletions
// the bot decides on its own. This one was asked for, in writing, with a
// confirmation code — honouring the dry run here would make the confirmation
// message a lie.
func TestEraseTenantIgnoresTheDryRun(t *testing.T) {
	root := t.TempDir()
	cat := newCatalogue(fixedNow)
	stored := writeMedia(t, root, "42/2026-03/01/stored", 0)
	cat.add(1, media.StatusStored, stored, fixedNow())

	p := newPurger(t, root, cat, true)
	files, rows, err := p.EraseTenant(context.Background(), testOwner)
	if err != nil {
		t.Fatalf("EraseTenant: %v", err)
	}
	if files != 1 || rows != 1 {
		t.Fatalf("files=%d rows=%d in dry run, want 1 and 1", files, rows)
	}
	if _, err := os.Stat(filepath.Join(root, stored)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the dry run held back an erasure the owner confirmed")
	}
}

// TestEraseTenantIsIdempotent: rerunning it after a completed erasure, and
// rerunning it over blobs an interrupted attempt already unlinked, must both be
// silent no-ops. That is what makes the whole command resumable.
func TestEraseTenantIsIdempotent(t *testing.T) {
	root := t.TempDir()
	cat := newCatalogue(fixedNow)
	stored := writeMedia(t, root, "42/2026-03/01/stored", 0)
	cat.add(1, media.StatusStored, stored, fixedNow())

	p := newPurger(t, root, cat, false)
	if _, _, err := p.EraseTenant(context.Background(), testOwner); err != nil {
		t.Fatalf("first EraseTenant: %v", err)
	}
	files, rows, err := p.EraseTenant(context.Background(), testOwner)
	if err != nil {
		t.Fatalf("second EraseTenant: %v", err)
	}
	if files != 0 || rows != 0 {
		t.Fatalf("the second run deleted files=%d rows=%d, want 0 and 0", files, rows)
	}

	// The crash window the ordering leaves open: a row still pointing at a blob
	// that is already gone. Rerunning must absorb it rather than fail.
	cat.add(2, media.StatusStored, "42/2026-03/01/already-unlinked", fixedNow())
	if _, rows, err = p.EraseTenant(context.Background(), testOwner); err != nil {
		t.Fatalf("EraseTenant over a missing blob: %v", err)
	}
	if rows != 1 {
		t.Fatalf("rows deleted = %d, want 1", rows)
	}
}

// TestEraseTenantRefusesIrregularEntries: a symlink planted in the media tree
// is refused and reported, never followed and never unlinked — the same rule
// the retention path follows. The erasure then returns an error rather than
// claiming a completeness it declined to reach, and the row deletions it did
// perform stand (the command is resumable).
func TestEraseTenantRefusesIrregularEntries(t *testing.T) {
	root := t.TempDir()
	cat := newCatalogue(fixedNow)

	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("not ours"), 0o600); err != nil {
		t.Fatalf("write bait: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "42/2026-03/01"), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	link := filepath.Join(root, "42/2026-03/01/link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable on this filesystem: %v", err)
	}

	p := newPurger(t, root, cat, false)
	_, _, err := p.EraseTenant(context.Background(), testOwner)
	if err == nil {
		t.Fatal("EraseTenant reported success over an entry it refused to touch")
	}
	if !errors.Is(err, ErrUnsafeTarget) && !strings.Contains(err.Error(), ErrUnsafeTarget.Error()) {
		t.Fatalf("error = %v, want an ErrUnsafeTarget refusal", err)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("the symlink was removed: %v", err)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("the symlink was followed and its target deleted: %v", err)
	}
}

// TestEraseTenantRefusesAnUnreachableRoot: an unmounted volume must not be read
// as "this tenant had no attachments". Every unlink would be a silent no-op and
// the owner would be told their files are gone while they sit on the volume that
// was actually meant.
func TestEraseTenantRefusesAnUnreachableRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	if err := os.MkdirAll(root, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	p := newPurger(t, root, newCatalogue(fixedNow), false)
	if err := os.Remove(root); err != nil {
		t.Fatalf("remove root: %v", err)
	}

	if _, _, err := p.EraseTenant(context.Background(), testOwner); err == nil {
		t.Fatal("EraseTenant accepted an unreachable media root")
	}
}
