package erasure

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/LouisMoretti/Undelete/bot/internal/storage"
)

// Repository stores the challenges in data_erasure_requests (migration 0006),
// which is under FORCE ROW LEVEL SECURITY: every query here goes through
// storage.DB.InTenant, like every other tenant table of this codebase. A code
// therefore cannot even be looked up outside the tenant it was issued to --
// the cross-tenant guarantee is enforced by the database, not by the WHERE
// clause alone.
type Repository struct {
	db *storage.DB
}

func NewRepository(db *storage.DB) *Repository { return &Repository{db: db} }

// Issue replaces the tenant's pending code with a new one.
//
// One statement, arbitrated by the partial unique index of migration 0008 (one
// 'pending' row per owner): the tenant never has two live codes, not even when
// two Requests race from two chats -- the second one waits on the first one's
// row and overwrites it, so the last code handed out is the only one
// spendable. A clear-then-insert pair could not promise that under READ
// COMMITTED: each transaction clears a snapshot without the other's row.
//
// Only the 'pending' row is replaced. A 'consumed' row is an erasure that
// started and may still need resuming, and a 'completed' one is the receipt a
// replayed confirmation is answered from; neither is matched by the index.
//
// expires_at is computed by PostgreSQL (now() + interval), not by the bot, so
// the deadline and the comparison that later enforces it are read off the same
// clock.
func (r *Repository) Issue(ctx context.Context, t Tenant, codeHash string, ttl time.Duration) (time.Time, error) {
	var expiresAt time.Time
	err := r.db.InTenant(ctx, t.OwnerUserID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO data_erasure_requests (
				owner_user_id, owner_telegram_user_id, business_connection_id,
				code_sha256, expires_at
			)
			VALUES ($1, $2, $3, $4, now() + make_interval(secs => $5))
			ON CONFLICT (owner_user_id) WHERE status = 'pending'
			DO UPDATE SET
				owner_telegram_user_id = EXCLUDED.owner_telegram_user_id,
				business_connection_id = EXCLUDED.business_connection_id,
				code_sha256            = EXCLUDED.code_sha256,
				created_at             = EXCLUDED.created_at,
				expires_at             = EXCLUDED.expires_at
			RETURNING expires_at
		`, t.OwnerUserID, t.OwnerTelegramUserID, t.BusinessConnectionID,
			codeHash, ttl.Seconds()).Scan(&expiresAt)
	})
	if err != nil {
		return time.Time{}, fmt.Errorf("issuing erasure request: %w", err)
	}
	return expiresAt, nil
}

// Claim spends a code and reports what it found.
//
// The single-use guarantee is the UPDATE itself, not a read followed by a
// write: two submissions of the same code both try to move the same row out of
// 'pending', PostgreSQL serialises them on that row, and the second one finds
// nothing to update. There is no window between checking and spending, because
// there is no check.
//
// The expiry is evaluated by the server (now()), against the expires_at the
// server itself wrote: the bot's own clock takes no part in deciding whether a
// code is still live.
func (r *Repository) Claim(ctx context.Context, ownerUserID int64, codeHash string) (ClaimState, error) {
	state := ClaimUnknown
	err := r.db.InTenant(ctx, ownerUserID, func(tx pgx.Tx) error {
		var id int64
		err := tx.QueryRow(ctx, `
			UPDATE data_erasure_requests
			SET status = 'consumed', consumed_at = COALESCE(consumed_at, now())
			WHERE owner_user_id = $1
			  AND code_sha256 = $2
			  AND status = 'pending'
			  AND expires_at > now()
			RETURNING id
		`, ownerUserID, codeHash).Scan(&id)
		if err == nil {
			state = ClaimGranted
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("claiming erasure request: %w", err)
		}

		// Nothing was spendable. The row still has to be classified, because
		// "expired", "already running" and "never existed" are three different
		// answers to the owner.
		var status string
		if err := tx.QueryRow(ctx, `
			SELECT status FROM data_erasure_requests
			WHERE owner_user_id = $1 AND code_sha256 = $2
		`, ownerUserID, codeHash).Scan(&status); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				state = ClaimUnknown
				return nil
			}
			return fmt.Errorf("reading erasure request: %w", err)
		}
		switch status {
		case "pending":
			// Still pending yet not claimable: the only remaining reason is
			// that the window closed.
			state = ClaimExpired
		case "consumed":
			state = ClaimResumable
		case "completed":
			state = ClaimCompleted
		}
		return nil
	})
	if err != nil {
		return ClaimUnknown, err
	}
	return state, nil
}

// Complete marks a consumed request completed. Restricted to 'consumed' so it
// can never resurrect a row the erasure already deleted, and COALESCE keeps the
// first completion timestamp when a replay reaches it.
//
// The same statement scrubs the identifying columns the Issue wrote
// (owner_telegram_user_id, business_connection_id): nothing reads them back
// from a completed row -- the replay is answered from the Business connection
// the message arrived through, never from this receipt -- so the row the
// erasure deliberately keeps holds the tenant key, the code hash and the
// timestamps, and no Telegram identifier (migration 0007 relaxed the two
// columns to nullable for exactly this). Pending and consumed rows keep their
// values; only the completed receipt is minimised.
func (r *Repository) Complete(ctx context.Context, ownerUserID int64, codeHash string) error {
	err := r.db.InTenant(ctx, ownerUserID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE data_erasure_requests
			SET status = 'completed',
			    completed_at = COALESCE(completed_at, now()),
			    owner_telegram_user_id = NULL,
			    business_connection_id = NULL
			WHERE owner_user_id = $1 AND code_sha256 = $2 AND status = 'consumed'
		`, ownerUserID, codeHash)
		return err
	})
	if err != nil {
		return fmt.Errorf("completing erasure request: %w", err)
	}
	return nil
}

// DeleteOthers removes every erasure request of the tenant but the one being
// spent. The kept row is the replay receipt: a tenant key, a code hash and
// three timestamps, its Telegram and connection identifiers scrubbed on
// completion (see Complete). Without it a replayed confirmation would read
// as an unknown code.
func (r *Repository) DeleteOthers(ctx context.Context, ownerUserID int64, keepHash string) (int64, error) {
	var deleted int64
	err := r.db.InTenant(ctx, ownerUserID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			DELETE FROM data_erasure_requests
			WHERE owner_user_id = $1 AND code_sha256 <> $2
		`, ownerUserID, keepHash)
		if err != nil {
			return err
		}
		deleted = tag.RowsAffected()
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("deleting erasure requests of tenant %d: %w", ownerUserID, err)
	}
	return deleted, nil
}
