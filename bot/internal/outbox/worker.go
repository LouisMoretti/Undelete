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
	defaultLease        = time.Minute
	maxBackoff          = 15 * time.Minute
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
}

type Store interface {
	Claim(context.Context, int64, time.Time, time.Duration) (*Job, error)
	MarkSent(context.Context, int64, int64, time.Time) error
	MarkRetry(context.Context, int64, int64, time.Time, string) error
	MarkFailed(context.Context, int64, int64, string) error
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
		if err := w.store.MarkSent(ctx, ownerUserID, job.ID, now); err != nil {
			return true, fmt.Errorf("acquittement outbox: %w", err)
		}
		w.logger.Info("alerte outbox envoyée", slog.Int64("outbox_id", job.ID), slog.Int("attempt", job.Attempts+1))
		return true, nil
	}

	code := "transport"
	var apiErr *telegram.APIError
	if errors.As(err, &apiErr) {
		code = fmt.Sprintf("telegram_%d", apiErr.Code)
		if apiErr.Code >= http.StatusBadRequest && apiErr.Code < http.StatusInternalServerError && apiErr.Code != http.StatusTooManyRequests {
			if markErr := w.store.MarkFailed(ctx, ownerUserID, job.ID, code); markErr != nil {
				return true, fmt.Errorf("échec définitif outbox: %w", markErr)
			}
			w.logger.Warn("alerte outbox en échec définitif", slog.Int64("outbox_id", job.ID), slog.String("error_class", code))
			return true, nil
		}
	}

	wait := retryDelay(job.Attempts)
	if apiErr != nil && apiErr.IsRateLimited() {
		wait = time.Duration(apiErr.RetryAfter) * time.Second
	}
	if markErr := w.store.MarkRetry(ctx, ownerUserID, job.ID, now.Add(wait), code); markErr != nil {
		return true, fmt.Errorf("planification retry outbox: %w", markErr)
	}
	w.logger.Warn("alerte outbox replanifiée", slog.Int64("outbox_id", job.ID), slog.String("error_class", code), slog.Duration("retry_in", wait))
	return true, nil
}

func retryDelay(attempts int) time.Duration {
	if attempts < 0 {
		attempts = 0
	}
	seconds := math.Pow(2, float64(attempts))
	wait := time.Duration(seconds) * time.Second
	if wait > maxBackoff {
		return maxBackoff
	}
	return wait
}
