package outbox

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/LouisMoretti/Undelete/bot/internal/storage"
)

// Repository accède à notification_outbox exclusivement via InTenant/RLS.
type Repository struct {
	db *storage.DB
}

func NewRepository(db *storage.DB) *Repository { return &Repository{db: db} }

// InsertTx ajoute un chunk dans la transaction qui marque deleted_at.
func InsertTx(ctx context.Context, tx pgx.Tx, ownerUserID, ownerTelegramUserID int64, businessConnectionID string, chatID, messageID int64, eventType string, chunkIndex int, text string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO notification_outbox (
			owner_user_id, owner_telegram_user_id, business_connection_id,
			chat_id, message_id, event_type, chunk_index, payload_text
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (owner_user_id, business_connection_id, chat_id, message_id, event_type, chunk_index)
		DO NOTHING
	`, ownerUserID, ownerTelegramUserID, businessConnectionID, chatID, messageID, eventType, chunkIndex, text)
	if err != nil {
		return fmt.Errorf("insertion outbox: %w", err)
	}
	return nil
}

func (r *Repository) Claim(ctx context.Context, ownerUserID int64, now time.Time, lease time.Duration) (*Job, error) {
	var job *Job
	err := r.db.InTenant(ctx, ownerUserID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			WITH candidate AS (
				SELECT id
				FROM notification_outbox
				WHERE status IN ('pending', 'processing')
				  AND next_attempt_at <= $1
				  AND (locked_until IS NULL OR locked_until <= $1)
				ORDER BY next_attempt_at, id
				LIMIT 1
				FOR UPDATE SKIP LOCKED
			)
			UPDATE notification_outbox o
			SET status = 'processing', locked_until = $1 + make_interval(secs => $2), updated_at = $1
			FROM candidate
			WHERE o.id = candidate.id
			RETURNING o.id, o.owner_user_id, o.owner_telegram_user_id,
			          o.business_connection_id, o.chat_id, o.message_id,
			          o.event_type, o.payload_text, o.attempts
		`, now, lease.Seconds())
		var claimed Job
		if err := row.Scan(&claimed.ID, &claimed.OwnerUserID, &claimed.OwnerTelegramUserID,
			&claimed.BusinessConnectionID, &claimed.ChatID, &claimed.MessageID,
			&claimed.EventType, &claimed.Text, &claimed.Attempts); err != nil {
			if err == pgx.ErrNoRows {
				return nil
			}
			return fmt.Errorf("claim outbox: %w", err)
		}
		job = &claimed
		return nil
	})
	return job, err
}

func (r *Repository) MarkSent(ctx context.Context, ownerUserID, id int64, now time.Time) error {
	return r.db.InTenant(ctx, ownerUserID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE notification_outbox
			SET status = 'sent', sent_at = $2, locked_until = NULL,
			    last_error_class = NULL, updated_at = $2
			WHERE id = $1
		`, id, now)
		return verifyUpdate(tag.RowsAffected(), id, err)
	})
}

func (r *Repository) MarkRetry(ctx context.Context, ownerUserID, id int64, next time.Time, errorClass string) error {
	return r.db.InTenant(ctx, ownerUserID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE notification_outbox
			SET status = 'pending', attempts = attempts + 1,
			    next_attempt_at = $2, locked_until = NULL,
			    last_error_class = $3, updated_at = now()
			WHERE id = $1
		`, id, next, errorClass)
		return verifyUpdate(tag.RowsAffected(), id, err)
	})
}

func (r *Repository) MarkFailed(ctx context.Context, ownerUserID, id int64, errorClass string) error {
	return r.db.InTenant(ctx, ownerUserID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE notification_outbox
			SET status = 'failed', attempts = attempts + 1,
			    locked_until = NULL, last_error_class = $2, updated_at = now()
			WHERE id = $1
		`, id, errorClass)
		return verifyUpdate(tag.RowsAffected(), id, err)
	})
}

func verifyUpdate(rowsAffected, id int64, err error) error {
	if err != nil {
		return fmt.Errorf("mise à jour outbox: %w", err)
	}
	if rowsAffected != 1 {
		return fmt.Errorf("mise à jour outbox: id %d introuvable", id)
	}
	return nil
}
