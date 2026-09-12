package outbox

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/LouisMoretti/Undelete/bot/internal/telegram"
	"github.com/LouisMoretti/Undelete/bot/internal/tenantexcl"
)

// blockingSender holds a delivery open until unblock is closed, modelling a
// job suspended after its reservation and before its send -- the window in
// which deleting the row cannot stop the send.
type blockingSender struct {
	mu       sync.Mutex
	requests []telegram.SendMessageRequest
	entered  chan struct{}
	unblock  chan struct{}
	once     sync.Once
}

func newBlockingSender() *blockingSender {
	return &blockingSender{entered: make(chan struct{}), unblock: make(chan struct{})}
}

func (s *blockingSender) SendMessageOnce(ctx context.Context, req telegram.SendMessageRequest) error {
	s.once.Do(func() { close(s.entered) })
	select {
	case <-s.unblock:
	case <-ctx.Done():
		return ctx.Err()
	}
	s.mu.Lock()
	s.requests = append(s.requests, req)
	s.mu.Unlock()
	return nil
}

// TestErasureWaitsForTheInFlightDelivery: a delivery suspended between its
// claim and its send holds the shared side for the whole ProcessOne, so the
// erasure's exclusive side -- the step that drains the queue -- cannot
// proceed until the send has settled. Deleting the row first would not stop
// this send; waiting for it means the drain that follows sees no in-flight
// remainder.
func TestErasureWaitsForTheInFlightDelivery(t *testing.T) {
	guard := tenantexcl.New()
	store := &fakeStore{job: testJob()}
	sender := newBlockingSender()
	worker := NewWorker(store, sender, silentLogger(), guard)

	processed := make(chan error, 1)
	go func() {
		_, err := worker.ProcessOne(context.Background(), 11)
		processed <- err
	}()

	select {
	case <-sender.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the worker never reached the delivery")
	}

	drained := make(chan struct{})
	go func() {
		release, err := guard.Exclusive(context.Background(), 11)
		if err != nil {
			t.Errorf("Exclusive: %v", err)
			return
		}
		defer release()
		close(drained)
	}()

	select {
	case <-drained:
		t.Fatal("the erasure drained the queue while a delivery was still suspended in it")
	case <-time.After(100 * time.Millisecond):
	}

	close(sender.unblock)

	select {
	case err := <-processed:
		if err != nil {
			t.Fatalf("ProcessOne: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the suspended delivery never settled")
	}
	if !store.sent {
		t.Fatal("the in-flight job was not acknowledged after its send")
	}

	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("the erasure never proceeded after the delivery settled")
	}
}

// TestWorkStartingDuringAnErasureWaitsForIt: a worker that wakes up while the
// tenant is being erased must not claim from a queue about to be drained; it
// waits, then claims afterwards -- from whatever the erasure left, here
// nothing.
func TestWorkStartingDuringAnErasureWaitsForIt(t *testing.T) {
	guard := tenantexcl.New()
	release, err := guard.Exclusive(context.Background(), 11)
	if err != nil {
		t.Fatalf("Exclusive: %v", err)
	}

	claimed := make(chan struct{})
	store := &claimSignallingStore{Store: &fakeStore{}, claimed: claimed}
	worker := NewWorker(store, &fakeSender{}, silentLogger(), guard)

	done := make(chan bool, 1)
	go func() {
		processed, err := worker.ProcessOne(context.Background(), 11)
		if err != nil {
			t.Errorf("ProcessOne: %v", err)
			done <- false
			return
		}
		done <- processed
	}()

	select {
	case <-claimed:
		t.Fatal("the worker claimed while the erasure held the tenant")
	case <-time.After(100 * time.Millisecond):
	}

	release()

	select {
	case processed := <-done:
		if processed {
			t.Fatal("the worker claimed a job from a queue the erasure drained")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the worker never proceeded after the erasure finished")
	}
}

// claimSignallingStore reports the first Claim so the test can tell a worker
// that reserved from one that is still waiting on the exclusion.
type claimSignallingStore struct {
	Store
	once    sync.Once
	claimed chan struct{}
}

func (s *claimSignallingStore) Claim(ctx context.Context, ownerUserID int64, lease time.Duration) (*Job, error) {
	s.once.Do(func() { close(s.claimed) })
	return s.Store.Claim(ctx, ownerUserID, lease)
}
