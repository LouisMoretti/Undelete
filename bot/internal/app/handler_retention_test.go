package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/LouisMoretti/Undelete/bot/internal/business"
	"github.com/LouisMoretti/Undelete/bot/internal/telegram"
)

// fakeRetention stands in for users.Repository: it holds one period per
// tenant, records every write, and can fail either half on demand. It also
// records the deadline of every call: the command runs on the poller's
// goroutine, so an unbounded one is a defect in itself.
type fakeRetention struct {
	days      map[int64]int
	setCalls  []retentionCall
	deadlines []time.Time
	getErr    error
	setErr    error
}

type retentionCall struct {
	ownerUserID int64
	days        int
}

func (f *fakeRetention) GetRetentionDays(ctx context.Context, ownerUserID int64) (int, error) {
	deadline, _ := ctx.Deadline()
	f.deadlines = append(f.deadlines, deadline)
	if f.getErr != nil {
		return 0, f.getErr
	}
	if days, ok := f.days[ownerUserID]; ok {
		return days, nil
	}
	return 0, errors.New("no such tenant")
}

func (f *fakeRetention) SetRetentionDays(ctx context.Context, ownerUserID int64, days int) error {
	deadline, _ := ctx.Deadline()
	f.deadlines = append(f.deadlines, deadline)
	if f.setErr != nil {
		return f.setErr
	}
	f.setCalls = append(f.setCalls, retentionCall{ownerUserID: ownerUserID, days: days})
	if f.days == nil {
		f.days = map[int64]int{}
	}
	f.days[ownerUserID] = days
	return nil
}

func newRetentionHandler(sender *fakeSender, store retentionStore) (*Handler, *fakeMessages) {
	biz := &fakeBusiness{connections: map[string]*business.Connection{"bc-1": enabledConn()}}
	msgs := &fakeMessages{}
	opts := []Option{}
	if sender != nil {
		opts = append(opts, WithCommandSender(sender))
	}
	if store != nil {
		opts = append(opts, WithRetention(store, 14))
	}
	return NewHandler(biz, msgs, &fakeMedia{}, testLogger(), opts...), msgs
}

// retentionAnswer asserts the wire contract shared by every /retention answer:
// exactly one message, to the owner alone, never into the monitored chat.
func retentionAnswer(t *testing.T, sender *fakeSender, msg *telegram.Message) string {
	t.Helper()
	if len(sender.sent) != 1 {
		t.Fatalf("messages sent = %d, want 1: %+v", len(sender.sent), sender.sent)
	}
	answer := sender.sent[0]
	if answer.ChatID != 700001 {
		t.Fatalf("chat_id = %d, want the owner (700001)", answer.ChatID)
	}
	if answer.ChatID == msg.Chat.ID {
		t.Fatal("the answer was sent into the monitored chat")
	}
	return answer.Text
}

// TestRetentionReadAnswersTheCurrentValue is the read half: a bare /retention
// answers the tenant's period, when the purge applies it, and the independence
// from the backups -- and the command itself is saved like any other message.
func TestRetentionReadAnswersTheCurrentValue(t *testing.T) {
	sender := &fakeSender{}
	store := &fakeRetention{days: map[int64]int{11: 30}}
	h, msgs := newRetentionHandler(sender, store)
	msg := ownerMessage("/retention")

	if err := h.HandleUpdate(context.Background(), telegram.Update{UpdateID: 1, BusinessMessage: msg}); err != nil {
		t.Fatalf("HandleUpdate: %v", err)
	}

	if len(msgs.saved) != 1 {
		t.Fatalf("saved messages = %d, want 1: a command is a message like any other", len(msgs.saved))
	}
	text := retentionAnswer(t, sender, msg)
	for _, needle := range []string{"30 days", "24 hours", "BACKUP_RETENTION_DAYS", "14 days"} {
		if !strings.Contains(text, needle) {
			t.Fatalf("the answer never mentions %q:\n%s", needle, text)
		}
	}
	if len(store.setCalls) != 0 {
		t.Fatal("a read wrote to the retention store")
	}
}

// TestRetentionSetAppliesTheBounds is the write half: 1 and 365 are accepted
// and stored for the resolved tenant, and the confirmation states the new
// value.
func TestRetentionSetAppliesTheBounds(t *testing.T) {
	for _, text := range []string{"/retention 1", "/retention 365", "/retention@undelete_bot 30", "/Retention 7"} {
		t.Run(text, func(t *testing.T) {
			sender := &fakeSender{}
			store := &fakeRetention{days: map[int64]int{11: 7}}
			h, msgs := newRetentionHandler(sender, store)
			msg := ownerMessage(text)

			if err := h.HandleUpdate(context.Background(), telegram.Update{UpdateID: 1, BusinessMessage: msg}); err != nil {
				t.Fatalf("HandleUpdate: %v", err)
			}

			if len(msgs.saved) != 1 {
				t.Fatalf("saved messages = %d, want 1", len(msgs.saved))
			}
			if len(store.setCalls) != 1 {
				t.Fatalf("writes = %d, want 1", len(store.setCalls))
			}
			if call := store.setCalls[0]; call.ownerUserID != 11 {
				t.Fatalf("the write went to tenant %d, want 11", call.ownerUserID)
			}
			answer := retentionAnswer(t, sender, msg)
			if !strings.Contains(answer, "Retention updated") {
				t.Fatalf("the answer is not a confirmation:\n%s", answer)
			}
			if !strings.Contains(answer, "BACKUP_RETENTION_DAYS") {
				t.Fatal("the confirmation never states the independence from the backups")
			}
		})
	}
}

// TestRetentionSetRefusesInvalidWithoutWriting: out of bounds, non-numeric,
// empty past the command word, or trailed by extra tokens. Every one of them
// is answered explicitly to the owner and changes nothing -- and never fails
// the update.
func TestRetentionSetRefusesInvalidWithoutWriting(t *testing.T) {
	for _, text := range []string{
		"/retention 0",
		"/retention 366",
		"/retention 1000",
		"/retention -1",
		"/retention -365",
		"/retention abc",
		"/retention 3.5",
		"/retention 30 days",
		"/retention 10 20",
		"/retention 0x1E",
	} {
		t.Run(text, func(t *testing.T) {
			sender := &fakeSender{}
			store := &fakeRetention{days: map[int64]int{11: 7}}
			h, msgs := newRetentionHandler(sender, store)
			msg := ownerMessage(text)

			if err := h.HandleUpdate(context.Background(), telegram.Update{UpdateID: 1, BusinessMessage: msg}); err != nil {
				t.Fatalf("HandleUpdate must swallow the refusal, got %v", err)
			}
			if len(msgs.saved) != 1 {
				t.Fatalf("saved messages = %d, want 1: the refusal concerns the setting, not the capture", len(msgs.saved))
			}
			if len(store.setCalls) != 0 {
				t.Fatalf("an invalid argument wrote to the retention store: %+v", store.setCalls)
			}
			answer := retentionAnswer(t, sender, msg)
			if !strings.Contains(answer, "was not changed") {
				t.Fatalf("the refusal is not explicit:\n%s", answer)
			}
			if !strings.Contains(answer, "1 and 365") {
				t.Fatalf("the refusal never states the bounds:\n%s", answer)
			}
		})
	}
}

// TestRetentionIgnoresEveryoneButTheOwner is the impersonation criterion: a
// contact, a message without a sender, or a chat id spoofed to look like the
// owner's gets nothing -- no answer, no retention write, and, because the
// command is dropped before the save, no stored message either.
func TestRetentionIgnoresEveryoneButTheOwner(t *testing.T) {
	messages := map[string]func(string) *telegram.Message{
		"a contact in the monitored chat": func(text string) *telegram.Message {
			msg := testMessage() // From.ID 700002, not the owner
			msg.Text = text
			return msg
		},
		"a message without a sender": func(text string) *telegram.Message {
			msg := testMessage()
			msg.From = nil
			msg.Text = text
			return msg
		},
		"an impostor claiming the owner's chat": func(text string) *telegram.Message {
			msg := testMessage()
			msg.From = &telegram.User{ID: 700003, FirstName: "Mallory"}
			msg.Chat = telegram.Chat{ID: 700001, Type: "private"}
			msg.Text = text
			return msg
		},
	}

	for name, build := range messages {
		for _, text := range []string{"/retention", "/retention 30"} {
			t.Run(name+" "+text, func(t *testing.T) {
				sender := &fakeSender{}
				store := &fakeRetention{days: map[int64]int{11: 7}}
				h, msgs := newRetentionHandler(sender, store)

				if err := h.HandleUpdate(context.Background(), telegram.Update{UpdateID: 1, BusinessMessage: build(text)}); err != nil {
					t.Fatalf("HandleUpdate: %v", err)
				}
				if len(sender.sent) != 0 {
					t.Fatalf("%d message(s) sent, want 0: %+v", len(sender.sent), sender.sent)
				}
				if len(store.setCalls) != 0 {
					t.Fatal("a third party reached the retention store")
				}
				if len(msgs.saved) != 0 {
					t.Fatalf("%d message(s) saved, want 0: a third party's command writes nothing", len(msgs.saved))
				}
			})
		}
	}
}

// TestRetentionAppliesToTheResolvedTenantOnly: the write lands on the tenant
// of the connection the command arrived through, and no other tenant is
// touched. There is deliberately no per-chat dimension to check: the setting
// belongs to the tenant.
func TestRetentionAppliesToTheResolvedTenantOnly(t *testing.T) {
	second := &business.Connection{ID: "bc-2", OwnerUserID: 22, OwnerTelegramUserID: 700002, CanReply: true, IsEnabled: true}
	biz := &fakeBusiness{connections: map[string]*business.Connection{
		"bc-1": enabledConn(),
		"bc-2": second,
	}}
	sender := &fakeSender{}
	store := &fakeRetention{days: map[int64]int{11: 7, 22: 60}}
	h := NewHandler(biz, &fakeMessages{}, &fakeMedia{}, testLogger(),
		WithCommandSender(sender), WithRetention(store, 14))

	msg := &telegram.Message{
		MessageID:            3,
		BusinessConnectionID: "bc-2",
		Chat:                 telegram.Chat{ID: 78, Type: "private"},
		From:                 &telegram.User{ID: 700002, FirstName: "Second"},
		Text:                 "/retention 90",
	}
	if err := h.HandleUpdate(context.Background(), telegram.Update{UpdateID: 1, BusinessMessage: msg}); err != nil {
		t.Fatalf("HandleUpdate: %v", err)
	}

	if len(store.setCalls) != 1 || store.setCalls[0] != (retentionCall{ownerUserID: 22, days: 90}) {
		t.Fatalf("writes = %+v, want exactly [{22 90}]", store.setCalls)
	}
	if store.days[11] != 7 {
		t.Fatalf("the other tenant now reads %d days, want 7", store.days[11])
	}
	if len(sender.sent) != 1 || sender.sent[0].ChatID != 700002 {
		t.Fatalf("the confirmation did not go to the second tenant alone: %+v", sender.sent)
	}

	// And the read half is tenant-scoped too: the first tenant still reads its
	// own value.
	sender.sent = nil
	if err := h.HandleUpdate(context.Background(), telegram.Update{UpdateID: 2, BusinessMessage: ownerMessage("/retention")}); err != nil {
		t.Fatalf("HandleUpdate: %v", err)
	}
	if len(sender.sent) != 1 || !strings.Contains(sender.sent[0].Text, "7 days") {
		t.Fatalf("the first tenant did not read its own value: %+v", sender.sent)
	}
}

// TestRetentionIgnoredOnARefusedConnection: a connection the mono-tenant guard
// rejects provides no owner, so even a well-formed command is dropped before
// the save -- silently, exactly as the capture would keep it.
func TestRetentionIgnoredOnARefusedConnection(t *testing.T) {
	sender := &fakeSender{}
	store := &fakeRetention{days: map[int64]int{11: 7}}
	msgs := &fakeMessages{}
	biz := &fakeBusiness{resolveErr: map[string]error{"bc-1": business.ErrOwnerMismatch}}
	h := NewHandler(biz, msgs, &fakeMedia{}, testLogger(),
		WithCommandSender(sender), WithRetention(store, 14))

	for _, text := range []string{"/retention", "/retention 30"} {
		if err := h.HandleUpdate(context.Background(), telegram.Update{
			UpdateID: 1, BusinessMessage: ownerMessage(text),
		}); err != nil {
			t.Fatalf("HandleUpdate: %v", err)
		}
	}
	if len(sender.sent) != 0 || len(store.setCalls) != 0 || len(msgs.saved) != 0 {
		t.Fatal("a refused connection reached the sender, the store, or the capture")
	}
}

// TestRetentionIgnoredOnADisabledConnection: a disabled connection saves
// nothing, so there is no context in which the command would be legitimate --
// exactly like /privacy, the command stays silent.
func TestRetentionIgnoredOnADisabledConnection(t *testing.T) {
	disabled := enabledConn()
	disabled.IsEnabled = false
	biz := &fakeBusiness{connections: map[string]*business.Connection{"bc-1": disabled}}
	sender := &fakeSender{}
	store := &fakeRetention{days: map[int64]int{11: 7}}
	msgs := &fakeMessages{}
	h := NewHandler(biz, msgs, &fakeMedia{}, testLogger(),
		WithCommandSender(sender), WithRetention(store, 14))

	for _, text := range []string{"/retention", "/retention 30"} {
		if err := h.HandleUpdate(context.Background(), telegram.Update{
			UpdateID: 1, BusinessMessage: ownerMessage(text),
		}); err != nil {
			t.Fatalf("HandleUpdate: %v", err)
		}
	}
	if len(sender.sent) != 0 || len(store.setCalls) != 0 || len(msgs.saved) != 0 {
		t.Fatal("a disabled connection reached the sender, the store, or the capture")
	}
}

// TestRetentionWithoutAStoreIsInert: a Handler built without WithRetention
// keeps saving everything it may, and answers nothing. No nil dereference on
// the poller path.
func TestRetentionWithoutAStoreIsInert(t *testing.T) {
	sender := &fakeSender{}
	h, msgs := newRetentionHandler(sender, nil)

	for _, text := range []string{"/retention", "/retention 30"} {
		if err := h.HandleUpdate(context.Background(), telegram.Update{UpdateID: 1, BusinessMessage: ownerMessage(text)}); err != nil {
			t.Fatalf("HandleUpdate: %v", err)
		}
	}
	if len(sender.sent) != 0 {
		t.Fatalf("%d message(s) sent without a store, want 0", len(sender.sent))
	}
	if len(msgs.saved) != 2 {
		t.Fatalf("saved messages = %d, want 2: the commands save, only the answers are withheld", len(msgs.saved))
	}
}

// TestRetentionStoreFailureSendsNothing: a store that cannot read or write
// must not produce an answer describing a value that was never read or a
// change that never happened. The failure stays a logged failure.
func TestRetentionStoreFailureSendsNothing(t *testing.T) {
	t.Run("read failure", func(t *testing.T) {
		sender := &fakeSender{}
		store := &fakeRetention{getErr: errors.New("database down")}
		h, _ := newRetentionHandler(sender, store)

		if err := h.HandleUpdate(context.Background(), telegram.Update{UpdateID: 1, BusinessMessage: ownerMessage("/retention")}); err != nil {
			t.Fatalf("HandleUpdate must swallow the failure, got %v", err)
		}
		if len(sender.sent) != 0 {
			t.Fatalf("%d message(s) sent for a value that was never read", len(sender.sent))
		}
	})

	t.Run("write failure", func(t *testing.T) {
		sender := &fakeSender{}
		store := &fakeRetention{days: map[int64]int{11: 7}, setErr: errors.New("database down")}
		h, _ := newRetentionHandler(sender, store)

		if err := h.HandleUpdate(context.Background(), telegram.Update{UpdateID: 1, BusinessMessage: ownerMessage("/retention 30")}); err != nil {
			t.Fatalf("HandleUpdate must swallow the failure, got %v", err)
		}
		if len(sender.sent) != 0 {
			t.Fatalf("%d message(s) sent for a change that never happened", len(sender.sent))
		}
	})
}

// TestRetentionRunsUnderADeadline: the read, the write and the answer all run
// on the poller's single goroutine, which getUpdates cannot leave until they
// return. Without a ceiling, a stuck database or an unbounded 429 retry_after
// would park the poller -- and delay the deleted_business_messages updates
// that carry content existing nowhere else.
func TestRetentionRunsUnderADeadline(t *testing.T) {
	for _, text := range []string{"/retention", "/retention 30"} {
		t.Run(text, func(t *testing.T) {
			sender := &fakeSender{}
			store := &fakeRetention{days: map[int64]int{11: 7}}
			h, _ := newRetentionHandler(sender, store)

			if err := h.HandleUpdate(context.Background(), telegram.Update{UpdateID: 1, BusinessMessage: ownerMessage(text)}); err != nil {
				t.Fatalf("HandleUpdate: %v", err)
			}
			// Measured AFTER the call: every deadline was set during it, so
			// none may reach further than now plus the timeout.
			ceiling := time.Now().Add(commandAnswerTimeout)

			deadlines := append(append([]time.Time{}, store.deadlines...), sender.deadlines...)
			if len(deadlines) < 2 {
				t.Fatalf("only %d bounded calls, want the store and the answer", len(deadlines))
			}
			for index, deadline := range deadlines {
				if deadline.IsZero() {
					t.Fatalf("call %d ran on a context with no deadline", index)
				}
				if deadline.After(ceiling) {
					t.Fatalf("call %d may run %v past commandAnswerTimeout (%v)", index, deadline.Sub(ceiling), commandAnswerTimeout)
				}
			}
		})
	}
}

// TestRetentionCommandInACaptionIsStillACommand: Telegram puts the text of a
// media message in caption, never in text, and the command is read from the
// same place the saved content comes from.
func TestRetentionCommandInACaptionIsStillACommand(t *testing.T) {
	sender := &fakeSender{}
	store := &fakeRetention{days: map[int64]int{11: 7}}
	h, _ := newRetentionHandler(sender, store)

	msg := ownerMessage("")
	msg.Caption = "/retention 30"
	msg.Photo = []telegram.PhotoSize{{FileID: "f-1", FileUniqueID: "u-1", Width: 10, Height: 10}}

	if err := h.HandleUpdate(context.Background(), telegram.Update{UpdateID: 1, BusinessMessage: msg}); err != nil {
		t.Fatalf("HandleUpdate: %v", err)
	}
	if len(store.setCalls) != 1 {
		t.Fatal("a command in a caption was ignored")
	}
}

// TestRetentionNeverTriggeredByAnEdit: rewriting an old message into
// "/retention 30" is not an act, and an edit storm must not turn into a write
// storm.
func TestRetentionNeverTriggeredByAnEdit(t *testing.T) {
	sender := &fakeSender{}
	store := &fakeRetention{days: map[int64]int{11: 7}}
	h, _ := newRetentionHandler(sender, store)

	if err := h.HandleUpdate(context.Background(), telegram.Update{UpdateID: 1, EditedBusinessMessage: ownerMessage("/retention 30")}); err != nil {
		t.Fatalf("HandleUpdate: %v", err)
	}
	if len(store.setCalls) != 0 || len(sender.sent) != 0 {
		t.Fatal("an edit changed the retention period")
	}
}

// TestRetentionIgnoresNonCommands: the near misses. A different command word
// (an underscore is part of one), a quoted command, and a leading space are
// all things a contact or the owner writes in a conversation, and none of them
// is an instruction to change the retention.
func TestRetentionIgnoresNonCommands(t *testing.T) {
	for _, text := range []string{
		"/retention_extra",
		"/retention_extra 30",
		"tell me about /retention",
		" /retention 30",
		"retention",
		"/retent",
	} {
		t.Run(text, func(t *testing.T) {
			sender := &fakeSender{}
			store := &fakeRetention{days: map[int64]int{11: 7}}
			h, _ := newRetentionHandler(sender, store)

			if err := h.HandleUpdate(context.Background(), telegram.Update{UpdateID: 1, BusinessMessage: ownerMessage(text)}); err != nil {
				t.Fatalf("HandleUpdate: %v", err)
			}
			if len(sender.sent) != 0 {
				t.Fatalf("%q triggered %d message(s), want 0", text, len(sender.sent))
			}
			if len(store.setCalls) != 0 {
				t.Fatalf("%q reached the retention store", text)
			}
		})
	}
}
