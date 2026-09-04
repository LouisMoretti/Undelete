// Bot command: entry point of the undelete service.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/LouisMoretti/Undelete/bot/internal/app"
	"github.com/LouisMoretti/Undelete/bot/internal/business"
	"github.com/LouisMoretti/Undelete/bot/internal/config"
	"github.com/LouisMoretti/Undelete/bot/internal/health"
	"github.com/LouisMoretti/Undelete/bot/internal/media"
	"github.com/LouisMoretti/Undelete/bot/internal/media/fetch"
	"github.com/LouisMoretti/Undelete/bot/internal/media/store"
	"github.com/LouisMoretti/Undelete/bot/internal/messages"
	"github.com/LouisMoretti/Undelete/bot/internal/metrics"
	"github.com/LouisMoretti/Undelete/bot/internal/outbox"
	"github.com/LouisMoretti/Undelete/bot/internal/storage"
	"github.com/LouisMoretti/Undelete/bot/internal/telegram"
	"github.com/LouisMoretti/Undelete/bot/internal/users"
)

// httpClientTimeout must stay strictly above the long-polling timeout
// (50s, see telegram/poller.go): otherwise the HTTP client would cut the
// request before Telegram gets a chance to respond.
const httpClientTimeout = 60 * time.Second

// retentionInterval sets the frequency of the retention purge. A daily
// purge is more than enough (retention_days minimum = 1 day).
const retentionInterval = 24 * time.Hour
const outboxInterval = time.Second

// mediaInterval paces the download of the attachments. Slower than the outbox:
// a media is only useful once its message is deleted, and hammering getFile
// every second would only spend rate limit on files nobody is waiting for.
const mediaInterval = 5 * time.Second

// backlogInterval sets the refresh rate of the outbox backlog gauge.
// Deliberately much slower than outboxInterval: the gauge is meant to spot
// a delivery that is falling behind, not to track every job to the second,
// and a COUNT(*) per tenant every second would cost more than the useful work.
const backlogInterval = 15 * time.Second

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	if err := run(logger); err != nil {
		logger.Error("stopping after fatal error", slog.String("error", err.Error()))
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

	// Migrations applied with the owner DSN, BEFORE the application pool
	// opens: the undelete_app role has no DDL rights.
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
	mediaRepo := media.NewRepository(db)
	businessSvc := business.NewService(db.Pool, client, usersRepo, cfg.OwnerTelegramUserID, logger)
	handler := app.NewHandler(businessSvc, messagesRepo, mediaRepo, logger)

	// Dedicated HTTP client for the downloads: a media transfer must not share
	// the connection pool of the long-polling client, whose timeout is sized
	// for getUpdates. store.Downloader bounds each attempt with its own
	// deadline, body transfer included.
	downloader, err := store.New(store.Config{
		BaseDir:    cfg.MediaDir,
		HTTPClient: &http.Client{},
		Logger:     logger,
	})
	if err != nil {
		return err
	}
	fetcher := fetch.New(mediaRepo, client, downloader, cfg.TelegramBotToken, logger)

	poller := telegram.NewPoller(client, logger)

	var wg sync.WaitGroup
	wg.Add(5)
	go func() {
		defer wg.Done()
		runRetentionLoop(ctx, usersRepo, messagesRepo, outboxRepo, logger)
	}()
	go func() {
		defer wg.Done()
		// WithMediaDir: the same root the paths in media_files are relative
		// to. Without it the worker would deliver every media alert as text.
		worker := outbox.NewWorker(outboxRepo, client, logger, outbox.WithMediaDir(cfg.MediaDir))
		runOutboxLoop(ctx, usersRepo, worker, logger)
	}()
	go func() {
		defer wg.Done()
		runMediaLoop(ctx, usersRepo, fetcher, logger)
	}()
	go func() {
		defer wg.Done()
		runBacklogLoop(ctx, usersRepo, outboxRepo, logger)
	}()
	go func() {
		defer wg.Done()
		// Readiness = reachable database AND fresh poller. The server only
		// receives the pool and the poller: neither the token nor the config,
		// nothing that could end up in an HTTP response.
		healthHandler := health.NewHandler(db.Pool, poller, metrics.Default(), nil)
		if err := health.Serve(ctx, cfg.HealthAddr, healthHandler, logger); err != nil {
			logger.Error("health server stopped on error", slog.String("error", err.Error()))
		}
	}()

	logger.Info("poller starting", slog.Any("allowed_updates", telegram.AllowedUpdates))

	err = poller.Run(ctx, handler.HandleUpdate)
	// Signal-driven shutdown cancels ctx: we wait for retention and the outbox
	// to finish their current iteration before closing the pool, otherwise a
	// leased alert would stay 'processing' until the lease expires and could
	// be redelivered on restart.
	stop()
	wg.Wait()
	if ctx.Err() != nil {
		logger.Info("shutdown requested, clean stop")
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
			logger.Error("outbox: failed to list tenants", slog.String("error", err.Error()))
		} else {
			for _, tenant := range tenants {
				for processed := 0; processed < 100; processed++ {
					didProcess, err := worker.ProcessOne(ctx, tenant.OwnerUserID)
					if err != nil {
						logger.Error("outbox: processing failed", slog.Int64("owner_user_id", tenant.OwnerUserID), slog.String("error", err.Error()))
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

// runMediaLoop downloads the attachments catalogued as pending, tenant by
// tenant (constraint #4: media_files is under FORCE RLS, so every read goes
// through InTenant).
//
// Separate from both the poller and the outbox: a download is slow and can
// fail, and neither the capture of new messages nor the delivery of alerts may
// wait on it. A media that is not stored yet simply does not travel with its
// alert -- the text goes out regardless.
func runMediaLoop(ctx context.Context, usersRepo *users.Repository, fetcher *fetch.Fetcher, logger *slog.Logger) {
	ticker := time.NewTicker(mediaInterval)
	defer ticker.Stop()

	for {
		tenants, err := usersRepo.ListTenantsForRetention(ctx)
		if err != nil {
			if ctx.Err() == nil {
				logger.Error("media fetch: failed to list tenants", slog.String("error", err.Error()))
			}
		} else {
			for _, tenant := range tenants {
				stored, err := fetcher.ProcessTenant(ctx, tenant.OwnerUserID)
				if err != nil && ctx.Err() == nil {
					logger.Error("media fetch: processing failed",
						slog.Int64("owner_user_id", tenant.OwnerUserID),
						slog.String("error", err.Error()))
				}
				if stored > 0 {
					logger.Info("media stored", slog.Int("files", stored))
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

// runBacklogLoop refreshes the undelete_outbox_backlog gauge. A separate
// loop from the outbox: a slow or failing COUNT(*) must not slow down alert
// delivery, and a stale gauge is less serious than a late alert.
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
			logger.Error("outbox backlog: count failed", slog.String("error", err.Error()))
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// runRetentionLoop runs PurgeExpired at regular intervals, for messages AND
// for notification_outbox: the latter holds payload_text (user content) and,
// without purging, its 'sent'/'failed' rows would grow indefinitely while
// escaping retention_days. Independent of the poller loop: a slow or failing
// purge must never delay the processing of Telegram updates (long-polling
// responsiveness constraint).
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
				logger.Error("retention purge: failed to list tenants", slog.String("error", err.Error()))
				continue
			}
			purged, err := messagesRepo.PurgeExpired(ctx, tenants)
			if err != nil {
				logger.Error("retention purge: failed", slog.String("error", err.Error()), slog.Int64("purged_before_error", purged))
				continue
			}
			purgedOutbox, err := outboxRepo.PurgeExpired(ctx, tenants)
			if err != nil {
				logger.Error("outbox retention purge: failed", slog.String("error", err.Error()), slog.Int64("purged_before_error", purgedOutbox))
				continue
			}
			logger.Info("retention purge complete",
				slog.Int64("purged", purged),
				slog.Int64("purged_outbox", purgedOutbox),
				slog.Int("tenants", len(tenants)))
		}
	}
}
