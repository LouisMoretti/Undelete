package tenantexcl

import (
	"context"
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

// TestConcurrentSharedHoldersCoexist: parallel tenants and parallel workers
// of one tenant never exclude each other, only the erasure excludes.
func TestConcurrentSharedHoldersCoexist(t *testing.T) {
	g := New()
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := g.Shared(ctx, 11)
			if err != nil {
				t.Errorf("Shared: %v", err)
				return
			}
			time.Sleep(10 * time.Millisecond)
			release()
		}()
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent shared holders deadlocked")
	}
}
