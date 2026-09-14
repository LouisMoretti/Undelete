package telegram

import (
	"context"
	"errors"
	"fmt"
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
// Handler sharded by chat.
//
// Fetch stays single and sequential: this loop is the only owner of the
// Telegram offset, so the acknowledgement order (and therefore the redelivery
// on crash) is exactly what it was when every update was handled inline.
// Execution is sharded instead: each fetched batch is dispatched to a
// Dispatcher, whose workers run different (connection, chat) partitions
// concurrently while preserving a strict FIFO order inside each partition.
//
// Why this split, and not a worker pool on the stream: a pool could process
// a deleted_business_messages BEFORE the corresponding business_message (two
// concurrent goroutines, execution order not guaranteed): the deletion would
// then find nothing in the database even though the message definitely
// exists on Telegram's side. Sharding on (connection, chat) keeps the save,
// the edit and the deletion of one chat on one FIFO worker, so that order
// can never invert, while two unrelated chats never wait on each other.
// Non-negotiable constraint 5 (Telegram delivers updates in emission order)
// is therefore honoured per partition rather than globally: the global order
// was only ever a means to the per-chat one, and the global form is what
// capped the throughput of every tenant on the slowest one.
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
	if logger == nil {
		// A nil logger must not panic the loop on the first network error:
		// default to discard rather than to trust every caller.
		logger = slog.New(discardHandler{})
	}
	return &Poller{client: client, logger: logger}
}

// discardHandler drops every record. Used as the last-resort logger when a
// constructor receives nil: logging nothing beats panicking.
type discardHandler struct{}

func (discardHandler) Enabled(context.Context, slog.Level) bool  { return false }
func (discardHandler) Handle(context.Context, slog.Record) error { return nil }
func (discardHandler) WithAttrs([]slog.Attr) slog.Handler        { return discardHandler{} }
func (discardHandler) WithGroup(string) slog.Handler             { return discardHandler{} }

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

// pollWait returns how long Run sleeps after a getUpdates failure: the
// exponential backoff, or retry_after on a 429, capped at maxBackoff so one
// abusive retry_after cannot freeze the sequential loop.
func pollWait(backoff time.Duration, err error) time.Duration {
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.IsRateLimited() {
		wait := time.Duration(apiErr.RetryAfter) * time.Second
		if wait > maxBackoff {
			return maxBackoff
		}
		return wait
	}
	return backoff
}

// ErrPollTimeoutTooShort is wrapped by Run when the HTTP client cannot
// survive the long-poll wait: the client would cut every getUpdates before
// Telegram answers, and the bot would spin on errors forever. Failing loudly
// at startup instead. Exported sentinel so the cause is identifiable with
// errors.Is rather than by the message text.
var ErrPollTimeoutTooShort = errors.New("telegram: HTTP client timeout too short for the long-poll wait")

// Run loops until the context is cancelled.
func (p *Poller) Run(ctx context.Context, handle Handler) error {
	// The invariant the long poll depends on: the HTTP client must outlive
	// the 50s server wait, otherwise every poll is cut before Telegram
	// answers and the bot spins on errors forever. Enforced here, not in a
	// comment: NewClient is also used for short-timeout unit clients that
	// must stay fast.
	if timeout := p.client.httpClient.Timeout; timeout <= pollTimeoutSeconds*time.Second {
		return fmt.Errorf("%w: timeout %v, long-poll wait %ds", ErrPollTimeoutTooShort, timeout, pollTimeoutSeconds)
	}

	backoff := minBackoff

	// The shard workers live as long as the run: one fixed set of
	// goroutines, never one per update. The Handler is called from them from
	// now on and must be safe for concurrent use (cf. NewDispatcher).
	dispatcher := NewDispatcher(handle, p.logger)
	defer dispatcher.Stop()

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		updates, err := p.client.GetUpdates(ctx, p.offset, pollTimeoutSeconds)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}

			wait := pollWait(backoff, err)
			var rateLimited *APIError
			if !(errors.As(err, &rateLimited) && rateLimited.IsRateLimited()) {
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

		// Sharded execution, sequential acknowledgement: every update of the
		// batch is processed on its partition's FIFO worker (partitions
		// concurrently, each partition in order), and the offset advances
		// over the batch only once all of them have completed. A handler
		// error is still per-update signal, and the offset still advances
		// past it: a poisoned update can delay the batch, never freeze the
		// bot. On shutdown the context aborts the wait; the offset then
		// advances only over the contiguous submitted prefix: Dispatch
		// reports ErrUpdateNotSubmitted for updates no worker ever saw,
		// and advancing over those would skip work no server-side
		// acknowledgement covers. The process exits right after, so the
		// next run redelivers from the last acked offset; every capture
		// write is idempotent.
		errs := dispatcher.Dispatch(ctx, updates)
		for i, u := range updates {
			if err := errs[i]; err != nil {
				metrics.AddUpdateErrors(1)
				p.logger.Error("update handling failed",
					slog.Int64("update_id", u.UpdateID),
					slog.String("error", err.Error()))
				if errors.Is(err, ErrUpdateNotSubmitted) {
					// Aborted before this update reached a worker (and
					// therefore everything past it too): stop the
					// acknowledgement here, the redelivery replays from
					// this update.
					break
				}
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
