// Package outbox durably delivers the Telegram alerts persisted in the database.
package outbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"time"

	"github.com/LouisMoretti/Undelete/bot/internal/metrics"
	"github.com/LouisMoretti/Undelete/bot/internal/telegram"
)

const (
	EventDeletedMessage = "deleted_message"
	// Strictly greater than the 60s HTTP timeout configured by cmd/bot.
	defaultLease        = 2 * time.Minute
	maxBackoff          = 15 * time.Minute
	maxDeliveryAttempts = 5
	// 2^10 s = 1024s already exceeds maxBackoff: beyond that, the exponentiation
	// is useless and would eventually overflow time.Duration (negative duration).
	maxBackoffAttempts = 10
)

// Job is an alert reserved by a worker.
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

// Store owns the clock: all timestamps written or compared are on the
// PostgreSQL side. The worker only supplies durations (lease, retry delay).
type Store interface {
	Claim(context.Context, int64, time.Duration) (*Job, error)
	MarkSent(context.Context, int64, int64, string) error
	MarkRetry(context.Context, int64, int64, string, time.Duration, string) error
	MarkFailed(context.Context, int64, int64, string, string) error
}

type Sender interface {
	SendMessageOnce(context.Context, telegram.SendMessageRequest) error
}

// Worker reserves then delivers one alert at a time. The lease makes a job
// available again if the process stops between Claim and acknowledgement:
// delivery is therefore at-least-once, a duplicate alert remains possible.
type Worker struct {
	store  Store
	sender Sender
	logger *slog.Logger
	lease  time.Duration
}

func NewWorker(store Store, sender Sender, logger *slog.Logger) *Worker {
	return &Worker{store: store, sender: sender, logger: logger, lease: defaultLease}
}

// ProcessOne processes at most one alert for the tenant. The content, the
// connection and the text of Telegram errors are never logged.
func (w *Worker) ProcessOne(ctx context.Context, ownerUserID int64) (bool, error) {
	job, err := w.store.Claim(ctx, ownerUserID, w.lease)
	if err != nil {
		if isShutdown(ctx, err) {
			return false, nil
		}
		return false, fmt.Errorf("outbox reservation: %w", err)
	}
	if job == nil {
		return false, nil
	}

	err = w.sender.SendMessageOnce(ctx, telegram.SendMessageRequest{
		ChatID: job.OwnerTelegramUserID,
		Text:   job.Text,
		// BusinessConnectionID is intentionally left empty: the alert comes from the bot.
	})
	if err == nil {
		if err := w.store.MarkSent(ctx, ownerUserID, job.ID, job.LeaseToken); err != nil {
			if isShutdown(ctx, err) {
				// Alert sent but not acknowledged: the lease will expire and the job
				// will be picked up again, so at worst a duplicate after restart.
				w.logger.Warn("outbox acknowledgement interrupted by shutdown, possible redelivery", slog.Int64("outbox_id", job.ID))
				return false, nil
			}
			return true, fmt.Errorf("outbox acknowledgement: %w", err)
		}
		w.logger.Info("outbox alert sent", slog.Int64("outbox_id", job.ID), slog.Int("attempt", job.Attempts+1))
		return true, nil
	}

	// Process shutdown: the context is dead, no MarkRetry/MarkFailed would
	// succeed. We let the lease expire; the job will be picked up again on
	// restart. A real Telegram HTTP timeout leaves ctx alive and therefore
	// follows the normal retry path.
	if isShutdown(ctx, err) {
		w.logger.Info("outbox delivery interrupted by shutdown, resuming after lease expiry", slog.Int64("outbox_id", job.ID))
		return false, nil
	}

	code := "transport"
	var apiErr *telegram.APIError
	if errors.As(err, &apiErr) {
		code = fmt.Sprintf("telegram_%d", apiErr.Code)
		if apiErr.Code >= http.StatusBadRequest && apiErr.Code < http.StatusInternalServerError && apiErr.Code != http.StatusTooManyRequests {
			if markErr := w.store.MarkFailed(ctx, ownerUserID, job.ID, job.LeaseToken, code); markErr != nil {
				return true, fmt.Errorf("outbox permanent failure: %w", markErr)
			}
			metrics.AddOutboxFailed(1)
			w.logger.Warn("outbox alert permanently failed", slog.Int64("outbox_id", job.ID), slog.String("error_class", code))
			return true, nil
		}
	}
	if job.Attempts+1 >= maxDeliveryAttempts {
		if markErr := w.store.MarkFailed(ctx, ownerUserID, job.ID, job.LeaseToken, code); markErr != nil {
			return true, fmt.Errorf("outbox attempts exhausted: %w", markErr)
		}
		metrics.AddOutboxFailed(1)
		w.logger.Warn("outbox alert failed after exhausting attempts", slog.Int64("outbox_id", job.ID), slog.String("error_class", code), slog.Int("attempt", job.Attempts+1))
		return true, nil
	}

	wait := retryDelay(job.Attempts)
	if apiErr != nil && apiErr.IsRateLimited() {
		wait = time.Duration(apiErr.RetryAfter) * time.Second
	}
	if markErr := w.store.MarkRetry(ctx, ownerUserID, job.ID, job.LeaseToken, wait, code); markErr != nil {
		return true, fmt.Errorf("outbox retry scheduling: %w", markErr)
	}
	metrics.AddOutboxRetries(1)
	w.logger.Warn("outbox alert rescheduled", slog.Int64("outbox_id", job.ID), slog.String("error_class", code), slog.Duration("retry_in", wait))
	return true, nil
}

// isShutdown distinguishes worker context cancellation (process shutdown) from
// a network timeout on the Telegram side: the HTTP client timeout leaves ctx
// alive and must remain replayable, whereas a dead ctx makes any database
// write impossible.
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
