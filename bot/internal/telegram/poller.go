package telegram

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/LouisMoretti/Undelete/bot/internal/metrics"
)

const (
	pollTimeoutSeconds = 50 // below the HTTP client timeout (see NewClient)
	minBackoff         = time.Second
	maxBackoff         = time.Minute
)

// Handler processes an update. The returned error is logged but NEVER blocks
// offset advancement (see Poller.Run): a poisoned update (a handler that
// always fails) must never freeze the bot.
type Handler func(ctx context.Context, update Update) error

// Poller performs the getUpdates long polling and delivers updates to a
// Handler strictly sequentially.
//
// Non-negotiable constraint 5: Telegram delivers updates in emission order.
// A parallel worker pool could process a deleted_business_messages BEFORE the
// corresponding business_message (two concurrent goroutines, execution order
// not guaranteed): the deletion would then find nothing in the database even
// though the message definitely exists on Telegram's side. This loop therefore
// stays deliberately sequential, one update processed at a time. Future
// scaling will come through sharding on chat_id (several independent
// pollers/handlers, each responsible for a subset of chats, order preserved
// INSIDE each shard), never through an unordered worker pool on a single
// stream.
type Poller struct {
	client *Client
	logger *slog.Logger
	offset int64

	// lastSuccessUnixNano records the timestamp of the last successful
	// getUpdates. Written by the Run loop, read by the readiness probe from
	// another goroutine: hence the atomic, while offset stays a plain field
	// (never read outside Run).
	lastSuccessUnixNano atomic.Int64
}

func NewPoller(client *Client, logger *slog.Logger) *Poller {
	return &Poller{client: client, logger: logger}
}

// LastSuccessfulPoll returns the time of the last successful getUpdates, or
// the zero value if no poll has succeeded yet since startup. Serves as a
// freshness signal for the readiness (health.FreshnessSource).
func (p *Poller) LastSuccessfulPoll() time.Time {
	nanos := p.lastSuccessUnixNano.Load()
	if nanos == 0 {
		return time.Time{}
	}
	return time.Unix(0, nanos)
}

// Run loops until the context is cancelled.
func (p *Poller) Run(ctx context.Context, handle Handler) error {
	backoff := minBackoff

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		updates, err := p.client.GetUpdates(ctx, p.offset, pollTimeoutSeconds)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}

			wait := backoff
			var apiErr *APIError
			if errors.As(err, &apiErr) && apiErr.IsRateLimited() {
				// Strict respect of retry_after (429): Telegram tells us
				// exactly how long to wait, we don't apply our own backoff
				// on top.
				wait = time.Duration(apiErr.RetryAfter) * time.Second
			} else {
				backoff *= 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
			}

			metrics.AddUpdateErrors(1)
			p.logger.Error("getUpdates failed", slog.String("error", err.Error()), slog.Duration("wait", wait))
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
			continue
		}

		backoff = minBackoff // success: reset the backoff
		p.lastSuccessUnixNano.Store(time.Now().UnixNano())
		metrics.AddUpdates(int64(len(updates)))

		for _, u := range updates {
			if err := handle(ctx, u); err != nil {
				metrics.AddUpdateErrors(1)
				p.logger.Error("update handling failed",
					slog.Int64("update_id", u.UpdateID),
					slog.String("error", err.Error()))
			}
			// The offset advances EVEN IF the handler failed. Explicit
			// constraint: if we only advanced the offset on success, an
			// update that always fails (handling bug, violated DB
			// constraint, etc.) would be redelivered identically on every
			// poll and freeze the bot indefinitely on that single update.
			p.offset = u.UpdateID + 1
		}
	}
}
