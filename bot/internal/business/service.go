// Package business gère la résolution des connexions Telegram Business et
// la table business_connections.
//
// business_connections n'est PAS protégée par RLS (voir le commentaire dans
// storage/migrations/0001_init.sql) : cette table est interrogée par id de
// connexion, avant même de connaître l'owner_user_id qui permettrait de
// poser le contexte RLS. C'est pourquoi ce package requête le pool
// applicatif directement plutôt que de passer par storage.DB.InTenant.
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

// Connection est la projection minimale nécessaire à la résolution : à
// quel owner appartient cette connexion, et est-elle active.
type Connection struct {
	ID          string
	OwnerUserID int64 // id interne (users.id), utilisé comme clé RLS
	// OwnerTelegramUserID est l'identifiant Telegram du owner, distinct de
	// OwnerUserID (clé primaire interne). C'est CET identifiant qu'il faut
	// utiliser comme chat_id pour joindre le owner en message direct (cf.
	// contrainte n°7 dans app/handler.go) : owner_user_id ne correspond à
	// rien côté Telegram.
	OwnerTelegramUserID int64
	CanReply            bool
	IsEnabled           bool
}

// ErrOwnerMismatch signale une connexion Business refusée par le garde-fou
// mono-tenant (OWNER_TELEGRAM_USER_ID).
var ErrOwnerMismatch = errors.New("business: telegram_user_id ne correspond pas à OWNER_TELEGRAM_USER_ID")

// Service résout les connexions Business via une chaîne à trois niveaux :
// cache mémoire -> base -> API Telegram (getBusinessConnection).
//
// Le troisième niveau (API) est indispensable, pas une simple optimisation :
// si le bot redémarre, il perd son cache mémoire ; s'il reçoit un
// business_message pour une connexion établie PENDANT qu'il était hors
// ligne, cette connexion n'est ni en cache (perdu au redémarrage) ni en
// base (jamais vu passer l'update business_connection correspondant, par
// exemple si l'historique des updates a expiré côté Telegram avant le
// redémarrage). Sans cet appel API, le bot ignorerait silencieusement des
// messages pourtant couverts par une connexion Business bien réelle.
type Service struct {
	pool   *pgxpool.Pool
	client *telegram.Client
	users  *users.Repository
	logger *slog.Logger

	ownerFilter int64 // OWNER_TELEGRAM_USER_ID ; 0 = pas de restriction

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

// Resolve renvoie la Connection associée à connectionID, en tentant dans
// l'ordre : cache mémoire, base, API Telegram. Une connexion trouvée via
// l'API est upsertée en base et posée en cache avant d'être renvoyée.
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
		// Le filtre doit aussi s'appliquer aux données historiques : une
		// connexion étrangère créée avant l'activation du garde-fou ne doit
		// pas redevenir autorisée via le cache ou la base après redémarrage.
		if !s.ownerAllowed(conn.OwnerTelegramUserID) {
			return nil, ErrOwnerMismatch
		}
		s.storeInCache(*conn)
		return conn, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("lecture business_connections %s: %w", connectionID, err)
	}

	// Ni en cache ni en base : dernier recours, l'API Telegram (cf.
	// commentaire de Service).
	apiConn, err := s.client.GetBusinessConnection(ctx, connectionID)
	if err != nil {
		return nil, fmt.Errorf("getBusinessConnection %s: %w", connectionID, err)
	}

	resolved, err := s.upsertFromTelegram(ctx, *apiConn)
	if err != nil {
		return nil, err
	}
	if resolved == nil {
		// Refusée par le garde-fou mono-tenant : pas d'entrée en cache/DB.
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
	// JOIN avec users : business_connections ne stocke que l'id interne
	// (owner_user_id), il faut le telegram_user_id pour pouvoir notifier le
	// owner (voir commentaire sur Connection.OwnerTelegramUserID).
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

// HandleBusinessConnection traite l'update business_connection : upsert
// user + connexion, message de bienvenue au owner. Applique le garde-fou
// mono-tenant si OWNER_TELEGRAM_USER_ID est défini.
func (s *Service) HandleBusinessConnection(ctx context.Context, tc telegram.BusinessConnection) error {
	resolved, err := s.upsertFromTelegram(ctx, tc)
	if err != nil {
		return err
	}
	if resolved == nil {
		s.logger.Warn("connexion business refusée : garde-fou mono-tenant",
			slog.String("business_connection_id", tc.ID))
		return nil // refus silencieux côté Telegram : pas une erreur de traitement
	}

	s.storeInCache(*resolved)

	if !resolved.IsEnabled {
		s.logger.Info("connexion business désactivée",
			slog.String("business_connection_id", resolved.ID),
			slog.Int64("owner_user_id", resolved.OwnerUserID))
		return nil
	}

	s.logger.Info("connexion business établie",
		slog.String("business_connection_id", resolved.ID),
		slog.Int64("owner_user_id", resolved.OwnerUserID),
		slog.Bool("can_reply", resolved.CanReply),
		slog.Bool("is_enabled", resolved.IsEnabled))

	s.notifyWelcome(ctx, tc)

	return nil
}

// notifyWelcome envoie l'alerte de bienvenue au owner.
//
// Contrainte n°7 : jamais de BusinessConnectionID ici, sous peine d'envoyer ce
// message EN TANT QUE le owner dans une conversation surveillée. L'échec est
// logué sans interrompre le traitement : la connexion est déjà persistée, la
// perte d'un message de bienvenue ne doit pas faire rejouer l'update.
//
// Ces trois lignes sont isolées pour que le contrat filaire de l'alerte soit
// testable sur le chemin de production lui-même, sans base de données (cf.
// TestWelcomeAlertContract).
func (s *Service) notifyWelcome(ctx context.Context, tc telegram.BusinessConnection) {
	if err := s.client.SendMessage(ctx, telegram.BuildWelcomeMessageRequest(tc.UserChatID, tc.User.ID)); err != nil {
		s.logger.Error("échec envoi message de bienvenue", slog.String("error", err.Error()))
	}
}

// upsertFromTelegram upsert user + business_connections à partir d'une
// BusinessConnection Telegram. Renvoie (nil, nil) si le garde-fou
// mono-tenant refuse la connexion (pas une erreur, un refus métier).
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
