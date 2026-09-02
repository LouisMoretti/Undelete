// Package business handles the resolution of Telegram Business connections
// and the business_connections table.
//
// business_connections is NOT protected by RLS (see the comment in
// storage/migrations/0001_init.sql): this table is queried by connection id,
// before even knowing the owner_user_id that would allow setting the RLS
// context. That is why this package queries the application pool directly
// rather than going through storage.DB.InTenant.
package business

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/LouisMoretti/Undelete/bot/internal/telegram"
	"github.com/LouisMoretti/Undelete/bot/internal/users"
)

// Connection is the minimal projection needed for resolution: which owner
// this connection belongs to, and whether it is active.
type Connection struct {
	ID          string
	OwnerUserID int64 // internal id (users.id), used as the RLS key
	// OwnerTelegramUserID is the Telegram identifier of the owner, distinct
	// from OwnerUserID (internal primary key). THIS is the identifier to use
	// as chat_id to reach the owner in a direct message (cf. constraint #7 in
	// app/handler.go): owner_user_id corresponds to nothing on the Telegram
	// side.
	OwnerTelegramUserID int64
	CanReply            bool
	IsEnabled           bool
}

// ErrOwnerMismatch signals a Business connection rejected by the mono-tenant
// guardrail (OWNER_TELEGRAM_USER_ID).
var ErrOwnerMismatch = errors.New("business: telegram_user_id does not match OWNER_TELEGRAM_USER_ID")

// Service resolves Business connections through a three-level chain:
// in-memory cache -> database -> Telegram API (getBusinessConnection).
//
// The third level (API) is essential, not a mere optimization: if the bot
// restarts, it loses its in-memory cache; if it receives a business_message
// for a connection established WHILE it was offline, that connection is
// neither in cache (lost on restart) nor in the database (the corresponding
// business_connection update was never seen, for example if the update
// history expired on the Telegram side before the restart). Without this API
// call, the bot would silently ignore messages that are nonetheless covered
// by a very real Business connection.
type Service struct {
	pool   *pgxpool.Pool
	client *telegram.Client
	users  *users.Repository
	logger *slog.Logger

	ownerFilter int64 // OWNER_TELEGRAM_USER_ID; 0 = no restriction

	mu    sync.RWMutex
	cache map[string]Connection
}

func NewService(pool *pgxpool.Pool, client *telegram.Client, usersRepo *users.Repository, ownerFilter int64, logger *slog.Logger) *Service {
	return &Service{
		pool:        pool,
		client:      client,
		users:       usersRepo,
		ownerFilter: ownerFilter,
		logger:      logger,
		cache:       make(map[string]Connection),
	}
}

// Resolve returns the Connection associated with connectionID, trying in
// order: in-memory cache, database, Telegram API. A connection found via the
// API is upserted to the database and stored in cache before being returned.
func (s *Service) Resolve(ctx context.Context, connectionID string) (*Connection, error) {
	s.mu.RLock()
	if c, ok := s.cache[connectionID]; ok {
		s.mu.RUnlock()
		if !s.ownerAllowed(c.OwnerTelegramUserID) {
			return nil, ErrOwnerMismatch
		}
		return &c, nil
	}
	s.mu.RUnlock()

	conn, err := s.getFromDB(ctx, connectionID)
	if err == nil {
		// The filter must also apply to historical data: a foreign connection
		// created before the guardrail was enabled must not become authorized
		// again via the cache or the database after a restart.
		if !s.ownerAllowed(conn.OwnerTelegramUserID) {
			return nil, ErrOwnerMismatch
		}
		s.storeInCache(*conn)
		return conn, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("reading business_connections %s: %w", connectionID, err)
	}

	// Neither in cache nor in the database: last resort, the Telegram API (cf.
	// Service comment).
	apiConn, err := s.client.GetBusinessConnection(ctx, connectionID)
	if err != nil {
		return nil, fmt.Errorf("getBusinessConnection %s: %w", connectionID, err)
	}

	resolved, err := s.upsertFromTelegram(ctx, *apiConn)
	if err != nil {
		return nil, err
	}
	if resolved == nil {
		// Rejected by the mono-tenant guardrail: no cache/DB entry.
		return nil, ErrOwnerMismatch
	}
	s.storeInCache(*resolved)
	return resolved, nil
}

func (s *Service) ownerAllowed(telegramUserID int64) bool {
	return s.ownerFilter == 0 || telegramUserID == s.ownerFilter
}

func (s *Service) storeInCache(c Connection) {
	s.mu.Lock()
	s.cache[c.ID] = c
	s.mu.Unlock()
}

func (s *Service) getFromDB(ctx context.Context, connectionID string) (*Connection, error) {
	var c Connection
	// JOIN with users: business_connections only stores the internal id
	// (owner_user_id); the telegram_user_id is needed to be able to notify
	// the owner (see the comment on Connection.OwnerTelegramUserID).
	err := s.pool.QueryRow(ctx, `
		SELECT bc.id, bc.owner_user_id, u.telegram_user_id, bc.can_reply, bc.is_enabled
		FROM business_connections bc
		JOIN users u ON u.id = bc.owner_user_id
		WHERE bc.id = $1
	`, connectionID).Scan(&c.ID, &c.OwnerUserID, &c.OwnerTelegramUserID, &c.CanReply, &c.IsEnabled)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// HandleBusinessConnection processes the business_connection update: upsert
// user + connection, welcome message to the owner. Applies the mono-tenant
// guardrail if OWNER_TELEGRAM_USER_ID is set.
func (s *Service) HandleBusinessConnection(ctx context.Context, tc telegram.BusinessConnection) error {
	resolved, err := s.upsertFromTelegram(ctx, tc)
	if err != nil {
		return err
	}
	if resolved == nil {
		s.logger.Warn("business connection refused: mono-tenant guardrail",
			slog.String("business_connection_id", tc.ID))
		return nil // silent refusal on the Telegram side: not a processing error
	}

	s.storeInCache(*resolved)

	if !resolved.IsEnabled {
		s.logger.Info("business connection disabled",
			slog.String("business_connection_id", resolved.ID),
			slog.Int64("owner_user_id", resolved.OwnerUserID))
		return nil
	}

	s.logger.Info("business connection established",
		slog.String("business_connection_id", resolved.ID),
		slog.Int64("owner_user_id", resolved.OwnerUserID),
		slog.Bool("can_reply", resolved.CanReply),
		slog.Bool("is_enabled", resolved.IsEnabled))

	s.notifyWelcome(ctx, tc)

	return nil
}

// notifyWelcome sends the welcome alert to the owner.
//
// Constraint #7: never a BusinessConnectionID here, lest this message be sent
// AS the owner in a monitored conversation. Failure is logged without
// interrupting processing: the connection is already persisted, losing a
// welcome message must not replay the update.
//
// These three lines are isolated so the wire contract of the alert is
// testable on the production path itself, without a database (cf.
// TestWelcomeAlertContract).
func (s *Service) notifyWelcome(ctx context.Context, tc telegram.BusinessConnection) {
	if err := s.client.SendMessage(ctx, telegram.BuildWelcomeMessageRequest(tc.UserChatID, tc.User.ID)); err != nil {
		s.logger.Error("failed to send welcome message", slog.String("error", err.Error()))
	}
}

// upsertFromTelegram upserts user + business_connections from a Telegram
// BusinessConnection. Returns (nil, nil) if the mono-tenant guardrail rejects
// the connection (not an error, a business refusal).
func (s *Service) upsertFromTelegram(ctx context.Context, tc telegram.BusinessConnection) (*Connection, error) {
	if !s.ownerAllowed(tc.User.ID) {
		return nil, nil
	}

	u, err := s.users.UpsertByTelegramID(ctx, tc.User.ID)
	if err != nil {
		return nil, fmt.Errorf("upsert owner: %w", err)
	}

	c := Connection{
		ID:                  tc.ID,
		OwnerUserID:         u.ID,
		OwnerTelegramUserID: tc.User.ID,
		CanReply:            tc.CanReply(),
		IsEnabled:           tc.IsEnabled,
	}

	_, err = s.pool.Exec(ctx, `
		INSERT INTO business_connections (id, owner_user_id, can_reply, is_enabled, updated_at)
		VALUES ($1, $2, $3, $4, now())
		ON CONFLICT (id) DO UPDATE SET
			owner_user_id = EXCLUDED.owner_user_id,
			can_reply     = EXCLUDED.can_reply,
			is_enabled    = EXCLUDED.is_enabled,
			updated_at    = now()
	`, c.ID, c.OwnerUserID, c.CanReply, c.IsEnabled)
	if err != nil {
		return nil, fmt.Errorf("upsert business_connections %s: %w", c.ID, err)
	}

	return &c, nil
}
