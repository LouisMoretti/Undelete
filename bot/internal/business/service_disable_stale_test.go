package business

import (
	"context"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// staleReadPool scripts a database whose resolution read predates a
// DisableOwner: the SELECT blocks until the test releases it and then returns
// the pre-disable (enabled) row, exactly the snapshot an in-flight Resolve
// holds while the erasure commits.
type staleReadPool struct {
	t           *testing.T
	conn        Connection
	readStarted chan struct{}
	releaseRead chan struct{}
	startOnce   sync.Once
}

func (p *staleReadPool) QueryRow(_ context.Context, _ string, args ...any) pgx.Row {
	if len(args) != 1 {
		p.t.Errorf("QueryRow args = %v, want the connection id", args)
		return fakeRow{err: pgx.ErrNoRows}
	}
	p.startOnce.Do(func() { close(p.readStarted) })
	<-p.releaseRead
	return fakeRow{conn: p.conn}
}

func (p *staleReadPool) Exec(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
	return pgconn.NewCommandTag("UPDATE 1"), nil
}

// TestResolveStaleDBReadCannotOverwriteDisable pins the B1 invariant:
// post-DisableOwner, no Resolve may serve enabled from a database read that
// predates the disable.
//
// Schedule (barrier-synchronised, no sleeps): a Resolve blocks inside its
// database read holding the pre-disable snapshot; DisableOwner commits
// (database UPDATE, then cache patch) while it is blocked; only then is the
// read released and the stale enabled row stored. The stored entry -- and
// every later Resolve -- must read disabled. With an unconditional store the
// stale write lands last and the final Resolve serves enabled until the TTL,
// keeping the erasure capturing for a tenant being erased.
func TestResolveStaleDBReadCannotOverwriteDisable(t *testing.T) {
	enabled := Connection{ID: "bc-stale", OwnerUserID: 7, OwnerTelegramUserID: 700, CanReply: true, IsEnabled: true}
	pool := &staleReadPool{
		t:           t,
		conn:        enabled,
		readStarted: make(chan struct{}),
		releaseRead: make(chan struct{}),
	}
	svc := NewService(pool, &concAPI{}, &concUsers{}, nil, testLogger())

	resolveErr := make(chan error, 1)
	go func() {
		_, err := svc.Resolve(context.Background(), "bc-stale")
		resolveErr <- err
	}()

	// The Resolve is now blocked inside its database read, holding the
	// pre-disable snapshot. Disable while it waits.
	<-pool.readStarted
	if _, err := svc.DisableOwner(context.Background(), 7); err != nil {
		t.Fatalf("DisableOwner = %v, want nil", err)
	}

	// Release the stale read: its store lands strictly after the disable.
	close(pool.releaseRead)
	if err := <-resolveErr; err != nil {
		t.Fatalf("in-flight Resolve = %v, want nil", err)
	}

	got, err := svc.Resolve(context.Background(), "bc-stale")
	if err != nil {
		t.Fatalf("post-disable Resolve = %v, want nil", err)
	}
	if got.IsEnabled {
		t.Fatal("post-disable Resolve returns IsEnabled=true (stale cache overwrite); the database row is disabled")
	}
	entry, ok := svc.cache.lookup("bc-stale")
	if !ok {
		t.Fatal("post-disable Resolve missed the cache")
	}
	if entry.conn.IsEnabled {
		t.Fatal("cache holds IsEnabled=true after DisableOwner: a stale database read overwrote the disable")
	}
}
