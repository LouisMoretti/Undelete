//go:build integration

package integration_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/LouisMoretti/Undelete/bot/internal/media"
	"github.com/LouisMoretti/Undelete/bot/internal/media/purge"
	"github.com/LouisMoretti/Undelete/bot/internal/storage"
	"github.com/LouisMoretti/Undelete/bot/internal/users"
)

// TestPostgreSQL16MediaRetentionPurge proves against a real PostgreSQL 16 the
// property the unit tests cannot: that the purge converges after a crash
// BETWEEN the two systems it has to keep in sync.
//
// The unit tests cover the state machine against an in-memory catalogue. What
// they cannot cover is the SQL itself -- the LIMIT-bounded deletes, the
// LEAST(interval) deadline, the keyset pagination -- nor the fact that every
// one of those statements runs under FORCE ROW LEVEL SECURITY, where a missing
// tenant context does not raise an error but silently matches zero rows.
//
// It lives under the `integration` tag so `make test-integration` picks it up
// and the lint+unit job skips it for lack of DSNs.
func TestPostgreSQL16MediaRetentionPurge(t *testing.T) {
	adminDSN := requireEnv(t, "POSTGRES_INTEGRATION_ADMIN_DSN")
	runtimeDSN := requireEnv(t, "POSTGRES_INTEGRATION_RUNTIME_DSN")
	if err := validateExplicitDestructiveOptIn(os.Getenv("POSTGRES_INTEGRATION_ALLOW_DESTRUCTIVE")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), setupTimeout)
	defer cancel()

	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	defer admin.Close(ctx)
	var databaseName string
	if err := admin.QueryRow(ctx, `SELECT current_database()`).Scan(&databaseName); err != nil {
		t.Fatalf("read current database for destructive interlock: %v", err)
	}
	if err := validateDestructiveInterlock(os.Getenv("POSTGRES_INTEGRATION_ALLOW_DESTRUCTIVE"), databaseName); err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := storage.RunMigrations(ctx, adminDSN, logger); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	db, err := storage.NewPool(ctx, runtimeDSN)
	if err != nil {
		t.Fatalf("open runtime pool: %v", err)
	}
	defer db.Close()

	userRepo := users.NewRepository(db.Pool)
	owner, err := userRepo.UpsertByTelegramID(ctx, 92101)
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	neighbour, err := userRepo.UpsertByTelegramID(ctx, 92102)
	if err != nil {
		t.Fatalf("create neighbour: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), `DELETE FROM users WHERE telegram_user_id IN (92101, 92102)`)
	})

	root := t.TempDir()
	repo := media.NewRepository(db)
	tenant := users.TenantRetention{OwnerUserID: owner.ID, RetentionDays: 7}

	newPurger := func(t *testing.T, dryRun bool) *purge.Purger {
		t.Helper()
		p, err := purge.New(purge.Config{MediaDir: root, Catalogue: repo, DryRun: dryRun, Logger: logger})
		if err != nil {
			t.Fatalf("new purger: %v", err)
		}
		return p
	}

	// store writes a file under the media root and catalogues it as stored,
	// exactly as the fetch loop would.
	var nextMessageID int64
	store := func(t *testing.T, ownerID int64, name string) (int64, string) {
		t.Helper()
		nextMessageID++
		rel := filepath.Join(fmt.Sprintf("%d", ownerID), "2026-01", "05", name)
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		content := []byte("payload-" + name)
		if err := os.WriteFile(full, content, 0o640); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
		sum := sha256.Sum256(content)

		id, err := repo.Save(ctx, ownerID, media.Record{
			BusinessConnectionID: "purge-conn",
			ChatID:               880,
			MessageID:            nextMessageID,
			TelegramFileID:       "file-handle",
			TelegramFileUniqueID: "unique-" + name,
			MediaType:            media.TypePhoto,
		})
		if err != nil {
			t.Fatalf("save %s: %v", name, err)
		}
		if err := repo.MarkStored(ctx, ownerID, id, media.StoredFile{
			RelativePath: rel,
			SHA256:       hex.EncodeToString(sum[:]),
			ByteSize:     int64(len(content)),
		}); err != nil {
			t.Fatalf("mark stored %s: %v", name, err)
		}
		return id, rel
	}

	// backdate ages a row. Through the admin connection: a superuser bypasses
	// RLS, so the fixture does not depend on the very tenant context under
	// test.
	backdate := func(t *testing.T, id int64, createdAgo, updatedAgo time.Duration) {
		t.Helper()
		if _, err := admin.Exec(ctx, `
			UPDATE media_files SET created_at = now() - $2::interval, updated_at = now() - $3::interval
			WHERE id = $1
		`, id, fmt.Sprintf("%d seconds", int64(createdAgo.Seconds())),
			fmt.Sprintf("%d seconds", int64(updatedAgo.Seconds()))); err != nil {
			t.Fatalf("backdate %d: %v", id, err)
		}
	}

	statusOf := func(t *testing.T, id int64) (string, string) {
		t.Helper()
		var status, path string
		if err := admin.QueryRow(ctx, `
			SELECT status, COALESCE(relative_path, '') FROM media_files WHERE id = $1
		`, id).Scan(&status, &path); err != nil {
			t.Fatalf("read status of %d: %v", id, err)
		}
		return status, path
	}

	rowExists := func(t *testing.T, id int64) bool {
		t.Helper()
		var found bool
		if err := admin.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM media_files WHERE id = $1)`, id).Scan(&found); err != nil {
			t.Fatalf("existence of %d: %v", id, err)
		}
		return found
	}

	onDisk := func(t *testing.T, rel string) bool {
		t.Helper()
		_, err := os.Lstat(filepath.Join(root, rel))
		return err == nil
	}

	t.Run("retention deletes the file then the row", func(t *testing.T) {
		id, rel := store(t, owner.ID, "expired")
		backdate(t, id, 30*24*time.Hour, 30*24*time.Hour)

		stats, err := newPurger(t, false).Run(phaseContext(t), []users.TenantRetention{tenant})
		if err != nil {
			t.Fatalf("purge: %v", err)
		}
		if onDisk(t, rel) {
			t.Fatal("the expired blob is still on disk")
		}
		status, path := statusOf(t, id)
		if status != media.StatusPurged || path != "" {
			t.Fatalf("row after purge: status=%q path=%q, want purged with no path", status, path)
		}
		if stats.FilesDeleted != 1 || stats.RowsPurged != 1 {
			t.Fatalf("stats = %+v", stats)
		}
	})

	// The crash this whole package exists for: the process died between the
	// unlink and the UPDATE, so the row still claims a file that is gone. The
	// next run converges because the unlink is idempotent -- an already absent
	// file is a success, and the status change happens anyway.
	t.Run("a crash between the unlink and the status change is repaired", func(t *testing.T) {
		id, rel := store(t, owner.ID, "crashed")
		backdate(t, id, 30*24*time.Hour, 30*24*time.Hour)
		// Simulate the crash: the file goes, the row does not move.
		if err := os.Remove(filepath.Join(root, rel)); err != nil {
			t.Fatalf("simulate crash: %v", err)
		}
		if status, _ := statusOf(t, id); status != media.StatusStored {
			t.Fatalf("precondition: status=%q, want stored", status)
		}

		if _, err := newPurger(t, false).Run(phaseContext(t), []users.TenantRetention{tenant}); err != nil {
			t.Fatalf("purge: %v", err)
		}
		status, path := statusOf(t, id)
		if status != media.StatusPurged || path != "" {
			t.Fatalf("row after repair: status=%q path=%q, want purged with no path", status, path)
		}
	})

	// Same missing file, but the owner is still entitled to that media: the
	// row goes back to the download queue rather than being written off.
	t.Run("a missing file within retention is queued for another download", func(t *testing.T) {
		id, rel := store(t, owner.ID, "vanished")
		if err := os.Remove(filepath.Join(root, rel)); err != nil {
			t.Fatalf("remove file: %v", err)
		}

		if _, err := newPurger(t, false).Run(phaseContext(t), []users.TenantRetention{tenant}); err != nil {
			t.Fatalf("purge: %v", err)
		}
		status, path := statusOf(t, id)
		if status != media.StatusPending || path != "" {
			t.Fatalf("row after repair: status=%q path=%q, want pending with no path", status, path)
		}

		pending, err := repo.ListPending(phaseContext(t), owner.ID, 10)
		if err != nil {
			t.Fatalf("list pending: %v", err)
		}
		var queued bool
		for _, f := range pending {
			queued = queued || f.ID == id
		}
		if !queued {
			t.Fatal("the requeued row is not visible to the fetch loop")
		}
		// And the retry window it was just granted is real: the row was
		// captured long before PendingMaxAge, so a stale-pending deadline read
		// from created_at alone would delete it on the very next pass, before
		// the fetch loop ever got its chance.
		backdate(t, id, purge.PendingMaxAge+time.Hour, 0)
		if _, err := newPurger(t, false).Run(phaseContext(t), []users.TenantRetention{tenant}); err != nil {
			t.Fatalf("second run: %v", err)
		}
		if !rowExists(t, id) {
			t.Fatal("the requeued row was deleted before the fetch loop had its retry window")
		}
		// Leave a clean slate for the phases below.
		if _, err := admin.Exec(ctx, `DELETE FROM media_files WHERE id = $1`, id); err != nil {
			t.Fatalf("cleanup: %v", err)
		}
	})

	t.Run("an unreferenced blob past the grace period is deleted", func(t *testing.T) {
		rel := filepath.Join(fmt.Sprintf("%d", owner.ID), "2026-01", "05", "orphan")
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(full, []byte("nobody knows me"), 0o640); err != nil {
			t.Fatalf("write orphan: %v", err)
		}
		old := time.Now().Add(-purge.OrphanGrace - time.Hour)
		if err := os.Chtimes(full, old, old); err != nil {
			t.Fatalf("age orphan: %v", err)
		}
		// A referenced file of the same tenant, in the same directory: the
		// sweep must tell them apart through the catalogue, not by name.
		_, keptRel := store(t, owner.ID, "referenced")

		stats, err := newPurger(t, false).Run(phaseContext(t), []users.TenantRetention{tenant})
		if err != nil {
			t.Fatalf("purge: %v", err)
		}
		if onDisk(t, rel) {
			t.Fatal("the orphan survived")
		}
		if !onDisk(t, keptRel) {
			t.Fatal("a referenced file was deleted as an orphan")
		}
		if stats.Orphans != 1 {
			t.Fatalf("orphans = %d, want 1", stats.Orphans)
		}
	})

	t.Run("rows without a file leave on their own deadlines", func(t *testing.T) {
		nextMessageID++
		stale, err := repo.Save(phaseContext(t), owner.ID, media.Record{
			BusinessConnectionID: "purge-conn",
			ChatID:               880,
			MessageID:            nextMessageID,
			TelegramFileID:       "never-downloaded",
			TelegramFileUniqueID: "unique-stale",
			MediaType:            media.TypeDocument,
		})
		if err != nil {
			t.Fatalf("save stale pending: %v", err)
		}
		backdate(t, stale, purge.PendingMaxAge+time.Hour, purge.PendingMaxAge+time.Hour)

		id, _ := store(t, owner.ID, "long-purged")
		if err := repo.MarkPurged(phaseContext(t), owner.ID, id); err != nil {
			t.Fatalf("mark purged: %v", err)
		}
		backdate(t, id, 60*24*time.Hour, purge.PurgedRowGrace+time.Hour)

		// Purged yesterday, still well within retention: the row is the only
		// remaining trace that an attachment existed, and a deletion alert
		// still needs it.
		recent, _ := store(t, owner.ID, "recently-purged")
		if err := repo.MarkPurged(phaseContext(t), owner.ID, recent); err != nil {
			t.Fatalf("mark purged: %v", err)
		}
		backdate(t, recent, 24*time.Hour, 24*time.Hour)

		if _, err := newPurger(t, false).Run(phaseContext(t), []users.TenantRetention{tenant}); err != nil {
			t.Fatalf("purge: %v", err)
		}
		if rowExists(t, stale) {
			t.Fatal("a pending row past its deadline survived")
		}
		if rowExists(t, id) {
			t.Fatal("a purged row past retention and grace survived")
		}
		if !rowExists(t, recent) {
			t.Fatal("a purged row still within retention was deleted")
		}
	})

	// Idempotence: the purge is replayed after every crash and every restart,
	// so a second run over the state the first produced must be a no-op.
	t.Run("a second run over the same state changes nothing", func(t *testing.T) {
		id, rel := store(t, owner.ID, "twice")
		backdate(t, id, 30*24*time.Hour, 30*24*time.Hour)

		p := newPurger(t, false)
		if _, err := p.Run(phaseContext(t), []users.TenantRetention{tenant}); err != nil {
			t.Fatalf("first run: %v", err)
		}
		firstStatus, firstPath := statusOf(t, id)

		second, err := p.Run(phaseContext(t), []users.TenantRetention{tenant})
		if err != nil {
			t.Fatalf("second run: %v", err)
		}
		if (second != purge.Stats{}) {
			t.Fatalf("the second run did work: %+v", second)
		}
		status, path := statusOf(t, id)
		if status != firstStatus || path != firstPath {
			t.Fatalf("state moved: (%q,%q) then (%q,%q)", firstStatus, firstPath, status, path)
		}
		if onDisk(t, rel) {
			t.Fatal("the blob came back")
		}
	})

	// The purge is a per-tenant loop for the same reason every other write is
	// (constraint #4). A run for one owner must not see, let alone delete,
	// another owner's row or blob.
	t.Run("purging one tenant leaves the other untouched", func(t *testing.T) {
		mine, myRel := store(t, owner.ID, "mine")
		backdate(t, mine, 30*24*time.Hour, 30*24*time.Hour)
		theirs, theirRel := store(t, neighbour.ID, "theirs")
		backdate(t, theirs, 30*24*time.Hour, 30*24*time.Hour)

		if _, err := newPurger(t, false).Run(phaseContext(t), []users.TenantRetention{tenant}); err != nil {
			t.Fatalf("purge: %v", err)
		}
		if onDisk(t, myRel) {
			t.Fatal("the purged tenant's blob survived")
		}
		if !onDisk(t, theirRel) {
			t.Fatal("the purge of one tenant deleted another tenant's blob")
		}
		if status, _ := statusOf(t, theirs); status != media.StatusStored {
			t.Fatalf("neighbour row status = %q, want stored", status)
		}
	})

	t.Run("a dry run reports without writing", func(t *testing.T) {
		id, rel := store(t, owner.ID, "dry")
		backdate(t, id, 30*24*time.Hour, 30*24*time.Hour)

		stats, err := newPurger(t, true).Run(phaseContext(t), []users.TenantRetention{tenant})
		if err != nil {
			t.Fatalf("dry run: %v", err)
		}
		if !onDisk(t, rel) {
			t.Fatal("the dry run deleted a blob")
		}
		if status, _ := statusOf(t, id); status != media.StatusStored {
			t.Fatalf("the dry run wrote to the catalogue: status = %q", status)
		}
		if stats.FilesDeleted != 1 {
			t.Fatalf("stats = %+v, want the expired blob reported", stats)
		}
	})
}
