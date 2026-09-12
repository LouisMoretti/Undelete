package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/LouisMoretti/Undelete/bot/internal/business"
	"github.com/LouisMoretti/Undelete/bot/internal/privacy"
	"github.com/LouisMoretti/Undelete/bot/internal/telegram"
)

// fakeSender records what the handler asks Telegram to send, and can fail on
// demand at a chosen call.
type fakeSender struct {
	sent    []telegram.SendMessageRequest
	failAt  int // 1-based index of the call that fails; 0 = never fails
	sendErr error
}

func (f *fakeSender) SendMessage(_ context.Context, req telegram.SendMessageRequest) error {
	f.sent = append(f.sent, req)
	if f.failAt > 0 && len(f.sent) == f.failAt {
		if f.sendErr == nil {
			f.sendErr = errors.New("telegram unavailable")
		}
		return f.sendErr
	}
	return nil
}

// ownerMessage is a /privacy typed by the account holder in a chat covered by
// the connection — the only shape the Bot API can deliver, since
// allowed_updates never includes a plain `message` (constraint #2).
func ownerMessage(text string) *telegram.Message {
	msg := testMessage()
	msg.From = &telegram.User{ID: 700001, FirstName: "Louis"}
	msg.Text = text
	return msg
}

func newPrivacyHandler(sender *fakeSender) (*Handler, *fakeMessages) {
	biz := &fakeBusiness{connections: map[string]*business.Connection{"bc-1": enabledConn()}}
	msgs := &fakeMessages{}
	opts := []Option{}
	if sender != nil {
		opts = append(opts, WithCommandSender(sender))
	}
	return NewHandler(biz, msgs, &fakeMedia{}, testLogger(), opts...), msgs
}

// TestPrivacyCommandAnswersTheOwner is the happy path: the holder types
// /privacy in a monitored chat, the message is saved like any other
// (constraint #8), and the policy comes back as a direct message to the
// holder — never into the chat it was typed in.
func TestPrivacyCommandAnswersTheOwner(t *testing.T) {
	sender := &fakeSender{}
	h, msgs := newPrivacyHandler(sender)
	msg := ownerMessage("/privacy")

	if err := h.HandleUpdate(context.Background(), telegram.Update{UpdateID: 1, BusinessMessage: msg}); err != nil {
		t.Fatalf("HandleUpdate: %v", err)
	}

	if len(msgs.saved) != 1 {
		t.Fatalf("saved messages = %d, want 1: a command is a message like any other", len(msgs.saved))
	}
	if len(sender.sent) == 0 {
		t.Fatal("no answer sent to the owner")
	}

	var rebuilt strings.Builder
	for index, req := range sender.sent {
		if req.ChatID != 700001 {
			t.Fatalf("chunk %d: chat_id = %d, want the owner (700001)", index, req.ChatID)
		}
		if req.ChatID == msg.Chat.ID {
			t.Fatalf("chunk %d was sent into the monitored chat", index)
		}
		rebuilt.WriteString(req.Text)
	}
	if rebuilt.String() != privacy.Text() {
		t.Fatal("the answer is not the policy document, whole and in order")
	}
	if len(sender.sent) < 2 {
		t.Fatalf("chunks = %d: the policy is longer than one Telegram message, the split must happen", len(sender.sent))
	}
}

// TestPrivacyCommandAcceptsTheWireShapes: Telegram appends @botname, and the
// user may type arguments or capitals. All of them answer.
func TestPrivacyCommandAcceptsTheWireShapes(t *testing.T) {
	for _, text := range []string{"/privacy", "/privacy@undelete_bot", "/privacy please", "/Privacy"} {
		t.Run(text, func(t *testing.T) {
			sender := &fakeSender{}
			h, _ := newPrivacyHandler(sender)
			if err := h.HandleUpdate(context.Background(), telegram.Update{UpdateID: 1, BusinessMessage: ownerMessage(text)}); err != nil {
				t.Fatalf("HandleUpdate: %v", err)
			}
			if len(sender.sent) == 0 {
				t.Fatalf("%q did not trigger an answer", text)
			}
		})
	}
}

// TestPrivacyCommandIgnoresEveryoneButTheOwner is the acceptance criterion of
// the issue: a contact writing /privacy in a monitored chat receives nothing,
// and neither does the holder — nothing at all is sent.
func TestPrivacyCommandIgnoresEveryoneButTheOwner(t *testing.T) {
	tests := []struct {
		name    string
		message func() *telegram.Message
	}{
		{
			name: "a contact in the monitored chat",
			message: func() *telegram.Message {
				msg := testMessage() // From.ID 700002, not the owner
				msg.Text = "/privacy"
				return msg
			},
		},
		{
			name: "a message without a sender",
			message: func() *telegram.Message {
				msg := testMessage()
				msg.From = nil
				msg.Text = "/privacy"
				return msg
			},
		},
		{
			name: "an impostor claiming the owner's chat",
			message: func() *telegram.Message {
				msg := testMessage()
				msg.From = &telegram.User{ID: 700003, FirstName: "Mallory"}
				msg.Chat = telegram.Chat{ID: 700001, Type: "private"}
				msg.Text = "/privacy"
				return msg
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sender := &fakeSender{}
			h, msgs := newPrivacyHandler(sender)
			if err := h.HandleUpdate(context.Background(), telegram.Update{UpdateID: 1, BusinessMessage: tt.message()}); err != nil {
				t.Fatalf("HandleUpdate: %v", err)
			}
			if len(sender.sent) != 0 {
				t.Fatalf("%d message(s) sent, want 0: %+v", len(sender.sent), sender.sent)
			}
			if len(msgs.saved) != 1 {
				t.Fatal("the message must still be saved: only the answer is withheld")
			}
		})
	}
}

// TestPrivacyCommandIgnoredOnRefusedConnections: a connection the mono-tenant
// guard rejects, or one that is disabled, provides no owner to answer.
func TestPrivacyCommandIgnoredOnRefusedConnections(t *testing.T) {
	disabled := enabledConn()
	disabled.IsEnabled = false

	tests := []struct {
		name string
		biz  *fakeBusiness
	}{
		{
			name: "connection refused by the mono-tenant guard",
			biz:  &fakeBusiness{resolveErr: map[string]error{"bc-1": business.ErrOwnerMismatch}},
		},
		{
			name: "disabled connection",
			biz:  &fakeBusiness{connections: map[string]*business.Connection{"bc-1": disabled}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sender := &fakeSender{}
			h := NewHandler(tt.biz, &fakeMessages{}, &fakeMedia{}, testLogger(), WithCommandSender(sender))
			if err := h.HandleUpdate(context.Background(), telegram.Update{UpdateID: 1, BusinessMessage: ownerMessage("/privacy")}); err != nil {
				t.Fatalf("HandleUpdate: %v", err)
			}
			if len(sender.sent) != 0 {
				t.Fatalf("%d message(s) sent, want 0", len(sender.sent))
			}
		})
	}
}

// TestNoAnswerWithoutACommand: an ordinary message, and a message that merely
// quotes the command, must stay silent. A contact writing about /privacy in a
// monitored chat is a conversation, not an instruction.
func TestNoAnswerWithoutACommand(t *testing.T) {
	for _, text := range []string{"hello", "tell me about /privacy", " /privacy", "privacy", "/unknown", "/privacy_policy"} {
		t.Run(text, func(t *testing.T) {
			sender := &fakeSender{}
			h, _ := newPrivacyHandler(sender)
			if err := h.HandleUpdate(context.Background(), telegram.Update{UpdateID: 1, BusinessMessage: ownerMessage(text)}); err != nil {
				t.Fatalf("HandleUpdate: %v", err)
			}
			if len(sender.sent) != 0 {
				t.Fatalf("%q triggered %d message(s), want 0", text, len(sender.sent))
			}
		})
	}
}

// TestPrivacyCommandInACaptionIsStillACommand: Telegram puts the text of a
// media message in caption, never in text. A command typed as the caption of
// a photo is read from the same place the saved content comes from.
func TestPrivacyCommandInACaptionIsStillACommand(t *testing.T) {
	sender := &fakeSender{}
	h, _ := newPrivacyHandler(sender)
	msg := ownerMessage("")
	msg.Caption = "/privacy"
	msg.Photo = []telegram.PhotoSize{{FileID: "f-1", FileUniqueID: "u-1", Width: 10, Height: 10}}

	if err := h.HandleUpdate(context.Background(), telegram.Update{UpdateID: 1, BusinessMessage: msg}); err != nil {
		t.Fatalf("HandleUpdate: %v", err)
	}
	if len(sender.sent) == 0 {
		t.Fatal("a command in a caption was ignored")
	}
}

// TestEditedMessageNeverTriggersACommand: rewriting an old message into
// "/privacy" is not an act, and an edit storm must not turn into an answer
// storm.
func TestEditedMessageNeverTriggersACommand(t *testing.T) {
	sender := &fakeSender{}
	h, _ := newPrivacyHandler(sender)

	if err := h.HandleUpdate(context.Background(), telegram.Update{UpdateID: 1, EditedBusinessMessage: ownerMessage("/privacy")}); err != nil {
		t.Fatalf("HandleUpdate: %v", err)
	}
	if len(sender.sent) != 0 {
		t.Fatalf("an edit triggered %d message(s), want 0", len(sender.sent))
	}
}

// TestPrivacyCommandWithoutSenderIsInert: a Handler built without
// WithCommandSender keeps saving everything and simply never answers. No nil
// dereference on the poller path.
func TestPrivacyCommandWithoutSenderIsInert(t *testing.T) {
	h, msgs := newPrivacyHandler(nil)
	if err := h.HandleUpdate(context.Background(), telegram.Update{UpdateID: 1, BusinessMessage: ownerMessage("/privacy")}); err != nil {
		t.Fatalf("HandleUpdate: %v", err)
	}
	if len(msgs.saved) != 1 {
		t.Fatalf("saved messages = %d, want 1", len(msgs.saved))
	}
}

// TestPrivacyAnswerFailureIsNotAnUpdateFailure: a failed send stops the
// remaining chunks (a policy missing its middle is worse than none) and never
// surfaces as an update error — the message is already saved, and replaying
// the update would re-save it.
func TestPrivacyAnswerFailureIsNotAnUpdateFailure(t *testing.T) {
	sender := &fakeSender{failAt: 1}
	h, msgs := newPrivacyHandler(sender)

	if err := h.HandleUpdate(context.Background(), telegram.Update{UpdateID: 1, BusinessMessage: ownerMessage("/privacy")}); err != nil {
		t.Fatalf("HandleUpdate must swallow a send failure, got %v", err)
	}
	if len(sender.sent) != 1 {
		t.Fatalf("send attempts = %d, want 1: the following chunks must be dropped", len(sender.sent))
	}
	if len(msgs.saved) != 1 {
		t.Fatalf("saved messages = %d, want 1", len(msgs.saved))
	}
}

// TestNoAnswerWhenTheMessageCouldNotBeSaved: the save failure is what the
// poller logs, and answering a command whose message was lost would claim a
// capture that did not happen.
func TestNoAnswerWhenTheMessageCouldNotBeSaved(t *testing.T) {
	biz := &fakeBusiness{connections: map[string]*business.Connection{"bc-1": enabledConn()}}
	msgs := &fakeMessages{saveErr: errors.New("database down")}
	sender := &fakeSender{}
	h := NewHandler(biz, msgs, &fakeMedia{}, testLogger(), WithCommandSender(sender))

	err := h.HandleUpdate(context.Background(), telegram.Update{UpdateID: 1, BusinessMessage: ownerMessage("/privacy")})
	if err == nil {
		t.Fatal("a save failure must surface as an error")
	}
	if len(sender.sent) != 0 {
		t.Fatalf("%d message(s) sent despite the save failure", len(sender.sent))
	}
}
