// Package outbox livre durablement les alertes Telegram persistées en base.
package outbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"time"

	"github.com/LouisMoretti/Undelete/bot/internal/telegram"
)

const (
	EventDeletedMessage = "deleted_message"
	// Strictement supérieur au timeout HTTP de 60s configuré par cmd/bot.
	defaultLease        = 2 * time.Minute
	maxBackoff          = 15 * time.Minute
	maxDeliveryAttempts = 5
	// 2^10 s = 1024s dépasse déjà maxBackoff : au-delà, l'exponentiation est
	// inutile et finirait par déborder time.Duration (durée négative).
	maxBackoffAttempts = 10
)

// Job est une alerte réservée par un worker.
type Job struct {
	ID                   int64
	OwnerUserID          int64
	OwnerTelegramUserID  int64
	BusinessConnectionID string
	ChatID               int64
	MessageID            int64
	EventType            string
	Text                 string
	Attempts             int
	LeaseToken           string
}

type Store interface {
	Claim(context.Context, int64, time.Time, time.Duration) (*Job, error)
	MarkSent(context.Context, int64, int64, string, time.Time) error
	MarkRetry(context.Context, int64, int64, string, time.Time, string) error
	MarkFailed(context.Context, int64, int64, string, string) error
}

type Sender interface {
	SendMessageOnce(context.Context, telegram.SendMessageRequest) error
}

// Worker réserve puis livre une alerte à la fois. Le lease rend un job de
// nouveau disponible si le processus s'arrête entre Claim et l'acquittement.
type Worker struct {
	store  Store
	sender Sender
	logger *slog.Logger
	lease  time.Duration
}

func NewWorker(store Store, sender Sender, logger *slog.Logger) *Worker {
	return &Worker{store: store, sender: sender, logger: logger, lease: defaultLease}
}

// ProcessOne traite au plus une alerte du tenant. Le contenu, la connexion et
// le texte des erreurs Telegram ne sont jamais journalisés.
func (w *Worker) ProcessOne(ctx context.Context, ownerUserID int64, now time.Time) (bool, error) {
	job, err := w.store.Claim(ctx, ownerUserID, now, w.lease)
	if err != nil {
		if isShutdown(ctx, err) {
			return false, nil
		}
		return false, fmt.Errorf("réservation outbox: %w", err)
	}
	if job == nil {
		return false, nil
	}

	err = w.sender.SendMessageOnce(ctx, telegram.SendMessageRequest{
		ChatID: job.OwnerTelegramUserID,
		Text:   job.Text,
		// BusinessConnectionID reste volontairement vide : l'alerte vient du bot.
	})
	if err == nil {
		if err := w.store.MarkSent(ctx, ownerUserID, job.ID, job.LeaseToken, now); err != nil {
			if isShutdown(ctx, err) {
				// Alerte partie mais non acquittée : le lease expirera et le job
				// sera repris, donc au pire un doublon après redémarrage.
				w.logger.Warn("acquittement outbox interrompu par l'arrêt, redélivrance possible", slog.Int64("outbox_id", job.ID))
				return false, nil
			}
			return true, fmt.Errorf("acquittement outbox: %w", err)
		}
		w.logger.Info("alerte outbox envoyée", slog.Int64("outbox_id", job.ID), slog.Int("attempt", job.Attempts+1))
		return true, nil
	}

	// Arrêt du processus : le contexte est mort, aucun MarkRetry/MarkFailed
	// n'aboutirait. On laisse le lease expirer, le job repartira au
	// redémarrage. Un vrai timeout HTTP Telegram laisse ctx vivant et suit
	// donc la voie normale du retry.
	if isShutdown(ctx, err) {
		w.logger.Info("livraison outbox interrompue par l'arrêt, reprise après expiration du lease", slog.Int64("outbox_id", job.ID))
		return false, nil
	}

	code := "transport"
	var apiErr *telegram.APIError
	if errors.As(err, &apiErr) {
		code = fmt.Sprintf("telegram_%d", apiErr.Code)
		if apiErr.Code >= http.StatusBadRequest && apiErr.Code < http.StatusInternalServerError && apiErr.Code != http.StatusTooManyRequests {
			if markErr := w.store.MarkFailed(ctx, ownerUserID, job.ID, job.LeaseToken, code); markErr != nil {
				return true, fmt.Errorf("échec définitif outbox: %w", markErr)
			}
			w.logger.Warn("alerte outbox en échec définitif", slog.Int64("outbox_id", job.ID), slog.String("error_class", code))
			return true, nil
		}
	}
	if job.Attempts+1 >= maxDeliveryAttempts {
		if markErr := w.store.MarkFailed(ctx, ownerUserID, job.ID, job.LeaseToken, code); markErr != nil {
			return true, fmt.Errorf("épuisement des tentatives outbox: %w", markErr)
		}
		w.logger.Warn("alerte outbox en échec après épuisement des tentatives", slog.Int64("outbox_id", job.ID), slog.String("error_class", code), slog.Int("attempt", job.Attempts+1))
		return true, nil
	}

	wait := retryDelay(job.Attempts)
	if apiErr != nil && apiErr.IsRateLimited() {
		wait = time.Duration(apiErr.RetryAfter) * time.Second
	}
	if markErr := w.store.MarkRetry(ctx, ownerUserID, job.ID, job.LeaseToken, now.Add(wait), code); markErr != nil {
		return true, fmt.Errorf("planification retry outbox: %w", markErr)
	}
	w.logger.Warn("alerte outbox replanifiée", slog.Int64("outbox_id", job.ID), slog.String("error_class", code), slog.Duration("retry_in", wait))
	return true, nil
}

// isShutdown distingue l'annulation du contexte du worker (arrêt du processus)
// d'un timeout réseau côté Telegram : le timeout du client HTTP laisse ctx
// vivant et doit rester rejouable, alors qu'un ctx mort rend toute écriture en
// base impossible.
func isShutdown(ctx context.Context, err error) bool {
	if ctx.Err() == nil {
		return false
	}
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func retryDelay(attempts int) time.Duration {
	if attempts < 0 {
		attempts = 0
	}
	if attempts >= maxBackoffAttempts {
		return maxBackoff
	}
	seconds := math.Pow(2, float64(attempts))
	wait := time.Duration(seconds) * time.Second
	if wait > maxBackoff {
		return maxBackoff
	}
	return wait
}
