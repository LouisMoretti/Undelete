package app

import (
	"context"
	"sync"
	"testing"

	"github.com/LouisMoretti/Undelete/bot/internal/business"
	"github.com/LouisMoretti/Undelete/bot/internal/media"
	"github.com/LouisMoretti/Undelete/bot/internal/messages"
	"github.com/LouisMoretti/Undelete/bot/internal/telegram"
)

// concBusiness resolves every connection as the same enabled owner. The
// shard workers call it concurrently; the fake guards its counters so the
// race detector checks the handler, not the test double.
type concBusiness struct {
	mu       sync.Mutex
	resolves int
}

func (f *concBusiness) Resolve(_ context.Context, id string) (*business.Connection, error) {
	f.mu.Lock()
	f.resolves++
	f.mu.Unlock()
	return &business.Connection{ID: id, OwnerUserID: 11, OwnerTelegramUserID: 700001, CanReply: true, IsEnabled: true}, nil
}

func (f *concBusiness) HandleBusinessConnection(_ context.Context, _ telegram.BusinessConnection) error {
	return nil
}

// concMessages records saves and deletions under a mutex, like the real
// repository serialises them per transaction.
type concMessages struct {
	mu      sync.Mutex
	saved   []messages.Record
	deleted int
}

func (f *concMessages) Save(_ context.Context, _ int64, m messages.Record, edited bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saved = append(f.saved, m)
	return nil
}

func (f *concMessages) MarkDeleted(_ context.Context, _, _ int64, _ string, _ int64, ids []int64) ([]messages.DeletedRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted += len(ids)
	return nil, nil
}

type concMedia struct {
	mu    sync.Mutex
	saved int
}

func (f *concMedia) Save(_ context.Context, _ int64, _ media.Record) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saved++
	return int64(f.saved), nil
}

// TestHandleUpdateConcurrentChats pins that the shard workers may call the
// handler simultaneously: the handler holds no mutable state of its own, so
// updates for different chats never interfere and none is lost.
func TestHandleUpdateConcurrentChats(t *testing.T) {
	biz := &concBusiness{}
	msgs := &concMessages{}
	med := &concMedia{}
	h := NewHandler(biz, msgs, med, testLogger())

	const chats = 16
	const perChat = 10
	var wg sync.WaitGroup
	for chat := int64(0); chat < chats; chat++ {
		for n := int64(0); n < perChat; n++ {
			wg.Add(1)
			go func(chat, n int64) {
				defer wg.Done()
				msg := &telegram.Message{
					MessageID:            n + 1,
					BusinessConnectionID: "bc-1",
					Chat:                 telegram.Chat{ID: 1000 + chat, Type: "private"},
					From:                 &telegram.User{ID: 700002, FirstName: "Zoe"},
					Text:                 "hello",
					Date:                 1700000000,
				}
				if err := h.HandleUpdate(context.Background(), telegram.Update{UpdateID: chat*perChat + n, BusinessMessage: msg}); err != nil {
					t.Errorf("HandleUpdate(chat=%d, n=%d) = %v, want nil", chat, n, err)
				}
			}(chat, n)
		}
	}
	wg.Wait()

	msgs.mu.Lock()
	defer msgs.mu.Unlock()
	if len(msgs.saved) != chats*perChat {
		t.Fatalf("saved = %d, want %d: concurrent updates lost messages", len(msgs.saved), chats*perChat)
	}
	perChatCount := make(map[int64]int)
	for _, r := range msgs.saved {
		perChatCount[r.ChatID]++
	}
	for chat := int64(0); chat < chats; chat++ {
		if perChatCount[1000+chat] != perChat {
			t.Fatalf("chat %d saved = %d, want %d", 1000+chat, perChatCount[1000+chat], perChat)
		}
	}
}

// TestHandleUpdateConcurrentMixedTypes pins that deletions, edits and saves
// racing across different chats do not interfere: every update is handled
// exactly once and every deletion is marked.
func TestHandleUpdateConcurrentMixedTypes(t *testing.T) {
	biz := &concBusiness{}
	msgs := &concMessages{}
	med := &concMedia{}
	h := NewHandler(biz, msgs, med, testLogger())

	const chats = 8
	var wg sync.WaitGroup
	for chat := int64(0); chat < chats; chat++ {
		chat := chat
		wg.Add(3)
		go func() {
			defer wg.Done()
			msg := &telegram.Message{MessageID: 1, BusinessConnectionID: "bc-1", Chat: telegram.Chat{ID: 2000 + chat}, Text: "hi", Date: 1700000000}
			if err := h.HandleUpdate(context.Background(), telegram.Update{UpdateID: chat*3 + 1, BusinessMessage: msg}); err != nil {
				t.Errorf("save: %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			msg := &telegram.Message{MessageID: 1, BusinessConnectionID: "bc-1", Chat: telegram.Chat{ID: 2000 + chat}, Text: "edited", Date: 1700000001}
			if err := h.HandleUpdate(context.Background(), telegram.Update{UpdateID: chat*3 + 2, EditedBusinessMessage: msg}); err != nil {
				t.Errorf("edit: %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			del := &telegram.BusinessMessagesDeleted{BusinessConnectionID: "bc-1", Chat: telegram.Chat{ID: 2000 + chat}, MessageIDs: []int64{1}}
			if err := h.HandleUpdate(context.Background(), telegram.Update{UpdateID: chat*3 + 3, DeletedBusinessMessages: del}); err != nil {
				t.Errorf("delete: %v", err)
			}
		}()
	}
	wg.Wait()

	msgs.mu.Lock()
	defer msgs.mu.Unlock()
	if len(msgs.saved) != chats*2 {
		t.Fatalf("saved = %d, want %d", len(msgs.saved), chats*2)
	}
	if msgs.deleted != chats {
		t.Fatalf("deleted = %d, want %d", msgs.deleted, chats)
	}
}
