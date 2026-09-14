//go:build integration

package integration_test

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/LouisMoretti/Undelete/bot/internal/media"
	"github.com/LouisMoretti/Undelete/bot/internal/messages"
	"github.com/LouisMoretti/Undelete/bot/internal/storage"
	"github.com/LouisMoretti/Undelete/bot/internal/users"
)

// TestPostgreSQL16QuotaUsageQueries proves on a real PostgreSQL 16 what unit
// tests cannot: that the quota tracker's source of truth (messages
// CountByOwner, media CountByOwner and SumStoredBytes) counts tenant-scoped
// rows through RLS -- tenant A sees exactly its own messages and bytes,
// tenant B's rows never leak in, and only 'stored' media bytes count toward
// the byte quota (pending declarations and purged files are not disk facts).
//
// Same harness as the sibling files: destructive interlock, idempotent
// migrations, fixture cleanup by telegram_user_id.
func TestPostgreSQL16QuotaUsageQueries(t *testing.T) {
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

	db, err := storage.NewPool(ctx, runtimeDSN)
	if err != nil {
		t.Fatalf("open runtime pool: %v", err)
	}
	defer db.Close()

	userRepo := users.NewRepository(db.Pool)
	ownerA, err := userRepo.UpsertByTelegramID(ctx, 93001)
	if err != nil {
		t.Fatalf("create owner A: %v", err)
	}
	ownerB, err := userRepo.UpsertByTelegramID(ctx, 93002)
	if err != nil {
		t.Fatalf("create owner B: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), `DELETE FROM users WHERE telegram_user_id IN (93001, 93002)`)
	})

	msgRepo := messages.NewRepository(db)
	for _, id := range []int64{1, 2, 3} {
		if err := msgRepo.Save(ctx, ownerA.ID, messages.Record{
			BusinessConnectionID: "quota-a",
			ChatID:               770,
			MessageID:            id,
			MessageType:          "text",
			TextContent:          "hello",
			TelegramDate:         1700000000,
		}, false); err != nil {
			t.Fatalf("save owner A message %d: %v", id, err)
		}
	}
	if err := msgRepo.Save(ctx, ownerB.ID, messages.Record{
		BusinessConnectionID: "quota-b",
		ChatID:               770,
		MessageID:            1,
		MessageType:          "text",
		TextContent:          "hello",
		TelegramDate:         1700000000,
	}, false); err != nil {
		t.Fatalf("save owner B message: %v", err)
	}

	mediaRepo := media.NewRepository(db)
	size := int64(1024)
	saveMedia := func(owner int64, connection string, messageID int64) int64 {
		id, err := mediaRepo.Save(ctx, owner, media.Record{
			BusinessConnectionID: connection,
			ChatID:               770,
			MessageID:            messageID,
			FileIndex:            0,
			TelegramFileID:       "file-handle",
			TelegramFileUniqueID: "file-unique",
			MediaType:            media.TypePhoto,
			ByteSize:             &size,
		})
		if err != nil {
			t.Fatalf("save media of tenant %d: %v", owner, err)
		}
		return id
	}
	// Owner A: one stored (512 real bytes -- the measured size wins over the
	// declared 1024), one pending (no disk fact), one purged (file gone).
	storedA := saveMedia(ownerA.ID, "quota-a", 1)
	pendingA := saveMedia(ownerA.ID, "quota-a", 2)
	purgedA := saveMedia(ownerA.ID, "quota-a", 3)
	// Owner B: one stored, 2048 bytes.
	storedB := saveMedia(ownerB.ID, "quota-b", 1)
	_ = pendingA
	storedFile := func(path string, bytes int64) media.StoredFile {
		return media.StoredFile{
			RelativePath: path,
			SHA256:       "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			ByteSize:     bytes,
		}
	}
	if err := mediaRepo.MarkStored(ctx, ownerA.ID, storedA, storedFile("93001/2026-01/01/a1", 512)); err != nil {
		t.Fatalf("mark owner A stored: %v", err)
	}
	if err := mediaRepo.MarkStored(ctx, ownerB.ID, storedB, storedFile("93002/2026-01/01/b1", 2048)); err != nil {
		t.Fatalf("mark owner B stored: %v", err)
	}
	if err := mediaRepo.MarkPurged(ctx, ownerA.ID, purgedA); err != nil {
		t.Fatalf("mark owner A purged: %v", err)
	}

	t.Run("message counts are tenant-scoped", func(t *testing.T) {
		ctx := phaseContext(t)
		countA, err := msgRepo.CountByOwner(ctx, ownerA.ID)
		if err != nil || countA != 3 {
			t.Fatalf("owner A message count = (%d, %v), want (3, nil)", countA, err)
		}
		countB, err := msgRepo.CountByOwner(ctx, ownerB.ID)
		if err != nil || countB != 1 {
			t.Fatalf("owner B message count = (%d, %v), want (1, nil)", countB, err)
		}
	})

	t.Run("media counts cover every status without leaking", func(t *testing.T) {
		ctx := phaseContext(t)
		countA, err := mediaRepo.CountByOwner(ctx, ownerA.ID)
		if err != nil || countA != 3 {
			t.Fatalf("owner A media count = (%d, %v), want (3, nil)", countA, err)
		}
		countB, err := mediaRepo.CountByOwner(ctx, ownerB.ID)
		if err != nil || countB != 1 {
			t.Fatalf("owner B media count = (%d, %v), want (1, nil)", countB, err)
		}
	})

	t.Run("byte sums count stored files only", func(t *testing.T) {
		ctx := phaseContext(t)
		bytesA, err := mediaRepo.SumStoredBytes(ctx, ownerA.ID)
		if err != nil || bytesA != 512 {
			t.Fatalf("owner A stored bytes = (%d, %v), want (512, nil)", bytesA, err)
		}
		bytesB, err := mediaRepo.SumStoredBytes(ctx, ownerB.ID)
		if err != nil || bytesB != 2048 {
			t.Fatalf("owner B stored bytes = (%d, %v), want (2048, nil)", bytesB, err)
		}
	})
}
