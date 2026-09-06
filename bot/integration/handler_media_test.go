//go:build integration

package integration_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/LouisMoretti/Undelete/bot/internal/app"
	"github.com/LouisMoretti/Undelete/bot/internal/business"
	"github.com/LouisMoretti/Undelete/bot/internal/media"
	"github.com/LouisMoretti/Undelete/bot/internal/messages"
	"github.com/LouisMoretti/Undelete/bot/internal/storage"
	"github.com/LouisMoretti/Undelete/bot/internal/telegram"
	"github.com/LouisMoretti/Undelete/bot/internal/users"
)

// TestHandlerCataloguesPhotoAttachment closes a coverage gap found while
// manually testing PR #48 against a real Telegram Business connection: a
// live photo sent by the owner was saved as an empty text-type message, with
// zero media_files row, because between #47 and #49 nothing ever called
// telegram.ExtractMedia / media.Repository.Save from the update-handling
// path. Neither handler_test.go (pure helpers only) nor any file in this
// package ever exercised app.Handler.HandleUpdate end to end, so the gap was
// invisible to `make check` and to `make test-integration` alike -- only a
// live Bot API round-trip caught it, after #49 had already fixed it upstream
// on `dev`.
//
// This test drives HandleUpdate exactly like telegram.Poller does, with a
// business_message that carries a photo, and asserts on both halves of the
// contract this package is supposed to protect: the messages row must be
// typed "photo" (not the historical hardcoded "text"), and a matching
// media_files row must exist. Deleting the `h.saveMedia(...)` call in
// bot/internal/app/handler.go (i.e. reverting to the pre-#49 shape) makes
// this test fail on both counts.
func TestHandlerCataloguesPhotoAttachment(t *testing.T) {
	adminDSN := requireEnv(t, "POSTGRES_INTEGRATION_ADMIN_DSN")
	runtimeDSN := requireEnv(t, "POSTGRES_INTEGRATION_RUNTIME_DSN")
	optIn := os.Getenv("POSTGRES_INTEGRATION_ALLOW_DESTRUCTIVE")
	if err := validateExplicitDestructiveOptIn(optIn); err != nil {
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
	if err := validateDestructiveInterlock(optIn, databaseName); err != nil {
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
	owner, err := userRepo.UpsertByTelegramID(ctx, 93001)
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	const connectionID = "bc-handler-media-gap-test"
	if _, err := db.Pool.Exec(ctx, `
		INSERT INTO business_connections (id, owner_user_id, can_reply, is_enabled)
		VALUES ($1, $2, true, true)
		ON CONFLICT (id) DO UPDATE SET owner_user_id = EXCLUDED.owner_user_id, is_enabled = true
	`, connectionID, owner.ID); err != nil {
		t.Fatalf("seed business connection: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), `DELETE FROM users WHERE telegram_user_id = 93001`)
	})

	// client stays nil: business.Service.Resolve is satisfied by the row just
	// inserted (cache miss -> DB hit) and never reaches the Telegram API for
	// a connection that already exists.
	businessSvc := business.NewService(db.Pool, nil, userRepo, 0, logger)
	messagesRepo := messages.NewRepository(db)
	mediaRepo := media.NewRepository(db)
	handler := app.NewHandler(businessSvc, messagesRepo, mediaRepo, logger)

	const chatID, messageID = 770001, 1
	update := telegram.Update{
		BusinessMessage: &telegram.Message{
			MessageID:            messageID,
			Chat:                 telegram.Chat{ID: chatID, Type: "private", FirstName: "Anaïs"},
			Date:                 1788019300,
			BusinessConnectionID: connectionID,
			Caption:              "vacation photo",
			Photo: []telegram.PhotoSize{
				{FileID: "photo-large", FileUniqueID: "unique-photo-large", Width: 1280, Height: 853, FileSize: 184320},
			},
		},
	}

	if err := handler.HandleUpdate(ctx, update); err != nil {
		t.Fatalf("HandleUpdate: %v", err)
	}

	t.Run("messages row is typed photo, not text", func(t *testing.T) {
		ctx := phaseContext(t)
		var messageType, textContent string
		err := db.InTenant(ctx, owner.ID, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `
				SELECT message_type, text_content FROM messages
				WHERE business_connection_id = $1 AND chat_id = $2 AND message_id = $3
			`, connectionID, chatID, messageID).Scan(&messageType, &textContent)
		})
		if err != nil {
			t.Fatalf("read back message: %v", err)
		}
		if messageType != telegram.MediaTypePhoto {
			t.Fatalf("message_type = %q, want %q (a photo saved as %q means the media pipeline is not wired into the handler)",
				messageType, telegram.MediaTypePhoto, messageType)
		}
		if textContent != "vacation photo" {
			t.Fatalf("text_content = %q, want the caption %q", textContent, "vacation photo")
		}
	})

	t.Run("media_files has a matching catalogue row", func(t *testing.T) {
		ctx := phaseContext(t)
		files, err := mediaRepo.GetByMessage(ctx, owner.ID, connectionID, chatID, messageID)
		if err != nil {
			t.Fatalf("GetByMessage: %v", err)
		}
		if len(files) != 1 {
			t.Fatalf("media_files rows for this message = %d, want 1 (a photo that reaches the handler must be catalogued, even before its bytes are downloaded)", len(files))
		}
		if files[0].TelegramFileUniqueID != "unique-photo-large" {
			t.Fatalf("telegram_file_unique_id = %q, want %q", files[0].TelegramFileUniqueID, "unique-photo-large")
		}
		if files[0].MediaType != media.TypePhoto {
			t.Fatalf("media_type = %q, want %q", files[0].MediaType, media.TypePhoto)
		}
	})
}
