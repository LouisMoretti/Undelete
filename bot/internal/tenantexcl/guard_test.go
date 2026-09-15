package tenantexcl

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// TestExclusiveWaitsForInFlightShared: the erasure must not start deleting
// while a worker is mid-delivery; it proceeds once the worker is done.
func TestExclusiveWaitsForInFlightShared(t *testing.T) {
	g := New()
	ctx := context.Background()

	releaseShared, err := g.Shared(ctx, 11)
	if err != nil {
		t.Fatalf("Shared: %v", err)
	}

	acquired := make(chan struct{})
	go func() {
		release, err := g.Exclusive(ctx, 11)
		if err != nil {
			t.Errorf("Exclusive: %v", err)
			return
		}
		close(acquired)
		release()
	}()

	select {
	case <-acquired:
		t.Fatal("the exclusive side proceeded while shared work was in flight")
	case <-time.After(50 * time.Millisecond):
	}

	releaseShared()

	select {
	case <-acquired:
	case <-time.After(2 * time.Second):
		t.Fatal("the exclusive side never proceeded after the shared work finished")
	}
}

// TestSharedWaitsForExclusive: work that starts during an erasure waits
// instead of racing it; once the erasure is done the worker finds an empty
// queue on its own.
func TestSharedWaitsForExclusive(t *testing.T) {
	g := New()
	ctx := context.Background()

	releaseExcl, err := g.Exclusive(ctx, 11)
	if err != nil {
		t.Fatalf("Exclusive: %v", err)
	}

	acquired := make(chan struct{})
	go func() {
		release, err := g.Shared(ctx, 11)
		if err != nil {
			t.Errorf("Shared: %v", err)
			return
		}
		close(acquired)
		release()
	}()

	select {
	case <-acquired:
		t.Fatal("new work started in the middle of an erasure")
	case <-time.After(50 * time.Millisecond):
	}

	releaseExcl()

	select {
	case <-acquired:
	case <-time.After(2 * time.Second):
		t.Fatal("work never started after the erasure finished")
	}
}

// TestTenantsAreIndependent: one tenant's erasure never parks another
// tenant's workers.
func TestTenantsAreIndependent(t *testing.T) {
	g := New()
	ctx := context.Background()

	release, err := g.Exclusive(ctx, 11)
	if err != nil {
		t.Fatalf("Exclusive: %v", err)
	}
	defer release()

	done := make(chan struct{})
	go func() {
		rel, err := g.Shared(ctx, 22)
		if err != nil {
			t.Errorf("Shared of another tenant: %v", err)
			return
		}
		rel()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("another tenant's work blocked on an unrelated erasure")
	}
}

// TestCancelledWaitStopsWaiting: a worker or a second erasure whose context
// is gone must not wait for the holder; the holder's own release still works.
func TestCancelledWaitStopsWaiting(t *testing.T) {
	g := New()

	release, err := g.Exclusive(context.Background(), 11)
	if err != nil {
		t.Fatalf("Exclusive: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := g.Shared(ctx, 11); err == nil {
		t.Fatal("a cancelled Shared waited instead of failing")
	}
	if _, err := g.Exclusive(ctx, 11); err == nil {
		t.Fatal("a cancelled Exclusive waited instead of failing")
	}

	release()
	if _, err := g.Shared(context.Background(), 11); err != nil {
		t.Fatalf("the lock stayed held after release: %v", err)
	}
}

// TestZeroValueGuardIsUsable: a Guard left at its zero value lazily creates
// its lock map on first touch and coordinates like a Guard from New.
func TestZeroValueGuardIsUsable(t *testing.T) {
	var g Guard
	ctx := context.Background()

	releaseShared, err := g.Shared(ctx, 11)
	if err != nil {
		t.Fatalf("Shared on a zero Guard: %v", err)
	}

	// Another tenant is unaffected while the first is held.
	releaseOther, err := g.Shared(ctx, 22)
	if err != nil {
		releaseShared()
		t.Fatalf("Shared of another tenant on a zero Guard: %v", err)
	}
	releaseOther()

	// The same tenant still excludes: an erasure waits for the worker.
	acquired := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		release, err := g.Exclusive(ctx, 11)
		if err != nil {
			errCh <- err
			return
		}
		close(acquired)
		release()
	}()

	select {
	case err := <-errCh:
		releaseShared()
		t.Fatalf("Exclusive: %v", err)
	case <-acquired:
		releaseShared()
		t.Fatal("the exclusive side proceeded while shared work was in flight")
	case <-time.After(100 * time.Millisecond):
	}

	releaseShared()

	select {
	case err := <-errCh:
		t.Fatalf("Exclusive: %v", err)
	case <-acquired:
	case <-time.After(5 * time.Second):
		t.Fatal("the exclusive side never proceeded after the shared work finished")
	}
}

// TestSharedWaitCancelledMidFlightReleasesAcquisition: a worker that stops
// waiting for an erasure (context gone while parked in the lock) must not
// leave an acquisition behind: once the erasure finishes, the tenant accepts
// new work again.
func TestSharedWaitCancelledMidFlightReleasesAcquisition(t *testing.T) {
	g := New()

	releaseExcl, err := g.Exclusive(context.Background(), 11)
	if err != nil {
		t.Fatalf("Exclusive: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	waiterDone := make(chan error, 1)
	go func() {
		release, err := g.Shared(ctx, 11)
		if err == nil {
			release()
		}
		waiterDone <- err
	}()

	// The waiter must be parked inside the lock, not already returned.
	select {
	case err := <-waiterDone:
		cancel()
		releaseExcl()
		t.Fatalf("Shared returned before its context was cancelled: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	cancel()

	select {
	case err := <-waiterDone:
		if !errors.Is(err, context.Canceled) {
			releaseExcl()
			t.Fatalf("cancelled Shared returned %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		releaseExcl()
		t.Fatal("a Shared waiter whose context was cancelled never stopped waiting")
	}

	releaseExcl()

	// The cancelled acquisition went through behind the release and its
	// cleanup must have unlocked it: the tenant accepts an erasure again.
	probeCtx, probeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer probeCancel()
	releaseProbe, err := g.Exclusive(probeCtx, 11)
	if err != nil {
		t.Fatalf("tenant lock stayed held after a cancelled waiter: %v", err)
	}
	releaseProbe()
}

// TestExclusiveWaitCancelledMidFlightReleasesAcquisition: mirror of the above
// for a second erasure parked behind in-flight work. Cancelling it must leave
// the tenant usable for workers afterwards.
func TestExclusiveWaitCancelledMidFlightReleasesAcquisition(t *testing.T) {
	g := New()

	releaseShared, err := g.Shared(context.Background(), 11)
	if err != nil {
		t.Fatalf("Shared: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	waiterDone := make(chan error, 1)
	go func() {
		release, err := g.Exclusive(ctx, 11)
		if err == nil {
			release()
		}
		waiterDone <- err
	}()

	// The waiter must be parked inside the lock, not already returned.
	select {
	case err := <-waiterDone:
		cancel()
		releaseShared()
		t.Fatalf("Exclusive returned before its context was cancelled: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	cancel()

	select {
	case err := <-waiterDone:
		if !errors.Is(err, context.Canceled) {
			releaseShared()
			t.Fatalf("cancelled Exclusive returned %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		releaseShared()
		t.Fatal("an Exclusive waiter whose context was cancelled never stopped waiting")
	}

	releaseShared()

	// The cancelled acquisition went through behind the release and its
	// cleanup must have unlocked it: the tenant accepts workers again.
	probeCtx, probeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer probeCancel()
	releaseProbe, err := g.Shared(probeCtx, 11)
	if err != nil {
		t.Fatalf("tenant lock stayed held after a cancelled waiter: %v", err)
	}
	releaseProbe()
}

// TestExclusiveCannotReacquireShared: the per-tenant lock is not reentrant.
// EraseTenant holds the exclusive side for its whole run, so it must never
// take the shared side of the same tenant inside it: that would wait on
// itself forever (self-deadlock). A bounded context turns that programming
// error into a visible failure instead of a wedged erasure.
func TestExclusiveCannotReacquireShared(t *testing.T) {
	g := New()

	release, err := g.Exclusive(context.Background(), 11)
	if err != nil {
		t.Fatalf("Exclusive: %v", err)
	}
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if nested, err := g.Shared(ctx, 11); err == nil {
		nested()
		t.Fatal("shared side acquirable while the exclusive side is held: re-acquiring it would self-deadlock the erasure")
	} else if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("nested Shared returned %v, want context.DeadlineExceeded", err)
	}

	// The failed nesting disturbs nothing: other tenants proceed.
	releaseOther, err := g.Shared(context.Background(), 22)
	if err != nil {
		t.Fatalf("Shared of another tenant: %v", err)
	}
	releaseOther()
}

// TestConcurrentFirstUseCreatesSingleTenantLock: the first touch of a tenant
// races the lazy creation of its lock; concurrent workers must still land on
// one shared lock, otherwise an erasure could drain one lock while workers
// hold another.
func TestConcurrentFirstUseCreatesSingleTenantLock(t *testing.T) {
	var g Guard // Zero value: first touches also race the lazy map init.
	const workers = 16

	start := make(chan struct{})
	releaseAll := make(chan struct{})
	var unblockOnce sync.Once
	unblock := func() { unblockOnce.Do(func() { close(releaseAll) }) }

	holding := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			release, err := g.Shared(context.Background(), 77)
			if err != nil {
				t.Errorf("Shared: %v", err)
				return
			}
			defer release()
			holding <- struct{}{}
			<-releaseAll
		}()
	}

	waitDone := make(chan struct{})
	go func() {
		wg.Wait()
		close(waitDone)
	}()
	waitWorkers := func() bool {
		select {
		case <-waitDone:
			return true
		case <-time.After(5 * time.Second):
			return false
		}
	}

	close(start)

	// Every worker holds at once: shared holders coexist on one lock.
	for i := 0; i < workers; i++ {
		select {
		case <-holding:
		case <-time.After(5 * time.Second):
			unblock()
			waitWorkers()
			t.Fatalf("only %d/%d workers acquired the shared side", i, workers)
		}
	}

	// While they all hold, one tenant's erasure waits for every one of them.
	exclAcquired := make(chan struct{})
	exclErr := make(chan error, 1)
	go func() {
		release, err := g.Exclusive(context.Background(), 77)
		if err != nil {
			exclErr <- err
			return
		}
		close(exclAcquired)
		release()
	}()
	select {
	case err := <-exclErr:
		unblock()
		waitWorkers()
		t.Fatalf("Exclusive: %v", err)
	case <-exclAcquired:
		unblock()
		waitWorkers()
		t.Fatal("the exclusive side proceeded while shared work was in flight")
	case <-time.After(100 * time.Millisecond):
	}

	unblock()
	if !waitWorkers() {
		t.Fatal("workers never released the shared side")
	}

	select {
	case err := <-exclErr:
		t.Fatalf("Exclusive: %v", err)
	case <-exclAcquired:
	case <-time.After(5 * time.Second):
		t.Fatal("the exclusive side never proceeded after the shared work finished")
	}

	// One tenant, one lock, stable across lookups.
	g.mu.Lock()
	n := len(g.locks)
	g.mu.Unlock()
	if n != 1 {
		t.Fatalf("one tenant created %d locks, want 1", n)
	}
	if g.forTenant(77) != g.forTenant(77) {
		t.Fatal("forTenant returned different locks for the same tenant")
	}
}
