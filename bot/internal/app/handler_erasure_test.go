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
// nothing and is answered nothing — neither in the chat, nor privately --
// and nothing is WRITTEN either.
//
// A confirmation carries a live code, so persisting it would store the very
// secret the challenge table only ever hashes; a bare command from a third
// party is dropped with it, uniformly, before the save. What the owner types
// is another matter: their bare command saves like any other message.
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
				if len(msgs.saved) != 0 {
					t.Fatalf("%d message(s) saved, want 0: a third party's command writes nothing", len(msgs.saved))
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
// message that carried it was read. The edit is dropped before the save, so
// the code is not even persisted.
func TestErasureNeverTriggeredByAnEdit(t *testing.T) {
	sender := &fakeSender{}
	eraser := &fakeEraser{}
	h, msgs := newErasureHandler(sender, eraser)

	if err := h.HandleUpdate(context.Background(), telegram.Update{
		UpdateID: 1, EditedBusinessMessage: ownerMessage("/delete_my_data ABCD2345"),
	}); err != nil {
		t.Fatalf("HandleUpdate: %v", err)
	}
	if len(eraser.confirmed) != 0 || len(sender.sent) != 0 {
		t.Fatal("an edit spent a confirmation code")
	}
	if len(msgs.saved) != 0 {
		t.Fatalf("%d edited message(s) saved, want 0: a code is never stored, not even through an edit", len(msgs.saved))
	}
}

// TestErasureWithoutAnEraserIsInert: a Handler built without WithDataEraser
// keeps saving everything it may, and answers nothing. The bare command
// carries no secret and saves like any other message; a confirmation is
// dropped before the save even without an eraser behind it -- a code that
// can never be spent must still never be stored. No nil dereference on the
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
	if len(msgs.saved) != 1 {
		t.Fatalf("saved messages = %d, want 1: the bare command saves, the confirmation never does", len(msgs.saved))
	}
}

// TestErasureIgnoredOnARefusedConnection: a connection the mono-tenant guard
// rejects provides no owner, so even a well-formed confirmation from the
// configured owner id is dropped before the save -- silently, exactly as the
// capture would keep it.
func TestErasureIgnoredOnARefusedConnection(t *testing.T) {
	sender := &fakeSender{}
	eraser := &fakeEraser{}
	biz := &fakeBusiness{resolveErr: map[string]error{"bc-1": business.ErrOwnerMismatch}}
	h := NewHandler(biz, &fakeMessages{}, &fakeMedia{}, testLogger(),
		WithCommandSender(sender), WithDataEraser(eraser, 14))

	for _, text := range []string{"/delete_my_data", "/delete_my_data ABCD2345"} {
		if err := h.HandleUpdate(context.Background(), telegram.Update{
			UpdateID: 1, BusinessMessage: ownerMessage(text),
		}); err != nil {
			t.Fatalf("HandleUpdate: %v", err)
		}
	}
	if len(sender.sent) != 0 || len(eraser.confirmed) != 0 || len(eraser.requests) != 0 {
		t.Fatal("a refused connection reached the eraser or the sender")
	}
}

// disabledErasureHandler is a Handler whose only connection is disabled --
// the state a tenant is in after step 1 of an erasure, or after one completed.
func disabledErasureHandler(sender *fakeSender, eraser *fakeEraser) (*Handler, *fakeMessages) {
	disabled := enabledConn()
	disabled.IsEnabled = false
	biz := &fakeBusiness{connections: map[string]*business.Connection{"bc-1": disabled}}
	msgs := &fakeMessages{}
	return NewHandler(biz, msgs, &fakeMedia{}, testLogger(),
		WithCommandSender(sender), WithDataEraser(eraser, 14)), msgs
}

// TestErasureResumesOnADisabledConnection is the F1 path, end to end at the
// handler level: the first submission fails halfway (the eraser reports it),
// the owner resubmits the SAME code through the now-disabled connection, and
// the erasure completes. Both submissions are served without saving anything
// and without re-enabling the capture.
func TestErasureResumesOnADisabledConnection(t *testing.T) {
	sender := &fakeSender{}
	eraser := &fakeEraser{confirmErr: errors.New("database down halfway")}
	h, msgs := disabledErasureHandler(sender, eraser)

	if err := h.HandleUpdate(context.Background(), telegram.Update{
		UpdateID: 1, BusinessMessage: ownerMessage("/delete_my_data ABCD2345"),
	}); err != nil {
		t.Fatalf("first HandleUpdate: %v", err)
	}
	if len(eraser.confirmed) != 1 {
		t.Fatal("the first submission never reached the eraser: a disabled connection would strand every resume")
	}
	if len(sender.sent) != 1 || !strings.Contains(sender.sent[0].Text, "did not finish") {
		t.Fatalf("the failure was not answered as resumable: %+v", sender.sent)
	}

	eraser.confirmErr = nil
	eraser.outcome = erasure.OutcomeErased
	if err := h.HandleUpdate(context.Background(), telegram.Update{
		UpdateID: 2, BusinessMessage: ownerMessage("/delete_my_data ABCD2345"),
	}); err != nil {
		t.Fatalf("second HandleUpdate: %v", err)
	}
	if len(eraser.confirmed) != 2 {
		t.Fatal("resubmitting the same code on a disabled connection reached nothing")
	}
	if len(sender.sent) != 2 || !strings.Contains(sender.sent[1].Text, "Data erasure complete") {
		t.Fatalf("the resumed erasure was not confirmed: %+v", sender.sent)
	}
	if len(msgs.saved) != 0 {
		t.Fatalf("%d message(s) saved, want 0: control commands on a disabled connection save nothing", len(msgs.saved))
	}
	for _, sent := range sender.sent {
		if sent.ChatID != 700001 {
			t.Fatalf("an erasure answer went to %d, want the owner alone", sent.ChatID)
		}
	}
}

// TestErasureReplayOnADisabledConnection: after a completed erasure every
// connection is disabled, and resubmitting the spent code must still be
// answered -- "already erased", not silence, and not "unknown code".
func TestErasureReplayOnADisabledConnection(t *testing.T) {
	sender := &fakeSender{}
	eraser := &fakeEraser{outcome: erasure.OutcomeAlreadyErased}
	h, msgs := disabledErasureHandler(sender, eraser)

	if err := h.HandleUpdate(context.Background(), telegram.Update{
		UpdateID: 1, BusinessMessage: ownerMessage("/delete_my_data ABCD2345"),
	}); err != nil {
		t.Fatalf("HandleUpdate: %v", err)
	}
	if len(eraser.confirmed) != 1 {
		t.Fatal("the replay never reached the eraser")
	}
	if len(sender.sent) != 1 || !strings.Contains(sender.sent[0].Text, "Nothing left to erase") {
		t.Fatalf("the replay was not answered as already-erased: %+v", sender.sent)
	}
	if len(msgs.saved) != 0 {
		t.Fatalf("%d message(s) saved, want 0", len(msgs.saved))
	}
}

// TestErasureBareRequestOnADisabledConnection: the owner can still ask for a
// fresh code after an erasure (or an interrupted one) -- served without
// saving, without re-enabling.
func TestErasureBareRequestOnADisabledConnection(t *testing.T) {
	sender := &fakeSender{}
	eraser := &fakeEraser{code: "WXYZ7788"}
	h, msgs := disabledErasureHandler(sender, eraser)

	if err := h.HandleUpdate(context.Background(), telegram.Update{
		UpdateID: 1, BusinessMessage: ownerMessage("/delete_my_data"),
	}); err != nil {
		t.Fatalf("HandleUpdate: %v", err)
	}
	if len(eraser.requests) != 1 {
		t.Fatal("the bare command on a disabled connection issued nothing")
	}
	if len(sender.sent) != 1 || !strings.Contains(sender.sent[0].Text, "WXYZ7788") {
		t.Fatalf("the challenge was not delivered: %+v", sender.sent)
	}
	if len(msgs.saved) != 0 {
		t.Fatalf("%d message(s) saved, want 0", len(msgs.saved))
	}
}

// TestConfirmationIsNeverSaved is the F6 secrecy rule at the handler level:
// the owner's confirmation is spent and answered, and no record of it --
// neither the message nor its media -- is written anywhere the handler owns.
func TestConfirmationIsNeverSaved(t *testing.T) {
	sender := &fakeSender{}
	eraser := &fakeEraser{outcome: erasure.OutcomeErased}
	h, msgs := newErasureHandler(sender, eraser)

	msg := ownerMessage("/delete_my_data ABCD2345")
	if err := h.HandleUpdate(context.Background(), telegram.Update{UpdateID: 1, BusinessMessage: msg}); err != nil {
		t.Fatalf("HandleUpdate: %v", err)
	}
	if len(eraser.confirmed) != 1 {
		t.Fatal("the confirmation was not spent")
	}
	if len(sender.sent) != 1 {
		t.Fatal("the confirmation was not answered")
	}
	if len(msgs.saved) != 0 {
		t.Fatalf("%d message(s) saved, want 0: the code must never reach messages.text_content", len(msgs.saved))
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

// TestErasureAnswerFailureIsNotAnUpdateFailure: no message precedes the
// command any more (a confirmation is never saved), so an undelivered answer
// must simply stay silent towards the poller -- and above all must not fail
// the update, which would look like a reason to retry the spend.
func TestErasureAnswerFailureIsNotAnUpdateFailure(t *testing.T) {
	sender := &fakeSender{failAt: 1}
	eraser := &fakeEraser{outcome: erasure.OutcomeErased}
	h, msgs := newErasureHandler(sender, eraser)

	if err := h.HandleUpdate(context.Background(), telegram.Update{
		UpdateID: 1, BusinessMessage: ownerMessage("/delete_my_data ABCD2345"),
	}); err != nil {
		t.Fatalf("HandleUpdate must swallow a send failure, got %v", err)
	}
	if len(msgs.saved) != 0 {
		t.Fatalf("saved messages = %d, want 0: confirmations are never saved", len(msgs.saved))
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
