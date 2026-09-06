package purge

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/LouisMoretti/Undelete/bot/internal/media"
	"github.com/LouisMoretti/Undelete/bot/internal/media/store"
	"github.com/LouisMoretti/Undelete/bot/internal/users"
)

// fakeCatalogue reimplements the media_files state machine in memory: the
// queries of media.Repository are plain SQL, but the DECISIONS the purge makes
// on top of them (which status goes where, and in what order relative to the
// filesystem) are what needs covering, and covering them must not need a
// database. The integration test proves the SQL half against a real
// PostgreSQL.
type fakeCatalogue struct {
	rows map[int64]*fakeRow
	now  func() time.Time

	// writes records every mutation, in order, so a test can assert both what
	// happened and what did not (dry run).
	writes []string
	// onMarkPurged runs just before a row is marked purged. Used to observe
	// the state of the disk at that instant.
	onMarkPurged func(id int64)
	// maxLimit is the largest LIMIT the purge ever asked for.
	maxLimit int
}

type fakeRow struct {
	file      media.File
	updatedAt time.Time
}

func newCatalogue(now func() time.Time) *fakeCatalogue {
	return &fakeCatalogue{rows: map[int64]*fakeRow{}, now: now}
}

func (c *fakeCatalogue) add(id int64, status, relPath string, createdAt time.Time) *fakeRow {
	row := &fakeRow{
		file: media.File{
			ID:           id,
			MediaType:    media.TypePhoto,
			RelativePath: relPath,
			Status:       status,
			CreatedAt:    createdAt,
		},
		updatedAt: createdAt,
	}
	c.rows[id] = row
	return row
}

func (c *fakeCatalogue) sorted() []*fakeRow {
	out := make([]*fakeRow, 0, len(c.rows))
	for _, row := range c.rows {
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].file.ID < out[j].file.ID })
	return out
}

func (c *fakeCatalogue) note(limit int) {
	if limit > c.maxLimit {
		c.maxLimit = limit
	}
}

func (c *fakeCatalogue) ListExpiredStored(_ context.Context, _, afterID int64, retentionDays, limit int) ([]media.File, error) {
	c.note(limit)
	cutoff := c.now().AddDate(0, 0, -retentionDays)
	var out []media.File
	for _, row := range c.sorted() {
		if row.file.Status == media.StatusStored && row.file.ID > afterID && row.file.CreatedAt.Before(cutoff) {
			out = append(out, row.file)
			if len(out) == limit {
				break
			}
		}
	}
	return out, nil
}

func (c *fakeCatalogue) ListStoredPage(_ context.Context, _, afterID int64, limit int) ([]media.File, error) {
	c.note(limit)
	var out []media.File
	for _, row := range c.sorted() {
		if row.file.Status == media.StatusStored && row.file.ID > afterID {
			out = append(out, row.file)
			if len(out) == limit {
				break
			}
		}
	}
	return out, nil
}

func (c *fakeCatalogue) KnownPaths(_ context.Context, _ int64, relPaths []string) (map[string]struct{}, error) {
	wanted := make(map[string]struct{}, len(relPaths))
	for _, p := range relPaths {
		wanted[p] = struct{}{}
	}
	known := map[string]struct{}{}
	for _, row := range c.rows {
		for _, p := range []string{row.file.RelativePath, row.file.ThumbnailRelativePath} {
			if p == "" {
				continue
			}
			if _, ok := wanted[p]; ok {
				known[p] = struct{}{}
			}
		}
	}
	return known, nil
}

func (c *fakeCatalogue) MarkPurged(_ context.Context, _, id int64) error {
	if c.onMarkPurged != nil {
		c.onMarkPurged(id)
	}
	row, ok := c.rows[id]
	if !ok {
		return media.ErrNotFound
	}
	c.writes = append(c.writes, fmt.Sprintf("purged:%d", id))
	row.file.Status = media.StatusPurged
	row.file.RelativePath = ""
	row.file.ThumbnailRelativePath = ""
	row.updatedAt = c.now()
	return nil
}

func (c *fakeCatalogue) MarkPendingRetry(_ context.Context, _, id int64) error {
	row, ok := c.rows[id]
	if !ok {
		return media.ErrNotFound
	}
	c.writes = append(c.writes, fmt.Sprintf("pending:%d", id))
	row.file.Status = media.StatusPending
	row.file.RelativePath = ""
	row.file.ThumbnailRelativePath = ""
	row.file.SHA256 = ""
	row.updatedAt = c.now()
	return nil
}

func (c *fakeCatalogue) DeleteStalePending(_ context.Context, _ int64, maxAge time.Duration, retentionDays, limit int) (int64, error) {
	c.note(limit)
	staleCutoff := c.now().Add(-maxAge)
	retentionCutoff := c.now().AddDate(0, 0, -retentionDays)
	var deleted int64
	for _, row := range c.sorted() {
		if deleted == int64(limit) {
			break
		}
		// Mirrors the SQL: retention is absolute, staleness is reset by a
		// requeue (which moves updated_at and not created_at).
		expired := row.file.CreatedAt.Before(retentionCutoff)
		stale := row.file.CreatedAt.Before(staleCutoff) && row.updatedAt.Before(staleCutoff)
		if row.file.Status == media.StatusPending && (expired || stale) {
			c.writes = append(c.writes, fmt.Sprintf("deleted:%d", row.file.ID))
			delete(c.rows, row.file.ID)
			deleted++
		}
	}
	return deleted, nil
}

func (c *fakeCatalogue) DeletePurged(_ context.Context, _ int64, grace time.Duration, retentionDays, limit int) (int64, error) {
	c.note(limit)
	retentionCutoff := c.now().AddDate(0, 0, -retentionDays)
	graceCutoff := c.now().Add(-grace)
	var deleted int64
	for _, row := range c.sorted() {
		if deleted == int64(limit) {
			break
		}
		if row.file.Status == media.StatusPurged &&
			row.file.CreatedAt.Before(retentionCutoff) &&
			row.updatedAt.Before(graceCutoff) {
			c.writes = append(c.writes, fmt.Sprintf("deleted:%d", row.file.ID))
			delete(c.rows, row.file.ID)
			deleted++
		}
	}
	return deleted, nil
}

// --- test helpers -----------------------------------------------------------

const testOwner = int64(42)

var testTenant = users.TenantRetention{OwnerUserID: testOwner, RetentionDays: 7}

func fixedNow() time.Time { return time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC) }

// writeMedia creates a file under the media root and returns its relative
// path, aged by the given duration.
func writeMedia(t *testing.T, root, rel string, age time.Duration) string {
	t.Helper()
	full := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(full, []byte("payload"), 0o640); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
	stamp := fixedNow().Add(-age)
	if err := os.Chtimes(full, stamp, stamp); err != nil {
		t.Fatalf("chtimes %s: %v", rel, err)
	}
	return rel
}

func newPurger(t *testing.T, root string, cat Catalogue, dryRun bool) *Purger {
	t.Helper()
	p, err := New(Config{MediaDir: root, Catalogue: cat, DryRun: dryRun, Now: fixedNow})
	if err != nil {
		t.Fatalf("new purger: %v", err)
	}
	return p
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Lstat(path)
	switch {
	case err == nil:
		return true
	case errors.Is(err, os.ErrNotExist):
		return false
	default:
		t.Fatalf("lstat %s: %v", path, err)
		return false
	}
}

// --- tests ------------------------------------------------------------------

// The ordering contract of the whole package: the blob leaves the disk BEFORE
// the row says it did. The reverse order would leave, on a crash, a file no row
// references any more -- the one mismatch the catalogue cannot detect on its
// own.
func TestExpiredMediaLosesItsFileBeforeItsRow(t *testing.T) {
	root := t.TempDir()
	cat := newCatalogue(fixedNow)
	rel := writeMedia(t, root, "42/2026-01/05/expired", 0)
	cat.add(1, media.StatusStored, rel, fixedNow().AddDate(0, 0, -30))

	onDiskAtMark := true
	cat.onMarkPurged = func(int64) { onDiskAtMark = exists(t, filepath.Join(root, rel)) }

	stats, err := newPurger(t, root, cat, false).Run(context.Background(), []users.TenantRetention{testTenant})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if onDiskAtMark {
		t.Fatal("the row was marked purged while its file was still on disk")
	}
	if exists(t, filepath.Join(root, rel)) {
		t.Fatal("expired file still on disk")
	}
	if cat.rows[1].file.Status != media.StatusPurged {
		t.Fatalf("status = %q, want purged", cat.rows[1].file.Status)
	}
	if stats.FilesDeleted != 1 || stats.RowsPurged != 1 {
		t.Fatalf("stats = %+v, want 1 file and 1 row", stats)
	}
}

// A media whose retention has NOT elapsed is not touched, neither on disk nor
// in the catalogue -- including by the reconciliation, which sees a stored row
// with a file exactly where it says it is.
func TestMediaWithinRetentionIsLeftAlone(t *testing.T) {
	root := t.TempDir()
	cat := newCatalogue(fixedNow)
	rel := writeMedia(t, root, "42/2026-02/28/fresh", 48*time.Hour)
	cat.add(1, media.StatusStored, rel, fixedNow().AddDate(0, 0, -2))

	if _, err := newPurger(t, root, cat, false).Run(context.Background(), []users.TenantRetention{testTenant}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !exists(t, filepath.Join(root, rel)) {
		t.Fatal("a file still within retention was deleted")
	}
	if len(cat.writes) != 0 {
		t.Fatalf("unexpected catalogue writes: %v", cat.writes)
	}
}

// The crash the whole design is about: the unlink went through, the status
// change did not. The repair depends on retention, and the two answers are not
// interchangeable -- one gets the media back, the other closes the row.
func TestReconciliationRepairsACrashBetweenUnlinkAndMark(t *testing.T) {
	root := t.TempDir()
	cat := newCatalogue(fixedNow)
	// Row 1: retention elapsed, file already gone. The interrupted run was
	// purging it; finish the job.
	cat.add(1, media.StatusStored, "42/2026-01/05/gone-expired", fixedNow().AddDate(0, 0, -30))
	// Row 2: still within retention, file gone (a lost disk, an operator's
	// rm). Nothing authorised that deletion: try to download it again.
	cat.add(2, media.StatusStored, "42/2026-02/28/gone-fresh", fixedNow().AddDate(0, 0, -1))

	stats, err := newPurger(t, root, cat, false).Run(context.Background(), []users.TenantRetention{testTenant})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := cat.rows[1].file.Status; got != media.StatusPurged {
		t.Fatalf("expired row with no file: status = %q, want purged", got)
	}
	if got := cat.rows[2].file.Status; got != media.StatusPending {
		t.Fatalf("fresh row with no file: status = %q, want pending", got)
	}
	if cat.rows[2].file.RelativePath != "" {
		t.Fatal("a requeued row still points at a file that does not exist")
	}
	if stats.Requeued != 1 || stats.RowsPurged != 1 {
		t.Fatalf("stats = %+v, want 1 requeued and 1 purged", stats)
	}
	if stats.FilesDeleted != 0 {
		t.Fatalf("nothing was on disk, yet %d deletions were reported", stats.FilesDeleted)
	}
}

// The other half of the reconciliation: a file nothing references any more.
// The grace period is the point -- a download that has just renamed its file
// and not yet committed MarkStored looks exactly like an orphan.
func TestOrphansAreDeletedOnlyOnceTheyAreOldEnough(t *testing.T) {
	root := t.TempDir()
	cat := newCatalogue(fixedNow)

	kept := writeMedia(t, root, "42/2026-02/28/referenced", 72*time.Hour)
	cat.add(1, media.StatusStored, kept, fixedNow().AddDate(0, 0, -3))
	orphan := writeMedia(t, root, "42/2026-01/09/orphan", OrphanGrace+time.Hour)
	young := writeMedia(t, root, "42/2026-03/01/just-renamed", time.Minute)

	stats, err := newPurger(t, root, cat, false).Run(context.Background(), []users.TenantRetention{testTenant})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if exists(t, filepath.Join(root, orphan)) {
		t.Fatal("an orphan past the grace period survived")
	}
	if !exists(t, filepath.Join(root, young)) {
		t.Fatal("a file younger than the grace period was deleted: a download in flight would be lost")
	}
	if !exists(t, filepath.Join(root, kept)) {
		t.Fatal("a referenced file was deleted as an orphan")
	}
	if stats.Orphans != 1 {
		t.Fatalf("orphans = %d, want 1", stats.Orphans)
	}
}

// The .dl-* leftovers of media/store are never referenced by construction, so
// age is the only signal available: old enough is a crash leftover, recent is a
// transfer in progress whose rename has not happened yet.
func TestTemporaryDownloadsAreDeletedOnlyWhenStale(t *testing.T) {
	root := t.TempDir()
	cat := newCatalogue(fixedNow)
	stale := writeMedia(t, root, "42/2026-01/09/"+store.TempPrefix+"crashed", StaleTempAge+time.Hour)
	inFlight := writeMedia(t, root, "42/2026-03/01/"+store.TempPrefix+"running", time.Minute)

	stats, err := newPurger(t, root, cat, false).Run(context.Background(), []users.TenantRetention{testTenant})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if exists(t, filepath.Join(root, stale)) {
		t.Fatal("a stale temporary file survived")
	}
	if !exists(t, filepath.Join(root, inFlight)) {
		t.Fatal("a download in flight was deleted")
	}
	if stats.TempRemoved != 1 || stats.Orphans != 0 {
		t.Fatalf("stats = %+v, want 1 temp and 0 orphans", stats)
	}
}

// A symlink in the media tree is never followed and never deleted: following
// one would turn a write anywhere in the tree into the deletion of an arbitrary
// file on the host. It is refused loudly instead, and the row is left stored so
// an operator can still find what happened.
func TestSymlinksAreRefusedNeverFollowed(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "precious")
	if err := os.WriteFile(outside, []byte("do not delete"), 0o640); err != nil {
		t.Fatalf("write victim: %v", err)
	}

	cat := newCatalogue(fixedNow)
	rel := "42/2026-01/05/linked"
	full := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Symlink(outside, full); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	cat.add(1, media.StatusStored, rel, fixedNow().AddDate(0, 0, -30))

	stats, err := newPurger(t, root, cat, false).Run(context.Background(), []users.TenantRetention{testTenant})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !exists(t, outside) {
		t.Fatal("the symlink was followed and its target deleted")
	}
	if !exists(t, full) {
		t.Fatal("the symlink itself was deleted, hiding the anomaly")
	}
	if cat.rows[1].file.Status != media.StatusStored {
		t.Fatalf("status = %q: a refused target must not be written off as purged", cat.rows[1].file.Status)
	}
	if stats.Refused == 0 || stats.FilesDeleted != 0 {
		t.Fatalf("stats = %+v, want a refusal and no deletion", stats)
	}
}

// A path that escapes the media root is refused before any filesystem call.
// Such a path cannot come from the generator, so this is a regression guard
// against exactly the day it can.
func TestPathsOutsideTheMediaRootAreRefused(t *testing.T) {
	root := t.TempDir()
	victimDir := t.TempDir()
	victim := filepath.Join(victimDir, "precious")
	if err := os.WriteFile(victim, []byte("do not delete"), 0o640); err != nil {
		t.Fatalf("write victim: %v", err)
	}

	escapes := []string{
		"../" + filepath.Base(victimDir) + "/precious",
		"42/../../" + filepath.Base(victimDir) + "/precious",
		"/etc/passwd",
		`42\..\..\precious`,
	}
	for _, rel := range escapes {
		t.Run(rel, func(t *testing.T) {
			cat := newCatalogue(fixedNow)
			cat.add(1, media.StatusStored, rel, fixedNow().AddDate(0, 0, -30))
			stats, err := newPurger(t, root, cat, false).Run(context.Background(), []users.TenantRetention{testTenant})
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if !exists(t, victim) {
				t.Fatal("a path traversal reached outside the media root")
			}
			if stats.Refused == 0 {
				t.Fatalf("stats = %+v, want a refusal", stats)
			}
			if cat.rows[1].file.Status != media.StatusStored {
				t.Fatalf("status = %q, want the row left untouched", cat.rows[1].file.Status)
			}
		})
	}
}

// Dry run: not a single write, on either side. It is the mode meant for the
// first run against a real storage tree, and it would be worthless if it
// removed "just" the orphans.
func TestDryRunWritesNothing(t *testing.T) {
	root := t.TempDir()
	cat := newCatalogue(fixedNow)
	expired := writeMedia(t, root, "42/2026-01/05/expired", 30*24*time.Hour)
	cat.add(1, media.StatusStored, expired, fixedNow().AddDate(0, 0, -30))
	orphan := writeMedia(t, root, "42/2026-01/09/orphan", OrphanGrace+time.Hour)
	cat.add(2, media.StatusPending, "", fixedNow().AddDate(0, 0, -30))
	cat.add(3, media.StatusPurged, "", fixedNow().AddDate(0, 0, -60)).updatedAt = fixedNow().Add(-2 * PurgedRowGrace)

	stats, err := newPurger(t, root, cat, true).Run(context.Background(), []users.TenantRetention{testTenant})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(cat.writes) != 0 {
		t.Fatalf("dry run performed catalogue writes: %v", cat.writes)
	}
	if !exists(t, filepath.Join(root, expired)) || !exists(t, filepath.Join(root, orphan)) {
		t.Fatal("dry run deleted a file")
	}
	// It still has to REPORT, otherwise there is nothing to review before
	// turning it off.
	if stats.FilesDeleted != 1 || stats.RowsPurged != 1 {
		t.Fatalf("stats = %+v, want the expired media reported", stats)
	}
	if stats.PendingDeleted != 0 || stats.RowsDeleted != 0 {
		t.Fatalf("stats = %+v, want the row-only phases skipped", stats)
	}
}

// Rows that describe no file leave on their own deadlines, and the second one
// is the subtle one: a 'purged' row is the last trace an attachment existed,
// and a deletion alert within retention still needs it.
func TestRowsWithoutAFileLeaveOnTheirOwnDeadlines(t *testing.T) {
	root := t.TempDir()
	cat := newCatalogue(fixedNow)
	cat.add(1, media.StatusPending, "", fixedNow().Add(-PendingMaxAge-time.Hour))
	cat.add(2, media.StatusPending, "", fixedNow().Add(-time.Hour))
	// Purged long ago and past retention: nothing left to say.
	cat.add(3, media.StatusPurged, "", fixedNow().AddDate(0, 0, -60)).updatedAt = fixedNow().Add(-PurgedRowGrace - time.Hour)
	// Purged yesterday because Telegram would not hand the file over, still
	// well within retention: the alert of a deletion tomorrow needs this row.
	cat.add(4, media.StatusPurged, "", fixedNow().AddDate(0, 0, -1)).updatedAt = fixedNow().Add(-24 * time.Hour)

	stats, err := newPurger(t, root, cat, false).Run(context.Background(), []users.TenantRetention{testTenant})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, ok := cat.rows[1]; ok {
		t.Fatal("a pending row past its deadline survived")
	}
	if _, ok := cat.rows[2]; !ok {
		t.Fatal("a recent pending row was deleted: its download is still due")
	}
	if _, ok := cat.rows[3]; ok {
		t.Fatal("a purged row past retention and grace survived")
	}
	if _, ok := cat.rows[4]; !ok {
		t.Fatal("a purged row still within retention was deleted: the deletion alert loses the only trace of the media")
	}
	if stats.PendingDeleted != 1 || stats.RowsDeleted != 1 {
		t.Fatalf("stats = %+v, want 1 pending and 1 purged row deleted", stats)
	}
}

// A pending row also has to obey the tenant's retention, even when
// PendingMaxAge has not elapsed: metadata may not outlive the retention the
// owner was promised.
func TestRetentionCapsThePendingDeadline(t *testing.T) {
	root := t.TempDir()
	cat := newCatalogue(fixedNow)
	cat.add(1, media.StatusPending, "", fixedNow().Add(-36*time.Hour))

	tenant := users.TenantRetention{OwnerUserID: testOwner, RetentionDays: 1}
	if _, err := newPurger(t, root, cat, false).Run(context.Background(), []users.TenantRetention{tenant}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, ok := cat.rows[1]; ok {
		t.Fatalf("a pending row outlived a %d-day retention", tenant.RetentionDays)
	}
}

// No pass may ever turn into an unbounded scan: the batches are capped, the
// number of rows examined per run is capped, and what did not fit resumes
// where it stopped instead of restarting from the beginning.
func TestSweepsAreBoundedAndResume(t *testing.T) {
	root := t.TempDir()
	cat := newCatalogue(fixedNow)
	const total = maxFilesPerRun + 200
	for id := int64(1); id <= total; id++ {
		rel := writeMedia(t, root, fmt.Sprintf("42/2026-02/28/file-%04d", id), 48*time.Hour)
		cat.add(id, media.StatusStored, rel, fixedNow().AddDate(0, 0, -2))
	}

	p := newPurger(t, root, cat, false)
	if _, err := p.Run(context.Background(), []users.TenantRetention{testTenant}); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if cat.maxLimit > batchSize {
		t.Fatalf("a query asked for %d rows, over the batch size %d", cat.maxLimit, batchSize)
	}
	if p.rowCursor[testOwner] == 0 {
		t.Fatal("the catalogue sweep did not stop at its bound: it claims to have covered everything")
	}
	if p.pathCursor[testOwner] == "" {
		t.Fatal("the disk sweep did not stop at its bound")
	}
	firstRow, firstPath := p.rowCursor[testOwner], p.pathCursor[testOwner]

	if _, err := p.Run(context.Background(), []users.TenantRetention{testTenant}); err != nil {
		t.Fatalf("second run: %v", err)
	}
	// Everything fits in two runs, so the second one completes the sweep and
	// resets both cursors -- having started AFTER the first one stopped.
	if p.rowCursor[testOwner] != 0 || p.pathCursor[testOwner] != "" {
		t.Fatalf("second run did not complete the sweep: rows=%d paths=%q (was %d / %q)",
			p.rowCursor[testOwner], p.pathCursor[testOwner], firstRow, firstPath)
	}
	// Nothing was expired or unreferenced: a bounded sweep must not be an
	// excuse to touch anything.
	if len(cat.writes) != 0 {
		t.Fatalf("unexpected writes: %v", cat.writes)
	}
}

// Idempotence is what makes the purge safe to interrupt: a second run over a
// state the first one produced changes nothing at all.
func TestSecondRunOverTheSameStateChangesNothing(t *testing.T) {
	root := t.TempDir()
	cat := newCatalogue(fixedNow)
	expired := writeMedia(t, root, "42/2026-01/05/expired", 30*24*time.Hour)
	cat.add(1, media.StatusStored, expired, fixedNow().AddDate(0, 0, -30))
	writeMedia(t, root, "42/2026-01/09/orphan", OrphanGrace+time.Hour)
	kept := writeMedia(t, root, "42/2026-02/28/fresh", 48*time.Hour)
	cat.add(2, media.StatusStored, kept, fixedNow().AddDate(0, 0, -2))
	cat.add(3, media.StatusPending, "", fixedNow().Add(-PendingMaxAge-time.Hour))

	p := newPurger(t, root, cat, false)
	if _, err := p.Run(context.Background(), []users.TenantRetention{testTenant}); err != nil {
		t.Fatalf("first run: %v", err)
	}
	afterFirst := snapshot(t, root, cat)
	writesAfterFirst := len(cat.writes)

	second, err := p.Run(context.Background(), []users.TenantRetention{testTenant})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if got := snapshot(t, root, cat); got != afterFirst {
		t.Fatalf("state moved on the second run:\n first: %s\nsecond: %s", afterFirst, got)
	}
	if len(cat.writes) != writesAfterFirst {
		t.Fatalf("the second run wrote again: %v", cat.writes[writesAfterFirst:])
	}
	if (second != Stats{}) {
		t.Fatalf("the second run reported work: %+v", second)
	}
}

// snapshot renders the disk and the catalogue as one comparable string.
func snapshot(t *testing.T, root string, cat *fakeCatalogue) string {
	t.Helper()
	var parts []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			rel, _ := filepath.Rel(root, path)
			parts = append(parts, "file:"+rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot walk: %v", err)
	}
	for _, row := range cat.sorted() {
		parts = append(parts, fmt.Sprintf("row:%d:%s:%s", row.file.ID, row.file.Status, row.file.RelativePath))
	}
	sort.Strings(parts)
	return strings.Join(parts, " ")
}

// One tenant's failure must not cost every other tenant its retention: a
// filesystem or catalogue error is collected and reported, the loop goes on.
func TestOneFailingTenantDoesNotStopTheOthers(t *testing.T) {
	root := t.TempDir()
	cat := newCatalogue(fixedNow)
	rel := writeMedia(t, root, "43/2026-01/05/expired", 30*24*time.Hour)
	cat.add(1, media.StatusStored, rel, fixedNow().AddDate(0, 0, -30))

	failing := &failingCatalogue{Catalogue: cat}
	p := newPurger(t, root, failing, false)
	stats, err := p.Run(context.Background(), []users.TenantRetention{
		{OwnerUserID: 1, RetentionDays: 7},
		{OwnerUserID: 43, RetentionDays: 7},
	})
	if err == nil {
		t.Fatal("the failing tenant was not reported")
	}
	if !errors.Is(err, errCatalogueDown) {
		t.Fatalf("err = %v, want it to wrap the underlying failure", err)
	}
	if stats.FilesDeleted != 1 || stats.RowsPurged != 1 {
		t.Fatalf("stats = %+v: the healthy tenant was not purged", stats)
	}
}

var errCatalogueDown = errors.New("catalogue unavailable")

// failingCatalogue fails every read for tenant 1 and delegates the rest.
type failingCatalogue struct {
	Catalogue
}

func (f *failingCatalogue) ListExpiredStored(ctx context.Context, ownerUserID, afterID int64, retentionDays, limit int) ([]media.File, error) {
	if ownerUserID == 1 {
		return nil, errCatalogueDown
	}
	return f.Catalogue.ListExpiredStored(ctx, ownerUserID, afterID, retentionDays, limit)
}

// A tenant that has never stored anything has no directory at all. That is a
// normal state, not an error.
func TestATenantWithNoStorageIsNotAnError(t *testing.T) {
	root := t.TempDir()
	cat := newCatalogue(fixedNow)
	stats, err := newPurger(t, root, cat, false).Run(context.Background(), []users.TenantRetention{testTenant})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if (stats != Stats{}) {
		t.Fatalf("stats = %+v, want an empty run", stats)
	}
}

// A refused row stays expired and stays 'stored', so it is handed back by the
// next query for as long as the anomaly is there. Without a cursor it would
// refill the batch on every pass, and a batch made entirely of refusals would
// stall the retention of every row behind them -- silently, since a purge that
// deletes nothing looks exactly like a purge with nothing to do.
func TestRefusedRowsDoNotStallTheRetentionBehindThem(t *testing.T) {
	root := t.TempDir()
	victim := filepath.Join(t.TempDir(), "precious")
	if err := os.WriteFile(victim, []byte("do not delete"), 0o640); err != nil {
		t.Fatalf("write victim: %v", err)
	}

	cat := newCatalogue(fixedNow)
	expiredAt := fixedNow().AddDate(0, 0, -30)
	// A full batch of unpurgeable rows, ahead of the real one by id.
	for id := int64(1); id <= batchSize; id++ {
		rel := fmt.Sprintf("42/2026-01/05/linked-%04d", id)
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.Symlink(victim, full); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		cat.add(id, media.StatusStored, rel, expiredAt)
	}
	reachable := writeMedia(t, root, "42/2026-01/05/expired", 30*24*time.Hour)
	cat.add(batchSize+1, media.StatusStored, reachable, expiredAt)

	stats, err := newPurger(t, root, cat, false).Run(context.Background(), []users.TenantRetention{testTenant})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if exists(t, filepath.Join(root, reachable)) {
		t.Fatal("an expired blob survived because refused rows filled the batch ahead of it")
	}
	if cat.rows[batchSize+1].file.Status != media.StatusPurged {
		t.Fatalf("status = %q, want the row behind the refusals purged", cat.rows[batchSize+1].file.Status)
	}
	if !exists(t, victim) {
		t.Fatal("a symlink was followed and its target deleted")
	}
	// One deletion, and every symlink refused by each phase that met it
	// (retention, the row sweep, the disk sweep) -- never followed by any of
	// them.
	if stats.FilesDeleted != 1 || stats.Refused < batchSize {
		t.Fatalf("stats = %+v, want 1 deletion and at least %d refusals", stats, batchSize)
	}
}

// A requeued row is waiting for a download that was granted a moment ago, not
// for the one it was created with. Deciding its deadline on created_at alone
// would delete it at the very next daily pass -- and with it the last trace
// that an attachment existed, while it is still well within retention.
func TestARequeuedRowKeepsItsFullRetryWindow(t *testing.T) {
	root := t.TempDir()
	cat := newCatalogue(fixedNow)
	// Captured five days ago (inside a seven-day retention, but far past
	// PendingMaxAge), and its file has vanished: exactly the crash leftover the
	// reconciliation requeues.
	rel := "42/2026-02/24/vanished"
	cat.add(1, media.StatusStored, rel, fixedNow().AddDate(0, 0, -5))
	if err := os.MkdirAll(filepath.Join(root, filepath.Dir(rel)), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	p := newPurger(t, root, cat, false)
	if _, err := p.Run(context.Background(), []users.TenantRetention{testTenant}); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if cat.rows[1].file.Status != media.StatusPending {
		t.Fatalf("status after the repair = %q, want pending", cat.rows[1].file.Status)
	}

	if _, err := p.Run(context.Background(), []users.TenantRetention{testTenant}); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if _, ok := cat.rows[1]; !ok {
		t.Fatal("the requeued row was deleted before the fetch loop had its retry window")
	}
}

// Retention still wins over the retry window: a requeue may postpone the
// staleness deadline, never the tenant's own retention.
func TestARequeuedRowStillObeysRetention(t *testing.T) {
	root := t.TempDir()
	cat := newCatalogue(fixedNow)
	// Requeued a moment ago (updated_at is recent), captured well past a
	// one-day retention.
	row := cat.add(1, media.StatusPending, "", fixedNow().AddDate(0, 0, -5))
	row.updatedAt = fixedNow()

	tenant := users.TenantRetention{OwnerUserID: testOwner, RetentionDays: 1}
	if _, err := newPurger(t, root, cat, false).Run(context.Background(), []users.TenantRetention{tenant}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, ok := cat.rows[1]; ok {
		t.Fatal("a recently requeued row outlived the tenant's retention")
	}
}

func TestNewRequiresItsDependencies(t *testing.T) {
	if _, err := New(Config{Catalogue: newCatalogue(fixedNow)}); err == nil {
		t.Fatal("a purger without a media root was accepted: every relative path would resolve against the working directory")
	}
	if _, err := New(Config{MediaDir: t.TempDir()}); err == nil {
		t.Fatal("a purger without a catalogue was accepted")
	}
}

// A media root that is not there is a volume that failed to mount, not a
// storage that happens to be empty. Reading it as the latter would requeue or
// write off the entire catalogue while the real blobs sit elsewhere, so the
// run refuses instead.
func TestAMissingMediaRootAbortsTheRun(t *testing.T) {
	cat := newCatalogue(fixedNow)
	cat.add(1, media.StatusStored, "42/2026-01/05/somewhere", fixedNow().AddDate(0, 0, -30))

	p := newPurger(t, filepath.Join(t.TempDir(), "never-mounted"), cat, false)
	if _, err := p.Run(context.Background(), []users.TenantRetention{testTenant}); err == nil {
		t.Fatal("the run went ahead on a missing media root")
	}
	if len(cat.writes) != 0 {
		t.Fatalf("the catalogue was written from an unreadable root: %v", cat.writes)
	}
}
