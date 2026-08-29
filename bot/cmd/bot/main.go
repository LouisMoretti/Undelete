// Commande bot : point d'entrée du service undelete.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/LouisMoretti/Undelete/bot/internal/app"
	"github.com/LouisMoretti/Undelete/bot/internal/business"
	"github.com/LouisMoretti/Undelete/bot/internal/config"
	"github.com/LouisMoretti/Undelete/bot/internal/messages"
	"github.com/LouisMoretti/Undelete/bot/internal/storage"
	"github.com/LouisMoretti/Undelete/bot/internal/telegram"
	"github.com/LouisMoretti/Undelete/bot/internal/users"
)

// httpClientTimeout doit rester strictement supérieur au timeout de
// long-polling (50s, voir telegram/poller.go) : sinon le client HTTP
// couperait la requête avant que Telegram n'ait eu la chance de répondre.
const httpClientTimeout = 60 * time.Second

// retentionInterval fixe la fréquence de la purge de rétention. Une purge
// quotidienne suffit largement (retention_days minimum = 1 jour).
const retentionInterval = 24 * time.Hour

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	if err := run(logger); err != nil {
		logger.Error("arrêt sur erreur fatale", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Migrations appliquées avec le DSN propriétaire, AVANT l'ouverture du
	// pool applicatif : le rôle undelete_app n'a pas les droits DDL.
	if err := storage.RunMigrations(ctx, cfg.MigrationDatabaseURL, logger); err != nil {
		return err
	}

	db, err := storage.NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()

	client := telegram.NewClient(cfg.TelegramBotToken, httpClientTimeout)
	usersRepo := users.NewRepository(db.Pool)
	messagesRepo := messages.NewRepository(db)
	businessSvc := business.NewService(db.Pool, client, usersRepo, cfg.OwnerTelegramUserID, logger)
	handler := app.NewHandler(businessSvc, messagesRepo, client, logger)

	go runRetentionLoop(ctx, usersRepo, messagesRepo, logger)

	poller := telegram.NewPoller(client, logger)
	logger.Info("démarrage du poller", slog.Any("allowed_updates", telegram.AllowedUpdates))

	err = poller.Run(ctx, handler.HandleUpdate)
	if ctx.Err() != nil {
		logger.Info("arrêt demandé, extinction propre")
		return nil
	}
	return err
}

// runRetentionLoop exécute PurgeExpired à intervalle régulier. Boucle
// indépendante du poller : une purge lente ou en erreur ne doit jamais
// retarder le traitement des updates Telegram (contrainte de réactivité du
// long-polling).
func runRetentionLoop(ctx context.Context, usersRepo *users.Repository, messagesRepo *messages.Repository, logger *slog.Logger) {
	ticker := time.NewTicker(retentionInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tenants, err := usersRepo.ListTenantsForRetention(ctx)
			if err != nil {
				logger.Error("purge rétention: échec listage tenants", slog.String("error", err.Error()))
				continue
			}
			purged, err := messagesRepo.PurgeExpired(ctx, tenants)
			if err != nil {
				logger.Error("purge rétention: échec", slog.String("error", err.Error()), slog.Int64("purged_before_error", purged))
				continue
			}
			logger.Info("purge rétention terminée", slog.Int64("purged", purged), slog.Int("tenants", len(tenants)))
		}
	}
}
