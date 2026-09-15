package app

import (
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/LouisMoretti/Undelete/bot/internal/metrics"
	"github.com/LouisMoretti/Undelete/bot/internal/telegram"
)

// The poller advances its offset past a failed update (a poison update must
// never freeze the bot), so an update that fails on a database blip or a
// Telegram 5xx is simply never captured. transientRetry retries such an
// update in place, a few times, before letting it go.
//
// Replaying the whole update is safe for the same reason a crash redelivery
// is: every capture write is an idempotent upsert, and every step that
// returns an error runs before the non-idempotent side effects (command
// answers, the welcome message), which report their own failures instead of
// returning them.
const (
	transientAttempts  = 3
	transientBaseDelay = 500 * time.Millisecond
	// transientMaxWait caps one wait, a server-provided retry_after included:
	// the retry runs on a poller shard, and the next getUpdates waits for it.
	transientMaxWait = 5 * time.Second
	// transientCooldown is how long an exhausted budget keeps the handler in
	// degraded mode: every update then gets a single attempt, so a real
	// outage costs one retry budget per cooldown, not one per update --
	// retrying each update of a shard's queue in turn would stall every
	// tenant's freshness for minutes and still lose them all.
	transientCooldown = 30 * time.Second
)

type transientRetry struct {
	attempts  int
	baseDelay time.Duration
	maxWait   time.Duration
	cooldown  time.Duration
	sleep     func(context.Context, time.Duration) error
	now       func() time.Time
	// degradedUntil is a Unix nano deadline, 0 when healthy. Shared by every
	// shard worker, hence atomic.
	degradedUntil atomic.Int64
}

func newTransientRetry() *transientRetry {
	return &transientRetry{
		attempts:  transientAttempts,
		baseDelay: transientBaseDelay,
		maxWait:   transientMaxWait,
		cooldown:  transientCooldown,
		sleep:     sleepCtx,
		now:       time.Now,
	}
}

// run calls attempt until it succeeds, fails permanently, or the budget is
// spent. The error returned is the last attempt's.
func (r *transientRetry) run(ctx context.Context, attempt func() error) error {
	err := attempt()
	if err == nil {
		r.healthy()
		return nil
	}
	if !isTransient(err) {
		return err
	}
	budget := r.attempts
	if until := r.degradedUntil.Load(); until != 0 && r.now().UnixNano() < until {
		budget = 1
	}
	for i := 1; i < budget; i++ {
		metrics.AddUpdateRetries(1)
		if r.sleep(ctx, r.wait(err, i)) != nil {
			return err
		}
		err = attempt()
		if err == nil {
			r.healthy()
			return nil
		}
		if !isTransient(err) {
			return err
		}
	}
	r.degradedUntil.Store(r.now().Add(r.cooldown).UnixNano())
	metrics.AddUpdatesDroppedTransient(1)
	return err
}

func (r *transientRetry) healthy() {
	if r.degradedUntil.Load() != 0 {
		r.degradedUntil.Store(0)
	}
}

// wait honours a 429's retry_after (the service's own recovery timeline),
// otherwise backs off exponentially with jitter so the shards that failed on
// the same blip do not all come back at the same instant.
func (r *transientRetry) wait(err error, retry int) time.Duration {
	var apiErr *telegram.APIError
	if errors.As(err, &apiErr) && apiErr.IsRateLimited() {
		return min(time.Duration(apiErr.RetryAfter)*time.Second, r.maxWait)
	}
	d := min(r.baseDelay<<(retry-1), r.maxWait)
	if d <= 0 {
		return 0
	}
	return d/2 + rand.N(d/2+1)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// isTransient reports a failure that the same update could plausibly survive
// a second later: a rate limit or 5xx from Telegram, a lost or refused
// database connection, a serialization failure, a server restarting.
// Everything else -- a constraint violation, a 4xx, a timeout that already
// consumed its whole budget, a shutdown -- is final for this update.
func isTransient(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var apiErr *telegram.APIError
	if errors.As(err, &apiErr) {
		return apiErr.Code == http.StatusTooManyRequests || apiErr.Code >= http.StatusInternalServerError
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "40001", // serialization_failure
			"40P01", // deadlock_detected
			"57P01", // admin_shutdown
			"57P02", // crash_shutdown
			"57P03": // cannot_connect_now
			return true
		}
		// Class 08: connection exception. Class 53: insufficient resources.
		return strings.HasPrefix(pgErr.Code, "08") || strings.HasPrefix(pgErr.Code, "53")
	}
	if pgconn.Timeout(err) {
		return false
	}
	var connectErr *pgconn.ConnectError
	if errors.As(err, &connectErr) || pgconn.SafeToRetry(err) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return !netErr.Timeout()
	}
	return errors.Is(err, io.ErrUnexpectedEOF)
}
