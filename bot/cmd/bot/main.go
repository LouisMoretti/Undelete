// Commande bot : point d'entrée du service undelete.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/LouisMoretti/Undelete/bot/internal/app"
	"github.com/LouisMoretti/Undelete/bot/internal/business"
	"github.com/LouisMoretti/Undelete/bot/internal/config"
	"github.com/LouisMoretti/Undelete/bot/internal/health"
	"github.com/LouisMoretti/Undelete/bot/internal/messages"
	"github.com/LouisMoretti/Undelete/bot/internal/metrics"
	"github.com/LouisMoretti/Undelete/bot/internal/outbox"
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
const outboxInterval = time.Second

// backlogInterval fixe le rafraîchissement de la jauge de backlog outbox.
// Volontairement bien plus lent que outboxInterval : la jauge sert à repérer
// une livraison qui décroche, pas à suivre chaque job à la seconde, et un
// COUNT(*) par tenant chaque seconde coûterait plus cher que le travail utile.
const backlogInterval = 15 * time.Second

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
	outboxRepo := outbox.NewRepository(db)
	businessSvc := business.NewService(db.Pool, client, usersRepo, cfg.OwnerTelegramUserID, logger)
	handler := app.NewHandler(businessSvc, messagesRepo, logger)

	poller := telegram.NewPoller(client, logger)

	var wg sync.WaitGroup
	wg.Add(4)
	go func() {
		defer wg.Done()
		runRetentionLoop(ctx, usersRepo, messagesRepo, outboxRepo, logger)
	}()
	go func() {
		defer wg.Done()
		runOutboxLoop(ctx, usersRepo, outbox.NewWorker(outboxRepo, client, logger), logger)
	}()
	go func() {
		defer wg.Done()
		runBacklogLoop(ctx, usersRepo, outboxRepo, logger)
	}()
	go func() {
		defer wg.Done()
		// Readiness = base joignable ET poller frais. Le serveur ne reçoit
		// que le pool et le poller : ni le jeton, ni la config, rien qui
		// puisse se retrouver dans une réponse HTTP.
		healthHandler := health.NewHandler(db.Pool, poller, metrics.Default(), nil)
		if err := health.Serve(ctx, cfg.HealthAddr, healthHandler, logger); err != nil {
			logger.Error("serveur de santé arrêté sur erreur", slog.String("error", err.Error()))
		}
	}()

	logger.Info("démarrage du poller", slog.Any("allowed_updates", telegram.AllowedUpdates))

	err = poller.Run(ctx, handler.HandleUpdate)
	// L'arrêt sur signal annule ctx : on attend que la rétention et l'outbox
	// terminent leur itération en cours avant de fermer le pool, sinon une
	// alerte réservée resterait 'processing' jusqu'à expiration du lease et
	// pourrait être redoublée au redémarrage.
	stop()
	wg.Wait()
	if ctx.Err() != nil {
		logger.Info("arrêt demandé, extinction propre")
		return nil
	}
	return err
}

func runOutboxLoop(ctx context.Context, usersRepo *users.Repository, worker *outbox.Worker, logger *slog.Logger) {
	ticker := time.NewTicker(outboxInterval)
	defer ticker.Stop()

	for {
		tenants, err := usersRepo.ListTenantsForRetention(ctx)
		if err != nil {
			logger.Error("outbox: échec listage tenants", slog.String("error", err.Error()))
		} else {
			for _, tenant := range tenants {
				for processed := 0; processed < 100; processed++ {
					didProcess, err := worker.ProcessOne(ctx, tenant.OwnerUserID)
					if err != nil {
						logger.Error("outbox: échec traitement", slog.Int64("owner_user_id", tenant.OwnerUserID), slog.String("error", err.Error()))
						break
					}
					if !didProcess {
						break
					}
				}
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// runBacklogLoop rafraîchit la jauge undelete_outbox_backlog. Boucle séparée
// de l'outbox : un COUNT(*) lent ou en erreur ne doit pas ralentir la
// livraison des alertes, et une jauge périmée est moins grave qu'une alerte
// en retard.
func runBacklogLoop(ctx context.Context, usersRepo *users.Repository, outboxRepo *outbox.Repository, logger *slog.Logger) {
	ticker := time.NewTicker(backlogInterval)
	defer ticker.Stop()

	for {
		tenants, err := usersRepo.ListTenantsForRetention(ctx)
		if err == nil {
			var backlog int64
			backlog, err = outboxRepo.CountBacklog(ctx, tenants)
			if err == nil {
				metrics.SetOutboxBacklog(backlog)
			}
		}
		if err != nil && ctx.Err() == nil {
			logger.Error("backlog outbox: échec du comptage", slog.String("error", err.Error()))
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// runRetentionLoop exécute PurgeExpired à intervalle régulier, pour messages
// ET pour notification_outbox : cette dernière contient payload_text (du
// contenu utilisateur) et, sans purge, ses lignes 'sent'/'failed' croîtraient
// indéfiniment en échappant à retention_days. Boucle indépendante du poller :
// une purge lente ou en erreur ne doit jamais retarder le traitement des
// updates Telegram (contrainte de réactivité du long-polling).
func runRetentionLoop(ctx context.Context, usersRepo *users.Repository, messagesRepo *messages.Repository, outboxRepo *outbox.Repository, logger *slog.Logger) {
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
			purgedOutbox, err := outboxRepo.PurgeExpired(ctx, tenants)
			if err != nil {
				logger.Error("purge rétention outbox: échec", slog.String("error", err.Error()), slog.Int64("purged_before_error", purgedOutbox))
				continue
			}
			logger.Info("purge rétention terminée",
				slog.Int64("purged", purged),
				slog.Int64("purged_outbox", purgedOutbox),
				slog.Int("tenants", len(tenants)))
		}
	}
}
