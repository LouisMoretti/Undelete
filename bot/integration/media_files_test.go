//go:build integration

package integration_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/LouisMoretti/Undelete/bot/internal/media"
	"github.com/LouisMoretti/Undelete/bot/internal/storage"
	"github.com/LouisMoretti/Undelete/bot/internal/users"
)

// TestPostgreSQL16MediaFilesSchema proves on a real PostgreSQL 16 what unit
// tests structurally cannot: that media_files is fail-closed outside a tenant
// context, isolated between tenants, and that its anti-collision and
// anti-traversal constraints are enforced by the server rather than only by the
// Go code that is supposed to respect them.
//
// It lives in this package (and under the `integration` tag) so
// `make test-integration` picks it up with no extra wiring, and skips itself in
// the lint+unit job for lack of DSNs.
func TestPostgreSQL16MediaFilesSchema(t *testing.T) {
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

	// Migrations are run here too, not only by postgres_test.go: within a
	// package Go runs test functions in source order across files sorted by
	// name, and this file sorts first. RunMigrations is idempotent (it consults
	// schema_migrations), so the two calls cost one no-op.
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
	ownerA, err := userRepo.UpsertByTelegramID(ctx, 92001)
	if err != nil {
		t.Fatalf("create owner A: %v", err)
	}
	ownerB, err := userRepo.UpsertByTelegramID(ctx, 92002)
	if err != nil {
		t.Fatalf("create owner B: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), `DELETE FROM users WHERE telegram_user_id IN (92001, 92002)`)
	})

	repo := media.NewRepository(db)
	record := func(connection string, messageID int64, index int) media.Record {
		size := int64(1024)
		width, height := 800, 600
		return media.Record{
			BusinessConnectionID: connection,
			ChatID:               770,
			MessageID:            messageID,
			FileIndex:            index,
			TelegramFileID:       "file-handle",
			TelegramFileUniqueID: "file-unique",
			MediaType:            media.TypePhoto,
			MimeType:             "image/jpeg",
			ByteSize:             &size,
			Width:                &width,
			Height:               &height,
		}
	}

	idA, err := repo.Save(ctx, ownerA.ID, record("owner-a", 1, 0))
	if err != nil {
		t.Fatalf("save owner A media: %v", err)
	}
	if _, err := repo.Save(ctx, ownerB.ID, record("owner-b", 1, 0)); err != nil {
		t.Fatalf("save owner B media: %v", err)
	}

	t.Run("RLS fails closed without tenant context", func(t *testing.T) {
		ctx := phaseContext(t)
		var count int
		if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM media_files`).Scan(&count); err != nil {
			t.Fatalf("raw select: %v", err)
		}
		if count != 0 {
			t.Fatalf("raw select exposed %d media rows without tenant context", count)
		}
		// FORCE RLS also applies to writes: an insert without context violates
		// the WITH CHECK, it does not silently land.
		if _, err := db.Pool.Exec(ctx, `
			INSERT INTO media_files (owner_user_id, business_connection_id, chat_id, message_id, telegram_file_id, telegram_file_unique_id, media_type)
			VALUES ($1, 'raw', 1, 1, 'f', 'u', 'photo')
		`, ownerA.ID); err == nil {
			t.Fatal("raw insert unexpectedly bypassed RLS WITH CHECK")
		}
		// A raw UPDATE is the fail-closed trap documented on PurgeExpired: no
		// error, zero rows.
		tag, err := db.Pool.Exec(ctx, `UPDATE media_files SET status = 'purged'`)
		if err != nil {
			t.Fatalf("raw update should be a zero-row no-op: %v", err)
		}
		if tag.RowsAffected() != 0 {
			t.Fatalf("raw update touched %d rows without tenant context", tag.RowsAffected())
		}
	})

	t.Run("tenants cannot see or update each other's media", func(t *testing.T) {
		ctx := phaseContext(t)
		filesA, err := repo.GetByMessage(ctx, ownerA.ID, "owner-a", 770, 1)
		if err != nil || len(filesA) != 1 {
			t.Fatalf("owner A reads its own media: files=%d err=%v", len(filesA), err)
		}
		if filesA[0].Status != media.StatusPending || filesA[0].RelativePath != "" {
			t.Fatalf("a freshly saved media should be pending with no path: %+v", filesA[0])
		}
		if *filesA[0].Width != 800 || filesA[0].MimeType != "image/jpeg" {
			t.Fatalf("optional metadata lost on the round trip: %+v", filesA[0])
		}

		leaked, err := repo.GetByMessage(ctx, ownerB.ID, "owner-a", 770, 1)
		if err != nil {
			t.Fatalf("cross-tenant read: %v", err)
		}
		if len(leaked) != 0 {
			t.Fatalf("owner B read %d of owner A's media rows", len(leaked))
		}

		// Owner B knows the id (it is a plain bigint, guessable): RLS must make
		// the update find nothing rather than write into A's row.
		err = repo.MarkPurged(ctx, ownerB.ID, idA)
		if !errors.Is(err, media.ErrNotFound) {
			t.Fatalf("cross-tenant MarkPurged = %v, want media.ErrNotFound", err)
		}

		pendingB, err := repo.ListPending(ctx, ownerB.ID, 10)
		if err != nil || len(pendingB) != 1 {
			t.Fatalf("owner B pending list: files=%d err=%v", len(pendingB), err)
		}
		if pendingB[0].BusinessConnectionID != "owner-b" {
			t.Fatalf("owner B's pending list contains %q", pendingB[0].BusinessConnectionID)
		}
	})

	t.Run("upsert is idempotent on redelivery and preserves the stored file", func(t *testing.T) {
		ctx := phaseContext(t)
		if err := repo.MarkStored(ctx, ownerA.ID, idA, media.StoredFile{
			RelativePath: "92001/owner-a/770/1/0.jpg",
			SHA256:       "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
			ByteSize:     2048,
		}); err != nil {
			t.Fatalf("mark stored: %v", err)
		}

		// Telegram redelivers the update with a refreshed file_id: the row must
		// be updated, not duplicated, and the on-disk facts must survive.
		redelivered := record("owner-a", 1, 0)
		redelivered.TelegramFileID = "file-handle-rotated"
		again, err := repo.Save(ctx, ownerA.ID, redelivered)
		if err != nil {
			t.Fatalf("redelivered save: %v", err)
		}
		if again != idA {
			t.Fatalf("redelivery created a new row id=%d, want the existing %d", again, idA)
		}

		files, err := repo.GetByMessage(ctx, ownerA.ID, "owner-a", 770, 1)
		if err != nil || len(files) != 1 {
			t.Fatalf("after redelivery: files=%d err=%v", len(files), err)
		}
		if files[0].TelegramFileID != "file-handle-rotated" {
			t.Fatalf("the rotated file_id was not refreshed: %+v", files[0])
		}
		if files[0].Status != media.StatusStored || files[0].RelativePath != "92001/owner-a/770/1/0.jpg" || *files[0].ByteSize != 2048 {
			t.Fatalf("the redelivery reset the on-disk state: %+v", files[0])
		}

		// A stored file is no longer a download candidate.
		pending, err := repo.ListPending(ctx, ownerA.ID, 10)
		if err != nil || len(pending) != 0 {
			t.Fatalf("stored media still listed as pending: files=%d err=%v", len(pending), err)
		}

		if err := repo.MarkPurged(ctx, ownerA.ID, idA); err != nil {
			t.Fatalf("mark purged: %v", err)
		}
		files, err = repo.GetByMessage(ctx, ownerA.ID, "owner-a", 770, 1)
		if err != nil || len(files) != 1 {
			t.Fatalf("after purge: files=%d err=%v", len(files), err)
		}
		if files[0].Status != media.StatusPurged || files[0].RelativePath != "" {
			t.Fatalf("a purged row must be kept without a path: %+v", files[0])
		}
	})

	t.Run("a poorer redelivery does not erase known metadata", func(t *testing.T) {
		ctx := phaseContext(t)
		if _, err := repo.Save(ctx, ownerA.ID, record("owner-a", 7, 0)); err != nil {
			t.Fatalf("initial save: %v", err)
		}

		// An edited_business_message that re-describes the same attachment
		// without repeating its MIME type or its dimensions: NULL means "this
		// update did not say", never "the value was cleared".
		poorer := record("owner-a", 7, 0)
		poorer.MimeType = ""
		poorer.ByteSize = nil
		poorer.Width = nil
		poorer.Height = nil
		if _, err := repo.Save(ctx, ownerA.ID, poorer); err != nil {
			t.Fatalf("poorer redelivery: %v", err)
		}

		files, err := repo.GetByMessage(ctx, ownerA.ID, "owner-a", 770, 7)
		if err != nil || len(files) != 1 {
			t.Fatalf("after poorer redelivery: files=%d err=%v", len(files), err)
		}
		f := files[0]
		if f.MimeType != "image/jpeg" || f.Width == nil || *f.Width != 800 || f.ByteSize == nil || *f.ByteSize != 1024 {
			t.Fatalf("the poorer redelivery erased known metadata: %+v", f)
		}
	})

	t.Run("MarkStored refuses a malformed hash before touching the row", func(t *testing.T) {
		ctx := phaseContext(t)
		id, err := repo.Save(ctx, ownerA.ID, record("owner-a", 8, 0))
		if err != nil {
			t.Fatalf("save: %v", err)
		}
		// Uppercase hex: the CHECK of migration 0004 would refuse it too, but
		// only after the blob was written under ./media. The Go guard must fire
		// first, and name the field.
		err = repo.MarkStored(ctx, ownerA.ID, id, media.StoredFile{
			RelativePath: "92001/owner-a/770/8/0.jpg",
			SHA256:       "9F86D081884C7D659A2FEAA0C55AD015A3BF4F1B2B0B822CD15D6C15B0F00A08",
			ByteSize:     2048,
		})
		if !errors.Is(err, media.ErrInvalidSHA256) {
			t.Fatalf("MarkStored with an uppercase hash = %v, want media.ErrInvalidSHA256", err)
		}
		files, err := repo.GetByMessage(ctx, ownerA.ID, "owner-a", 770, 8)
		if err != nil || len(files) != 1 {
			t.Fatalf("after the refused MarkStored: files=%d err=%v", len(files), err)
		}
		if files[0].Status != media.StatusPending || files[0].RelativePath != "" {
			t.Fatalf("the refused MarkStored still touched the row: %+v", files[0])
		}
	})

	t.Run("several files per message and albums coexist", func(t *testing.T) {
		ctx := phaseContext(t)
		// Two attachments on the same message: distinct file_index, so the
		// anti-collision constraint accepts both.
		for index := 0; index < 2; index++ {
			r := record("owner-a", 2, index)
			r.MediaGroupID = "album-1"
			if _, err := repo.Save(ctx, ownerA.ID, r); err != nil {
				t.Fatalf("save file_index %d: %v", index, err)
			}
		}
		// Second message of the same album.
		r := record("owner-a", 3, 0)
		r.MediaGroupID = "album-1"
		r.MediaType = media.TypeVideo
		if _, err := repo.Save(ctx, ownerA.ID, r); err != nil {
			t.Fatalf("save album sibling: %v", err)
		}

		files, err := repo.GetByMessage(ctx, ownerA.ID, "owner-a", 770, 2)
		if err != nil || len(files) != 2 {
			t.Fatalf("multi-file message: files=%d err=%v", len(files), err)
		}
		if files[0].FileIndex != 0 || files[1].FileIndex != 1 {
			t.Fatalf("attachments returned out of order: %+v", files)
		}

		var albumSize int
		if err := db.InTenant(ctx, ownerA.ID, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM media_files WHERE media_group_id = 'album-1'`).Scan(&albumSize)
		}); err != nil {
			t.Fatalf("count album: %v", err)
		}
		if albumSize != 3 {
			t.Fatalf("album holds %d files across its messages, want 3", albumSize)
		}
	})

	t.Run("the server enforces the schema guarantees", func(t *testing.T) {
		ctx := phaseContext(t)
		// Anti-collision: the same (message, file_index) twice in ONE statement
		// cannot be resolved by the upsert, so the constraint itself speaks.
		if err := db.InTenant(ctx, ownerA.ID, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				INSERT INTO media_files (owner_user_id, business_connection_id, chat_id, message_id, file_index, telegram_file_id, telegram_file_unique_id, media_type)
				VALUES ($1, 'owner-a', 770, 4, 0, 'f', 'u', 'photo'), ($1, 'owner-a', 770, 4, 0, 'f', 'u', 'photo')
			`, ownerA.ID)
			return err
		}); err == nil {
			t.Fatal("the anti-collision UNIQUE constraint let a duplicate through")
		}

		// Anti-traversal: even if the Go validation were bypassed, the server
		// refuses. Each of these is rejected by a CHECK, not by ValidateRelativePath.
		for _, path := range []string{"/etc/passwd", "../../etc/passwd", "a/../../etc/passwd", "a/./b", `a\b`, "a//b", "a/", ""} {
			err := db.InTenant(ctx, ownerA.ID, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `
					INSERT INTO media_files (owner_user_id, business_connection_id, chat_id, message_id, file_index, telegram_file_id, telegram_file_unique_id, media_type, relative_path)
					VALUES ($1, 'owner-a', 770, 5, 0, 'f', 'u', 'photo', $2)
				`, ownerA.ID, path)
				return err
			})
			if err == nil {
				t.Fatalf("the server accepted the unsafe relative path %q", path)
			}
			if err := media.ValidateRelativePath(path); err == nil {
				t.Fatalf("ValidateRelativePath accepts %q while the server rejects it: the two layers disagree", path)
			}
		}

		// Unknown media type, negative size and malformed hash are all refused.
		for _, insert := range []struct {
			name    string
			columns string
			values  string
		}{
			{name: "media type", columns: "media_type", values: "'screenshot'"},
			{name: "negative size", columns: "media_type, byte_size", values: "'photo', -1"},
			{name: "uppercase hash", columns: "media_type, sha256", values: "'photo', '9F86D081884C7D659A2FEAA0C55AD015A3BF4F1B2B0B822CD15D6C15B0F00A08'"},
			{name: "truncated hash", columns: "media_type, sha256", values: "'photo', 'deadbeef'"},
			{name: "stored without path", columns: "media_type, status", values: "'photo', 'stored'"},
		} {
			err := db.InTenant(ctx, ownerA.ID, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `
					INSERT INTO media_files (owner_user_id, business_connection_id, chat_id, message_id, file_index, telegram_file_id, telegram_file_unique_id, `+insert.columns+`)
					VALUES ($1, 'owner-a', 770, 6, 0, 'f', 'u', `+insert.values+`)
				`, ownerA.ID)
				return err
			})
			if err == nil {
				t.Fatalf("the server accepted an invalid %s", insert.name)
			}
		}
	})
}
