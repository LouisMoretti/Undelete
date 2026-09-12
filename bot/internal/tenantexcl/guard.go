// Package tenantexcl coordinates, per tenant, the erasure of a tenant with
// the background workers that keep writing that same tenant's data.
//
// Without it, three interleavings defeat /delete_my_data, and all three are
// established by reading the code paths, not by any particular timing:
//
//   - the media fetcher loads a batch of pending rows, the erasure deletes
//     the catalogue and the tree, then the fetcher finishes a download and
//     writes a blob nothing references any more (the confirmation "gone from
//     the disk" becomes false);
//   - the outbox worker claims a job, the erasure deletes the row, then the
//     worker delivers the payload it already holds and acknowledges a lease
//     that no longer exists;
//   - two Confirm calls for the same code both enter the erasure: the claim
//     guarantees a single pending -> consumed transition, not a single
//     executor.
//
// The coordination is a per-tenant readers/writer lock. Workers hold the
// read side for the whole unit of work they must not be interrupted in the
// middle of (one fetch batch, one claim -> deliver -> acknowledge cycle);
// the erasure holds the write side for its whole run (claim -> steps ->
// complete). An in-flight unit finishes first, then the erasure drains what
// is left; a unit that starts after the erasure finds nothing to do.
//
// The scope is one process: the bot runs a single poller, a single fetcher
// and a single outbox loop, so an in-memory lock covers every writer. A
// second process with its own Guard would not be excluded; the deployment
// (a single container, docker-compose.yml) has exactly one of each.
package tenantexcl

import (
	"context"
	"sync"
)

// Guard holds one readers/writer lock per tenant, created on demand. The
// zero value is usable. A Guard must not be copied after first use.
type Guard struct {
	mu    sync.Mutex
	locks map[int64]*sync.RWMutex
}

// New returns a Guard ready to coordinate one process.
func New() *Guard {
	return &Guard{locks: make(map[int64]*sync.RWMutex)}
}

// forTenant returns the lock of a tenant, creating it once. The map only
// grows: tenants are long-lived rows of the users table, and one small lock
// per tenant ever seen is bounded by the tenant count itself.
func (g *Guard) forTenant(ownerUserID int64) *sync.RWMutex {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.locks == nil {
		g.locks = make(map[int64]*sync.RWMutex)
	}
	lock, ok := g.locks[ownerUserID]
	if !ok {
		lock = &sync.RWMutex{}
		g.locks[ownerUserID] = lock
	}
	return lock
}

// acquire runs lock() in a goroutine so a cancelled context stops waiting
// instead of blocking until the holder finishes. On cancellation the
// acquisition may still go through afterwards; the release function then runs
// immediately on the already-acquired lock, so no acquisition is ever leaked.
func acquire(ctx context.Context, lock func(), unlock func()) (func(), error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	done := make(chan struct{})
	go func() {
		lock()
		close(done)
	}()
	select {
	case <-ctx.Done():
		go func() {
			<-done
			unlock()
		}()
		return nil, ctx.Err()
	case <-done:
		return unlock, nil
	}
}

// Shared holds the read side for one tenant until release is called. Workers
// use it around a whole unit of work; while it is held no Exclusive of the
// same tenant can proceed, and while an Exclusive is in progress this call
// waits (or fails on a cancelled context) instead of starting new work.
func (g *Guard) Shared(ctx context.Context, ownerUserID int64) (release func(), err error) {
	lock := g.forTenant(ownerUserID)
	return acquire(ctx, lock.RLock, lock.RUnlock)
}

// Exclusive holds the write side for one tenant until release is called. The
// erasure uses it around its whole run; acquiring it waits for the in-flight
// units of that tenant to finish, and workers starting afterwards wait for
// the erasure instead of racing it.
func (g *Guard) Exclusive(ctx context.Context, ownerUserID int64) (release func(), err error) {
	lock := g.forTenant(ownerUserID)
	return acquire(ctx, lock.Lock, lock.Unlock)
}
