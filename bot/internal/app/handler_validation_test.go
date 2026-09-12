package app

import (
	"context"
	"testing"

	"github.com/LouisMoretti/Undelete/bot/internal/business"
	"github.com/LouisMoretti/Undelete/bot/internal/telegram"
)

type countingBusiness struct {
	fakeBusiness
	resolveCalls int
}

func (f *countingBusiness) Resolve(ctx context.Context, id string) (*business.Connection, error) {
	f.resolveCalls++
	return f.fakeBusiness.Resolve(ctx, id)
}

func TestSaveMessageRejectsEmptyConnectionWithoutResolve(t *testing.T) {
	biz := &countingBusiness{fakeBusiness: fakeBusiness{connections: map[string]*business.Connection{"bc-1": enabledConn()}}}
	msgs := &fakeMessages{}
	h := NewHandler(biz, msgs, &fakeMedia{}, testLogger())

	msg := testMessage()
	msg.BusinessConnectionID = ""
	if err := h.HandleUpdate(context.Background(), telegram.Update{UpdateID: 1, BusinessMessage: msg}); err != nil {
		t.Fatalf("HandleUpdate: %v", err)
	}
	if biz.resolveCalls != 0 {
		t.Fatalf("Resolve called %d times for empty connection id, want 0 (no useless API call)", biz.resolveCalls)
	}
	if len(msgs.saved) != 0 {
		t.Fatalf("nothing must be saved for empty connection id")
	}
}

func TestSaveMessageRejectsZeroIDs(t *testing.T) {
	for _, mutate := range []struct {
		name string
		f    func(*telegram.Message)
	}{
		{"zero chat", func(m *telegram.Message) { m.Chat.ID = 0 }},
		{"zero message", func(m *telegram.Message) { m.MessageID = 0 }},
	} {
		t.Run(mutate.name, func(t *testing.T) {
			biz := &fakeBusiness{connections: map[string]*business.Connection{"bc-1": enabledConn()}}
			msgs := &fakeMessages{}
			h := NewHandler(biz, msgs, &fakeMedia{}, testLogger())
			msg := testMessage()
			mutate.f(msg)
			if err := h.HandleUpdate(context.Background(), telegram.Update{UpdateID: 1, BusinessMessage: msg}); err != nil {
				t.Fatalf("HandleUpdate: %v", err)
			}
			if len(msgs.saved) != 0 {
				t.Fatalf("zero ids must not be persisted (would pollute chats with chat_id=0)")
			}
		})
	}
}

func TestHandleDeletedIgnoresEmptyBatch(t *testing.T) {
	biz := &countingBusiness{fakeBusiness: fakeBusiness{connections: map[string]*business.Connection{"bc-1": enabledConn()}}}
	msgs := &fakeMessages{}
	h := NewHandler(biz, msgs, &fakeMedia{}, testLogger())

	del := &telegram.BusinessMessagesDeleted{BusinessConnectionID: "bc-1", Chat: telegram.Chat{ID: 77}, MessageIDs: nil}
	if err := h.HandleUpdate(context.Background(), telegram.Update{UpdateID: 2, DeletedBusinessMessages: del}); err != nil {
		t.Fatalf("HandleUpdate: %v", err)
	}
	if biz.resolveCalls != 0 {
		t.Fatalf("empty message_ids must short-circuit before Resolve")
	}

	del2 := &telegram.BusinessMessagesDeleted{BusinessConnectionID: "", Chat: telegram.Chat{ID: 77}, MessageIDs: []int64{1}}
	if err := h.HandleUpdate(context.Background(), telegram.Update{UpdateID: 3, DeletedBusinessMessages: del2}); err != nil {
		t.Fatalf("HandleUpdate: %v", err)
	}
	if len(msgs.saved) != 0 {
		t.Fatalf("no save expected")
	}
}
