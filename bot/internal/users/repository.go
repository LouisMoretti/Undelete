// Package users manages the users table: queried directly via the
// application pool, WITHOUT going through storage.DB.InTenant. Unlike
// messages, users is not protected by RLS: it is the root table that
// establishes tenant identity itself (you cannot set
// app.current_owner_user_id before knowing, precisely, which owner_user_id
// corresponds to a telegram_user_id).
package users

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Repository provides access to the users table.
type Repository struct {
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

// User reflects a row of the users table.
type User struct {
	ID             int64
	TelegramUserID int64
	RetentionDays  int
}

// TenantRetention is the minimal projection needed for the retention purge
// (messages.Repository.PurgeExpired).
type TenantRetention struct {
	OwnerUserID   int64
	RetentionDays int
}

// UpsertByTelegramID creates the user if it does not exist yet, or returns
// the existing row otherwise (idempotent: a Business connection may be
// notified several times by Telegram for the same account holder).
func (r *Repository) UpsertByTelegramID(ctx context.Context, telegramUserID int64) (*User, error) {
	var u User
	err := r.pool.QueryRow(ctx, `
		INSERT INTO users (telegram_user_id)
		VALUES ($1)
		ON CONFLICT (telegram_user_id) DO UPDATE SET telegram_user_id = EXCLUDED.telegram_user_id
		RETURNING id, telegram_user_id, retention_days
	`, telegramUserID).Scan(&u.ID, &u.TelegramUserID, &u.RetentionDays)
	if err != nil {
		return nil, fmt.Errorf("upsert user %d: %w", telegramUserID, err)
	}
	return &u, nil
}

// ListTenantsForRetention returns all tenants with their retention period,
// used by messages.Repository.PurgeExpired to loop tenant by tenant (never a
// global DELETE, cf. constraint #4).
func (r *Repository) ListTenantsForRetention(ctx context.Context) ([]TenantRetention, error) {
	rows, err := r.pool.Query(ctx, `SELECT id, retention_days FROM users`)
	if err != nil {
		return nil, fmt.Errorf("listing tenants: %w", err)
	}
	defer rows.Close()

	var tenants []TenantRetention
	for rows.Next() {
		var t TenantRetention
		if err := rows.Scan(&t.OwnerUserID, &t.RetentionDays); err != nil {
			return nil, fmt.Errorf("reading tenant: %w", err)
		}
		tenants = append(tenants, t)
	}
	return tenants, rows.Err()
}
