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
	"strconv"
	"strings"

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

// Retention bounds, in days. They mirror the CHECK constraint on
// users.retention_days (migration 0001): the Go validation rejects invalid
// values with an answerable error, the constraint stays as the backstop for
// any path that bypasses it.
const (
	MinRetentionDays     = 1
	MaxRetentionDays     = 365
	DefaultRetentionDays = 7
)

// ParseRetentionDays parses the argument of /retention into a number of days.
//
// Exactly one integer token between MinRetentionDays and MaxRetentionDays
// (both inclusive) is accepted. Anything else -- empty, non-numeric, out of
// bounds, or trailed by extra tokens -- is an error and the caller must change
// nothing. Pure so the command can be exercised without a database.
func ParseRetentionDays(argument string) (int, error) {
	fields := strings.Fields(argument)
	if len(fields) != 1 {
		return 0, fmt.Errorf("expected a single number of days between %d and %d, got %q", MinRetentionDays, MaxRetentionDays, argument)
	}
	days, err := strconv.Atoi(fields[0])
	if err != nil {
		return 0, fmt.Errorf("expected a single number of days between %d and %d, got %q", MinRetentionDays, MaxRetentionDays, argument)
	}
	if days < MinRetentionDays || days > MaxRetentionDays {
		return 0, fmt.Errorf("retention %d out of bounds: expected between %d and %d days", days, MinRetentionDays, MaxRetentionDays)
	}
	return days, nil
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

// GetRetentionDays returns the retention period of one tenant, in days. It is
// the read half of /retention: the caller resolved the tenant from the
// Business connection first and never derives the id from anything else.
func (r *Repository) GetRetentionDays(ctx context.Context, ownerUserID int64) (int, error) {
	var days int
	err := r.pool.QueryRow(ctx, `SELECT retention_days FROM users WHERE id = $1`, ownerUserID).Scan(&days)
	if err != nil {
		return 0, fmt.Errorf("reading retention of tenant %d: %w", ownerUserID, err)
	}
	return days, nil
}

// SetRetentionDays sets the retention period of one tenant, in days. It is the
// write half of /retention: the value is validated in Go first (so the caller
// can answer the owner instead of failing), the CHECK constraint stays as the
// backstop. There is deliberately no per-chat variant: retention is a property
// of the tenant, never of a conversation.
func (r *Repository) SetRetentionDays(ctx context.Context, ownerUserID int64, days int) error {
	if days < MinRetentionDays || days > MaxRetentionDays {
		return fmt.Errorf("retention %d out of bounds: expected between %d and %d days", days, MinRetentionDays, MaxRetentionDays)
	}
	tag, err := r.pool.Exec(ctx, `UPDATE users SET retention_days = $1 WHERE id = $2`, days, ownerUserID)
	if err != nil {
		return fmt.Errorf("setting retention of tenant %d: %w", ownerUserID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("setting retention of tenant %d: no such tenant", ownerUserID)
	}
	return nil
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
