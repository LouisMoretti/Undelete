// Package purge extends retention to the attachments: it removes the expired
// blobs from disk, then the rows that described them, and repairs the
// discrepancies a crash between those two steps leaves behind.
//
// # Why a reconciliation is unavoidable
//
// PostgreSQL and the filesystem cannot be committed together. Deleting a media
// is therefore always two operations, and a process killed between them leaves
// a state that no single transaction can express: a row that says 'stored'
// pointing at a file that is gone, or a file on disk that no row references.
// Neither is corruption, both are normal, and both are permanent unless
// something comes back for them. That something is this package.
//
// The ordering is chosen so that only ONE of the two mismatches is possible:
// the file goes first, the row second. A crash in between leaves a row that
// over-promises (it claims a file we no longer have), never a file nobody
// remembers -- and an over-promising row is the mismatch the catalogue can see
// on its own, from the row itself, without scanning anything.
//
// # The state machine
//
//	pending  --- older than PendingMaxAge or than retention ---> row DELETED
//	              (nothing was ever written for it, so no file to remove)
//
//	stored   --- retention elapsed ---> unlink file, THEN status purged
//	         --- file missing, within retention ---> back to pending (retry)
//	         --- file missing, retention elapsed ---> status purged
//
//	purged   --- retention elapsed AND PurgedRowGrace elapsed ---> row DELETED
//
// Every transition is idempotent: unlinking an already absent file is a
// no-op, marking an already purged row purged again changes nothing, and a
// second run over the same state does nothing at all. Interrupting a run at
// any point and rerunning it converges to the same result -- which is the
// whole property being bought here.
//
// # Bounded, always
//
// Every phase is bounded: a LIMIT per batch, a maximum number of batches, a
// maximum number of files examined per run, and a hard cap on directory
// entries visited. A sweep that does not finish resumes where it stopped (a
// keyset cursor for the catalogue, a lexical path cursor for the disk) instead
// of restarting from the beginning, so the coverage is complete over several
// runs without any single run being unbounded. The purge shares the daily
// retention loop with everything else; it may never turn into an open-ended
// scan of a disk that has grown for a year.
//
// # Refusals
//
// Nothing is ever deleted through a path that has not been revalidated against
// the media root, and nothing that is not a plain regular file is ever
// removed: os.Lstat, never os.Stat, so a symlink planted in the media tree is
// refused and logged rather than followed to whatever it points at.
package purge

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/LouisMoretti/Undelete/bot/internal/media"
	"github.com/LouisMoretti/Undelete/bot/internal/media/store"
	"github.com/LouisMoretti/Undelete/bot/internal/users"
)

// Retention constants. They are deliberately constants rather than settings:
// each one encodes a decision that has a reason, and a wrong value silently
// destroys data instead of failing.
const (
	// PendingMaxAge is how long a row may wait for a download that never
	// happened. The fetch loop retries every few seconds and gives up
	// definitively on its own for anything hopeless, so a row still pending
	// two days later is waiting on a Telegram handle that will not come back.
	// The tenant's retention applies too, whichever expires first.
	PendingMaxAge = 48 * time.Hour

	// PurgedRowGrace is the extra delay, counted from the moment the row
	// became 'purged', before the row itself is deleted. It only ever adds to
	// retention, never replaces it: a row purged because Telegram would not
	// hand the file over is the last trace that an attachment existed, and a
	// deletion alert within retention still needs it.
	PurgedRowGrace = 7 * 24 * time.Hour

	// OrphanGrace is how old a file must be before "no row references it" is
	// treated as "nothing ever will". A download publishes the file with
	// os.Rename and only then commits MarkStored: between the two, the file is
	// legitimately unreferenced. The grace is what keeps the reconciliation
	// from deleting a media the fetch loop is in the middle of storing.
	OrphanGrace = 24 * time.Hour

	// StaleTempAge applies to the .dl-* temporary files of media/store. They
	// are never referenced by a row, by construction, so they are only ever
	// deleted on age: a crash leftover, not a transfer in progress.
	StaleTempAge = 24 * time.Hour
)

// Bounds. Sized so a full pass stays cheap on the daily retention loop, and so
// that a tenant with a very large catalogue is swept over several days rather
// than in one run that never returns.
const (
	// batchSize is the LIMIT of a single catalogue query.
	batchSize = 200
	// maxBatchesPerTenant caps how many batches one phase may chain in a
	// single run.
	maxBatchesPerTenant = 25
	// maxFilesPerRun caps the reconciliation on both sides: rows checked
	// against the disk, and files checked against the catalogue.
	maxFilesPerRun = 1000
	// maxWalkEntries is the emergency stop of the directory walk, counted in
	// entries VISITED (including the ones skipped before the cursor). It is
	// not the useful bound -- maxFilesPerRun is -- it just guarantees the walk
	// terminates on a pathological tree.
	maxWalkEntries = 200_000
	// pathBatch is how many disk paths are resolved against the catalogue in
	// one query.
	pathBatch = 200
)

// ErrUnsafeTarget is returned (wrapped) when the purge refuses to unlink
// something: a path that does not resolve inside the media root, or an entry
// that is not a plain regular file. Exported sentinel, matched with errors.Is
// like media.ErrUnsafeRelativePath.
var ErrUnsafeTarget = errors.New("unsafe media purge target")

// Catalogue is the media_files side of the purge. It is an interface, not the
// concrete *media.Repository, so the state machine above is testable without a
// database -- every branch of it is a decision about deleting data.
//
// Every method is tenant-scoped: the implementation goes through InTenant, and
// this package therefore loops tenant by tenant (constraint #4), exactly like
// messages.PurgeExpired.
type Catalogue interface {
	ListExpiredStored(ctx context.Context, ownerUserID, afterID int64, retentionDays, limit int) ([]media.File, error)
	ListStoredPage(ctx context.Context, ownerUserID, afterID int64, limit int) ([]media.File, error)
	KnownPaths(ctx context.Context, ownerUserID int64, relPaths []string) (map[string]struct{}, error)
	MarkPurged(ctx context.Context, ownerUserID, id int64) error
	MarkPendingRetry(ctx context.Context, ownerUserID, id int64) error
	DeleteStalePending(ctx context.Context, ownerUserID int64, maxAge time.Duration, retentionDays, limit int) (int64, error)
	DeletePurged(ctx context.Context, ownerUserID int64, grace time.Duration, retentionDays, limit int) (int64, error)
}

// Config configures a Purger.
type Config struct {
	// MediaDir is the storage root, the same one media_files paths are
	// relative to. Required.
	MediaDir string
	// Catalogue is the media_files access. Required.
	Catalogue Catalogue
	// DryRun logs what would happen and changes nothing. See the Purger
	// comment for what it covers and what it deliberately does not.
	DryRun bool
	// Logger receives the traces. Never a message content; relative paths only
	// at Debug level.
	Logger *slog.Logger
	// Now is injectable so tests control ages deterministically.
	Now func() time.Time
}

// Stats counts what one run did, for the log line at the end of the retention
// cycle. In dry-run mode the disk counters report what WOULD have happened.
type Stats struct {
	// FilesDeleted counts blobs unlinked because their retention elapsed.
	FilesDeleted int64
	// RowsPurged counts rows moved to 'purged'.
	RowsPurged int64
	// PendingDeleted counts pending rows dropped for a download that never
	// completed.
	PendingDeleted int64
	// RowsDeleted counts 'purged' rows removed from the catalogue.
	RowsDeleted int64
	// Requeued counts stored rows sent back to pending because their file was
	// missing while still within retention.
	Requeued int64
	// Orphans counts files on disk that no row referenced any more.
	Orphans int64
	// TempRemoved counts leftover .dl-* temporary files from an interrupted
	// download.
	TempRemoved int64
	// Refused counts entries the purge declined to touch (outside the root,
	// symlink, not a regular file). A non-zero value deserves a look: it is
	// either a bug in path generation or something that has no business being
	// in the media tree.
	Refused int64
}

func (s *Stats) add(other Stats) {
	s.FilesDeleted += other.FilesDeleted
	s.RowsPurged += other.RowsPurged
	s.PendingDeleted += other.PendingDeleted
	s.RowsDeleted += other.RowsDeleted
	s.Requeued += other.Requeued
	s.Orphans += other.Orphans
	s.TempRemoved += other.TempRemoved
	s.Refused += other.Refused
}

// LogAttrs renders the counters for a single structured log line.
func (s Stats) LogAttrs() []any {
	return []any{
		slog.Int64("media_files_deleted", s.FilesDeleted),
		slog.Int64("media_rows_purged", s.RowsPurged),
		slog.Int64("media_pending_deleted", s.PendingDeleted),
		slog.Int64("media_rows_deleted", s.RowsDeleted),
		slog.Int64("media_requeued", s.Requeued),
		slog.Int64("media_orphans_deleted", s.Orphans),
		slog.Int64("media_temp_deleted", s.TempRemoved),
		slog.Int64("media_refused", s.Refused),
	}
}

// Purger applies the retention and the reconciliation to one tenant at a time.
//
// # Dry run
//
// DryRun performs no write at all: no unlink, no UPDATE, no DELETE. The disk
// phases still report what they would have removed, because that is where the
// irreversible risk lives -- an unlinked blob is gone, whereas a deleted row is
// still in last night's pg_dump. The row-only phases (stale pending, purged
// rows) are skipped rather than simulated: counting them would mean a second
// query whose only purpose is a log line.
//
// A dry run also stops after the first batch of each phase. Nothing is written,
// so the next batch would return the exact same rows.
type Purger struct {
	cfg  Config
	root string
	// rowCursor and pathCursor resume the two reconciliation sweeps where the
	// previous run stopped. In memory on purpose: they are an optimisation of
	// the coverage, not a correctness requirement, and a restart simply
	// restarts the sweep from the beginning. Persisting them would mean a
	// table whose only content is a scan position.
	rowCursor  map[int64]int64
	pathCursor map[int64]string
}

// New validates the configuration and resolves the media root once.
func New(cfg Config) (*Purger, error) {
	if strings.TrimSpace(cfg.MediaDir) == "" {
		return nil, errors.New("media purge: MediaDir is required")
	}
	if cfg.Catalogue == nil {
		return nil, errors.New("media purge: Catalogue is required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(discardHandler{})
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	root, err := filepath.Abs(cfg.MediaDir)
	if err != nil {
		return nil, fmt.Errorf("media purge: resolving root: %w", err)
	}
	return &Purger{
		cfg:        cfg,
		root:       root,
		rowCursor:  make(map[int64]int64),
		pathCursor: make(map[int64]string),
	}, nil
}

// Run applies retention and reconciliation to every tenant and returns what it
// did.
//
// Tenant by tenant, never globally: media_files is under FORCE ROW LEVEL
// SECURITY, and a query without a tenant context does not fail, it matches zero
// rows -- the purge would look healthy forever while purging nothing
// (constraint #4, same trap as messages.PurgeExpired).
//
// A tenant that fails does not stop the others: a filesystem error on one
// storage subtree must not leave every other tenant's retention unapplied. The
// errors are joined and returned once the loop is done.
func (p *Purger) Run(ctx context.Context, tenants []users.TenantRetention) (Stats, error) {
	var total Stats
	var failures []error

	// The media root has to be there before anything is decided from its
	// contents. A missing root is a misconfigured or unmounted volume, not an
	// empty storage: reconcileRows would read "every file of every tenant is
	// gone" and dutifully requeue or write off the entire catalogue, while the
	// blobs sit untouched under the root that was actually meant. Refusing the
	// run costs one day of retention; the alternative costs the catalogue.
	info, err := os.Stat(p.root)
	switch {
	case err != nil:
		return total, fmt.Errorf("media purge: unreachable media root: %w", err)
	case !info.IsDir():
		return total, fmt.Errorf("media purge: media root %s is not a directory", p.root)
	}

	for _, tenant := range tenants {
		if ctx.Err() != nil {
			break
		}
		stats, err := p.runTenant(ctx, tenant)
		total.add(stats)
		if err != nil && ctx.Err() == nil {
			p.cfg.Logger.Error("media purge: tenant failed",
				slog.Int64("owner_user_id", tenant.OwnerUserID),
				slog.String("error", err.Error()))
			failures = append(failures, fmt.Errorf("tenant %d: %w", tenant.OwnerUserID, err))
		}
	}
	return total, errors.Join(failures...)
}

// runTenant runs the four phases in the only order that is safe to interrupt:
// retention first (it is what the owner asked for), then the row-only
// cleanups, then the reconciliation that picks up whatever the previous runs
// left half-done.
func (p *Purger) runTenant(ctx context.Context, tenant users.TenantRetention) (Stats, error) {
	var stats Stats
	var failures []error

	collect := func(s Stats, err error) {
		stats.add(s)
		if err != nil {
			failures = append(failures, err)
		}
	}

	collect(p.purgeExpired(ctx, tenant))
	collect(p.purgeRows(ctx, tenant))
	collect(p.reconcileRows(ctx, tenant))
	collect(p.reconcileDisk(ctx, tenant))

	return stats, errors.Join(failures...)
}

// purgeExpired unlinks the blobs whose retention elapsed, then marks their row
// purged. That order is the contract of this package: the crash window it
// leaves open is the one reconcileRows can close.
//
// A refused entry (symlink, not a regular file, path outside the root) leaves
// its row untouched and 'stored'. Marking it purged would erase the only
// pointer to something that is still on disk, and the point of refusing is to
// keep a human able to look at it. Which is exactly why the batches advance on
// a keyset cursor: a refused row stays expired and would otherwise be handed
// back at the head of every following batch, and enough of them would fill the
// batch entirely and stall the retention of everything behind them. The cursor
// skips them for the rest of the run; the next run retries them from scratch.
func (p *Purger) purgeExpired(ctx context.Context, tenant users.TenantRetention) (Stats, error) {
	var stats Stats
	var cursor int64
	capped := true

	for batch := 0; batch < maxBatchesPerTenant; batch++ {
		if ctx.Err() != nil {
			return stats, nil
		}
		files, err := p.cfg.Catalogue.ListExpiredStored(ctx, tenant.OwnerUserID, cursor, tenant.RetentionDays, batchSize)
		if err != nil {
			return stats, err
		}
		for _, file := range files {
			if ctx.Err() != nil {
				return stats, nil
			}
			cursor = file.ID
			unlinked, err := p.removeFiles(file, "retention")
			stats.add(unlinked)
			if err != nil {
				// Refusals are already counted and logged; they must not abort
				// the retention of the rest of the batch.
				continue
			}
			if p.cfg.DryRun {
				stats.RowsPurged++
				continue
			}
			if err := p.cfg.Catalogue.MarkPurged(ctx, tenant.OwnerUserID, file.ID); err != nil {
				// The row went between the read and the write (another run,
				// the tenant being deleted): the file is gone, which was the
				// point. Nothing to repair.
				if errors.Is(err, media.ErrNotFound) {
					continue
				}
				return stats, err
			}
			stats.RowsPurged++
		}
		if p.cfg.DryRun || len(files) < batchSize {
			capped = false
			break
		}
	}
	if capped {
		// The bound did its job, and that is worth seeing: a tenant capturing
		// more than maxBatchesPerTenant*batchSize expiring media a day falls
		// further behind on every pass, and without this line the summary of a
		// run that could not keep up looks exactly like a healthy one.
		p.cfg.Logger.Warn("media purge: retention stopped at its per-run bound, resuming tomorrow",
			slog.Int64("owner_user_id", tenant.OwnerUserID),
			slog.Int("files_deleted", int(stats.FilesDeleted)),
			slog.Int64("refused", stats.Refused))
	}
	return stats, nil
}

// purgeRows deletes the rows that describe no file: the pending ones that
// waited too long, and the purged ones whose grace elapsed. Pure catalogue
// work, no filesystem involved -- by construction neither status has a blob to
// remove.
func (p *Purger) purgeRows(ctx context.Context, tenant users.TenantRetention) (Stats, error) {
	var stats Stats
	if p.cfg.DryRun {
		p.cfg.Logger.Info("media purge: dry run, catalogue row deletions skipped",
			slog.Int64("owner_user_id", tenant.OwnerUserID))
		return stats, nil
	}

	for batch := 0; batch < maxBatchesPerTenant; batch++ {
		if ctx.Err() != nil {
			return stats, nil
		}
		deleted, err := p.cfg.Catalogue.DeleteStalePending(ctx, tenant.OwnerUserID,
			PendingMaxAge, tenant.RetentionDays, batchSize)
		stats.PendingDeleted += deleted
		if err != nil {
			return stats, err
		}
		if deleted < batchSize {
			break
		}
	}

	for batch := 0; batch < maxBatchesPerTenant; batch++ {
		if ctx.Err() != nil {
			return stats, nil
		}
		deleted, err := p.cfg.Catalogue.DeletePurged(ctx, tenant.OwnerUserID,
			PurgedRowGrace, tenant.RetentionDays, batchSize)
		stats.RowsDeleted += deleted
		if err != nil {
			return stats, err
		}
		if deleted < batchSize {
			break
		}
	}
	return stats, nil
}

// reconcileRows walks the catalogue and checks that every 'stored' row still
// has its file. It is the half of the reconciliation that repairs a crash
// between the unlink and the status change.
//
// A missing file is decided on age, and the two answers are not
// interchangeable:
//
//   - still within retention: the owner is entitled to that media, so the row
//     goes back to pending and the fetch loop tries again. Worst case Telegram
//     no longer has it, and the fetch loop marks it purged itself.
//   - retention elapsed: the file was on its way out anyway. The row is marked
//     purged, which is exactly what the interrupted run was about to do.
func (p *Purger) reconcileRows(ctx context.Context, tenant users.TenantRetention) (Stats, error) {
	var stats Stats
	owner := tenant.OwnerUserID
	cursor := p.rowCursor[owner]
	expiry := p.cfg.Now().AddDate(0, 0, -tenant.RetentionDays)

	for scanned := 0; scanned < maxFilesPerRun; {
		if ctx.Err() != nil {
			return stats, nil
		}
		limit := min(batchSize, maxFilesPerRun-scanned)
		files, err := p.cfg.Catalogue.ListStoredPage(ctx, owner, cursor, limit)
		if err != nil {
			return stats, err
		}
		for _, file := range files {
			cursor = file.ID
			scanned++

			full, err := p.resolve(file.RelativePath)
			if err != nil {
				stats.Refused++
				p.refused(file.RelativePath, "row path refused", err)
				continue
			}
			info, err := os.Lstat(full)
			switch {
			case err == nil && info.Mode().IsRegular():
				continue
			case err == nil:
				// Not a regular file where a stored media should be. Never
				// followed, never removed, never rewritten.
				stats.Refused++
				p.refused(file.RelativePath, "stored path is not a regular file",
					fmt.Errorf("%w: %s", ErrUnsafeTarget, info.Mode().Type()))
				continue
			case !errors.Is(err, fs.ErrNotExist):
				return stats, fmt.Errorf("inspecting stored media %d: %w", file.ID, err)
			}

			expired := file.CreatedAt.Before(expiry)
			if p.cfg.DryRun {
				p.cfg.Logger.Info("media purge: dry run, would repair a missing file",
					slog.Int64("media_file_id", file.ID),
					slog.Bool("expired", expired))
				continue
			}
			if expired {
				if err := p.cfg.Catalogue.MarkPurged(ctx, owner, file.ID); err != nil {
					if errors.Is(err, media.ErrNotFound) {
						continue
					}
					return stats, err
				}
				stats.RowsPurged++
				continue
			}
			if err := p.cfg.Catalogue.MarkPendingRetry(ctx, owner, file.ID); err != nil {
				if errors.Is(err, media.ErrNotFound) {
					continue
				}
				return stats, err
			}
			stats.Requeued++
			p.cfg.Logger.Warn("media purge: stored file missing, queued for another download",
				slog.Int64("media_file_id", file.ID))
		}
		if len(files) < limit {
			// Sweep complete: the next run starts over from the beginning.
			cursor = 0
			break
		}
	}

	p.rowCursor[owner] = cursor
	return stats, nil
}

// reconcileDisk walks the tenant's storage subtree and removes what the
// catalogue no longer knows about. It is the half of the reconciliation that
// answers "the row is gone, who deletes the file?".
//
// Two kinds of leftovers, two different rules:
//
//   - a .dl-* temporary file is never referenced by anything, by construction
//     (media/store publishes with a rename). It is deleted purely on age, so an
//     in-flight download is never pulled out from under itself.
//   - any other unreferenced file is an orphan, but only once it is older than
//     OrphanGrace: a download that has just renamed its file and not yet
//     committed MarkStored is legitimately unreferenced for a few milliseconds.
//
// The walk never follows a symlink (filepath.WalkDir reports it as an entry,
// it does not descend into it) and never deletes one.
func (p *Purger) reconcileDisk(ctx context.Context, tenant users.TenantRetention) (Stats, error) {
	var stats Stats
	owner := tenant.OwnerUserID
	// Rooted at the tenant's own subtree: the storage layout is
	// <root>/<ownerUserID>/<yyyy-mm>/<dd>/<key>, so the scope of the walk is
	// the tenant scope, and one tenant's run can never look at another's files.
	tenantRoot := filepath.Join(p.root, strconv.FormatInt(owner, 10))

	cursor := p.pathCursor[owner]
	var (
		batch     []candidate
		lastRel   string
		visited   int
		examined  int
		completed = true
		flushErr  error
	)

	flush := func() bool {
		if len(batch) == 0 {
			return true
		}
		s, err := p.sweepBatch(ctx, owner, batch)
		stats.add(s)
		batch = batch[:0]
		if err != nil {
			flushErr = err
			return false
		}
		return true
	}

	walkErr := filepath.WalkDir(tenantRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				// A tenant that has never stored anything has no subtree, and
				// a directory removed under the walk is exactly what this
				// package does elsewhere. Neither is an error.
				return nil
			}
			return err
		}
		if ctx.Err() != nil {
			completed = false
			return fs.SkipAll
		}
		visited++
		if visited > maxWalkEntries {
			completed = false
			return fs.SkipAll
		}
		if entry.IsDir() {
			return nil
		}

		rel, relErr := filepath.Rel(p.root, path)
		if relErr != nil {
			stats.Refused++
			return nil
		}
		// WalkDir walks in lexical order, so a path at or before the cursor was
		// already examined by a previous run. Tested BEFORE the refusals, so an
		// anomaly is reported once, when the sweep first reaches it, instead of
		// being re-counted by every resumed pass over the same prefix.
		//
		// This compares whole relative paths where WalkDir orders entry NAMES
		// per directory. The two agree because the layout is uniform --
		// <owner>/<yyyy-mm>/<dd>/<key>, every file at the same depth, every
		// directory component fixed-width -- and a future layout with files and
		// directories side by side in one parent would have to revisit it.
		if rel <= cursor {
			return nil
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			stats.Refused++
			p.refused(rel, "symlink in the media tree",
				fmt.Errorf("%w: symlink", ErrUnsafeTarget))
			return nil
		}
		if !entry.Type().IsRegular() {
			stats.Refused++
			p.refused(rel, "irregular entry in the media tree",
				fmt.Errorf("%w: %s", ErrUnsafeTarget, entry.Type()))
			return nil
		}
		if examined >= maxFilesPerRun {
			completed = false
			return fs.SkipAll
		}

		info, infoErr := entry.Info()
		if infoErr != nil {
			if errors.Is(infoErr, fs.ErrNotExist) {
				return nil
			}
			return infoErr
		}
		examined++
		lastRel = rel
		batch = append(batch, candidate{rel: rel, modTime: info.ModTime()})
		if len(batch) >= pathBatch && !flush() {
			return fs.SkipAll
		}
		return nil
	})
	if walkErr == nil && flushErr == nil {
		flush()
	}

	switch {
	case flushErr != nil:
		return stats, flushErr
	case walkErr != nil:
		return stats, fmt.Errorf("walking media of tenant %d: %w", owner, walkErr)
	}

	// An interrupted walk resumes after the last path it examined; a complete
	// one starts over next time. lastRel stays empty when the run only skipped
	// already-seen paths, in which case the cursor must not be reset.
	switch {
	case completed:
		p.pathCursor[owner] = ""
	case lastRel != "":
		p.pathCursor[owner] = lastRel
	}
	return stats, nil
}

// candidate is one file found on disk, awaiting the catalogue's verdict.
type candidate struct {
	rel     string
	modTime time.Time
}

// sweepBatch resolves one batch of disk paths against the catalogue and
// removes what nothing references any more.
func (p *Purger) sweepBatch(ctx context.Context, owner int64, batch []candidate) (Stats, error) {
	var stats Stats

	paths := make([]string, 0, len(batch))
	for _, c := range batch {
		paths = append(paths, c.rel)
	}
	known, err := p.cfg.Catalogue.KnownPaths(ctx, owner, paths)
	if err != nil {
		return stats, err
	}

	now := p.cfg.Now()
	for _, c := range batch {
		if _, referenced := known[c.rel]; referenced {
			continue
		}
		age := now.Sub(c.modTime)
		temp := strings.HasPrefix(filepath.Base(c.rel), store.TempPrefix)
		switch {
		case temp && age < StaleTempAge:
			// A download in progress. Removing it would make its final rename
			// fail and lose a media nobody asked us to delete.
			continue
		case !temp && age < OrphanGrace:
			// Possibly a file whose MarkStored has not committed yet.
			continue
		}
		removed, err := p.remove(c.rel, "unreferenced")
		if err != nil {
			stats.Refused++
			p.refused(c.rel, "unreferenced entry refused", err)
			continue
		}
		if !removed {
			continue
		}
		if temp {
			stats.TempRemoved++
		} else {
			stats.Orphans++
		}
	}
	return stats, nil
}

// removeFiles unlinks the blob of a row and its thumbnail, and reports what it
// did. A refusal on either one is returned so the caller leaves the row alone.
func (p *Purger) removeFiles(file media.File, reason string) (Stats, error) {
	var stats Stats
	for _, rel := range []string{file.RelativePath, file.ThumbnailRelativePath} {
		if rel == "" {
			continue
		}
		removed, err := p.remove(rel, reason)
		if err != nil {
			stats.Refused++
			p.cfg.Logger.Warn("media purge: refusing to delete",
				slog.Int64("media_file_id", file.ID),
				slog.String("error", err.Error()))
			return stats, err
		}
		if removed {
			stats.FilesDeleted++
		}
	}
	return stats, nil
}

// remove unlinks one file inside the media root, and reports whether there was
// anything to unlink.
//
// Refuses, in this order: a relative path that is not a safe one (the same
// validation the path passed before being written to the database, applied
// again at the only point where it becomes a real unlink), a resolved path
// outside the root, and anything that is not a plain regular file. os.Lstat
// rather than os.Stat: a symlink must be seen as a symlink, not as the file it
// points at -- following one would let anything planted in the media tree
// delete an arbitrary file elsewhere on the host.
//
// An already absent file is a success, not an error: that is what makes the
// whole purge replayable after a crash.
func (p *Purger) remove(rel, reason string) (bool, error) {
	full, err := p.resolve(rel)
	if err != nil {
		return false, err
	}
	info, err := os.Lstat(full)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("inspecting media file: %w", err)
	case !info.Mode().IsRegular():
		return false, fmt.Errorf("%w: %s is not a regular file (%s)",
			ErrUnsafeTarget, rel, info.Mode().Type())
	}

	if p.cfg.DryRun {
		p.cfg.Logger.Info("media purge: dry run, would delete a file",
			slog.String("reason", reason),
			slog.Int64("bytes", info.Size()))
		p.cfg.Logger.Debug("media purge: dry run target", slog.String("relative_path", rel))
		return true, nil
	}
	if err := os.Remove(full); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// Lost a race with a concurrent purge or an operator: the outcome
			// is the one we wanted.
			return false, nil
		}
		return false, fmt.Errorf("deleting media file: %w", err)
	}
	p.cfg.Logger.Debug("media purge: file deleted",
		slog.String("reason", reason), slog.String("relative_path", rel))
	return true, nil
}

// resolve turns a stored relative path into an absolute one, refusing anything
// that would land outside the media root.
//
// media.ValidateRelativePath is the same check the path went through before
// reaching the database (and that migration 0004 mirrors in a CHECK); the
// containment test after the join is the defence in depth that would catch a
// future regression in either.
func (p *Purger) resolve(rel string) (string, error) {
	if err := media.ValidateRelativePath(rel); err != nil {
		return "", err
	}
	full := filepath.Join(p.root, rel)
	if !strings.HasPrefix(full, p.root+string(os.PathSeparator)) {
		return "", fmt.Errorf("%w: %q resolves outside the media root", ErrUnsafeTarget, rel)
	}
	return full, nil
}

// refused logs a refusal. The class of the problem at Warn (an operator has to
// see it), the path only at Debug, like every other log in the media packages.
func (p *Purger) refused(rel, message string, err error) {
	p.cfg.Logger.Warn("media purge: "+message, slog.String("error", err.Error()))
	p.cfg.Logger.Debug("media purge: refused target", slog.String("relative_path", rel))
}

// discardHandler makes Config.Logger optional without a nil check on every
// call site (same pattern as media/store).
type discardHandler struct{}

func (discardHandler) Enabled(context.Context, slog.Level) bool  { return false }
func (discardHandler) Handle(context.Context, slog.Record) error { return nil }
func (h discardHandler) WithAttrs([]slog.Attr) slog.Handler      { return h }
func (h discardHandler) WithGroup(string) slog.Handler           { return h }
