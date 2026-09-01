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

var ErrLeaseLost = errors.New("lease outbox perdu")

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

// Claim réserve au plus un job du tenant. Toutes les dates (échéance de
// retry, expiration de lease) sont évaluées sur l'horloge du serveur
// PostgreSQL : aucun `now` Go n'entre dans la décision, une dérive entre
// l'horloge du bot et celle de la base ne peut donc ni masquer un job ni le
// rendre réclamable trop tôt.
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
			          o.event_type, o.payload_text, o.attempts, o.lease_token
		`, lease.Seconds(), leaseToken)
		var claimed Job
		if err := row.Scan(&claimed.ID, &claimed.OwnerUserID, &claimed.OwnerTelegramUserID,
			&claimed.BusinessConnectionID, &claimed.ChatID, &claimed.MessageID,
			&claimed.EventType, &claimed.Text, &claimed.Attempts, &claimed.LeaseToken); err != nil {
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

// MarkSent acquitte le job. sent_at et updated_at sont datés par PostgreSQL,
// comme les comparaisons faites par Claim.
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

// MarkRetry replanifie le job dans `wait` : le délai est calculé côté Go
// (backoff, retry_after de Telegram) mais l'échéance absolue est dérivée de
// l'horloge PostgreSQL, celle-là même que Claim compare.
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
				return fmt.Errorf("purge outbox tenant %d: %w", tenant.OwnerUserID, err)
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
		return "", fmt.Errorf("génération token de lease: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

func verifyLeaseUpdate(rowsAffected, id int64, err error) error {
	if err != nil {
		return fmt.Errorf("mise à jour outbox: %w", err)
	}
	if rowsAffected != 1 {
		return fmt.Errorf("%w: id %d", ErrLeaseLost, id)
	}
	return nil
}
