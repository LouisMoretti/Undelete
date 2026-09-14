package telegram

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestShardKeyDeterministic pins the partition contract of issue #18: the key
// is connection + chat, stable for one update no matter how often it is
// computed.
func TestShardKeyDeterministic(t *testing.T) {
	msg := func(conn string, chat int64) *Message {
		return &Message{MessageID: 9, BusinessConnectionID: conn, Chat: Chat{ID: chat, Type: "private"}}
	}

	same := []Update{
		{UpdateID: 1, BusinessMessage: msg("bc-1", 77)},
		{UpdateID: 2, EditedBusinessMessage: msg("bc-1", 77)},
		{UpdateID: 3, DeletedBusinessMessages: &BusinessMessagesDeleted{BusinessConnectionID: "bc-1", Chat: Chat{ID: 77}}},
	}
	first := ShardKey(same[0])
	if first == "" {
		t.Fatal("ShardKey of a business message must not be empty")
	}
	for i, u := range same[1:] {
		if got := ShardKey(u); got != first {
			t.Fatalf("update type %d shards to %q, want %q (message, edit and deletion of one chat share a partition)", i, got, first)
		}
		if got := ShardKey(u); got != ShardKey(u) {
			t.Fatal("ShardKey is not deterministic")
		}
	}

	otherChat := ShardKey(Update{UpdateID: 4, BusinessMessage: msg("bc-1", 78)})
	otherConn := ShardKey(Update{UpdateID: 5, BusinessMessage: msg("bc-2", 77)})
	if otherChat == first {
		t.Fatal("two chats of one connection must not share a partition")
	}
	if otherConn == first {
		t.Fatal("two connections must not share a partition")
	}

	connKey := ShardKey(Update{UpdateID: 6, BusinessConnection: &BusinessConnection{ID: "bc-1"}})
	if connKey == "" || connKey != ShardKey(Update{UpdateID: 7, BusinessConnection: &BusinessConnection{ID: "bc-1"}}) {
		t.Fatal("business_connection updates need a stable, non-empty partition key")
	}
}

// TestDispatcherPreservesPerPartitionOrder is the core of issue #18: within
// one partition, updates complete in submission order even while other
// partitions run concurrently. A deletion must never overtake the save it
// refers to.
func TestDispatcherPreservesPerPartitionOrder(t *testing.T) {
	var mu sync.Mutex
	order := map[string][]int64{}

	d := NewDispatcher(func(_ context.Context, u Update) error {
		mu.Lock()
		order[ShardKey(u)] = append(order[ShardKey(u)], u.UpdateID)
		mu.Unlock()
		// Yield so a broken dispatcher that ran partitions on one worker
		// interleaved, or one that reordered a queue, shows up.
		time.Sleep(time.Millisecond)
		return nil
	}, silentTestLogger())
	defer d.Stop()

	const perPartition = 25
	var batch []Update
	for id := int64(1); id <= perPartition; id++ {
		batch = append(batch,
			Update{UpdateID: id * 3, BusinessMessage: &Message{MessageID: id, BusinessConnectionID: "bc-A", Chat: Chat{ID: 1, Type: "private"}}},
			Update{UpdateID: id*3 + 1, BusinessMessage: &Message{MessageID: id, BusinessConnectionID: "bc-B", Chat: Chat{ID: 2, Type: "private"}}},
			Update{UpdateID: id*3 + 2, BusinessMessage: &Message{MessageID: id, BusinessConnectionID: "bc-C", Chat: Chat{ID: 3, Type: "private"}}},
		)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if errs := d.Dispatch(ctx, batch); len(errs) != len(batch) {
		t.Fatalf("Dispatch returned %d errors, want %d", len(errs), len(batch))
	} else {
		for i, err := range errs {
			if err != nil {
				t.Fatalf("batch[%d] error = %v, want nil", i, err)
			}
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(order) != 3 {
		t.Fatalf("partitions touched = %d, want 3", len(order))
	}
	for key, ids := range order {
		if len(ids) != perPartition {
			t.Fatalf("partition %q got %d updates, want %d", key, len(ids), perPartition)
		}
		for i := 1; i < len(ids); i++ {
			if ids[i] < ids[i-1] {
				t.Fatalf("partition %q out of order: %v", key, ids)
			}
		}
	}
}

// TestDispatcherProcessesPartitionsConcurrently proves the throughput half of
// issue #18: two partitions overlap in time. The first submitted update waits
// for the second partition to start; on a sequential loop it would time out.
func TestDispatcherProcessesPartitionsConcurrently(t *testing.T) {
	secondStarted := make(chan struct{})
	releaseFirst := make(chan struct{})

	msg := func(conn string, chat int64) *Message {
		return &Message{MessageID: 1, BusinessConnectionID: conn, Chat: Chat{ID: chat, Type: "private"}}
	}
	d := NewDispatcher(func(_ context.Context, u Update) error {
		switch u.UpdateID {
		case 1:
			select {
			case <-secondStarted:
			case <-time.After(10 * time.Second):
				return errors.New("first partition never overlapped with the second: processing is sequential")
			}
			close(releaseFirst)
		case 2:
			close(secondStarted)
			select {
			case <-releaseFirst:
			case <-time.After(10 * time.Second):
				return errors.New("second partition stuck")
			}
		}
		return nil
	}, silentTestLogger())
	defer d.Stop()

	batch := []Update{
		{UpdateID: 1, BusinessMessage: msg("bc-A", 1)},
		{UpdateID: 2, BusinessMessage: msg("bc-B", 2)},
	}
	if got := ShardKey(batch[0]); got == ShardKey(batch[1]) {
		t.Fatal("test setup broken: the two updates must land on different partitions")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for i, err := range d.Dispatch(ctx, batch) {
		if err != nil {
			t.Fatalf("batch[%d] error = %v", i, err)
		}
	}
}

// connChatForShard finds a (connection, chat) pair hashing onto the given
// shard. The dispatcher under test spreads by hash, so tests that need every
// worker busy (backpressure, shutdown) place their updates deterministically
// instead of hoping a random spread covers all shards.
func connChatForShard(t *testing.T, shard int) (string, int64) {
	t.Helper()
	conn := fmt.Sprintf("bc-probe-%d", shard)
	for chat := int64(0); ; chat++ {
		if shardIndex(chatKey(conn, chat)) == shard {
			return conn, chat
		}
	}
}

func probeMessage(conn string, chat, id int64) Update {
	return Update{UpdateID: id, BusinessMessage: &Message{
		MessageID:            id,
		BusinessConnectionID: conn,
		Chat:                 Chat{ID: chat, Type: "private"},
	}}
}

// TestDispatcherBackpressureBounded pins the memory bound: with every worker
// parked, exactly numShards*(shardQueueDepth+1) updates are accepted and the
// next submit blocks until a worker moves.
func TestDispatcherBackpressureBounded(t *testing.T) {
	entered := make(chan struct{}, numShards*(shardQueueDepth+2))
	release := make(chan struct{})

	d := NewDispatcher(func(ctx context.Context, _ Update) error {
		entered <- struct{}{}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}, silentTestLogger())
	defer d.Stop()

	// Primer: one update per shard, so every worker parks inside the handler
	// before the fill below is submitted. Same (connection, chat) per shard
	// so the placement is exact, not a statistical spread.
	var primer []Update
	var id int64 = 1
	for shard := 0; shard < numShards; shard++ {
		conn, chat := connChatForShard(t, shard)
		primer = append(primer, probeMessage(conn, chat, id))
		id++
	}
	primerDone := make(chan []error, 1)
	go func() { primerDone <- d.Dispatch(context.Background(), primer) }()
	for i := 0; i < numShards; i++ {
		select {
		case <-entered:
		case <-time.After(10 * time.Second):
			t.Fatal("workers did not park")
		}
	}

	// Fill: shardQueueDepth more updates per shard (every queue exactly
	// full), plus one extra for shard 0. Every slot is taken and no worker
	// can move before release, so the extra submit must block.
	var fill []Update
	for shard := 0; shard < numShards; shard++ {
		conn, chat := connChatForShard(t, shard)
		for n := 0; n < shardQueueDepth; n++ {
			fill = append(fill, probeMessage(conn, chat, id))
			id++
		}
	}
	conn0, chat0 := connChatForShard(t, 0)
	fill = append(fill, probeMessage(conn0, chat0, id))
	fillDone := make(chan []error, 1)
	go func() { fillDone <- d.Dispatch(context.Background(), fill) }()

	// The dispatcher must absorb exactly its bounded capacity, then block:
	// every worker holds one update and every queue holds shardQueueDepth.
	select {
	case <-fillDone:
		t.Fatal("Dispatch returned while all workers are parked: the queue is unbounded")
	case <-time.After(200 * time.Millisecond):
	}
	close(release)

	for name, ch := range map[string]chan []error{"primer": primerDone, "fill": fillDone} {
		select {
		case errs := <-ch:
			for i, err := range errs {
				if err != nil {
					t.Fatalf("%s[%d] error after release = %v", name, i, err)
				}
			}
		case <-time.After(30 * time.Second):
			t.Fatalf("%s Dispatch did not finish after the workers were released", name)
		}
	}
}

// TestDispatcherHandlerErrorIsolated pins the poisoned-update rule under
// sharding: one failing update reports its error in its own slot and never
// blocks the others.
func TestDispatcherHandlerErrorIsolated(t *testing.T) {
	poison := errors.New("poisoned update")
	d := NewDispatcher(func(_ context.Context, u Update) error {
		if u.UpdateID == 2 {
			return poison
		}
		return nil
	}, silentTestLogger())
	defer d.Stop()

	batch := []Update{
		{UpdateID: 1, BusinessMessage: &Message{MessageID: 1, BusinessConnectionID: "bc-A", Chat: Chat{ID: 1}}},
		{UpdateID: 2, BusinessMessage: &Message{MessageID: 2, BusinessConnectionID: "bc-B", Chat: Chat{ID: 2}}},
		{UpdateID: 3, BusinessMessage: &Message{MessageID: 3, BusinessConnectionID: "bc-C", Chat: Chat{ID: 3}}},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	errs := d.Dispatch(ctx, batch)
	if len(errs) != 3 {
		t.Fatalf("Dispatch returned %d errors, want 3", len(errs))
	}
	if errs[0] != nil || errs[2] != nil {
		t.Fatalf("healthy updates must succeed, got %v / %v", errs[0], errs[2])
	}
	if !errors.Is(errs[1], poison) {
		t.Fatalf("batch[1] error = %v, want the poison", errs[1])
	}
}

// TestDispatcherSurvivesHandlerPanic pins the failure isolation one step
// further than the poisoned update: a panicking handler is reported as an
// error in its own slot, and its partition keeps serving later updates
// instead of wedging its queue behind a dead worker.
func TestDispatcherSurvivesHandlerPanic(t *testing.T) {
	d := NewDispatcher(func(_ context.Context, u Update) error {
		if u.UpdateID == 1 {
			panic("boom")
		}
		return nil
	}, silentTestLogger())
	defer d.Stop()

	same := &Message{MessageID: 1, BusinessConnectionID: "bc-A", Chat: Chat{ID: 1, Type: "private"}}
	batch := []Update{
		{UpdateID: 1, BusinessMessage: same},
		{UpdateID: 2, BusinessMessage: same},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	errs := d.Dispatch(ctx, batch)
	if len(errs) != 2 {
		t.Fatalf("Dispatch returned %d errors, want 2", len(errs))
	}
	if errs[0] == nil {
		t.Fatal("panicking update must report an error, got nil")
	}
	if errs[1] != nil {
		t.Fatalf("update behind a panic must still be served, got %v", errs[1])
	}
}

// TestDispatcherShutdownAbortsPending pins the clean-stop half of issue #18:
// cancelling the context unblocks Dispatch promptly instead of hanging on a
// parked shard.
func TestDispatcherShutdownAbortsPending(t *testing.T) {
	entered := make(chan struct{}, numShards*2)
	d := NewDispatcher(func(ctx context.Context, _ Update) error {
		entered <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	}, silentTestLogger())
	defer d.Stop()

	ctx, cancel := context.WithCancel(context.Background())
	// One update per shard, placed exactly: every worker parks in the
	// handler, none idles on an empty queue.
	var batch []Update
	for shard := 0; shard < numShards; shard++ {
		conn, chat := connChatForShard(t, shard)
		batch = append(batch, probeMessage(conn, chat, int64(shard+1)))
	}
	done := make(chan []error, 1)
	go func() { done <- d.Dispatch(ctx, batch) }()

	// Wait until every worker is parked in the handler, then cancel: Dispatch
	// must return instead of waiting for handlers that only unblock on cancel.
	for i := 0; i < numShards; i++ {
		select {
		case <-entered:
		case <-time.After(10 * time.Second):
			t.Fatal("workers did not start")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Dispatch did not abort on context cancellation")
	}
}
