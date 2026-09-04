package outbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/LouisMoretti/Undelete/bot/internal/storage"
	"github.com/LouisMoretti/Undelete/bot/internal/users"
)

var ErrLeaseLost = errors.New("outbox lease lost")

// Repository accesses notification_outbox exclusively via InTenant/RLS.
type Repository struct {
	db *storage.DB
}

func NewRepository(db *storage.DB) *Repository { return &Repository{db: db} }

// InsertTx adds a text chunk in the transaction that marks deleted_at.
func InsertTx(ctx context.Context, tx pgx.Tx, ownerUserID, ownerTelegramUserID int64, businessConnectionID string, chatID, messageID int64, eventType string, chunkIndex int, text string) error {
	return insertTx(ctx, tx, ownerUserID, ownerTelegramUserID, businessConnectionID,
		chatID, messageID, eventType, chunkIndex, text, PayloadKindText, nil)
}

// InsertMediaTx adds the media entry of an alert, in the SAME transaction as
// its text chunks: either the deletion is recorded with everything it takes to
// notify it, or nothing is.
//
// text is the fallback: what the worker sends, followed by
// telegram.MediaUnavailableNote, when the files cannot be delivered. It is
// never sent on the nominal path.
func InsertMediaTx(ctx context.Context, tx pgx.Tx, ownerUserID, ownerTelegramUserID int64, businessConnectionID string, chatID, messageID int64, eventType string, chunkIndex int, text string, payload MediaPayload) error {
	raw, err := encodeMediaPayload(payload)
	if err != nil {
		return err
	}
	return insertTx(ctx, tx, ownerUserID, ownerTelegramUserID, businessConnectionID,
		chatID, messageID, eventType, chunkIndex, text, PayloadKindMedia, raw)
}

func insertTx(ctx context.Context, tx pgx.Tx, ownerUserID, ownerTelegramUserID int64, businessConnectionID string, chatID, messageID int64, eventType string, chunkIndex int, text, payloadKind string, mediaPayload []byte) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO notification_outbox (
			owner_user_id, owner_telegram_user_id, business_connection_id,
			chat_id, message_id, event_type, chunk_index, payload_text,
			payload_kind, media_payload
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (owner_user_id, business_connection_id, chat_id, message_id, event_type, chunk_index)
		DO NOTHING
	`, ownerUserID, ownerTelegramUserID, businessConnectionID, chatID, messageID,
		eventType, chunkIndex, text, payloadKind, mediaPayload)
	if err != nil {
		return fmt.Errorf("outbox insert: %w", err)
	}
	return nil
}

// Claim reserves at most one job for the tenant. All timestamps (retry
// deadline, lease expiry) are evaluated on the PostgreSQL server clock: no Go
// `now` enters the decision, so a drift between the bot clock and the
// database clock can neither hide a job nor make it claimable too early.
func (r *Repository) Claim(ctx context.Context, ownerUserID int64, lease time.Duration) (*Job, error) {
	leaseToken, err := newLeaseToken()
	if err != nil {
		return nil, err
	}
	var job *Job
	err = r.db.InTenant(ctx, ownerUserID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			WITH candidate AS (
				SELECT current_job.id
				FROM notification_outbox current_job
				WHERE current_job.status IN ('pending', 'processing')
				  AND current_job.next_attempt_at <= clock_timestamp()
				  AND (current_job.locked_until IS NULL OR current_job.locked_until <= clock_timestamp())
				  AND NOT EXISTS (
					SELECT 1 FROM notification_outbox prior
					WHERE prior.owner_user_id = current_job.owner_user_id
					  AND prior.business_connection_id = current_job.business_connection_id
					  AND prior.chat_id = current_job.chat_id
					  AND prior.message_id = current_job.message_id
					  AND prior.event_type = current_job.event_type
					  AND prior.chunk_index < current_job.chunk_index
					  AND prior.status NOT IN ('sent', 'failed')
				  )
				ORDER BY current_job.next_attempt_at, current_job.id
				LIMIT 1
				FOR UPDATE SKIP LOCKED
			)
			UPDATE notification_outbox o
			SET status = 'processing',
			    locked_until = clock_timestamp() + make_interval(secs => $1),
			    lease_token = $2, updated_at = clock_timestamp()
			FROM candidate
			WHERE o.id = candidate.id
			RETURNING o.id, o.owner_user_id, o.owner_telegram_user_id,
			          o.business_connection_id, o.chat_id, o.message_id,
			          o.event_type, o.payload_text, o.attempts, o.lease_token,
			          o.payload_kind, o.media_payload
		`, lease.Seconds(), leaseToken)
		var claimed Job
		var mediaPayload []byte
		if err := row.Scan(&claimed.ID, &claimed.OwnerUserID, &claimed.OwnerTelegramUserID,
			&claimed.BusinessConnectionID, &claimed.ChatID, &claimed.MessageID,
			&claimed.EventType, &claimed.Text, &claimed.Attempts, &claimed.LeaseToken,
			&claimed.PayloadKind, &mediaPayload); err != nil {
			if err == pgx.ErrNoRows {
				return nil
			}
			return fmt.Errorf("outbox claim: %w", err)
		}
		if claimed.PayloadKind == PayloadKindMedia {
			// A payload that cannot be decoded must not strand the alert: the
			// job then simply carries no media, and the worker delivers its
			// fallback text -- the same outcome as a file missing from disk.
			claimed.Media, _ = decodeMediaPayload(mediaPayload)
		}
		job = &claimed
		return nil
	})
	return job, err
}

// MarkSent acknowledges the job. sent_at and updated_at are stamped by
// PostgreSQL, as are the comparisons made by Claim.
func (r *Repository) MarkSent(ctx context.Context, ownerUserID, id int64, leaseToken string) error {
	return r.db.InTenant(ctx, ownerUserID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE notification_outbox
			SET status = 'sent', sent_at = clock_timestamp(), locked_until = NULL,
			    lease_token = NULL, last_error_class = NULL, updated_at = clock_timestamp()
			WHERE id = $1 AND status = 'processing' AND lease_token = $2
		`, id, leaseToken)
		return verifyLeaseUpdate(tag.RowsAffected(), id, err)
	})
}

// MarkRetry reschedules the job within `wait`: the delay is computed on the
// Go side (backoff, Telegram retry_after) but the absolute deadline is derived
// from the PostgreSQL clock, the very one Claim compares against.
func (r *Repository) MarkRetry(ctx context.Context, ownerUserID, id int64, leaseToken string, wait time.Duration, errorClass string) error {
	return r.db.InTenant(ctx, ownerUserID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE notification_outbox
			SET status = 'pending', attempts = attempts + 1,
			    next_attempt_at = clock_timestamp() + make_interval(secs => $3),
			    locked_until = NULL, lease_token = NULL,
			    last_error_class = $4, updated_at = clock_timestamp()
			WHERE id = $1 AND status = 'processing' AND lease_token = $2
		`, id, leaseToken, wait.Seconds(), errorClass)
		return verifyLeaseUpdate(tag.RowsAffected(), id, err)
	})
}

func (r *Repository) MarkFailed(ctx context.Context, ownerUserID, id int64, leaseToken, errorClass string) error {
	return r.db.InTenant(ctx, ownerUserID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE notification_outbox
			SET status = 'failed', attempts = attempts + 1,
			    locked_until = NULL, lease_token = NULL,
			    last_error_class = $3, updated_at = clock_timestamp()
			WHERE id = $1 AND status = 'processing' AND lease_token = $2
		`, id, leaseToken, errorClass)
		return verifyLeaseUpdate(tag.RowsAffected(), id, err)
	})
}

// CountBacklog sums, tenant by tenant, the alerts still waiting to be
// delivered (status pending or processing). It is the source of the
// undelete_outbox_backlog gauge: an aggregated counter with no breakdown by
// tenant, chat or message -- exposing the backlog PER tenant would publish
// each owner's activity on /metrics.
//
// The InTenant loop is not a stylistic detail: notification_outbox has FORCE
// ROW LEVEL SECURITY and the application role does not have BYPASSRLS. A
// single `SELECT count(*) FROM notification_outbox` run on the application
// pool without app.current_owner_user_id set would see NO rows and always
// return 0, without the slightest error -- exactly the kind of falsely
// reassuring metric this issue aims to avoid. So we reuse the PurgeExpired
// pattern, with the tenant context set.
func (r *Repository) CountBacklog(ctx context.Context, tenants []users.TenantRetention) (int64, error) {
	var total int64
	for _, tenant := range tenants {
		err := r.db.InTenant(ctx, tenant.OwnerUserID, func(tx pgx.Tx) error {
			var count int64
			if err := tx.QueryRow(ctx, `
				SELECT count(*) FROM notification_outbox
				WHERE owner_user_id = $1 AND status IN ('pending', 'processing')
			`, tenant.OwnerUserID).Scan(&count); err != nil {
				return fmt.Errorf("outbox backlog count for tenant %d: %w", tenant.OwnerUserID, err)
			}
			total += count
			return nil
		})
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

func (r *Repository) PurgeExpired(ctx context.Context, tenants []users.TenantRetention) (int64, error) {
	var total int64
	for _, tenant := range tenants {
		err := r.db.InTenant(ctx, tenant.OwnerUserID, func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `
				DELETE FROM notification_outbox
				WHERE owner_user_id = $1
				  AND status IN ('sent', 'failed')
				  AND created_at < clock_timestamp() - make_interval(days => $2)
			`, tenant.OwnerUserID, tenant.RetentionDays)
			if err != nil {
				return fmt.Errorf("outbox purge for tenant %d: %w", tenant.OwnerUserID, err)
			}
			total += tag.RowsAffected()
			return nil
		})
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

func newLeaseToken() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("lease token generation: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

func verifyLeaseUpdate(rowsAffected, id int64, err error) error {
	if err != nil {
		return fmt.Errorf("outbox update: %w", err)
	}
	if rowsAffected != 1 {
		return fmt.Errorf("%w: id %d", ErrLeaseLost, id)
	}
	return nil
}
