//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/LouisMoretti/Undelete/bot/internal/app"
	"github.com/LouisMoretti/Undelete/bot/internal/business"
	"github.com/LouisMoretti/Undelete/bot/internal/media"
	"github.com/LouisMoretti/Undelete/bot/internal/messages"
	"github.com/LouisMoretti/Undelete/bot/internal/privacy"
	"github.com/LouisMoretti/Undelete/bot/internal/storage"
	"github.com/LouisMoretti/Undelete/bot/internal/telegram"
	"github.com/LouisMoretti/Undelete/bot/internal/users"
)

// recordingSender captures the sendMessage calls the handler would make.
type recordingSender struct {
	sent []telegram.SendMessageRequest
}

func (r *recordingSender) SendMessage(_ context.Context, req telegram.SendMessageRequest) error {
	r.sent = append(r.sent, req)
	return nil
}

// TestPrivacyCommandAnswersOnlyTheConnectionOwner drives HandleUpdate exactly
// like telegram.Poller does, against a real database, for the one criterion
// unit tests cannot fully close: "answer only the owner" depends on the owner
// identity resolved from business_connections JOIN users, not from anything
// carried by the update itself.
//
// A fake businessService would assert the handler's own logic against the
// handler's own assumptions. Here the connection is a real row, the owner's
// telegram_user_id comes back through the real resolution chain, and the
// contact's /privacy is compared against THAT.
func TestPrivacyCommandAnswersOnlyTheConnectionOwner(t *testing.T) {
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

	const ownerTelegramID, contactTelegramID = 94001, 94002
	userRepo := users.NewRepository(db.Pool)
	owner, err := userRepo.UpsertByTelegramID(ctx, ownerTelegramID)
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	const connectionID = "bc-handler-privacy-test"
	if _, err := db.Pool.Exec(ctx, `
		INSERT INTO business_connections (id, owner_user_id, can_reply, is_enabled)
		VALUES ($1, $2, true, true)
		ON CONFLICT (id) DO UPDATE SET owner_user_id = EXCLUDED.owner_user_id, is_enabled = true
	`, connectionID, owner.ID); err != nil {
		t.Fatalf("seed business connection: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), `DELETE FROM users WHERE telegram_user_id = $1`, ownerTelegramID)
	})

	// client stays nil: the connection row exists, so Resolve never reaches
	// the Telegram API (cache miss -> DB hit).
	businessSvc := business.NewService(db.Pool, nil, userRepo, 0, logger)
	sender := &recordingSender{}
	handler := app.NewHandler(businessSvc, messages.NewRepository(db), media.NewRepository(db), logger,
		app.WithCommandSender(sender))

	const chatID = 780001
	command := func(messageID, fromID int64) telegram.Update {
		return telegram.Update{
			BusinessMessage: &telegram.Message{
				MessageID:            messageID,
				Chat:                 telegram.Chat{ID: chatID, Type: "private", FirstName: "Anaïs"},
				From:                 &telegram.User{ID: fromID, FirstName: "Sender"},
				Date:                 1788019300,
				BusinessConnectionID: connectionID,
				Text:                 "/privacy",
			},
		}
	}

	if err := handler.HandleUpdate(ctx, command(1, contactTelegramID)); err != nil {
		t.Fatalf("HandleUpdate (contact): %v", err)
	}
	if len(sender.sent) != 0 {
		t.Fatalf("a contact's /privacy produced %d message(s), want 0", len(sender.sent))
	}

	if err := handler.HandleUpdate(ctx, command(2, ownerTelegramID)); err != nil {
		t.Fatalf("HandleUpdate (owner): %v", err)
	}
	if len(sender.sent) == 0 {
		t.Fatal("the owner's /privacy produced no answer")
	}

	var rebuilt strings.Builder
	for index, req := range sender.sent {
		if req.ChatID != ownerTelegramID {
			t.Fatalf("chunk %d: chat_id = %d, want the owner's telegram_user_id (%d)", index, req.ChatID, ownerTelegramID)
		}
		// Each chunk announces its rank and the real total, so a delivery that
		// stops short reads as incomplete instead of passing for the whole
		// policy. Stripping the label is also how the document is checked
		// whole.
		prefix := fmt.Sprintf("Privacy policy (%d/%d)\n\n", index+1, len(sender.sent))
		if !strings.HasPrefix(req.Text, prefix) {
			t.Fatalf("chunk %d does not start with %q", index, prefix)
		}
		rebuilt.WriteString(strings.TrimPrefix(req.Text, prefix))
	}
	if rebuilt.String() != privacy.Text() {
		t.Fatal("the delivered answer is not the policy document, whole and in order")
	}

	// The exhaustive-and-automatic-saving constraint: a command is a message like any other, and both of them
	// were saved -- withholding the answer never withholds the capture.
	t.Run("both commands were saved like ordinary messages", func(t *testing.T) {
		ctx := phaseContext(t)
		var saved int
		err := db.InTenant(ctx, owner.ID, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `
				SELECT count(*) FROM messages
				WHERE business_connection_id = $1 AND chat_id = $2
			`, connectionID, chatID).Scan(&saved)
		})
		if err != nil {
			t.Fatalf("count saved messages: %v", err)
		}
		if saved != 2 {
			t.Fatalf("saved messages = %d, want 2", saved)
		}
	})
}
