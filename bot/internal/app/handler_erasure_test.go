package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/LouisMoretti/Undelete/bot/internal/business"
	"github.com/LouisMoretti/Undelete/bot/internal/erasure"
	"github.com/LouisMoretti/Undelete/bot/internal/telegram"
)

// fakeEraser stands in for erasure.Service: it records what the handler asked
// for, and can return any outcome or failure the command has to answer.
type fakeEraser struct {
	requests   []erasure.Tenant
	confirmed  []string
	tenants    []erasure.Tenant
	code       string
	outcome    erasure.Outcome
	requestErr error
	confirmErr error
	// deadlines records the deadline of every call: the erasure runs on the
	// poller's goroutine, so an unbounded one is a defect in itself.
	deadlines []time.Time
}

func (f *fakeEraser) Request(ctx context.Context, t erasure.Tenant) (erasure.Challenge, error) {
	deadline, _ := ctx.Deadline()
	f.deadlines = append(f.deadlines, deadline)
	f.requests = append(f.requests, t)
	if f.requestErr != nil {
		return erasure.Challenge{}, f.requestErr
	}
	code := f.code
	if code == "" {
		code = "ABCD2345"
	}
	return erasure.Challenge{Code: code, ExpiresAt: time.Now().Add(erasure.ChallengeTTL)}, nil
}

func (f *fakeEraser) Confirm(ctx context.Context, t erasure.Tenant, code string) (erasure.Outcome, error) {
	deadline, _ := ctx.Deadline()
	f.deadlines = append(f.deadlines, deadline)
	f.tenants = append(f.tenants, t)
	f.confirmed = append(f.confirmed, code)
	if f.confirmErr != nil {
		return erasure.OutcomeErased, f.confirmErr
	}
	return f.outcome, nil
}

func newErasureHandler(sender *fakeSender, eraser dataEraser) (*Handler, *fakeMessages) {
	biz := &fakeBusiness{connections: map[string]*business.Connection{"bc-1": enabledConn()}}
	msgs := &fakeMessages{}
	opts := []Option{}
	if sender != nil {
		opts = append(opts, WithCommandSender(sender))
	}
	if eraser != nil {
		opts = append(opts, WithDataEraser(eraser, 14))
	}
	return NewHandler(biz, msgs, &fakeMedia{}, testLogger(), opts...), msgs
}

// TestErasureCommandIssuesAChallengeToTheOwner is the first half of the
// command: typed alone, it hands the owner a code — privately, never into the
// chat it was typed in, and the message that carried it is saved like any other.
func TestErasureCommandIssuesAChallengeToTheOwner(t *testing.T) {
	sender := &fakeSender{}
	eraser := &fakeEraser{code: "WXYZ7788"}
	h, msgs := newErasureHandler(sender, eraser)
	msg := ownerMessage("/delete_my_data")

	if err := h.HandleUpdate(context.Background(), telegram.Update{UpdateID: 1, BusinessMessage: msg}); err != nil {
		t.Fatalf("HandleUpdate: %v", err)
	}

	if len(msgs.saved) != 1 {
		t.Fatalf("saved messages = %d, want 1: a command is a message like any other", len(msgs.saved))
	}
	if len(eraser.requests) != 1 {
		t.Fatalf("challenge requests = %d, want 1", len(eraser.requests))
	}
	if len(eraser.confirmed) != 0 {
		t.Fatal("the bare command must not confirm anything")
	}
	if got := eraser.requests[0]; got.OwnerUserID != 11 || got.OwnerTelegramUserID != 700001 || got.BusinessConnectionID != "bc-1" {
		t.Fatalf("the challenge was issued for %+v, not for the connection's owner", got)
	}

	if len(sender.sent) != 1 {
		t.Fatalf("messages sent = %d, want 1", len(sender.sent))
	}
	answer := sender.sent[0]
	if answer.ChatID != 700001 {
		t.Fatalf("chat_id = %d, want the owner (700001)", answer.ChatID)
	}
	if answer.ChatID == msg.Chat.ID {
		t.Fatal("the code was sent into the monitored chat")
	}
	if !strings.Contains(answer.Text, "WXYZ7788") {
		t.Fatal("the answer does not carry the issued code")
	}
}

// TestErasureConfirmationSpendsTheTypedCode: the second half. Whatever the
// owner typed after the command is handed to the eraser verbatim — normalising
// it is the erasure package's decision, not the handler's.
func TestErasureConfirmationSpendsTheTypedCode(t *testing.T) {
	sender := &fakeSender{}
	eraser := &fakeEraser{outcome: erasure.OutcomeErased}
	h, _ := newErasureHandler(sender, eraser)

	if err := h.HandleUpdate(context.Background(), telegram.Update{
		UpdateID: 1, BusinessMessage: ownerMessage("/delete_my_data abcd-2345"),
	}); err != nil {
		t.Fatalf("HandleUpdate: %v", err)
	}

	if len(eraser.requests) != 0 {
		t.Fatal("a confirmation must not issue a new code")
	}
	if len(eraser.confirmed) != 1 || eraser.confirmed[0] != "abcd-2345" {
		t.Fatalf("confirmed codes = %v, want [abcd-2345]", eraser.confirmed)
	}
	if len(sender.sent) != 1 || sender.sent[0].ChatID != 700001 {
		t.Fatalf("the confirmation did not go to the owner alone: %+v", sender.sent)
	}
	if !strings.Contains(sender.sent[0].Text, "BACKUP_RETENTION_DAYS") {
		t.Fatal("the confirmation never states the residual survival in the backups")
	}
}

// TestEveryErasureOutcomeIsAnswered: a refused code must not leave the owner
// wondering whether their data is gone. Each outcome has its own message, and
// none of them reaches the monitored chat.
func TestEveryErasureOutcomeIsAnswered(t *testing.T) {
	tests := []struct {
		name    string
		outcome erasure.Outcome
		err     error
		needle  string
	}{
		{name: "erased", outcome: erasure.OutcomeErased, needle: "Data erasure complete"},
		{name: "already erased", outcome: erasure.OutcomeAlreadyErased, needle: "Nothing left to erase"},
		{name: "expired", outcome: erasure.OutcomeExpired, needle: "has expired"},
		{name: "unknown", outcome: erasure.OutcomeUnknown, needle: "is not valid"},
		{name: "failed", err: errors.New("database down"), needle: "did not finish"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sender := &fakeSender{}
			eraser := &fakeEraser{outcome: tt.outcome, confirmErr: tt.err}
			h, _ := newErasureHandler(sender, eraser)

			if err := h.HandleUpdate(context.Background(), telegram.Update{
				UpdateID: 1, BusinessMessage: ownerMessage("/delete_my_data ABCD2345"),
			}); err != nil {
				t.Fatalf("HandleUpdate must swallow the outcome, got %v", err)
			}
			if len(sender.sent) != 1 {
				t.Fatalf("messages sent = %d, want 1", len(sender.sent))
			}
			if sender.sent[0].ChatID != 700001 {
				t.Fatalf("chat_id = %d, want the owner", sender.sent[0].ChatID)
			}
			if !strings.Contains(sender.sent[0].Text, tt.needle) {
				t.Fatalf("the answer never mentions %q:\n%s", tt.needle, sender.sent[0].Text)
			}
		})
	}
}

// TestErasureIgnoresEveryoneButTheOwner is the acceptance criterion that
// matters most here: a contact typing either form of the command erases
// nothing and is answered nothing — neither in the chat, nor privately.
func TestErasureIgnoresEveryoneButTheOwner(t *testing.T) {
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
		for _, text := range []string{"/delete_my_data", "/delete_my_data ABCD2345"} {
			t.Run(name+" "+text, func(t *testing.T) {
				sender := &fakeSender{}
				eraser := &fakeEraser{}
				h, msgs := newErasureHandler(sender, eraser)

				if err := h.HandleUpdate(context.Background(), telegram.Update{UpdateID: 1, BusinessMessage: build(text)}); err != nil {
					t.Fatalf("HandleUpdate: %v", err)
				}
				if len(sender.sent) != 0 {
					t.Fatalf("%d message(s) sent, want 0: %+v", len(sender.sent), sender.sent)
				}
				if len(eraser.requests) != 0 || len(eraser.confirmed) != 0 {
					t.Fatal("a third party reached the eraser")
				}
				if len(msgs.saved) != 1 {
					t.Fatal("the message must still be saved: only the answer and the erasure are withheld")
				}
			})
		}
	}
}

// TestErasureIgnoresNonCommands: the near misses. A different command word (an
// underscore is part of one), a quoted command, and a leading space are all
// things a contact or the owner writes in a conversation, and none of them is
// an instruction to delete an account.
func TestErasureIgnoresNonCommands(t *testing.T) {
	for _, text := range []string{
		"/delete_my_data_extra",
		"/delete_my_data_extra ABCD2345",
		"tell me about /delete_my_data",
		" /delete_my_data",
		"delete_my_data",
		"/delete_my",
	} {
		t.Run(text, func(t *testing.T) {
			sender := &fakeSender{}
			eraser := &fakeEraser{}
			h, _ := newErasureHandler(sender, eraser)

			if err := h.HandleUpdate(context.Background(), telegram.Update{UpdateID: 1, BusinessMessage: ownerMessage(text)}); err != nil {
				t.Fatalf("HandleUpdate: %v", err)
			}
			if len(sender.sent) != 0 {
				t.Fatalf("%q triggered %d message(s), want 0", text, len(sender.sent))
			}
			if len(eraser.requests) != 0 || len(eraser.confirmed) != 0 {
				t.Fatalf("%q reached the eraser", text)
			}
		})
	}
}

// TestErasureNeverTriggeredByAnEdit: rewriting an old message into a
// confirmation is not an act, and it would let a code be spent long after the
// message that carried it was read.
func TestErasureNeverTriggeredByAnEdit(t *testing.T) {
	sender := &fakeSender{}
	eraser := &fakeEraser{}
	h, _ := newErasureHandler(sender, eraser)

	if err := h.HandleUpdate(context.Background(), telegram.Update{
		UpdateID: 1, EditedBusinessMessage: ownerMessage("/delete_my_data ABCD2345"),
	}); err != nil {
		t.Fatalf("HandleUpdate: %v", err)
	}
	if len(eraser.confirmed) != 0 || len(sender.sent) != 0 {
		t.Fatal("an edit spent a confirmation code")
	}
}

// TestErasureWithoutAnEraserIsInert: a Handler built without WithDataEraser
// keeps saving everything and answers nothing. No nil dereference on the
// poller path, and above all no confirmation for an erasure nothing performed.
func TestErasureWithoutAnEraserIsInert(t *testing.T) {
	sender := &fakeSender{}
	h, msgs := newErasureHandler(sender, nil)

	for _, text := range []string{"/delete_my_data", "/delete_my_data ABCD2345"} {
		if err := h.HandleUpdate(context.Background(), telegram.Update{UpdateID: 1, BusinessMessage: ownerMessage(text)}); err != nil {
			t.Fatalf("HandleUpdate: %v", err)
		}
	}
	if len(sender.sent) != 0 {
		t.Fatalf("%d message(s) sent without an eraser, want 0", len(sender.sent))
	}
	if len(msgs.saved) != 2 {
		t.Fatalf("saved messages = %d, want 2", len(msgs.saved))
	}
}

// TestErasureIgnoredOnRefusedConnections: a connection the mono-tenant guard
// rejects, or a disabled one, provides no owner — and a disabled connection is
// also what a tenant looks like right after an erasure, which must not be a
// path back into one.
func TestErasureIgnoredOnRefusedConnections(t *testing.T) {
	disabled := enabledConn()
	disabled.IsEnabled = false

	tests := []struct {
		name string
		biz  *fakeBusiness
	}{
		{name: "refused by the mono-tenant guard", biz: &fakeBusiness{resolveErr: map[string]error{"bc-1": business.ErrOwnerMismatch}}},
		{name: "disabled connection", biz: &fakeBusiness{connections: map[string]*business.Connection{"bc-1": disabled}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sender := &fakeSender{}
			eraser := &fakeEraser{}
			h := NewHandler(tt.biz, &fakeMessages{}, &fakeMedia{}, testLogger(),
				WithCommandSender(sender), WithDataEraser(eraser, 14))

			if err := h.HandleUpdate(context.Background(), telegram.Update{
				UpdateID: 1, BusinessMessage: ownerMessage("/delete_my_data ABCD2345"),
			}); err != nil {
				t.Fatalf("HandleUpdate: %v", err)
			}
			if len(sender.sent) != 0 || len(eraser.confirmed) != 0 {
				t.Fatal("a refused connection reached the eraser or the sender")
			}
		})
	}
}

// TestErasureRunsUnderDeadlines: the challenge, the deletion and the answer all
// run on the poller's single goroutine, which getUpdates cannot leave until they
// return. Without a ceiling, a stuck database or an unbounded 429 retry_after
// would park the poller — and delay the deleted_business_messages updates that
// carry content existing nowhere else.
func TestErasureRunsUnderDeadlines(t *testing.T) {
	for _, text := range []string{"/delete_my_data", "/delete_my_data ABCD2345"} {
		t.Run(text, func(t *testing.T) {
			sender := &fakeSender{}
			eraser := &fakeEraser{}
			h, _ := newErasureHandler(sender, eraser)

			if err := h.HandleUpdate(context.Background(), telegram.Update{UpdateID: 1, BusinessMessage: ownerMessage(text)}); err != nil {
				t.Fatalf("HandleUpdate: %v", err)
			}
			// Measured AFTER the call: every deadline was set during it, so none
			// may reach further than now plus the widest bound the command uses.
			ceiling := time.Now().Add(erasureTimeout)

			deadlines := append(append([]time.Time{}, eraser.deadlines...), sender.deadlines...)
			if len(deadlines) < 2 {
				t.Fatalf("only %d bounded calls, want the eraser and the answer", len(deadlines))
			}
			for index, deadline := range deadlines {
				if deadline.IsZero() {
					t.Fatalf("call %d ran on a context with no deadline", index)
				}
				if deadline.After(ceiling) {
					t.Fatalf("call %d may run %v past erasureTimeout (%v)", index, deadline.Sub(ceiling), erasureTimeout)
				}
			}
		})
	}
}

// TestErasureAnswerFailureIsNotAnUpdateFailure: the message is already saved,
// and an undelivered confirmation must never replay the update that carried the
// command — which would submit the same code a second time.
func TestErasureAnswerFailureIsNotAnUpdateFailure(t *testing.T) {
	sender := &fakeSender{failAt: 1}
	eraser := &fakeEraser{outcome: erasure.OutcomeErased}
	h, msgs := newErasureHandler(sender, eraser)

	if err := h.HandleUpdate(context.Background(), telegram.Update{
		UpdateID: 1, BusinessMessage: ownerMessage("/delete_my_data ABCD2345"),
	}); err != nil {
		t.Fatalf("HandleUpdate must swallow a send failure, got %v", err)
	}
	if len(msgs.saved) != 1 {
		t.Fatalf("saved messages = %d, want 1", len(msgs.saved))
	}
	if len(eraser.confirmed) != 1 {
		t.Fatalf("the code was submitted %d times, want 1", len(eraser.confirmed))
	}
}

// TestAFailedChallengeSendsNothing: an eraser that cannot even record the
// challenge must not produce a message telling the owner to confirm with a code
// that exists nowhere.
func TestAFailedChallengeSendsNothing(t *testing.T) {
	sender := &fakeSender{}
	eraser := &fakeEraser{requestErr: errors.New("database down")}
	h, _ := newErasureHandler(sender, eraser)

	if err := h.HandleUpdate(context.Background(), telegram.Update{
		UpdateID: 1, BusinessMessage: ownerMessage("/delete_my_data"),
	}); err != nil {
		t.Fatalf("HandleUpdate must swallow the failure, got %v", err)
	}
	if len(sender.sent) != 0 {
		t.Fatalf("%d message(s) sent for a challenge that was never issued", len(sender.sent))
	}
}

// TestErasureCommandInACaptionIsStillACommand: Telegram puts the text of a
// media message in caption, never in text, and the command is read from the
// same place the saved content comes from.
func TestErasureCommandInACaptionIsStillACommand(t *testing.T) {
	sender := &fakeSender{}
	eraser := &fakeEraser{outcome: erasure.OutcomeErased}
	h, _ := newErasureHandler(sender, eraser)

	msg := ownerMessage("")
	msg.Caption = "/delete_my_data ABCD2345"
	msg.Photo = []telegram.PhotoSize{{FileID: "f-1", FileUniqueID: "u-1", Width: 10, Height: 10}}

	if err := h.HandleUpdate(context.Background(), telegram.Update{UpdateID: 1, BusinessMessage: msg}); err != nil {
		t.Fatalf("HandleUpdate: %v", err)
	}
	if len(eraser.confirmed) != 1 {
		t.Fatal("a command in a caption was ignored")
	}
}
