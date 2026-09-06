// Package outbox durably delivers the Telegram alerts persisted in the database.
package outbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/LouisMoretti/Undelete/bot/internal/media"
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
	// PayloadKind is PayloadKindText (default) or PayloadKindMedia. A media
	// job whose Media is nil -- unreadable payload -- still delivers Text,
	// followed by the unavailability note.
	PayloadKind string
	Media       *MediaPayload
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

// MediaSender is the OPTIONAL half of Sender: a sender that also knows how to
// upload the restored files. It is a separate interface on purpose -- a sender
// that does not implement it (or a worker without a media root) still delivers
// every media alert, as text plus the unavailability note.
type MediaSender interface {
	SendMediaOnce(context.Context, telegram.MediaAlert) error
}

// Worker reserves then delivers one alert at a time. The lease makes a job
// available again if the process stops between Claim and acknowledgement:
// delivery is therefore at-least-once, a duplicate alert remains possible.
type Worker struct {
	store    Store
	sender   Sender
	logger   *slog.Logger
	lease    time.Duration
	mediaDir string
}

// WorkerOption tweaks a Worker at construction time.
type WorkerOption func(*Worker)

// WithMediaDir gives the worker the storage root the media relative paths are
// resolved against (./media in production). Without it, media alerts fall back
// to text: better a text alert than an upload from a path we cannot vouch for.
func WithMediaDir(dir string) WorkerOption {
	return func(w *Worker) { w.mediaDir = dir }
}

func NewWorker(store Store, sender Sender, logger *slog.Logger, opts ...WorkerOption) *Worker {
	w := &Worker{store: store, sender: sender, logger: logger, lease: defaultLease}
	for _, opt := range opts {
		opt(w)
	}
	return w
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

	err = w.deliver(ctx, job)
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
		w.logger.Info("outbox alert sent",
			slog.Int64("outbox_id", job.ID),
			slog.Int("attempt", job.Attempts+1),
			slog.String("payload_kind", payloadKind(job)))
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

// payloadKind normalises the kind for the logs: a job read from an older row
// (or built by a test) carries an empty kind, which is a text alert.
func payloadKind(job *Job) string {
	if job.PayloadKind == PayloadKindMedia {
		return PayloadKindMedia
	}
	return PayloadKindText
}

// errNoMediaDelivery means this worker cannot upload files at all for this job
// (sender without media support, no media root configured, unreadable
// payload). Not a Telegram failure: the text fallback applies immediately,
// without a single request.
var errNoMediaDelivery = errors.New("outbox: media delivery unavailable")

// deliver sends one job. A media job tries its files first and falls back to
// its text when they can never go out; a text job is unchanged.
//
// The invariant this function protects: an alert is NEVER lost. Every path
// either delivers something to the owner, or returns an error that leaves the
// job replayable.
func (w *Worker) deliver(ctx context.Context, job *Job) error {
	if job.PayloadKind != PayloadKindMedia {
		return w.sendText(ctx, job, false)
	}

	err := w.sendMedia(ctx, job)
	if err == nil {
		return nil
	}
	if !mediaIsHopeless(ctx, err) {
		// 429, 5xx, transport error, shutdown: the media can still go out
		// later, so the job keeps the existing backoff instead of degrading to
		// text on the first hiccup.
		return err
	}
	w.logger.Warn("media alert falling back to text",
		slog.Int64("outbox_id", job.ID), slog.String("error_class", mediaErrorClass(err)))
	return w.sendText(ctx, job, true)
}

// sendText delivers the textual alert. withNote appends the unavailability
// note, so the owner knows a media existed and is not left with a silent hole.
func (w *Worker) sendText(ctx context.Context, job *Job, withNote bool) error {
	text := job.Text
	if withNote {
		text = strings.TrimRight(text, "\n") + "\n\n" + telegram.MediaUnavailableNote
	}
	return w.sender.SendMessageOnce(ctx, telegram.SendMessageRequest{
		ChatID: job.OwnerTelegramUserID,
		Text:   text,
		// BusinessConnectionID is intentionally left empty: the alert comes from the bot.
	})
}

// sendMedia uploads the files of a media job, in the payload order (which is
// the album order: message_id then file_index).
func (w *Worker) sendMedia(ctx context.Context, job *Job) error {
	mediaSender, ok := w.sender.(MediaSender)
	if !ok || w.mediaDir == "" || job.Media == nil {
		return errNoMediaDelivery
	}

	items := make([]telegram.MediaAlertItem, 0, len(job.Media.Items))
	for _, item := range job.Media.Items {
		path, err := w.mediaPath(item.RelativePath)
		if err != nil {
			return err
		}
		items = append(items, telegram.MediaAlertItem{
			Type:     item.MediaType,
			Path:     path,
			FileName: item.FileName,
			Caption:  item.Caption,
		})
	}
	return mediaSender.SendMediaOnce(ctx, telegram.MediaAlert{
		ChatID: job.OwnerTelegramUserID,
		Items:  items,
	})
}

// mediaPath resolves a stored relative path against the media root. The path
// was generated server-side and validated before being written to the
// database; it is validated AGAIN here, because this is the point where it
// becomes a file the bot opens and uploads to a chat.
func (w *Worker) mediaPath(relative string) (string, error) {
	if err := media.ValidateRelativePath(relative); err != nil {
		return "", err
	}
	return filepath.Join(w.mediaDir, relative), nil
}

// mediaIsHopeless reports an error that retrying could never clear, and that
// must therefore degrade to the text alert rather than consume the backoff.
func mediaIsHopeless(ctx context.Context, err error) bool {
	if isShutdown(ctx, err) {
		return false
	}
	switch {
	case errors.Is(err, errNoMediaDelivery),
		errors.Is(err, telegram.ErrMediaUnavailable),
		errors.Is(err, telegram.ErrMediaTooLarge),
		errors.Is(err, media.ErrUnsafeRelativePath):
		return true
	}
	var apiErr *telegram.APIError
	if errors.As(err, &apiErr) {
		// A 4xx on an upload is a definitive refusal (unsupported format,
		// file too big for the type, chat unreachable) -- except 429, which is
		// exactly what the outbox backoff exists for.
		return apiErr.Code >= http.StatusBadRequest &&
			apiErr.Code < http.StatusInternalServerError &&
			apiErr.Code != http.StatusTooManyRequests
	}
	return false
}

// mediaErrorClass names the cause for the logs. Like every other log in this
// package it exposes a CLASS, never a path, an id or a Telegram message.
func mediaErrorClass(err error) string {
	switch {
	case errors.Is(err, errNoMediaDelivery):
		return "media_unsupported"
	case errors.Is(err, telegram.ErrMediaUnavailable):
		return "media_missing"
	case errors.Is(err, telegram.ErrMediaTooLarge):
		return "media_too_large"
	case errors.Is(err, media.ErrUnsafeRelativePath):
		return "media_unsafe_path"
	}
	var apiErr *telegram.APIError
	if errors.As(err, &apiErr) {
		return fmt.Sprintf("telegram_%d", apiErr.Code)
	}
	return "media_error"
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
