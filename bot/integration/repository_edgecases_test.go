//go:build integration

package integration_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/LouisMoretti/Undelete/bot/internal/media"
	"github.com/LouisMoretti/Undelete/bot/internal/messages"
	"github.com/LouisMoretti/Undelete/bot/internal/storage"
	"github.com/LouisMoretti/Undelete/bot/internal/users"
)

// TestPostgreSQL16RepositoryEdgeCases pins on a real PostgreSQL 16 the
// boundary semantics the nominal suites never exercise: an edited message
// with no parent, a deletion batch with no match, and hostile media paths
// refused by the repository before the CHECK.
func TestPostgreSQL16RepositoryEdgeCases(t *testing.T) {
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
		t.Fatalf("read current database: %v", err)
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
	ownerA, err := userRepo.UpsertByTelegramID(ctx, 93203)
	if err != nil {
		t.Fatalf("create owner A: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), `DELETE FROM users WHERE telegram_user_id IN (93203)`)
	})
	if _, err := db.Pool.Exec(ctx, `
		INSERT INTO business_connections (id, owner_user_id, can_reply, is_enabled)
		VALUES ('bc-edge-a', $1, true, true)
		ON CONFLICT (id) DO UPDATE SET owner_user_id = EXCLUDED.owner_user_id, is_enabled = true
	`, ownerA.ID); err != nil {
		t.Fatalf("seed connection: %v", err)
	}

	msgRepo := messages.NewRepository(db)
	mediaRepo := media.NewRepository(db)

	t.Run("edited orphan and deleted orphan", func(t *testing.T) {
		// Edited-orphan documents current repository semantics: an edit with
		// no parent upserts a row (edited_at set) rather than a no-op. Pinned
		// so a future change to no-op semantics updates this test knowingly.
		orphan := messages.Record{
			BusinessConnectionID: "bc-edge-a", ChatID: 88107, MessageID: 7,
			FromDisplay: "ghost", MessageType: "text", TextContent: "only the edit",
			TelegramDate: 1788019201, ChatTitle: "t", ChatType: "private",
		}
		if err := msgRepo.Save(context.Background(), ownerA.ID, orphan, true); err != nil {
			t.Fatalf("save edited orphan: %v", err)
		}
		var editedAt *time.Time
		if err := db.InTenant(context.Background(), ownerA.ID, func(tx pgx.Tx) error {
			return tx.QueryRow(context.Background(), `SELECT edited_at FROM messages WHERE business_connection_id = 'bc-edge-a' AND chat_id = 88107 AND message_id = 7`).Scan(&editedAt)
		}); err != nil || editedAt == nil {
			t.Fatalf("edited orphan should carry edited_at, got %v / %v", editedAt, err)
		}
		// Deleted-orphan is a clean no-op: nothing found, no outbox row.
		found, err := msgRepo.MarkDeleted(context.Background(), ownerA.ID, 93203, "bc-edge-a", 88107, []int64{999999})
		if err != nil {
			t.Fatalf("MarkDeleted orphan: %v", err)
		}
		if len(found) != 0 {
			t.Fatalf("orphan delete returned %d rows", len(found))
		}
		var outboxRows int
		if err := db.InTenant(context.Background(), ownerA.ID, func(tx pgx.Tx) error {
			return tx.QueryRow(context.Background(), `SELECT count(*) FROM notification_outbox WHERE chat_id = 88107 AND message_id = 999999`).Scan(&outboxRows)
		}); err != nil || outboxRows != 0 {
			t.Fatalf("orphan delete queued %d outbox rows", outboxRows)
		}
	})

	t.Run("media MarkStored refuses hostile paths", func(t *testing.T) {
		id, err := mediaRepo.Save(context.Background(), ownerA.ID, media.Record{
			BusinessConnectionID: "bc-edge-a", ChatID: 88108, MessageID: 8,
			TelegramFileID: "fh", TelegramFileUniqueID: "edge-unique-8", MediaType: media.TypePhoto,
		})
		if err != nil {
			t.Fatalf("media save: %v", err)
		}
		sha := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
		for _, rel := range []string{"../../escape", "/abs/path", "a//b", "a/./b", ""} {
			if err := mediaRepo.MarkStored(context.Background(), ownerA.ID, id, media.StoredFile{RelativePath: rel, SHA256: sha, ByteSize: 3}); err == nil {
				t.Fatalf("MarkStored(%q) unexpectedly accepted", rel)
			}
		}
		if err := mediaRepo.MarkStored(context.Background(), ownerA.ID, id, media.StoredFile{RelativePath: "ok/path", SHA256: sha, ByteSize: 3, ThumbnailRelativePath: "../thumb"}); err == nil {
			t.Fatal("MarkStored with hostile thumbnail unexpectedly accepted")
		}
	})
}
