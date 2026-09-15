package storage

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// InstanceLockKey is the session advisory lock one running bot holds for its
// whole life. The tenant exclusion (tenantexcl.Guard), the outbox lease and
// the erasure's step sequence are all sized for ONE process: a second one --
// a deploy overlapping the old container, a stray `docker compose run` --
// would bring back every interleaving the Guard exists to exclude, since its
// lock lives in memory. This key turns "the deployment runs one container"
// from a documented assumption into an enforced one.
const InstanceLockKey int64 = 74617309141002

// ErrInstanceLockLost means the lock's session dropped and another process
// took the lock before this one could take it back: the caller must stop.
var ErrInstanceLockLost = errors.New("storage: the instance lock was taken over by another process")

var errInstanceLockReleased = errors.New("storage: instance lock released")

const (
	defaultInstanceLockRetry = 5 * time.Second
	defaultInstanceLockCheck = 15 * time.Second
	instanceLockPingTimeout  = 5 * time.Second
)

// InstanceLock is the held lock. It owns a dedicated connection, outside the
// pool: a session-level advisory lock is tied to the session that took it,
// and a pooled connection could be recycled from under it.
type InstanceLock struct {
	dsn    string
	logger *slog.Logger
	retry  time.Duration
	check  time.Duration

	mu       sync.Mutex
	conn     *pgx.Conn
	released bool
}

// InstanceLockOption tunes the timings (tests).
type InstanceLockOption func(*InstanceLock)

// WithInstanceLockTiming sets how often a waiting or reconnecting process
// retries, and how often Hold checks the session.
func WithInstanceLockTiming(retry, check time.Duration) InstanceLockOption {
	return func(l *InstanceLock) {
		l.retry = retry
		l.check = check
	}
}

// AcquireInstanceLock blocks until this process holds the instance lock. A
// process finding it taken waits rather than failing: during a rollout the
// new container simply starts once the old one has stopped. An unreachable
// database fails at once, like any other boot step: the restart policy owns
// that retry.
func AcquireInstanceLock(ctx context.Context, dsn string, logger *slog.Logger, opts ...InstanceLockOption) (*InstanceLock, error) {
	l := &InstanceLock{dsn: dsn, logger: logger, retry: defaultInstanceLockRetry, check: defaultInstanceLockCheck}
	for _, opt := range opts {
		opt(l)
	}
	waiting := false
	for {
		held, err := l.tryLock(ctx)
		if err != nil {
			l.Release()
			return nil, err
		}
		if held {
			if waiting {
				logger.Info("instance lock acquired, starting")
			}
			return l, nil
		}
		if !waiting {
			logger.Warn("another bot instance holds the instance lock: waiting for it to stop")
			waiting = true
		}
		if err := sleepCtx(ctx, l.retry); err != nil {
			l.Release()
			return nil, err
		}
	}
}

// Hold keeps the lock until ctx ends. It checks the session periodically;
// when the session is gone (database restart, network cut), the server has
// released the lock with it, so Hold reconnects and takes it back. It returns
// ErrInstanceLockLost if another process got there first, ctx.Err() on
// shutdown. The connection is closed on return.
func (l *InstanceLock) Hold(ctx context.Context) error {
	defer l.Release()
	ticker := time.NewTicker(l.check)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
		if l.alive(ctx) {
			continue
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		l.logger.Warn("instance lock session lost, taking the lock back")
		if err := l.retake(ctx); errors.Is(err, errInstanceLockReleased) {
			return nil
		} else if err != nil {
			return err
		}
	}
}

func (l *InstanceLock) retake(ctx context.Context) error {
	for {
		held, err := l.tryLock(ctx)
		if held {
			l.logger.Info("instance lock taken back")
			return nil
		}
		if err == nil {
			return ErrInstanceLockLost
		}
		if errors.Is(err, errInstanceLockReleased) {
			return err
		}
		if err := sleepCtx(ctx, l.retry); err != nil {
			return err
		}
	}
}

// Release closes the session, which releases the lock server-side.
func (l *InstanceLock) Release() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.released = true
	l.closeLocked()
}

func (l *InstanceLock) alive(ctx context.Context) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.conn == nil {
		return false
	}
	pingCtx, cancel := context.WithTimeout(ctx, instanceLockPingTimeout)
	defer cancel()
	if err := l.conn.Ping(pingCtx); err != nil {
		l.closeLocked()
		return false
	}
	return true
}

// tryLock takes the lock on the current session, opening one if needed. A
// failed statement drops the session: its state is unknown.
func (l *InstanceLock) tryLock(ctx context.Context) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released {
		return false, errInstanceLockReleased
	}
	if l.conn == nil {
		conn, err := pgx.Connect(ctx, l.dsn)
		if err != nil {
			return false, fmt.Errorf("connecting for the instance lock: %w", err)
		}
		l.conn = conn
	}
	var held bool
	if err := l.conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, InstanceLockKey).Scan(&held); err != nil {
		l.closeLocked()
		return false, fmt.Errorf("taking the instance lock: %w", err)
	}
	return held, nil
}

func (l *InstanceLock) closeLocked() {
	if l.conn == nil {
		return
	}
	closeCtx, cancel := context.WithTimeout(context.Background(), instanceLockPingTimeout)
	defer cancel()
	_ = l.conn.Close(closeCtx)
	l.conn = nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
