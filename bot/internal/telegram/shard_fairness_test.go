package telegram

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// TestDispatchSubmitsIdleShardsPastFullShard pins that a full slow shard
// queue never head-of-line-blocks the same batch's updates for idle shards.
//
// Batch: shardQueueDepth+2 updates on one parked partition (one in the
// worker, a full queue behind it, one more that cannot submit) followed by
// one update for an idle partition. Submitting strictly in batch order parks
// the whole submit on the slow shard and starves the idle update; the fair
// submit defers the overflowing slow update and delivers the idle one while
// the slow partition is still parked. It also pins that the deferral never
// reorders the slow partition itself.
func TestDispatchSubmitsIdleShardsPastFullShard(t *testing.T) {
	slowConn, slowChat := connChatForShard(t, 0)
	idleConn, idleChat := connChatForShard(t, 1)

	release := make(chan struct{})
	idleDone := make(chan struct{})
	var slowEnterOnce sync.Once
	slowEntered := make(chan struct{})
	var mu sync.Mutex
	var slowOrder []int64

	d := NewDispatcher(func(ctx context.Context, u Update) error {
		if u.BusinessMessage != nil && u.BusinessMessage.BusinessConnectionID == slowConn {
			slowEnterOnce.Do(func() { close(slowEntered) })
			mu.Lock()
			slowOrder = append(slowOrder, u.UpdateID)
			mu.Unlock()
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		select {
		case <-idleDone:
		default:
			close(idleDone)
		}
		return nil
	}, silentTestLogger())
	defer d.Stop()

	// One more slow update than the shard holds (worker + full queue): the
	// overflow is what used to wedge the submit.
	var batch []Update
	var id int64 = 1
	for i := 0; i < shardQueueDepth+2; i++ {
		batch = append(batch, probeMessage(slowConn, slowChat, id))
		id++
	}
	batch = append(batch, probeMessage(idleConn, idleChat, id))

	dispatchDone := make(chan []error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { dispatchDone <- d.Dispatch(ctx, batch) }()

	select {
	case <-slowEntered:
	case <-time.After(10 * time.Second):
		t.Fatal("slow partition never reached its worker")
	}
	// The slow worker is now parked with a full queue behind it and the
	// overflow unsubmitted: the idle update of the same batch must still go
	// through.
	select {
	case <-idleDone:
	case <-time.After(10 * time.Second):
		cancel()
		<-dispatchDone
		t.Fatal("idle partition's same-batch update starved behind the full slow shard queue")
	}

	close(release)
	var errs []error
	select {
	case errs = <-dispatchDone:
	case <-time.After(10 * time.Second):
		t.Fatal("Dispatch never returned after the slow partition was released")
	}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("errs[%d] = %v, want nil", i, err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(slowOrder) != shardQueueDepth+2 {
		t.Fatalf("slow partition completed %d updates, want %d", len(slowOrder), shardQueueDepth+2)
	}
	for i := 1; i < len(slowOrder); i++ {
		if slowOrder[i] != slowOrder[i-1]+1 {
			t.Fatalf("slow partition completion order = %v, want strict batch order (deferral must not reorder)", slowOrder)
		}
	}
}

// TestDispatchMarksNeverSubmittedOnCancel pins the C3 contract: when
// cancellation aborts the submit, slots no worker ever saw report
// ErrUpdateNotSubmitted (not a plain context error), while submitted slots
// keep the handler's own error. The poller tells the two apart and only
// stops its offset at the sentinel.
//
// The slow shard is primed deterministically: its worker is parked on a
// primer and its queue is filled to exactly shardQueueDepth by direct sends,
// so the overflow CANNOT submit whatever the scheduling -- no timing, no
// coin flips between a ready send and a fired context.
func TestDispatchMarksNeverSubmittedOnCancel(t *testing.T) {
	slowConn, slowChat := connChatForShard(t, 0)
	idleConn, idleChat := connChatForShard(t, 1)
	slowIdx := shardIndex(chatKey(slowConn, slowChat))

	release := make(chan struct{})
	entered := make(chan struct{})
	var enterOnce sync.Once
	idleDone := make(chan struct{})
	var idleOnce sync.Once

	d := NewDispatcher(func(ctx context.Context, u Update) error {
		if u.BusinessMessage != nil && u.BusinessMessage.BusinessConnectionID == slowConn {
			enterOnce.Do(func() { close(entered) })
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		idleOnce.Do(func() { close(idleDone) })
		return nil
	}, silentTestLogger())
	defer d.Stop()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Primer parks the slow worker inside its handler.
	primerDone := make(chan []error, 1)
	go func() { primerDone <- d.Dispatch(ctx, []Update{probeMessage(slowConn, slowChat, 1001)}) }()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("slow worker never parked")
	}

	// Fill the slow queue exactly: the worker is parked, so every send
	// lands in the buffer and none is consumed.
	for i := 0; i < shardQueueDepth; i++ {
		done := make(chan error, 1)
		d.shards[slowIdx] <- shardRequest{update: probeMessage(slowConn, slowChat, int64(2000+i)), ctx: ctx, done: done}
	}

	// Overflow (same slow shard, queue full: can never submit) plus one
	// idle update (free shard: submits and runs).
	batch := []Update{probeMessage(slowConn, slowChat, 3001), probeMessage(idleConn, idleChat, 3002)}
	dispatchDone := make(chan []error, 1)
	go func() { dispatchDone <- d.Dispatch(ctx, batch) }()

	select {
	case <-idleDone:
	case <-time.After(10 * time.Second):
		t.Fatal("idle update never ran while the slow shard stayed full")
	}
	// The overflow is still unsubmitted (queue full, worker parked):
	// cancelling now must mark it, not silently advance over it.
	cancel()

	var errs []error
	select {
	case errs = <-dispatchDone:
	case <-time.After(10 * time.Second):
		t.Fatal("Dispatch never returned after cancellation")
	}
	if len(errs) != 2 {
		t.Fatalf("errs has %d slots, want 2", len(errs))
	}
	if !errors.Is(errs[0], ErrUpdateNotSubmitted) {
		t.Fatalf("errs[0] = %v, want ErrUpdateNotSubmitted (the overflow never reached a worker)", errs[0])
	}
	if errs[1] != nil {
		t.Fatalf("errs[1] = %v, want nil (the idle update completed)", errs[1])
	}
}
