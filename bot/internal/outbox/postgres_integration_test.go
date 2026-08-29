package outbox_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/LouisMoretti/Undelete/bot/internal/messages"
	"github.com/LouisMoretti/Undelete/bot/internal/outbox"
	"github.com/LouisMoretti/Undelete/bot/internal/storage"
)

func TestPostgresOutboxIsAtomicTenantAwareAndIdempotent(t *testing.T) {
	dsn := os.Getenv("OUTBOX_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("OUTBOX_TEST_DATABASE_URL absent")
	}
	ctx := context.Background()
	db, err := storage.NewPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var ownerID int64
	if err := db.Pool.QueryRow(ctx, `INSERT INTO users (telegram_user_id) VALUES (900001) RETURNING id`).Scan(&ownerID); err != nil {
		t.Fatal(err)
	}
	defer db.Pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, ownerID)

	repo := messages.NewRepository(db)
	record := messages.Record{BusinessConnectionID: "bc-integration", ChatID: 81, MessageID: 91, MessageType: "text", TextContent: "secret", TelegramDate: 1}
	if err := repo.Save(ctx, ownerID, record, false); err != nil {
		t.Fatal(err)
	}

	for delivery := 0; delivery < 2; delivery++ {
		found, err := repo.MarkDeleted(ctx, ownerID, 900001, record.BusinessConnectionID, record.ChatID, []int64{record.MessageID})
		if err != nil || len(found) != 1 {
			t.Fatalf("redélivrance %d: found=%d err=%v", delivery, len(found), err)
		}
	}

	var count int
	if err := db.InTenant(ctx, ownerID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM notification_outbox`).Scan(&count)
	}); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("l'outbox contient %d lignes après redélivrance, attendu 1", count)
	}

	outboxRepo := outbox.NewRepository(db)
	now := time.Now()
	job, err := outboxRepo.Claim(ctx, ownerID, now, time.Minute)
	if err != nil || job == nil {
		t.Fatalf("claim PostgreSQL: job=%v err=%v", job, err)
	}
	if duplicate, err := outboxRepo.Claim(ctx, ownerID, now.Add(30*time.Second), time.Minute); err != nil || duplicate != nil {
		t.Fatalf("claim avant expiration du lease: job=%v err=%v", duplicate, err)
	}
	if recovered, err := outboxRepo.Claim(ctx, ownerID, now.Add(time.Minute), time.Minute); err != nil || recovered == nil {
		t.Fatalf("reprise après expiration du lease: job=%v err=%v", recovered, err)
	}
	next := now.Add(2 * time.Minute)
	if err := outboxRepo.MarkRetry(ctx, ownerID, job.ID, next, "timeout"); err != nil {
		t.Fatal(err)
	}
	if early, err := outboxRepo.Claim(ctx, ownerID, next.Add(-time.Second), time.Minute); err != nil || early != nil {
		t.Fatalf("claim avant next_attempt_at: job=%v err=%v", early, err)
	}
	retried, err := outboxRepo.Claim(ctx, ownerID, next, time.Minute)
	if err != nil || retried == nil || retried.Attempts != 1 {
		t.Fatalf("claim après retry: job=%v err=%v", retried, err)
	}
	if err := outboxRepo.MarkSent(ctx, ownerID, retried.ID, next); err != nil {
		t.Fatal(err)
	}
	if sentAgain, err := outboxRepo.Claim(ctx, ownerID, next.Add(2*time.Minute), time.Minute); err != nil || sentAgain != nil {
		t.Fatalf("un job sent a été repris: job=%v err=%v", sentAgain, err)
	}

	permanent := record
	permanent.MessageID = 93
	if err := repo.Save(ctx, ownerID, permanent, false); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.MarkDeleted(ctx, ownerID, 900001, permanent.BusinessConnectionID, permanent.ChatID, []int64{permanent.MessageID}); err != nil {
		t.Fatal(err)
	}
	failedJob, err := outboxRepo.Claim(ctx, ownerID, next.Add(3*time.Minute), time.Minute)
	if err != nil || failedJob == nil {
		t.Fatalf("claim avant échec définitif: job=%v err=%v", failedJob, err)
	}
	if err := outboxRepo.MarkFailed(ctx, ownerID, failedJob.ID, "telegram_400"); err != nil {
		t.Fatal(err)
	}
	if failedAgain, err := outboxRepo.Claim(ctx, ownerID, next.Add(5*time.Minute), time.Minute); err != nil || failedAgain != nil {
		t.Fatalf("un job failed a été repris: job=%v err=%v", failedAgain, err)
	}

	if err := db.InTenant(ctx, ownerID+1, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM notification_outbox`).Scan(&count)
	}); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("RLS expose %d lignes à un autre tenant", count)
	}

	ownerDSN := os.Getenv("OUTBOX_TEST_MIGRATION_DATABASE_URL")
	if ownerDSN == "" {
		t.Skip("OUTBOX_TEST_MIGRATION_DATABASE_URL absent pour la preuve de rollback atomique")
	}
	owner, err := pgx.Connect(ctx, ownerDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close(ctx)
	if _, err := owner.Exec(ctx, `
		CREATE OR REPLACE FUNCTION reject_test_outbox() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.message_id = 92 THEN RAISE EXCEPTION 'test outbox failure'; END IF;
			RETURN NEW;
		END $$;
		CREATE TRIGGER reject_test_outbox BEFORE INSERT ON notification_outbox
		FOR EACH ROW EXECUTE FUNCTION reject_test_outbox();
	`); err != nil {
		t.Fatal(err)
	}
	defer owner.Exec(ctx, `DROP TRIGGER IF EXISTS reject_test_outbox ON notification_outbox; DROP FUNCTION IF EXISTS reject_test_outbox()`)

	failing := record
	failing.MessageID = 92
	if err := repo.Save(ctx, ownerID, failing, false); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.MarkDeleted(ctx, ownerID, 900001, failing.BusinessConnectionID, failing.ChatID, []int64{failing.MessageID}); err == nil {
		t.Fatal("le marquage devait échouer avec l'insertion outbox")
	}
	var deleted bool
	if err := db.InTenant(ctx, ownerID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT deleted_at IS NOT NULL FROM messages WHERE message_id = $1`, failing.MessageID).Scan(&deleted)
	}); err != nil {
		t.Fatal(err)
	}
	if deleted {
		t.Fatal("deleted_at a été committé malgré l'échec outbox")
	}
}
