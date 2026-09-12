//go:build integration

package integration_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/LouisMoretti/Undelete/bot/internal/messages"
	"github.com/LouisMoretti/Undelete/bot/internal/outbox"
	"github.com/LouisMoretti/Undelete/bot/internal/storage"
	"github.com/LouisMoretti/Undelete/bot/internal/users"
)

// TestPostgreSQL16OutboxLifecycle proves on a real PostgreSQL 16 the outbox
// behaviour the unit suite cannot: RLS fail-closed on notification_outbox,
// the Claim → MarkSent / MarkRetry / MarkFailed lease lifecycle (expiry,
// delay, wrong token, cross-tenant isolation), chunk ordering, the malformed
// media payload fallback, and the per-tenant PurgeExpired + CountBacklog.
func TestPostgreSQL16OutboxLifecycle(t *testing.T) {
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
	ownerA, err := userRepo.UpsertByTelegramID(ctx, 93201)
	if err != nil {
		t.Fatalf("create owner A: %v", err)
	}
	ownerB, err := userRepo.UpsertByTelegramID(ctx, 93202)
	if err != nil {
		t.Fatalf("create owner B: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), `DELETE FROM users WHERE telegram_user_id IN (93201, 93202)`)
	})
	for _, seed := range []struct {
		conn  string
		owner int64
	}{
		{"bc-hard-a", ownerA.ID}, {"bc-hard-b", ownerB.ID},
	} {
		if _, err := db.Pool.Exec(ctx, `
			INSERT INTO business_connections (id, owner_user_id, can_reply, is_enabled)
			VALUES ($1, $2, true, true)
			ON CONFLICT (id) DO UPDATE SET owner_user_id = EXCLUDED.owner_user_id, is_enabled = true
		`, seed.conn, seed.owner); err != nil {
			t.Fatalf("seed connection %s: %v", seed.conn, err)
		}
	}

	msgRepo := messages.NewRepository(db)
	outboxRepo := outbox.NewRepository(db)

	save := func(ownerID int64, conn string, chatID, msgID int64, text string) {
		t.Helper()
		rec := messages.Record{
			BusinessConnectionID: conn, ChatID: chatID, MessageID: msgID,
			FromDisplay: "lifecycle", MessageType: "text", TextContent: text,
			TelegramDate: 1788019201, ChatTitle: "t", ChatType: "private",
		}
		if err := msgRepo.Save(context.Background(), ownerID, rec, false); err != nil {
			t.Fatalf("save %s/%d: %v", conn, msgID, err)
		}
	}
	insertChunk := func(ownerID, ownerTgID int64, conn string, chatID, msgID int64, chunk int, text string) {
		t.Helper()
		if err := db.InTenant(context.Background(), ownerID, func(tx pgx.Tx) error {
			return outbox.InsertTx(context.Background(), tx, ownerID, ownerTgID, conn, chatID, msgID, outbox.EventDeletedMessage, chunk, text)
		}); err != nil {
			t.Fatalf("insert chunk %d: %v", chunk, err)
		}
	}

	t.Run("outbox RLS fails closed without tenant context", func(t *testing.T) {
		ctx := phaseContext(t)
		var count int
		if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM notification_outbox`).Scan(&count); err != nil {
			t.Fatalf("raw select: %v", err)
		}
		if count != 0 {
			t.Fatalf("raw select exposed %d outbox rows without tenant context", count)
		}
		tag, err := db.Pool.Exec(ctx, `UPDATE notification_outbox SET status = 'sent'`)
		if err != nil {
			t.Fatalf("fail-closed raw update should be a zero-row no-op: %v", err)
		}
		if tag.RowsAffected() != 0 {
			t.Fatalf("raw update touched %d rows without tenant context", tag.RowsAffected())
		}
		tag, err = db.Pool.Exec(ctx, `DELETE FROM notification_outbox`)
		if err != nil {
			t.Fatalf("fail-closed raw delete should be a zero-row no-op: %v", err)
		}
		if tag.RowsAffected() != 0 {
			t.Fatalf("raw delete touched %d rows without tenant context", tag.RowsAffected())
		}
		if _, err := db.Pool.Exec(ctx, `INSERT INTO notification_outbox (owner_user_id, owner_telegram_user_id, business_connection_id, chat_id, message_id, event_type, chunk_index, payload_text) VALUES ($1, 93201, 'raw', 1, 1, 'deleted_message', 0, 'x')`, ownerA.ID); err == nil {
			t.Fatal("raw insert unexpectedly bypassed RLS WITH CHECK")
		}
	})

	t.Run("claim/sent/retry/failed lifecycle with leases", func(t *testing.T) {
		// Fresh alert via the real path: save then mark deleted.
		save(ownerA.ID, "bc-hard-a", 88101, 1, "lifecycle")
		found, err := msgRepo.MarkDeleted(context.Background(), ownerA.ID, 93201, "bc-hard-a", 88101, []int64{1})
		if err != nil || len(found) != 1 {
			t.Fatalf("MarkDeleted = %d rows, %v", len(found), err)
		}
		job, err := outboxRepo.Claim(context.Background(), ownerA.ID, 2*time.Minute)
		if err != nil || job == nil {
			t.Fatalf("Claim = (%v, %v), want job", job, err)
		}
		if job.Text == "" || job.BusinessConnectionID != "bc-hard-a" {
			t.Fatalf("unexpected job: %+v", job)
		}
		// Wrong lease token must fail loudly, never silently ack another job.
		if err := outboxRepo.MarkSent(context.Background(), ownerA.ID, job.ID, "wrong-token"); !errors.Is(err, outbox.ErrLeaseLost) {
			t.Fatalf("MarkSent with wrong token = %v, want ErrLeaseLost", err)
		}
		// Cross-tenant ack of the same id must also lose the lease.
		if err := outboxRepo.MarkSent(context.Background(), ownerB.ID, job.ID, job.LeaseToken); !errors.Is(err, outbox.ErrLeaseLost) {
			t.Fatalf("cross-tenant MarkSent = %v, want ErrLeaseLost", err)
		}
		if err := outboxRepo.MarkSent(context.Background(), ownerA.ID, job.ID, job.LeaseToken); err != nil {
			t.Fatalf("MarkSent: %v", err)
		}
		if again, err := outboxRepo.Claim(context.Background(), ownerA.ID, 2*time.Minute); err != nil || again != nil {
			t.Fatalf("Claim after sent = (%v, %v), want (nil, nil)", again, err)
		}
	})

	t.Run("lease expiry allows reclaim, retry delay blocks claim", func(t *testing.T) {
		insertChunk(ownerA.ID, 93201, "bc-hard-a", 88102, 2, 0, "reclaim me")
		job, err := outboxRepo.Claim(context.Background(), ownerA.ID, 150*time.Millisecond)
		if err != nil || job == nil {
			t.Fatalf("Claim = (%v, %v)", job, err)
		}
		time.Sleep(400 * time.Millisecond) // let locked_until pass on the server clock
		reclaimed, err := outboxRepo.Claim(context.Background(), ownerA.ID, 2*time.Minute)
		if err != nil || reclaimed == nil || reclaimed.ID != job.ID {
			t.Fatalf("reclaim after expiry = (%v, %v), want same id %d", reclaimed, err, job.ID)
		}
		if err := outboxRepo.MarkRetry(context.Background(), ownerA.ID, job.ID, reclaimed.LeaseToken, time.Hour, "telegram_429"); err != nil {
			t.Fatalf("MarkRetry: %v", err)
		}
		if blocked, err := outboxRepo.Claim(context.Background(), ownerA.ID, 2*time.Minute); err != nil || blocked != nil {
			t.Fatalf("Claim during retry delay = (%v, %v), want (nil, nil)", blocked, err)
		}
		// Age the retry deadline past with the owner role, then fail terminally.
		if _, err := admin.Exec(context.Background(), `UPDATE notification_outbox SET next_attempt_at = now() - interval '1 minute' WHERE id = $1`, job.ID); err != nil {
			t.Fatalf("age retry deadline: %v", err)
		}
		job2, err := outboxRepo.Claim(context.Background(), ownerA.ID, 2*time.Minute)
		if err != nil || job2 == nil {
			t.Fatalf("Claim after deadline = (%v, %v)", job2, err)
		}
		if err := outboxRepo.MarkFailed(context.Background(), ownerA.ID, job2.ID, job2.LeaseToken, "telegram_400"); err != nil {
			t.Fatalf("MarkFailed: %v", err)
		}
		if again, err := outboxRepo.Claim(context.Background(), ownerA.ID, 2*time.Minute); err != nil || again != nil {
			t.Fatalf("Claim after failed = (%v, %v), want (nil, nil)", again, err)
		}
	})

	t.Run("chunk ordering and cross-tenant claim isolation", func(t *testing.T) {
		insertChunk(ownerA.ID, 93201, "bc-hard-a", 88103, 3, 0, "part zero")
		insertChunk(ownerA.ID, 93201, "bc-hard-a", 88103, 3, 1, "part one")
		first, err := outboxRepo.Claim(context.Background(), ownerA.ID, 2*time.Minute)
		if err != nil || first == nil {
			t.Fatalf("Claim = (%v, %v)", first, err)
		}
		// Chunk 1 must stay blocked while chunk 0 is still processing.
		second, err := outboxRepo.Claim(context.Background(), ownerA.ID, 2*time.Minute)
		if err != nil {
			t.Fatalf("second Claim: %v", err)
		}
		if second != nil {
			t.Fatalf("chunk 1 claimed while chunk 0 unfinished (id %d), ordering broken", second.ID)
		}
		// The other tenant must not see this job at all.
		if foreign, err := outboxRepo.Claim(context.Background(), ownerB.ID, 2*time.Minute); err != nil || foreign != nil {
			t.Fatalf("cross-tenant Claim = (%v, %v), want (nil, nil)", foreign, err)
		}
		if err := outboxRepo.MarkSent(context.Background(), ownerA.ID, first.ID, first.LeaseToken); err != nil {
			t.Fatalf("MarkSent chunk 0: %v", err)
		}
		next, err := outboxRepo.Claim(context.Background(), ownerA.ID, 2*time.Minute)
		if err != nil || next == nil || !strings.Contains(next.Text, "part one") {
			t.Fatalf("Claim chunk 1 = (%v, %v), want part one", next, err)
		}
		_ = outboxRepo.MarkSent(context.Background(), ownerA.ID, next.ID, next.LeaseToken)
	})

	t.Run("malformed media payload falls back to text", func(t *testing.T) {
		// '{}' is valid JSONB (passes the column type) but carries no items:
		// Claim must return the job with Media == nil and no error, so the
		// worker delivers the text fallback instead of stranding the alert.
		if err := db.InTenant(context.Background(), ownerA.ID, func(tx pgx.Tx) error {
			_, err := tx.Exec(context.Background(), `
				INSERT INTO notification_outbox (owner_user_id, owner_telegram_user_id, business_connection_id, chat_id, message_id, event_type, chunk_index, payload_text, payload_kind, media_payload)
				VALUES ($1, 93201, 'bc-hard-a', 88104, 4, 'deleted_message', 0, 'fallback text', 'media', '{}')
				ON CONFLICT DO NOTHING
			`, ownerA.ID)
			return err
		}); err != nil {
			t.Fatalf("insert malformed media row: %v", err)
		}
		job, err := outboxRepo.Claim(context.Background(), ownerA.ID, 2*time.Minute)
		if err != nil || job == nil {
			t.Fatalf("Claim = (%v, %v)", job, err)
		}
		if job.Media != nil {
			t.Fatalf("malformed payload must decode to nil Media, got %+v", job.Media)
		}
		_ = outboxRepo.MarkSent(context.Background(), ownerA.ID, job.ID, job.LeaseToken)
	})

	t.Run("purge is per tenant and backlog sums tenants", func(t *testing.T) {
		// Aged sent rows FIRST: Claim serves oldest-first, so inserting the
		// backlog before claiming would make aged() ack the wrong row.
		aged := func(ownerID, ownerTgID int64, conn string) int64 {
			t.Helper()
			var id int64
			if err := db.InTenant(context.Background(), ownerID, func(tx pgx.Tx) error {
				if err := outbox.InsertTx(context.Background(), tx, ownerID, ownerTgID, conn, 88106, 6, outbox.EventDeletedMessage, 0, "aged"); err != nil {
					return err
				}
				return tx.QueryRow(context.Background(), `SELECT id FROM notification_outbox WHERE owner_user_id = $1 AND chat_id = 88106`, ownerID).Scan(&id)
			}); err != nil {
				t.Fatalf("insert aged: %v", err)
			}
			job, err := outboxRepo.Claim(context.Background(), ownerID, 2*time.Minute)
			if err != nil || job == nil {
				t.Fatalf("claim aged: %v", err)
			}
			if err := outboxRepo.MarkSent(context.Background(), ownerID, job.ID, job.LeaseToken); err != nil {
				t.Fatalf("send aged: %v", err)
			}
			return id
		}
		idA := aged(ownerA.ID, 93201, "bc-hard-a")
		idB := aged(ownerB.ID, 93202, "bc-hard-b")
		if _, err := admin.Exec(context.Background(), `UPDATE notification_outbox SET created_at = now() - interval '10 days' WHERE id IN ($1, $2)`, idA, idB); err != nil {
			t.Fatalf("age sent rows: %v", err)
		}
		// Two pending alerts that stay (inserted after the aged rows were
		// already sent, so no Claim ambiguity above).
		insertChunk(ownerA.ID, 93201, "bc-hard-a", 88105, 5, 0, "backlog A")
		insertChunk(ownerB.ID, 93202, "bc-hard-b", 88105, 5, 0, "backlog B")
		backlog, err := outboxRepo.CountBacklog(context.Background(), []users.TenantRetention{{OwnerUserID: ownerA.ID}, {OwnerUserID: ownerB.ID}})
		if err != nil {
			t.Fatalf("CountBacklog: %v", err)
		}
		if backlog < 2 {
			t.Fatalf("backlog = %d, want >= 2 pending rows", backlog)
		}
		purged, err := outboxRepo.PurgeExpired(context.Background(), []users.TenantRetention{{OwnerUserID: ownerA.ID, RetentionDays: 7}})
		if err != nil {
			t.Fatalf("purge A: %v", err)
		}
		if purged != 1 {
			t.Fatalf("purge A = %d, want exactly the 1 aged sent row of tenant A", purged)
		}
		// Tenant B's aged row must still be there.
		var remaining int
		if err := db.InTenant(context.Background(), ownerB.ID, func(tx pgx.Tx) error {
			return tx.QueryRow(context.Background(), `SELECT count(*) FROM notification_outbox WHERE id = $1`, idB).Scan(&remaining)
		}); err != nil || remaining != 1 {
			t.Fatalf("tenant B row leaked into tenant A purge: count=%d err=%v", remaining, err)
		}
	})
}
