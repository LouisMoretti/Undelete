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

	"github.com/jackc/pgx/v5"

	"github.com/LouisMoretti/Undelete/bot/internal/business"
	"github.com/LouisMoretti/Undelete/bot/internal/erasure"
	"github.com/LouisMoretti/Undelete/bot/internal/media"
	"github.com/LouisMoretti/Undelete/bot/internal/media/purge"
	"github.com/LouisMoretti/Undelete/bot/internal/messages"
	"github.com/LouisMoretti/Undelete/bot/internal/outbox"
	"github.com/LouisMoretti/Undelete/bot/internal/storage"
	"github.com/LouisMoretti/Undelete/bot/internal/tenantexcl"
	"github.com/LouisMoretti/Undelete/bot/internal/users"
)

// TestPostgreSQL16Erasure proves against a real PostgreSQL 16 what the unit
// tests cannot: that /delete_my_data deletes ONE tenant, entirely, and that the
// tenant next to it does not lose a single row or a single file.
//
// The unit tests cover the state machine of the challenge and the order of the
// steps against in-memory doubles. What they cannot cover is the SQL underneath
// -- every statement of which runs under FORCE ROW LEVEL SECURITY, where a
// missing or wrong tenant context does not raise an error but silently matches
// zero rows. An erasure that matched zero rows and reported success is the exact
// failure this test exists to catch.
func TestPostgreSQL16Erasure(t *testing.T) {
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
	erased, err := userRepo.UpsertByTelegramID(ctx, 92301)
	if err != nil {
		t.Fatalf("create the tenant to erase: %v", err)
	}
	neighbour, err := userRepo.UpsertByTelegramID(ctx, 92302)
	if err != nil {
		t.Fatalf("create the neighbour tenant: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), `DELETE FROM users WHERE telegram_user_id IN (92301, 92302)`)
	})

	root := t.TempDir()
	messagesRepo := messages.NewRepository(db)
	outboxRepo := outbox.NewRepository(db)
	mediaRepo := media.NewRepository(db)
	// A nil Telegram client is enough: the erasure only ever calls DisableOwner,
	// which is pure SQL plus the in-memory cache. Nothing here reaches the API.
	businessSvc := business.NewService(db.Pool, nil, userRepo, 0, logger)
	mediaPurger, err := purge.New(purge.Config{MediaDir: root, Catalogue: mediaRepo, Logger: logger})
	if err != nil {
		t.Fatalf("new purger: %v", err)
	}
	challenges := erasure.NewRepository(db)
	service, err := erasure.New(erasure.Config{
		Challenges:  challenges,
		Connections: businessSvc,
		Outbox:      outboxRepo,
		Media:       mediaPurger,
		Messages:    messagesRepo,
		Guard:       tenantexcl.New(),
		Logger:      logger,
	})
	if err != nil {
		t.Fatalf("new erasure service: %v", err)
	}

	tenantOf := func(u *users.User, connection string) erasure.Tenant {
		return erasure.Tenant{
			OwnerUserID:          u.ID,
			OwnerTelegramUserID:  u.TelegramUserID,
			BusinessConnectionID: connection,
		}
	}

	// seed gives a tenant the full set: a business connection, two messages
	// (one of them deleted, which writes the alert chunks into the outbox), a
	// chat label, and one catalogued attachment with its blob on disk.
	seed := func(t *testing.T, u *users.User, connection string) string {
		t.Helper()
		if _, err := admin.Exec(ctx, `
			INSERT INTO business_connections (id, owner_user_id, can_reply, is_enabled)
			VALUES ($1, $2, true, true)
			ON CONFLICT (id) DO UPDATE SET is_enabled = true
		`, connection, u.ID); err != nil {
			t.Fatalf("seed connection %s: %v", connection, err)
		}
		for _, id := range []int64{1, 2} {
			if err := messagesRepo.Save(ctx, u.ID, messages.Record{
				BusinessConnectionID: connection,
				ChatID:               990,
				MessageID:            id,
				FromDisplay:          "integration",
				MessageType:          "text",
				TextContent:          fmt.Sprintf("%s message %d", connection, id),
				TelegramDate:         1788019201,
				ChatTitle:            "chat " + connection,
				ChatType:             "private",
			}, false); err != nil {
				t.Fatalf("seed message %d of %s: %v", id, connection, err)
			}
		}
		if _, err := messagesRepo.MarkDeleted(ctx, u.ID, u.TelegramUserID, connection, 990, []int64{1}); err != nil {
			t.Fatalf("seed outbox of %s: %v", connection, err)
		}

		rel := filepath.Join(fmt.Sprintf("%d", u.ID), "2026-01", "05", connection+"-blob")
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		content := []byte("payload-" + connection)
		if err := os.WriteFile(full, content, 0o640); err != nil {
			t.Fatalf("write blob of %s: %v", connection, err)
		}
		sum := sha256.Sum256(content)
		id, err := mediaRepo.Save(ctx, u.ID, media.Record{
			BusinessConnectionID: connection,
			ChatID:               990,
			MessageID:            2,
			TelegramFileID:       "file-handle",
			TelegramFileUniqueID: "unique-" + connection,
			MediaType:            media.TypePhoto,
		})
		if err != nil {
			t.Fatalf("seed media of %s: %v", connection, err)
		}
		if err := mediaRepo.MarkStored(ctx, u.ID, id, media.StoredFile{
			RelativePath: rel,
			SHA256:       hex.EncodeToString(sum[:]),
			ByteSize:     int64(len(content)),
		}); err != nil {
			t.Fatalf("store media of %s: %v", connection, err)
		}
		return rel
	}

	// counts reads the tenant's rows through the ADMIN connection: a superuser
	// bypasses RLS, so "the rows are gone" is established independently of the
	// very tenant context under test. A count taken through InTenant could not
	// tell a deleted row from one merely hidden.
	counts := func(t *testing.T, ownerID int64) map[string]int {
		t.Helper()
		out := map[string]int{}
		for _, table := range []string{"messages", "chats", "notification_outbox", "media_files", "data_erasure_requests"} {
			var count int
			if err := admin.QueryRow(ctx,
				fmt.Sprintf(`SELECT count(*) FROM %s WHERE owner_user_id = $1`, table), ownerID).Scan(&count); err != nil {
				t.Fatalf("count %s of %d: %v", table, ownerID, err)
			}
			out[table] = count
		}
		return out
	}

	erasedBlob := seed(t, erased, "erase-conn")
	neighbourBlob := seed(t, neighbour, "keep-conn")

	before := counts(t, neighbour.ID)
	if before["messages"] != 2 || before["chats"] != 1 || before["notification_outbox"] == 0 || before["media_files"] != 1 {
		t.Fatalf("the neighbour fixture is not what the test assumes: %+v", before)
	}

	t.Run("the challenge table fails closed without a tenant context", func(t *testing.T) {
		ctx := phaseContext(t)
		challenge, err := service.Request(ctx, tenantOf(erased, "erase-conn"))
		if err != nil {
			t.Fatalf("Request: %v", err)
		}
		var visible int
		if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM data_erasure_requests`).Scan(&visible); err != nil {
			t.Fatalf("raw select: %v", err)
		}
		if visible != 0 {
			t.Fatalf("raw select exposed %d erasure requests without a tenant context", visible)
		}
		// And the code itself is not in the table: only its sha256 is.
		var stored int
		if err := admin.QueryRow(ctx,
			`SELECT count(*) FROM data_erasure_requests WHERE code_sha256 = $1`,
			erasure.HashCode(challenge.Code)).Scan(&stored); err != nil {
			t.Fatalf("read the stored hash: %v", err)
		}
		if stored != 1 {
			t.Fatalf("the issued code is stored as %d hashed rows, want 1", stored)
		}
	})

	t.Run("a code is single use and tenant scoped", func(t *testing.T) {
		ctx := phaseContext(t)
		challenge, err := service.Request(ctx, tenantOf(erased, "erase-conn"))
		if err != nil {
			t.Fatalf("Request: %v", err)
		}
		hash := erasure.HashCode(challenge.Code)

		// The neighbour submitting the same code sees nothing at all: RLS hides
		// the row, and the answer is indistinguishable from a code that never
		// existed.
		state, err := challenges.Claim(ctx, neighbour.ID, hash)
		if err != nil {
			t.Fatalf("cross-tenant claim: %v", err)
		}
		if state != erasure.ClaimUnknown {
			t.Fatalf("the neighbour claimed another tenant's code: state=%v", state)
		}

		first, err := challenges.Claim(ctx, erased.ID, hash)
		if err != nil {
			t.Fatalf("first claim: %v", err)
		}
		second, err := challenges.Claim(ctx, erased.ID, hash)
		if err != nil {
			t.Fatalf("second claim: %v", err)
		}
		if first != erasure.ClaimGranted {
			t.Fatalf("first claim = %v, want ClaimGranted", first)
		}
		if second != erasure.ClaimResumable {
			t.Fatalf("second claim = %v: a spent code must never be granted twice", second)
		}
	})

	t.Run("an expired code is refused", func(t *testing.T) {
		ctx := phaseContext(t)
		challenge, err := service.Request(ctx, tenantOf(erased, "erase-conn"))
		if err != nil {
			t.Fatalf("Request: %v", err)
		}
		if _, err := admin.Exec(ctx, `
			UPDATE data_erasure_requests SET expires_at = now() - interval '1 second'
			WHERE owner_user_id = $1 AND code_sha256 = $2
		`, erased.ID, erasure.HashCode(challenge.Code)); err != nil {
			t.Fatalf("expire the code: %v", err)
		}

		outcome, err := service.Confirm(ctx, tenantOf(erased, "erase-conn"), challenge.Code)
		if err != nil {
			t.Fatalf("Confirm: %v", err)
		}
		if outcome != erasure.OutcomeExpired {
			t.Fatalf("outcome = %v, want OutcomeExpired", outcome)
		}
		if got := counts(t, erased.ID); got["messages"] != 2 {
			t.Fatalf("an expired code deleted data: %+v", got)
		}
	})

	t.Run("a confirmed code erases the tenant and only the tenant", func(t *testing.T) {
		ctx := phaseContext(t)
		tenant := tenantOf(erased, "erase-conn")

		// Resolve first, deliberately: it fills the in-memory cache of
		// business.Service, which is the copy a poller would answer from. An
		// erasure that only disables the row in PostgreSQL would leave this
		// cached connection enabled for the lifetime of the process, and the
		// bot would go on saving messages the owner just had erased.
		warm, err := businessSvc.Resolve(ctx, "erase-conn")
		if err != nil {
			t.Fatalf("warm the connection cache: %v", err)
		}
		if !warm.IsEnabled {
			t.Fatal("the fixture connection is not enabled")
		}

		challenge, err := service.Request(ctx, tenant)
		if err != nil {
			t.Fatalf("Request: %v", err)
		}

		outcome, err := service.Confirm(ctx, tenant, challenge.Code)
		if err != nil {
			t.Fatalf("Confirm: %v", err)
		}
		if outcome != erasure.OutcomeErased {
			t.Fatalf("outcome = %v, want OutcomeErased", outcome)
		}

		got := counts(t, erased.ID)
		for _, table := range []string{"messages", "chats", "notification_outbox", "media_files"} {
			if got[table] != 0 {
				t.Fatalf("%d rows survived in %s", got[table], table)
			}
		}
		// The receipt of the spent code survives, and nothing else: it is what
		// makes a replayed confirmation answerable.
		if got["data_erasure_requests"] != 1 {
			t.Fatalf("%d erasure requests survived, want exactly the completed one", got["data_erasure_requests"])
		}
		var status string
		if err := admin.QueryRow(ctx, `
			SELECT status FROM data_erasure_requests WHERE owner_user_id = $1
		`, erased.ID).Scan(&status); err != nil {
			t.Fatalf("read the surviving request: %v", err)
		}
		if status != "completed" {
			t.Fatalf("the surviving request is %q, want completed", status)
		}

		if _, err := os.Lstat(filepath.Join(root, erasedBlob)); !os.IsNotExist(err) {
			t.Fatalf("the erased tenant's blob is still on disk: %v", err)
		}

		var enabled bool
		if err := admin.QueryRow(ctx,
			`SELECT bool_or(is_enabled) FROM business_connections WHERE owner_user_id = $1`, erased.ID).Scan(&enabled); err != nil {
			t.Fatalf("read the connections: %v", err)
		}
		if enabled {
			t.Fatal("a Business connection of the erased tenant is still enabled")
		}
		cached, err := businessSvc.Resolve(ctx, "erase-conn")
		if err != nil {
			t.Fatalf("resolve after the erasure: %v", err)
		}
		if cached.IsEnabled {
			t.Fatal("the cached connection still reports enabled: the poller would keep saving")
		}

		// The whole point of the test: the neighbour is untouched, in every
		// table and on disk.
		after := counts(t, neighbour.ID)
		for table, want := range before {
			if after[table] != want {
				t.Fatalf("the neighbour's %s went from %d to %d rows", table, want, after[table])
			}
		}
		if _, err := os.Stat(filepath.Join(root, neighbourBlob)); err != nil {
			t.Fatalf("the neighbour's blob was deleted: %v", err)
		}
		var neighbourEnabled bool
		if err := admin.QueryRow(ctx,
			`SELECT bool_and(is_enabled) FROM business_connections WHERE owner_user_id = $1`, neighbour.ID).Scan(&neighbourEnabled); err != nil {
			t.Fatalf("read the neighbour's connections: %v", err)
		}
		if !neighbourEnabled {
			t.Fatal("the neighbour's Business connection was disabled")
		}

		// Submitting the same code again: no error, no second deletion, and the
		// neighbour still untouched.
		replay, err := service.Confirm(ctx, tenant, challenge.Code)
		if err != nil {
			t.Fatalf("replayed Confirm: %v", err)
		}
		if replay != erasure.OutcomeAlreadyErased {
			t.Fatalf("replayed outcome = %v, want OutcomeAlreadyErased", replay)
		}
		if again := counts(t, neighbour.ID); again["messages"] != before["messages"] {
			t.Fatalf("the replay touched the neighbour: %+v", again)
		}
	})

	t.Run("an interrupted erasure is resumed by the same code", func(t *testing.T) {
		ctx := phaseContext(t)
		// A crash between two steps leaves the request 'consumed'. The state is
		// reproduced here by claiming without erasing -- which is exactly what a
		// process killed right after the claim leaves behind.
		tenant := tenantOf(neighbour, "keep-conn")
		challenge, err := service.Request(ctx, tenant)
		if err != nil {
			t.Fatalf("Request: %v", err)
		}
		state, err := challenges.Claim(ctx, neighbour.ID, erasure.HashCode(challenge.Code))
		if err != nil {
			t.Fatalf("Claim: %v", err)
		}
		if state != erasure.ClaimGranted {
			t.Fatalf("Claim = %v, want ClaimGranted", state)
		}

		outcome, err := service.Confirm(ctx, tenant, challenge.Code)
		if err != nil {
			t.Fatalf("resumed Confirm: %v", err)
		}
		if outcome != erasure.OutcomeErased {
			t.Fatalf("outcome = %v, want OutcomeErased: an interrupted erasure must be resumable", outcome)
		}
		got := counts(t, neighbour.ID)
		for _, table := range []string{"messages", "chats", "notification_outbox", "media_files"} {
			if got[table] != 0 {
				t.Fatalf("the resumed erasure left %d rows in %s", got[table], table)
			}
		}
		if _, err := os.Lstat(filepath.Join(root, neighbourBlob)); !os.IsNotExist(err) {
			t.Fatalf("the resumed erasure left the blob on disk: %v", err)
		}
	})
}
