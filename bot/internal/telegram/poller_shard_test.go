package telegram

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// shardedBatchJSON builds one getUpdates batch carrying updates for two
// different chats: update ids odd for chat 11 on bc-A, even for chat 22 on
// bc-B.
func shardedBatchJSON(ids ...int64) string {
	out := ""
	for i, id := range ids {
		conn, chat := "bc-A", 11
		if id%2 == 0 {
			conn, chat = "bc-B", 22
		}
		if i > 0 {
			out += ","
		}
		out += fmt.Sprintf(`{"update_id":%d,"business_message":{"message_id":%d,"business_connection_id":%q,"chat":{"id":%d,"type":"private"},"date":1700000000,"text":"hi"}}`, id, id, conn, chat)
	}
	return out
}

// TestPollerRunOverlapsDifferentChats is the end-to-end half of issue #18:
// the poller no longer serialises independent chats. The handler for chat 11
// waits for chat 22 to start; a strictly sequential poller would time out.
func TestPollerRunOverlapsDifferentChats(t *testing.T) {
	script := &scriptedPoller{t: t, bodies: []string{shardedBatchJSON(1, 2)}}
	srv := newScriptServer(t, script)
	client := NewClient("token", 61*time.Second, WithBaseURL(srv.URL+"/bot"))
	poller := NewPoller(client, silentTestLogger())

	otherStarted := make(chan struct{})
	releaseFirst := make(chan struct{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Atomic: the handler now runs on shard workers concurrently, so two
	// chats of one batch increment this from different goroutines.
	var seen atomic.Int64
	_ = poller.Run(ctx, func(_ context.Context, u Update) error {
		seen.Add(1)
		var chat int64
		if u.BusinessMessage != nil {
			chat = u.BusinessMessage.Chat.ID
		}
		switch chat {
		case 11:
			select {
			case <-otherStarted:
			case <-time.After(10 * time.Second):
				t.Error("chat 11 never overlapped with chat 22: chats are processed sequentially")
			}
			close(releaseFirst)
		case 22:
			close(otherStarted)
			select {
			case <-releaseFirst:
			case <-time.After(10 * time.Second):
				t.Error("chat 22 stuck behind chat 11")
			}
		default:
			t.Errorf("unexpected chat %d", chat)
		}
		if seen.Load() == 2 {
			cancel()
		}
		return nil
	})
	if got := ShardKey(Update{BusinessMessage: &Message{BusinessConnectionID: "bc-A", Chat: Chat{ID: 11}}}); got ==
		ShardKey(Update{BusinessMessage: &Message{BusinessConnectionID: "bc-B", Chat: Chat{ID: 22}}}) {
		t.Fatal("test setup broken: the two chats must shard apart")
	}
}

// TestPollerRunKeepsSameChatOrder pins the ordering half of issue #18 at the
// poller level: message, edit then deletion of one chat arrive at the handler
// in emission order, even though other chats interleave in the same batch.
func TestPollerRunKeepsSameChatOrder(t *testing.T) {
	body := `{"update_id":1,"business_message":{"message_id":9,"business_connection_id":"bc-A","chat":{"id":11,"type":"private"},"date":1700000000,"text":"hi"}}` +
		`,{"update_id":2,"business_message":{"message_id":10,"business_connection_id":"bc-B","chat":{"id":22,"type":"private"},"date":1700000000,"text":"other"}}` +
		`,{"update_id":3,"edited_business_message":{"message_id":9,"business_connection_id":"bc-A","chat":{"id":11,"type":"private"},"date":1700000001,"text":"edited"}}` +
		`,{"update_id":4,"deleted_business_messages":{"business_connection_id":"bc-A","chat":{"id":11,"type":"private"},"message_ids":[9]}}`
	script := &scriptedPoller{t: t, bodies: []string{body}}
	srv := newScriptServer(t, script)
	client := NewClient("token", 61*time.Second, WithBaseURL(srv.URL+"/bot"))
	poller := NewPoller(client, silentTestLogger())

	var mu sync.Mutex
	var chat11 []int64
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = poller.Run(ctx, func(_ context.Context, u Update) error {
		var conn string
		switch {
		case u.BusinessMessage != nil:
			conn = u.BusinessMessage.BusinessConnectionID
		case u.EditedBusinessMessage != nil:
			conn = u.EditedBusinessMessage.BusinessConnectionID
		case u.DeletedBusinessMessages != nil:
			conn = u.DeletedBusinessMessages.BusinessConnectionID
		}
		if conn == "bc-A" {
			mu.Lock()
			chat11 = append(chat11, u.UpdateID)
			mu.Unlock()
		}
		if u.UpdateID == 4 {
			cancel()
		}
		return nil
	})

	mu.Lock()
	defer mu.Unlock()
	if len(chat11) != 3 || chat11[0] != 1 || chat11[1] != 3 || chat11[2] != 4 {
		t.Fatalf("same-chat arrival order = %v, want [1 3 4] (message, edit, deletion)", chat11)
	}
	if poller.offset != 5 {
		t.Fatalf("offset = %d, want 5 (advanced past the whole sharded batch)", poller.offset)
	}
}

// TestPollerRunSlowShardIsolatesOthers pins the slow-shard strategy: a chat
// whose handler parks does not stop the other chats of the same batch. No
// wall-clock assertion: the fast chats must simply finish while the slow one
// is still parked.
func TestPollerRunSlowShardIsolatesOthers(t *testing.T) {
	script := &scriptedPoller{t: t, bodies: []string{shardedBatchJSON(1, 2, 3)}}
	srv := newScriptServer(t, script)
	client := NewClient("token", 61*time.Second, WithBaseURL(srv.URL+"/bot"))
	poller := NewPoller(client, silentTestLogger())

	slowEntered := make(chan struct{})
	releaseSlow := make(chan struct{})
	fastDone := make(chan struct{}, 2)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var completed atomic.Int64
	watchErr := make(chan error, 1)
	go func() {
		// Driver for the isolation check: it must run concurrently with Run
		// (which blocks until the batch completes), not after it.
		select {
		case <-slowEntered:
		case <-time.After(10 * time.Second):
			watchErr <- errors.New("slow update never reached the handler")
			cancel()
			return
		}
		// Update 2 belongs to chat 22 and must complete while update 1 is
		// still parked. (Update 3 belongs to chat 11 again, so it
		// legitimately waits behind update 1 on the same partition.)
		select {
		case <-fastDone:
		case <-time.After(10 * time.Second):
			watchErr <- errors.New("a parked shard blocked the other chat of its batch")
			cancel()
			return
		}
		close(releaseSlow)
		watchErr <- nil
	}()
	_ = poller.Run(ctx, func(_ context.Context, u Update) error {
		defer func() {
			// The last update of the batch ends the run from inside the
			// handler: Run only notices cancellation between polls, so a
			// test that never cancels here would wait for the next
			// long-poll forever.
			if completed.Add(1) == 3 {
				cancel()
			}
		}()
		if u.BusinessMessage != nil && u.BusinessMessage.Chat.ID == 11 && u.UpdateID == 1 {
			close(slowEntered)
			select {
			case <-releaseSlow:
			case <-ctx.Done():
			}
			return nil
		}
		fastDone <- struct{}{}
		return nil
	})

	select {
	case err := <-watchErr:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("isolation driver never finished")
	}
}
