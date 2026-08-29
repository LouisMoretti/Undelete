// Package users gère la table users : requêtée directement via le pool
// applicatif, SANS passer par storage.DB.InTenant. Contrairement à
// messages, users n'est pas protégée par RLS : c'est la table racine qui
// établit l'identité tenant elle-même (on ne peut pas poser
// app.current_owner_user_id avant de savoir, justement, quel est
// l'owner_user_id correspondant à un telegram_user_id).
package users

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Repository donne accès à la table users.
type Repository struct {
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

// User reflète une ligne de la table users.
type User struct {
	ID             int64
	TelegramUserID int64
	RetentionDays  int
}

// TenantRetention est la projection minimale nécessaire à la purge de
// rétention (messages.Repository.PurgeExpired).
type TenantRetention struct {
	OwnerUserID   int64
	RetentionDays int
}

// UpsertByTelegramID crée l'utilisateur s'il n'existe pas encore, ou renvoie
// la ligne existante sinon (idempotent : une connexion Business peut être
// notifiée plusieurs fois par Telegram pour le même titulaire).
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

// ListTenantsForRetention renvoie tous les tenants avec leur durée de
// rétention, utilisée par messages.Repository.PurgeExpired pour boucler
// tenant par tenant (jamais un DELETE global, cf. contrainte n°4).
func (r *Repository) ListTenantsForRetention(ctx context.Context) ([]TenantRetention, error) {
	rows, err := r.pool.Query(ctx, `SELECT id, retention_days FROM users`)
	if err != nil {
		return nil, fmt.Errorf("liste des tenants: %w", err)
	}
	defer rows.Close()

	var tenants []TenantRetention
	for rows.Next() {
		var t TenantRetention
		if err := rows.Scan(&t.OwnerUserID, &t.RetentionDays); err != nil {
			return nil, fmt.Errorf("lecture tenant: %w", err)
		}
		tenants = append(tenants, t)
	}
	return tenants, rows.Err()
}
