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
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/LouisMoretti/Undelete/bot/internal/media"
	"github.com/LouisMoretti/Undelete/bot/internal/messages"
	"github.com/LouisMoretti/Undelete/bot/internal/outbox"
	"github.com/LouisMoretti/Undelete/bot/internal/storage"
	"github.com/LouisMoretti/Undelete/bot/internal/users"
)

// TestPostgreSQL16MediaCatalogueLifecycle proves on a real PostgreSQL 16 the
// catalogue methods that no other integration test calls nominally: the retry
// requeue (MarkPendingRetry), the retention cursors (ListExpiredStored,
// ListStoredPage), the reconciliation lookup (KnownPaths) and the catalogue
// sweepers (DeleteStalePending, DeletePurged) -- plus ListTenantsForRetention,
// the tenant loop every background pass (retention, outbox, media fetch)
// iterates.
//
// Each subtest owns its rows (distinct message ids); the tenant cleanup
// cascades to every table.
func TestPostgreSQL16MediaCatalogueLifecycle(t *testing.T) {
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
	owner, err := userRepo.UpsertByTelegramID(ctx, 93011)
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	neighbour, err := userRepo.UpsertByTelegramID(ctx, 93012)
	if err != nil {
		t.Fatalf("create neighbour: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), `DELETE FROM users WHERE telegram_user_id IN (93011, 93012)`)
	})

	t.Run("ListTenantsForRetention lists every tenant", func(t *testing.T) {
		tenants, err := userRepo.ListTenantsForRetention(phaseContext(t))
		if err != nil {
			t.Fatalf("list tenants: %v", err)
		}
		seen := map[int64]int{}
		for _, tenant := range tenants {
			seen[tenant.OwnerUserID] = tenant.RetentionDays
		}
		if _, ok := seen[owner.ID]; !ok {
			t.Fatalf("owner %d missing from %v", owner.ID, seen)
		}
		if _, ok := seen[neighbour.ID]; !ok {
			t.Fatalf("neighbour %d missing from %v", neighbour.ID, seen)
		}
	})

	repo := media.NewRepository(db)
	shaOf := func(name string) string {
		sum := sha256.Sum256([]byte("payload-" + name))
		return hex.EncodeToString(sum[:])
	}
	pending := func(messageID int64, unique string) media.Record {
		return media.Record{
			BusinessConnectionID: "catalogue-conn", ChatID: 7711, MessageID: messageID,
			TelegramFileID: "file-handle", TelegramFileUniqueID: unique, MediaType: media.TypePhoto,
		}
	}
	stored := func(t *testing.T, ownerID, messageID int64, name string) int64 {
		t.Helper()
		id, err := repo.Save(ctx, ownerID, pending(messageID, "unique-"+name))
		if err != nil {
			t.Fatalf("save %s: %v", name, err)
		}
		rel := fmt.Sprintf("%d/2026/01/01/%s/photo", ownerID, name)
		if err := repo.MarkStored(ctx, ownerID, id, media.StoredFile{
			RelativePath: rel, SHA256: shaOf(name), ByteSize: 8,
		}); err != nil {
			t.Fatalf("mark stored %s: %v", name, err)
		}
		return id
	}
	backdate := func(t *testing.T, id int64, ago time.Duration) {
		t.Helper()
		if _, err := admin.Exec(ctx, `UPDATE media_files SET created_at = now() - $2::interval, updated_at = now() - $2::interval WHERE id = $1`,
			id, fmt.Sprintf("%d seconds", int64(ago.Seconds()))); err != nil {
			t.Fatalf("backdate %d: %v", id, err)
		}
	}
	statusOf := func(t *testing.T, id int64) string {
		t.Helper()
		var status string
		if err := admin.QueryRow(ctx, `SELECT status FROM media_files WHERE id = $1`, id).Scan(&status); err != nil {
			t.Fatalf("status of %d: %v", id, err)
		}
		return status
	}

	t.Run("MarkPendingRetry requeues a stored row", func(t *testing.T) {
		ctx := phaseContext(t)
		id := stored(t, owner.ID, 11, "requeue")
		if err := repo.MarkPendingRetry(ctx, owner.ID, id); err != nil {
			t.Fatalf("mark pending retry: %v", err)
		}
		if got := statusOf(t, id); got != media.StatusPending {
			t.Fatalf("status = %q, want pending", got)
		}
		files, err := repo.ListPending(ctx, owner.ID, 10)
		if err != nil {
			t.Fatalf("list pending: %v", err)
		}
		found := false
		for _, f := range files {
			if f.ID == id {
				found = true
				if f.RelativePath != "" || f.SHA256 != "" {
					t.Fatalf("requeued row must point at nothing: %+v", f)
				}
			}
		}
		if !found {
			t.Fatal("requeued row missing from ListPending")
		}
	})

	t.Run("retention cursors page expired stored rows", func(t *testing.T) {
		ctx := phaseContext(t)
		idFresh := stored(t, owner.ID, 21, "fresh")
		idOld1 := stored(t, owner.ID, 22, "old1")
		idOld2 := stored(t, owner.ID, 23, "old2")
		backdate(t, idOld1, 30*24*time.Hour)
		backdate(t, idOld2, 30*24*time.Hour)

		page, err := repo.ListExpiredStored(ctx, owner.ID, 0, 7, 10)
		if err != nil {
			t.Fatalf("list expired: %v", err)
		}
		ids := map[int64]bool{}
		for _, f := range page {
			ids[f.ID] = true
		}
		if !ids[idOld1] || !ids[idOld2] || ids[idFresh] {
			t.Fatalf("expired set wrong: fresh=%v old1=%v old2=%v", ids[idFresh], ids[idOld1], ids[idOld2])
		}
		// Cursor: past the first expired row, only the second remains.
		var first int64
		for _, f := range page {
			if first == 0 || f.ID < first {
				first = f.ID
			}
		}
		rest, err := repo.ListExpiredStored(ctx, owner.ID, first, 7, 10)
		if err != nil {
			t.Fatalf("list expired after cursor: %v", err)
		}
		if len(rest) != 1 {
			t.Fatalf("cursor page = %d rows, want 1", len(rest))
		}
		// ListStoredPage sees every stored row of THIS tenant, keyset ordered.
		all, err := repo.ListStoredPage(ctx, owner.ID, 0, 100)
		if err != nil {
			t.Fatalf("list stored page: %v", err)
		}
		for i := 1; i < len(all); i++ {
			if all[i].ID <= all[i-1].ID {
				t.Fatal("ListStoredPage is not ordered by id")
			}
		}
		other, err := repo.ListStoredPage(ctx, neighbour.ID, 0, 100)
		if err != nil {
			t.Fatalf("list stored page neighbour: %v", err)
		}
		if len(other) != 0 {
			t.Fatalf("neighbour sees %d rows of this tenant", len(other))
		}
	})

	t.Run("KnownPaths answers per tenant", func(t *testing.T) {
		ctx := phaseContext(t)
		rel := fmt.Sprintf("%d/2026/01/01/known/photo", owner.ID)
		id, err := repo.Save(ctx, owner.ID, pending(31, "unique-known"))
		if err != nil {
			t.Fatalf("save: %v", err)
		}
		if err := repo.MarkStored(ctx, owner.ID, id, media.StoredFile{RelativePath: rel, SHA256: shaOf("known"), ByteSize: 8}); err != nil {
			t.Fatalf("mark stored: %v", err)
		}
		known, err := repo.KnownPaths(ctx, owner.ID, []string{rel, "nobody/2026/ghost.jpg"})
		if err != nil {
			t.Fatalf("known paths: %v", err)
		}
		if _, ok := known[rel]; !ok {
			t.Fatalf("own path %q not known: %v", rel, known)
		}
		if _, ok := known["nobody/2026/ghost.jpg"]; ok {
			t.Fatal("ghost path reported known")
		}
		foreign, err := repo.KnownPaths(ctx, neighbour.ID, []string{rel})
		if err != nil {
			t.Fatalf("known paths neighbour: %v", err)
		}
		if len(foreign) != 0 {
			t.Fatal("neighbour must not know this tenant's paths")
		}
		empty, err := repo.KnownPaths(ctx, owner.ID, nil)
		if err != nil || len(empty) != 0 {
			t.Fatalf("empty query must return empty, got %v, %v", empty, err)
		}
	})

	t.Run("catalogue sweepers delete stale pending and graced purged", func(t *testing.T) {
		ctx := phaseContext(t)
		staleID, err := repo.Save(ctx, owner.ID, pending(41, "unique-stale"))
		if err != nil {
			t.Fatalf("save stale: %v", err)
		}
		backdate(t, staleID, 30*24*time.Hour)
		purgedID := stored(t, owner.ID, 42, "graced")
		if err := repo.MarkPurged(ctx, owner.ID, purgedID); err != nil {
			t.Fatalf("mark purged: %v", err)
		}
		backdate(t, purgedID, 30*24*time.Hour)

		deleted, err := repo.DeleteStalePending(ctx, owner.ID, time.Hour, 7, 100)
		if err != nil {
			t.Fatalf("delete stale pending: %v", err)
		}
		if deleted != 1 {
			t.Fatalf("deleted stale = %d, want 1", deleted)
		}
		var exists bool
		if err := admin.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM media_files WHERE id = $1)`, staleID).Scan(&exists); err != nil {
			t.Fatalf("stale existence: %v", err)
		}
		if exists {
			t.Fatal("stale pending row still present")
		}
		deleted, err = repo.DeletePurged(ctx, owner.ID, time.Hour, 7, 100)
		if err != nil {
			t.Fatalf("delete purged: %v", err)
		}
		if deleted != 1 {
			t.Fatalf("deleted purged = %d, want 1", deleted)
		}
	})

	t.Run("per-tenant retention purges each tenant on its own clock", func(t *testing.T) {
		ctx := phaseContext(t)
		msgRepo := messages.NewRepository(db)
		save := func(ownerID int64, id int64) {
			if err := msgRepo.Save(ctx, ownerID, messages.Record{
				BusinessConnectionID: "retention-conn", ChatID: 7712, MessageID: id,
				MessageType: "text", TextContent: "retention", TelegramDate: 1788019201,
			}, false); err != nil {
				t.Fatalf("save %d/%d: %v", ownerID, id, err)
			}
		}
		save(owner.ID, 51)
		save(neighbour.ID, 51)
		if _, err := admin.Exec(ctx, `UPDATE messages SET saved_at = now() - interval '10 days' WHERE message_id = 51`); err != nil {
			t.Fatalf("age fixtures: %v", err)
		}
		// Owner keeps 30 days: nothing goes. Neighbour keeps 7: one row goes.
		purged, err := msgRepo.PurgeExpired(ctx, []users.TenantRetention{
			{OwnerUserID: owner.ID, RetentionDays: 30},
			{OwnerUserID: neighbour.ID, RetentionDays: 7},
		})
		if err != nil {
			t.Fatalf("purge: %v", err)
		}
		if purged != 1 {
			t.Fatalf("purged = %d, want 1 (neighbour only)", purged)
		}
	})

	t.Run("concurrent claims fence each other", func(t *testing.T) {
		ctx := phaseContext(t)
		outboxRepo := outbox.NewRepository(db)
		if err := db.InTenant(ctx, owner.ID, func(tx pgx.Tx) error {
			return outbox.InsertTx(ctx, tx, owner.ID, owner.TelegramUserID,
				"fence-conn", 7713, 60, outbox.EventDeletedMessage, 0, "race")
		}); err != nil {
			t.Fatalf("insert job: %v", err)
		}
		won := make(chan int64, 2)
		done := make(chan struct{})
		for i := 0; i < 2; i++ {
			go func() {
				defer func() { done <- struct{}{} }()
				job, err := outboxRepo.Claim(context.Background(), owner.ID, time.Minute)
				if err != nil || job == nil {
					return
				}
				won <- job.ID
			}()
		}
		<-done
		<-done
		close(won)
		var winners []int64
		for id := range won {
			winners = append(winners, id)
		}
		if len(winners) != 1 {
			t.Fatalf("concurrent claims won %d times, want exactly 1", len(winners))
		}
	})
}
