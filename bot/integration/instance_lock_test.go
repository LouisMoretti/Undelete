//go:build integration

package integration_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/LouisMoretti/Undelete/bot/internal/storage"
)

// TestPostgreSQL16InstanceLock proves the single-instance guarantee against a
// real server: only a real session can be terminated from outside, and only
// the server decides who gets an advisory lock.
func TestPostgreSQL16InstanceLock(t *testing.T) {
	adminDSN := requireEnv(t, "POSTGRES_INTEGRATION_ADMIN_DSN")
	runtimeDSN := requireEnv(t, "POSTGRES_INTEGRATION_RUNTIME_DSN")
	ctx, cancel := context.WithTimeout(context.Background(), setupTimeout)
	defer cancel()

	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	defer admin.Close(ctx)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// holder returns the backend pid holding the instance lock, 0 if none. A
	// bigint advisory key is stored as (classid = high 32 bits, objid = low 32
	// bits, objsubid = 1).
	holder := func(t *testing.T) int32 {
		t.Helper()
		var pid int32
		err := admin.QueryRow(ctx, `
			SELECT pid FROM pg_locks
			WHERE locktype = 'advisory' AND granted AND objsubid = 1
			  AND classid = ($1::bigint >> 32)::oid
			  AND objid = ($1::bigint & 4294967295)::oid
		`, storage.InstanceLockKey).Scan(&pid)
		if errors.Is(err, pgx.ErrNoRows) {
			return 0
		}
		if err != nil {
			t.Fatalf("read the lock holder: %v", err)
		}
		return pid
	}
	terminate := func(t *testing.T, pid int32) {
		t.Helper()
		if _, err := admin.Exec(ctx, `SELECT pg_terminate_backend($1)`, pid); err != nil {
			t.Fatalf("terminate backend %d: %v", pid, err)
		}
	}
	eventually := func(t *testing.T, what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	acquireAsync := func(opts ...storage.InstanceLockOption) <-chan *storage.InstanceLock {
		out := make(chan *storage.InstanceLock, 1)
		go func() {
			lock, err := storage.AcquireInstanceLock(ctx, runtimeDSN, logger, opts...)
			if err != nil {
				t.Errorf("AcquireInstanceLock: %v", err)
				close(out)
				return
			}
			out <- lock
		}()
		return out
	}
	fast := storage.WithInstanceLockTiming(50*time.Millisecond, 100*time.Millisecond)

	t.Run("a second instance waits for the first to stop", func(t *testing.T) {
		first, err := storage.AcquireInstanceLock(ctx, runtimeDSN, logger, fast)
		if err != nil {
			t.Fatalf("first AcquireInstanceLock: %v", err)
		}
		second := acquireAsync(fast)
		select {
		case <-second:
			t.Fatal("a second instance acquired the lock while the first held it")
		case <-time.After(300 * time.Millisecond):
		}
		first.Release()
		select {
		case lock := <-second:
			if lock == nil {
				t.Fatal("the second instance failed instead of acquiring")
			}
			lock.Release()
		case <-time.After(5 * time.Second):
			t.Fatal("the second instance never acquired the released lock")
		}
		eventually(t, "the lock to be free", func() bool { return holder(t) == 0 })
	})

	t.Run("a dropped session is taken back", func(t *testing.T) {
		lock, err := storage.AcquireInstanceLock(ctx, runtimeDSN, logger, fast)
		if err != nil {
			t.Fatalf("AcquireInstanceLock: %v", err)
		}
		holdCtx, stopHold := context.WithCancel(ctx)
		held := make(chan error, 1)
		go func() { held <- lock.Hold(holdCtx) }()

		before := holder(t)
		if before == 0 {
			t.Fatal("no session holds the lock after AcquireInstanceLock")
		}
		terminate(t, before)
		eventually(t, "a new session to hold the lock", func() bool {
			now := holder(t)
			return now != 0 && now != before
		})
		select {
		case err := <-held:
			t.Fatalf("Hold returned %v after taking the lock back, want it to keep holding", err)
		default:
		}

		stopHold()
		if err := <-held; !errors.Is(err, context.Canceled) {
			t.Fatalf("Hold on shutdown = %v, want context.Canceled", err)
		}
		eventually(t, "the lock to be released on shutdown", func() bool { return holder(t) == 0 })
	})

	t.Run("a session taken over by another instance stops the holder", func(t *testing.T) {
		// The holder checks its session slowly, the rival retries fast: the
		// rival wins the lock between the drop and the holder noticing it.
		lock, err := storage.AcquireInstanceLock(ctx, runtimeDSN, logger,
			storage.WithInstanceLockTiming(50*time.Millisecond, 600*time.Millisecond))
		if err != nil {
			t.Fatalf("AcquireInstanceLock: %v", err)
		}
		held := make(chan error, 1)
		go func() { held <- lock.Hold(ctx) }()

		rival := acquireAsync(fast)
		time.Sleep(150 * time.Millisecond)
		terminate(t, holder(t))

		var won *storage.InstanceLock
		select {
		case won = <-rival:
			if won == nil {
				t.Fatal("the rival failed instead of acquiring")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the rival never acquired the dropped lock")
		}
		defer won.Release()

		select {
		case err := <-held:
			if !errors.Is(err, storage.ErrInstanceLockLost) {
				t.Fatalf("Hold = %v, want ErrInstanceLockLost", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the holder kept running after another instance took its lock")
		}
	})
}
