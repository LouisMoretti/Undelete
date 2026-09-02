package outbox_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/LouisMoretti/Undelete/bot/internal/messages"
	"github.com/LouisMoretti/Undelete/bot/internal/outbox"
	"github.com/LouisMoretti/Undelete/bot/internal/storage"
	"github.com/LouisMoretti/Undelete/bot/internal/users"
)

func openRuntimeDB(t *testing.T) (context.Context, *storage.DB) {
	t.Helper()
	dsn := os.Getenv("OUTBOX_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("OUTBOX_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	db, err := storage.NewPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	return ctx, db
}

func createTestOwner(t *testing.T, ctx context.Context, db *storage.DB, telegramID int64) int64 {
	t.Helper()
	var ownerID int64
	if err := db.Pool.QueryRow(ctx, `INSERT INTO users (telegram_user_id) VALUES ($1) RETURNING id`, telegramID).Scan(&ownerID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, ownerID) })
	return ownerID
}

func TestPostgresOutboxIsAtomicTenantAwareAndIdempotent(t *testing.T) {
	dsn := os.Getenv("OUTBOX_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("OUTBOX_TEST_DATABASE_URL not set")
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
			t.Fatalf("redelivery %d: found=%d err=%v", delivery, len(found), err)
		}
	}

	var count int
	if err := db.InTenant(ctx, ownerID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM notification_outbox`).Scan(&count)
	}); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("outbox contains %d rows after redelivery, expected 1", count)
	}

	outboxRepo := outbox.NewRepository(db)
	// The lease and next_attempt_at are evaluated AND written on the PostgreSQL
	// clock (clock_timestamp): no Go timestamp enters the scenario, transitions
	// are driven via real leases, explicit retry delays and the fencing token.
	// The 1 s lease (not "the shortest possible") is a deliberate choice: in CI,
	// two consecutive Claims can be tens of ms apart (scheduling + DB round
	// trip); a 30 ms lease could expire BEFORE the duplicated claim, and the
	// 60 ms sleep was only 2x the lease. The 1 s / 1.2 s margins absorb this
	// latency in both directions, at the cost of one second of test time.
	job, err := outboxRepo.Claim(ctx, ownerID, time.Second)
	if err != nil || job == nil || job.LeaseToken == "" {
		t.Fatalf("PostgreSQL claim: job=%+v err=%v", job, err)
	}
	if duplicate, err := outboxRepo.Claim(ctx, ownerID, time.Minute); err != nil || duplicate != nil {
		t.Fatalf("claim before lease expiry: job=%v err=%v", duplicate, err)
	}
	time.Sleep(1200 * time.Millisecond)
	recovered, err := outboxRepo.Claim(ctx, ownerID, time.Minute)
	if err != nil || recovered == nil || recovered.ID != job.ID {
		t.Fatalf("recovery after lease expiry: job=%v err=%v", recovered, err)
	}
	// The old lease must no longer be able to acknowledge the reclaimed row.
	if err := outboxRepo.MarkSent(ctx, ownerID, job.ID, job.LeaseToken); !errors.Is(err, outbox.ErrLeaseLost) {
		t.Fatalf("MarkSent with stale lease = %v, expected ErrLeaseLost", err)
	}
	// A retry scheduled far in the future blocks any immediate reclamation.
	if err := outboxRepo.MarkRetry(ctx, ownerID, recovered.ID, recovered.LeaseToken, time.Hour, "timeout"); err != nil {
		t.Fatal(err)
	}
	if early, err := outboxRepo.Claim(ctx, ownerID, time.Minute); err != nil || early != nil {
		t.Fatalf("claim before next_attempt_at: job=%v err=%v", early, err)
	}

	permanent := record
	permanent.MessageID = 93
	if err := repo.Save(ctx, ownerID, permanent, false); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.MarkDeleted(ctx, ownerID, 900001, permanent.BusinessConnectionID, permanent.ChatID, []int64{permanent.MessageID}); err != nil {
		t.Fatal(err)
	}
	failedJob, err := outboxRepo.Claim(ctx, ownerID, time.Minute)
	if err != nil || failedJob == nil {
		t.Fatalf("claim before permanent failure: job=%v err=%v", failedJob, err)
	}
	if err := outboxRepo.MarkFailed(ctx, ownerID, failedJob.ID, failedJob.LeaseToken, "telegram_400"); err != nil {
		t.Fatal(err)
	}
	if failedAgain, err := outboxRepo.Claim(ctx, ownerID, time.Minute); err != nil || failedAgain != nil {
		t.Fatalf("a failed job was reclaimed: job=%v err=%v", failedAgain, err)
	}

	if err := db.InTenant(ctx, ownerID+1, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM notification_outbox`).Scan(&count)
	}); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("RLS exposes %d rows to another tenant", count)
	}

	ownerDSN := os.Getenv("OUTBOX_TEST_MIGRATION_DATABASE_URL")
	if ownerDSN == "" {
		t.Skip("OUTBOX_TEST_MIGRATION_DATABASE_URL not set for the atomic rollback proof")
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
		t.Fatal("the marking should have failed with the outbox insert")
	}
	var deleted bool
	if err := db.InTenant(ctx, ownerID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT deleted_at IS NOT NULL FROM messages WHERE message_id = $1`, failing.MessageID).Scan(&deleted)
	}); err != nil {
		t.Fatal(err)
	}
	if deleted {
		t.Fatal("deleted_at was committed despite the outbox failure")
	}
}

func TestPostgresRuntimeRoleCanUseOutboxTableAndSequence(t *testing.T) {
	ctx, db := openRuntimeDB(t)
	ownerID := createTestOwner(t, ctx, db, 900010)
	if err := db.InTenant(ctx, ownerID, func(tx pgx.Tx) error {
		var id int64
		return tx.QueryRow(ctx, `
			INSERT INTO notification_outbox (
				owner_user_id, owner_telegram_user_id, business_connection_id,
				chat_id, message_id, event_type, chunk_index, payload_text
			) VALUES ($1, 900010, 'bc-grant', 1, 1, 'deleted_message', 0, 'payload')
			RETURNING id
		`, ownerID).Scan(&id)
	}); err != nil {
		t.Fatalf("the runtime role cannot insert via the outbox table/sequence: %v", err)
	}
}

func TestPostgresClaimPreservesChunkOrderAcrossRetry(t *testing.T) {
	ctx, db := openRuntimeDB(t)
	ownerID := createTestOwner(t, ctx, db, 900011)
	if err := db.InTenant(ctx, ownerID, func(tx pgx.Tx) error {
		for chunk := 0; chunk < 2; chunk++ {
			if err := outbox.InsertTx(ctx, tx, ownerID, 900011, "bc-order", 2, 2, outbox.EventDeletedMessage, chunk, "payload"); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	repo := outbox.NewRepository(db)
	first, err := repo.Claim(ctx, ownerID, time.Minute)
	if err != nil || first == nil {
		t.Fatalf("claim chunk 0: job=%v err=%v", first, err)
	}
	if err := repo.MarkRetry(ctx, ownerID, first.ID, first.LeaseToken, time.Hour, "timeout"); err != nil {
		t.Fatal(err)
	}
	blocked, err := repo.Claim(ctx, ownerID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if blocked != nil {
		t.Fatalf("next chunk claimed before the first one's retry: %+v", blocked)
	}
	// The retry is real: next_attempt_at is pulled into the past on the server
	// side rather than advancing a Go clock, which no longer has any effect.
	if err := db.InTenant(ctx, ownerID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE notification_outbox SET next_attempt_at = clock_timestamp() WHERE id = $1`, first.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	retried, err := repo.Claim(ctx, ownerID, time.Minute)
	if err != nil || retried == nil || retried.ID != first.ID {
		t.Fatalf("retry chunk 0: job=%v err=%v", retried, err)
	}
	if err := repo.MarkSent(ctx, ownerID, retried.ID, retried.LeaseToken); err != nil {
		t.Fatal(err)
	}
	next, err := repo.Claim(ctx, ownerID, time.Minute)
	if err != nil || next == nil || next.ID == first.ID {
		t.Fatalf("claim chunk 1 after chunk 0 sent: job=%v err=%v", next, err)
	}
}

func TestPostgresPurgeExpiredIsTenantScopedAndKeepsActiveJobs(t *testing.T) {
	ctx, db := openRuntimeDB(t)
	ownerA := createTestOwner(t, ctx, db, 900012)
	ownerB := createTestOwner(t, ctx, db, 900013)
	insert := func(ownerID, telegramID int64, status string, ageDays int, messageID int64) {
		t.Helper()
		if err := db.InTenant(ctx, ownerID, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				INSERT INTO notification_outbox (
					owner_user_id, owner_telegram_user_id, business_connection_id,
					chat_id, message_id, event_type, chunk_index, payload_text, status, created_at
				) VALUES ($1, $2, 'bc-purge', 3, $3, 'deleted_message', 0, 'private', $4, now() - make_interval(days => $5))
			`, ownerID, telegramID, messageID, status, ageDays)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	insert(ownerA, 900012, "sent", 8, 1)
	insert(ownerA, 900012, "failed", 8, 2)
	insert(ownerA, 900012, "pending", 8, 3)
	insert(ownerA, 900012, "sent", 1, 4)
	insert(ownerB, 900013, "sent", 8, 5)

	repo := outbox.NewRepository(db)
	purged, err := repo.PurgeExpired(ctx, []users.TenantRetention{{OwnerUserID: ownerA, RetentionDays: 7}})
	if err != nil || purged != 2 {
		t.Fatalf("PurgeExpired = (%d, %v), expected (2, nil)", purged, err)
	}
	for ownerID, want := range map[int64]int{ownerA: 2, ownerB: 1} {
		var count int
		if err := db.InTenant(ctx, ownerID, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM notification_outbox`).Scan(&count)
		}); err != nil {
			t.Fatal(err)
		}
		if count != want {
			t.Fatalf("tenant %d: %d jobs remaining, expected %d", ownerID, count, want)
		}
	}
}

func TestPostgresStaleWorkerCannotAcknowledgeReclaimedLease(t *testing.T) {
	ctx, db := openRuntimeDB(t)
	ownerID := createTestOwner(t, ctx, db, 900014)
	if err := db.InTenant(ctx, ownerID, func(tx pgx.Tx) error {
		return outbox.InsertTx(ctx, tx, ownerID, 900014, "bc-fencing", 4, 6, outbox.EventDeletedMessage, 0, "payload")
	}); err != nil {
		t.Fatal(err)
	}

	repo := outbox.NewRepository(db)
	workerA, err := repo.Claim(ctx, ownerID, 20*time.Millisecond)
	if err != nil || workerA == nil || workerA.LeaseToken == "" {
		t.Fatalf("claim A: job=%+v err=%v", workerA, err)
	}
	time.Sleep(50 * time.Millisecond)
	workerB, err := repo.Claim(ctx, ownerID, time.Minute)
	if err != nil || workerB == nil || workerB.ID != workerA.ID || workerB.LeaseToken == workerA.LeaseToken {
		t.Fatalf("reclaim B: A=%+v B=%+v err=%v", workerA, workerB, err)
	}
	if err := repo.MarkSent(ctx, ownerID, workerA.ID, workerA.LeaseToken); !errors.Is(err, outbox.ErrLeaseLost) {
		t.Fatalf("stale A ack = %v, expected ErrLeaseLost", err)
	}
	if err := repo.MarkRetry(ctx, ownerID, workerA.ID, workerA.LeaseToken, 0, "timeout"); !errors.Is(err, outbox.ErrLeaseLost) {
		t.Fatalf("stale A retry = %v, expected ErrLeaseLost", err)
	}
	var status, leaseToken string
	if err := db.InTenant(ctx, ownerID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status, lease_token FROM notification_outbox WHERE id = $1`, workerB.ID).Scan(&status, &leaseToken)
	}); err != nil {
		t.Fatal(err)
	}
	if status != "processing" || leaseToken != workerB.LeaseToken {
		t.Fatalf("B's state overwritten by A: status=%s token=%s", status, leaseToken)
	}
}

// TestPostgresCountBacklogAggregatesAllTenantsDespiteRLS covers the
// undelete_outbox_backlog gauge, and above all the rationale for its
// tenant-by-tenant loop: the same COUNT(*) run on the application pool WITHOUT
// tenant context returns 0 without error (RLS), so a naively written gauge
// would report "no backlog" whatever the real state.
func TestPostgresCountBacklogAggregatesAllTenantsDespiteRLS(t *testing.T) {
	ctx, db := openRuntimeDB(t)
	ownerA := createTestOwner(t, ctx, db, 900015)
	ownerB := createTestOwner(t, ctx, db, 900016)
	insert := func(ownerID, telegramID int64, status string, messageID int64) {
		t.Helper()
		if err := db.InTenant(ctx, ownerID, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				INSERT INTO notification_outbox (
					owner_user_id, owner_telegram_user_id, business_connection_id,
					chat_id, message_id, event_type, chunk_index, payload_text, status
				) VALUES ($1, $2, 'bc-backlog', 5, $3, 'deleted_message', 0, 'private', $4)
			`, ownerID, telegramID, messageID, status)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	insert(ownerA, 900015, "pending", 1)
	insert(ownerA, 900015, "processing", 2)
	insert(ownerA, 900015, "sent", 3)   // delivered: outside backlog
	insert(ownerA, 900015, "failed", 4) // abandoned: outside backlog
	insert(ownerB, 900016, "pending", 5)

	repo := outbox.NewRepository(db)
	tenants := []users.TenantRetention{{OwnerUserID: ownerA, RetentionDays: 7}, {OwnerUserID: ownerB, RetentionDays: 7}}
	backlog, err := repo.CountBacklog(ctx, tenants)
	if err != nil {
		t.Fatal(err)
	}
	if backlog != 3 {
		t.Fatalf("CountBacklog = %d, expected 3 (2 for A, 1 for B)", backlog)
	}

	// Only one tenant requested: the gauge only aggregates what it is given.
	if backlog, err := repo.CountBacklog(ctx, tenants[1:]); err != nil || backlog != 1 {
		t.Fatalf("CountBacklog(B alone) = (%d, %v), expected (1, nil)", backlog, err)
	}

	var naive int64
	if err := db.Pool.QueryRow(ctx, `
		SELECT count(*) FROM notification_outbox WHERE status IN ('pending', 'processing')
	`).Scan(&naive); err != nil {
		t.Fatal(err)
	}
	if naive != 0 {
		t.Fatalf("COUNT(*) outside InTenant = %d, expected 0: RLS no longer hides rows, CountBacklog's tenant-by-tenant loop would lose its justification", naive)
	}
}
