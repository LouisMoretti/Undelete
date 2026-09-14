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
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

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

// ErrOwnerNotAllowed signals a Business connection whose account holder is not
// in the onboarding allowlist (OWNER_ALLOWLIST_TELEGRAM_USER_IDS). An empty
// allowlist is open onboarding and never produces this error.
var ErrOwnerNotAllowed = errors.New("business: telegram_user_id is not in the owner allowlist")

// ErrConnectionUnknown signals a connection id Telegram does not recognise --
// the state a connection reaches once the account holder removes the bot from
// their Business account. It is a refusal, not a failure: the updates Telegram
// still has buffered for that connection are dropped, and nothing is captured
// through it ever again.
var ErrConnectionUnknown = errors.New("business: telegram does not recognise this business connection")

// ErrConnectionOwnerConflict signals an update that would move an existing
// Business connection to a different owner.
//
// Telegram never reuses a connection id across account holders, so this cannot
// happen in normal operation; if it ever does, it is either a Bot API
// contract we misread or an attempt to have one tenant's connection start
// feeding another tenant's rows. Either way the answer is the same: refuse the
// write, keep the row as it is, and say so loudly.
var ErrConnectionOwnerConflict = errors.New("business: business connection already belongs to another owner")

// pool is the minimal database surface the service needs. The resolution
// table lives outside RLS and is queried directly, but an interface -- not
// *pgxpool.Pool -- keeps the resolution chain unit-testable with an
// in-memory fake instead of a real PostgreSQL (same pattern as the consumer
// interfaces in app, outbox and fetch).
type pool interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

// userStore upserts owners. Narrow interface for the same testability reason:
// resolving a connection must not require the users table.
type userStore interface {
	UpsertByTelegramID(context.Context, int64) (*users.User, error)
}

// connectionAPI is the Telegram surface the service needs: resolving a
// connection the database never saw, and welcoming a new one.
type connectionAPI interface {
	GetBusinessConnection(context.Context, string) (*telegram.BusinessConnection, error)
	SendMessage(context.Context, telegram.SendMessageRequest) error
}

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
	pool   pool
	client connectionAPI
	users  userStore
	logger *slog.Logger

	// allowed is the onboarding allowlist, keyed by Telegram user id. An
	// EMPTY (or nil) allowlist is open onboarding: any Telegram Business
	// account holder may connect the bot and becomes a tenant of their own.
	// A non-empty one admits exactly those holders, which is how a deployment
	// that used to be mono-tenant keeps the guarantee it had.
	//
	// The allowlist is about ADMISSION, never about isolation: whatever it
	// contains, every tenant's data stays behind the same RLS policies, keyed
	// on the owner_user_id resolved from this table.
	allowed map[int64]struct{}

	cache *connectionCache
	// welcomeTimeout bounds the welcome message send (cf. notifyWelcome).
	// Overridable in tests only; production uses defaultWelcomeTimeout.
	welcomeTimeout time.Duration
}

// welcomeTimeout bounds the welcome message send: without it one welcome
// could park its shard partition for the whole SendMessage retry budget
// (three attempts honouring up to a minute of 429 wait each), delaying the
// offset advancement of its batch. Thirty seconds tolerates a slow send
// while keeping the partition stall within the handler ceilings the
// slow-shard strategy documents. A lost welcome is benign: the connection is
// already persisted, and the failure is logged, never replayed.
const defaultWelcomeTimeout = 30 * time.Second

// Option configures a Service. Used for what only tests need to steer (the
// cache clock), so production call sites stay a single constructor call.
type Option func(*Service)

// WithClock replaces the clock the connection cache ages its entries on.
// Tests use it to cross the TTL without sleeping; production uses time.Now.
func WithClock(now func() time.Time) Option {
	return func(s *Service) { s.cache.now = now }
}

// WithWelcomeTimeout replaces the bound on the welcome message send. Tests
// use it to prove the bound without waiting out the production value;
// production never uses this option.
func WithWelcomeTimeout(d time.Duration) Option {
	return func(s *Service) { s.welcomeTimeout = d }
}

// NewService builds the resolution service. allowedOwnerTelegramUserIDs is the
// onboarding allowlist (empty = open onboarding, cf. Service.allowed).
func NewService(pool pool, client connectionAPI, usersRepo userStore, allowedOwnerTelegramUserIDs []int64, logger *slog.Logger, opts ...Option) *Service {
	allowed := make(map[int64]struct{}, len(allowedOwnerTelegramUserIDs))
	for _, id := range allowedOwnerTelegramUserIDs {
		allowed[id] = struct{}{}
	}
	s := &Service{
		pool:           pool,
		client:         client,
		users:          usersRepo,
		allowed:        allowed,
		logger:         logger,
		cache:          newConnectionCache(connectionCacheTTL, connectionCacheMaxEntries, time.Now),
		welcomeTimeout: defaultWelcomeTimeout,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Resolve returns the Connection associated with connectionID, trying in
// order: in-memory cache, database, Telegram API. A connection found via the
// API is upserted to the database and stored in cache before being returned.
//
// Three refusals are distinguished from a failure, because the caller must drop
// the update instead of retrying it: ErrOwnerNotAllowed (the holder is not
// admitted), ErrConnectionUnknown (Telegram no longer knows this connection)
// and ErrConnectionOwnerConflict (the connection belongs to another tenant).
func (s *Service) Resolve(ctx context.Context, connectionID string) (*Connection, error) {
	if connectionID == "" {
		return nil, fmt.Errorf("empty business_connection_id")
	}
	if entry, ok := s.cache.lookup(connectionID); ok {
		switch entry.kind {
		case entryUnknown:
			return nil, fmt.Errorf("%w: %s", ErrConnectionUnknown, connectionID)
		case entryRefused:
			// A memoised refusal is re-checked, never served on trust: if the
			// holder it names is admitted after all, the memo is ignored and
			// the resolution falls through to the database.
			if !s.ownerAllowed(entry.refusedOwnerTelegramUserID) {
				return nil, ErrOwnerNotAllowed
			}
		case entryResolved:
			if !s.ownerAllowed(entry.conn.OwnerTelegramUserID) {
				return nil, ErrOwnerNotAllowed
			}
			conn := entry.conn
			return &conn, nil
		}
	}

	conn, err := s.getFromDB(ctx, connectionID)
	if err == nil {
		// The allowlist must also apply to historical data: a connection
		// persisted while onboarding was open must not stay authorized once
		// the deployment restricts it again, through the cache or through the
		// database after a restart.
		if !s.ownerAllowed(conn.OwnerTelegramUserID) {
			// Memoised like the revocation is: the holder is not admitted, and
			// that verdict will not change before the entry expires, so the
			// updates they keep sending cost one read in total rather than one
			// each. Only the refusal is remembered -- no tenant key, nothing
			// this connection could later be resolved with.
			s.cache.storeRefused(connectionID, conn.OwnerTelegramUserID)
			return nil, ErrOwnerNotAllowed
		}
		// storeDB, not store: this row was read from the database, so the
		// read may predate a concurrent DisableOwner -- a stale enabled row
		// must not overwrite the disabled entry the erasure just patched in.
		s.cache.storeDB(*conn)
		return conn, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("reading business_connections %s: %w", connectionID, err)
	}

	// Neither in cache nor in the database: last resort, the Telegram API (cf.
	// Service comment).
	apiConn, err := s.client.GetBusinessConnection(ctx, connectionID)
	if err != nil {
		if isUnknownConnection(err) {
			// Revocation, as the Bot API expresses it: the account holder
			// removed the bot, so the id designates nothing any more. Cached
			// as a refusal so the updates Telegram still has buffered for it
			// cost one API call in total rather than one each.
			s.cache.storeUnknown(connectionID)
			s.logger.Warn("business connection unknown to Telegram, treated as revoked",
				slog.String("business_connection_id", connectionID))
			return nil, fmt.Errorf("%w: %s", ErrConnectionUnknown, connectionID)
		}
		return nil, fmt.Errorf("getBusinessConnection %s: %w", connectionID, err)
	}
	if apiConn.ID != connectionID {
		// Caching under apiConn.ID would leave connectionID permanently
		// unresolved and re-ask Telegram for every single update carrying it.
		return nil, fmt.Errorf("getBusinessConnection %s answered for connection %q", connectionID, apiConn.ID)
	}

	resolved, err := s.upsertFromTelegram(ctx, *apiConn)
	if err != nil {
		return nil, err
	}
	if resolved == nil {
		// Rejected by the onboarding allowlist: neither a users row nor a
		// business_connections row is written (upsertFromTelegram refuses
		// before both). The refusal itself is memoised, so a stranger who
		// connects the bot to their own Business account and then types costs
		// one getBusinessConnection in total instead of one per message -- on
		// the poller goroutine and on the rate budget the admitted tenants
		// share.
		s.cache.storeRefused(connectionID, apiConn.User.ID)
		return nil, ErrOwnerNotAllowed
	}
	s.cache.store(*resolved)
	return resolved, nil
}

// isUnknownConnection reports a getBusinessConnection refusal that means "no
// such connection" rather than "try again".
//
// The Bot API answers a removed connection with a 400 and a description that
// has no stable machine-readable form, so the status code is what we key on: a
// 400 is a refusal of the REQUEST, which for a call whose only parameter is the
// connection id can only be about that id. 429 (rate limited), 5xx and every
// transport failure fall through as genuine errors and are retried by the next
// update -- treating those as a revocation would silently stop capturing for a
// live tenant.
func isUnknownConnection(err error) bool {
	var apiErr *telegram.APIError
	return errors.As(err, &apiErr) && apiErr.Code == http.StatusBadRequest
}

// DisableOwner disables every Business connection of one owner and returns how
// many rows changed. It is the first step of /delete_my_data
// (internal/erasure): from here on, saveMessage ignores everything arriving
// through those connections, so every later step of the erasure works on a set
// that only shrinks.
//
// The cache update is not an optimisation, it is half the point. Resolve
// answers from s.cache before ever reading the database, so a connection
// disabled in PostgreSQL alone would keep being resolved as enabled for the
// lifetime of the process -- and the poller would go on saving messages the
// owner just asked to have erased. The two writes are ordered database first:
// a failed UPDATE must leave the cache describing what the table actually says.
//
// The cache is rewritten even when no row changed. Zero rows means the
// connections were already disabled, which is exactly the state the cache must
// agree with.
//
// Nothing here is permanent against the owner's will: reconnecting from the
// Telegram settings sends a business_connection update that re-enables the
// connection and starts a fresh capture. An erasure deletes what was captured,
// it does not ban the account.
func (s *Service) DisableOwner(ctx context.Context, ownerUserID int64) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE business_connections
		SET is_enabled = false, updated_at = now()
		WHERE owner_user_id = $1 AND is_enabled
	`, ownerUserID)
	if err != nil {
		return 0, fmt.Errorf("disabling business connections of owner %d: %w", ownerUserID, err)
	}

	s.cache.disableOwner(ownerUserID)

	s.logger.Info("business connections disabled for erasure",
		slog.Int64("owner_user_id", ownerUserID),
		slog.Int64("connections", tag.RowsAffected()))
	return tag.RowsAffected(), nil
}

// ownerAllowed applies the onboarding allowlist. An empty allowlist admits
// every account holder: that is open onboarding, the multi-tenant default.
func (s *Service) ownerAllowed(telegramUserID int64) bool {
	if len(s.allowed) == 0 {
		return true
	}
	_, ok := s.allowed[telegramUserID]
	return ok
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
// user + connection, welcome message to the owner. It is the whole connection
// lifecycle as Telegram expresses it, in one update type:
//
//   - onboarding: the first update for an id creates the owner (users) and the
//     connection, and welcomes the holder;
//   - deactivation: is_enabled=false -- the holder switched the bot off for
//     that connection, or removed the bot altogether, which the Bot API reports
//     the same way. Capture stops at the next message, the cache included;
//   - reactivation: is_enabled=true again -- capture resumes, and the holder is
//     welcomed again.
//
// The same three transitions apply to a connection an erasure disabled: nothing
// here bans an account, reconnecting is always allowed and starts a fresh
// capture (cf. DisableOwner).
//
// Applies the onboarding allowlist (OWNER_ALLOWLIST_TELEGRAM_USER_IDS) if the
// deployment sets one.
func (s *Service) HandleBusinessConnection(ctx context.Context, tc telegram.BusinessConnection) error {
	resolved, err := s.upsertFromTelegram(ctx, tc)
	if err != nil {
		return err
	}
	if resolved == nil {
		s.logger.Warn("business connection refused: account holder not in the owner allowlist",
			slog.String("business_connection_id", tc.ID))
		return nil // silent refusal on the Telegram side: not a processing error
	}

	s.cache.store(*resolved)

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

// notifyWelcome sends the welcome alert to the owner, bounded by
// welcomeTimeout (cf. the constant): this runs on a shard worker of the
// poller, so an unbounded send would park the partition -- and its batch's
// offset advancement -- on Telegram's backoff.
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
	timeout := s.welcomeTimeout
	if timeout <= 0 {
		timeout = defaultWelcomeTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := s.client.SendMessage(ctx, telegram.BuildWelcomeMessageRequest(tc.UserChatID, tc.User.ID)); err != nil {
		s.logger.Error("failed to send welcome message", slog.String("error", err.Error()))
	}
}

// upsertFromTelegram upserts user + business_connections from a Telegram
// BusinessConnection. Returns (nil, nil) if the onboarding allowlist rejects
// the connection (not an error, a business refusal).
//
// The upsert deliberately does NOT rewrite owner_user_id. A connection belongs
// to the account holder it was created for, for its whole life; letting an
// update move it would mean one tenant's connection could start writing into
// another tenant's rows -- exactly the isolation multi-tenancy is supposed to
// hold. The WHERE on the conflict clause is what enforces it in PostgreSQL
// rather than in Go: of two concurrent writers, the one that would change the
// owner updates nothing and gets no row back (ErrConnectionOwnerConflict).
//
// The owner upsert that precedes it may leave a users row behind when the
// connection is then refused. That is not a leak of anything: in open
// onboarding that holder could create the same row with a connection of their
// own, and under an allowlist they never get this far.
func (s *Service) upsertFromTelegram(ctx context.Context, tc telegram.BusinessConnection) (*Connection, error) {
	if tc.ID == "" || tc.User.ID == 0 {
		return nil, fmt.Errorf("business connection with empty id or zero user id")
	}
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

	var storedOwnerUserID int64
	err = s.pool.QueryRow(ctx, `
		INSERT INTO business_connections (id, owner_user_id, can_reply, is_enabled, updated_at)
		VALUES ($1, $2, $3, $4, now())
		ON CONFLICT (id) DO UPDATE SET
			can_reply     = EXCLUDED.can_reply,
			is_enabled    = EXCLUDED.is_enabled,
			updated_at    = now()
		WHERE business_connections.owner_user_id = EXCLUDED.owner_user_id
		RETURNING owner_user_id
	`, c.ID, c.OwnerUserID, c.CanReply, c.IsEnabled).Scan(&storedOwnerUserID)
	if errors.Is(err, pgx.ErrNoRows) {
		// No row came back: the conflict target matched an existing connection
		// whose owner differs. Logged here rather than only at the caller, so
		// the event is visible even where the error is swallowed as a refusal.
		s.logger.Warn("business connection refused: it already belongs to another owner",
			slog.String("business_connection_id", c.ID),
			slog.Int64("claiming_owner_user_id", c.OwnerUserID))
		return nil, fmt.Errorf("%w: %s", ErrConnectionOwnerConflict, c.ID)
	}
	if err != nil {
		return nil, fmt.Errorf("upsert business_connections %s: %w", c.ID, err)
	}

	return &c, nil
}
