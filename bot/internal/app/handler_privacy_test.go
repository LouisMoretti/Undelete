package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/LouisMoretti/Undelete/bot/internal/business"
	"github.com/LouisMoretti/Undelete/bot/internal/privacy"
	"github.com/LouisMoretti/Undelete/bot/internal/telegram"
)

// fakeSender records what the handler asks Telegram to send, and can fail on
// demand at a chosen call. It also records the deadline each call was given:
// the answer runs on the poller's goroutine, so the absence of a deadline is a
// defect in itself.
type fakeSender struct {
	sent      []telegram.SendMessageRequest
	deadlines []time.Time
	failAt    int // 1-based index of the call that fails; 0 = never fails
	sendErr   error
}

func (f *fakeSender) SendMessage(ctx context.Context, req telegram.SendMessageRequest) error {
	deadline, _ := ctx.Deadline()
	f.deadlines = append(f.deadlines, deadline)
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
// allowed_updates never includes a plain `message` (the explicit
// `allowed_updates` constraint).
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
// (the exhaustive-and-automatic-saving constraint), and the policy comes back as a direct message to the
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
		rebuilt.WriteString(policyBody(t, sender.sent, index))
	}
	if rebuilt.String() != privacy.Text() {
		t.Fatal("the answer is not the policy document, whole and in order")
	}
	if len(sender.sent) < 2 {
		t.Fatalf("chunks = %d: the policy is longer than one Telegram message, the split must happen", len(sender.sent))
	}
}

// policyBody strips the label of a chunk and, in doing so, asserts that the
// total it announces is the number of messages actually sent: a label promising
// three messages when two went out would hide exactly what it exists to reveal.
func policyBody(t *testing.T, sent []telegram.SendMessageRequest, index int) string {
	t.Helper()
	prefix := fmt.Sprintf("Privacy policy (%d/%d)\n\n", index+1, len(sent))
	if !strings.HasPrefix(sent[index].Text, prefix) {
		t.Fatalf("chunk %d does not start with %q", index, prefix)
	}
	return strings.TrimPrefix(sent[index].Text, prefix)
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

	// What the owner is left with must be readable as incomplete: the chunk
	// that did go out announces a total the owner never received.
	if !strings.HasPrefix(sender.sent[0].Text, "Privacy policy (1/") {
		t.Fatalf("the delivered chunk carries no label: %q", sender.sent[0].Text[:40])
	}
	if strings.HasPrefix(sender.sent[0].Text, "Privacy policy (1/1)") {
		t.Fatal("the delivered chunk claims to be the whole policy, yet the rest was dropped")
	}
}

// TestPrivacyAnswerRunsUnderADeadline: the answer is built and sent on the
// poller's single goroutine, which getUpdates cannot leave until it returns.
// telegram.Client retries three times and honours an unbounded 429
// retry_after, so the deadline is the only thing keeping one /privacy from
// parking the poller -- and delaying the deleted_business_messages updates
// that carry content existing nowhere else.
func TestPrivacyAnswerRunsUnderADeadline(t *testing.T) {
	sender := &fakeSender{}
	h, _ := newPrivacyHandler(sender)

	if err := h.HandleUpdate(context.Background(), telegram.Update{UpdateID: 1, BusinessMessage: ownerMessage("/privacy")}); err != nil {
		t.Fatalf("HandleUpdate: %v", err)
	}
	// The ceiling is measured AFTER the call: every deadline was set during it,
	// so none of them may reach further than now plus the timeout.
	ceiling := time.Now().Add(commandAnswerTimeout)

	if len(sender.deadlines) == 0 {
		t.Fatal("no answer sent to the owner")
	}
	for index, deadline := range sender.deadlines {
		if deadline.IsZero() {
			t.Fatalf("chunk %d was sent on a context with no deadline: an unbounded retry_after would block the poller", index)
		}
		if deadline.After(ceiling) {
			t.Fatalf("chunk %d may run %v past commandAnswerTimeout (%v)",
				index, deadline.Sub(ceiling), commandAnswerTimeout)
		}
	}
}

// blockingSender waits for its context to end before returning, the way
// telegram.Client does while honouring a retry_after.
type blockingSender struct{}

func (blockingSender) SendMessage(ctx context.Context, _ telegram.SendMessageRequest) error {
	<-ctx.Done()
	return ctx.Err()
}

// TestPrivacyAnswerDoesNotWaitForAStalledSender is the behaviour the deadline
// buys: a sender that never comes back releases the poller anyway, and the
// failure stays a logged failure.
//
// The bound exercised here is a 50 ms parent deadline rather than the 10 s
// constant -- same mechanism, and a unit test that sleeps ten seconds is a test
// nobody runs. That the constant itself is applied is what
// TestPrivacyAnswerRunsUnderADeadline asserts.
func TestPrivacyAnswerDoesNotWaitForAStalledSender(t *testing.T) {
	biz := &fakeBusiness{connections: map[string]*business.Connection{"bc-1": enabledConn()}}
	h := NewHandler(biz, &fakeMessages{}, &fakeMedia{}, testLogger(), WithCommandSender(blockingSender{}))

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- h.HandleUpdate(ctx, telegram.Update{UpdateID: 1, BusinessMessage: ownerMessage("/privacy")})
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("a stalled send must not surface as an update failure, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("HandleUpdate is still waiting on the sender: the command answer is not bounded")
	}
}

// TestPrivacyAnswerWithNoChunksLogsAnError: BuildPrivacyMessageRequests
// returns nil for an empty document, so a zero-chunk answer must log an error
// and send nothing -- never the success line with chunks=0.
func TestPrivacyAnswerWithNoChunksLogsAnError(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	biz := &fakeBusiness{connections: map[string]*business.Connection{"bc-1": enabledConn()}}
	sender := &fakeSender{}
	h := NewHandler(biz, &fakeMessages{}, &fakeMedia{}, logger, WithCommandSender(sender))

	h.sendPrivacyAnswer(context.Background(), enabledConn(), nil)

	if len(sender.sent) != 0 {
		t.Fatalf("%d message(s) sent, want 0: there were no chunks to send", len(sender.sent))
	}
	if !strings.Contains(logs.String(), "produced no chunks") {
		t.Fatalf("no error logged for the zero-chunk answer: %q", logs.String())
	}
	if strings.Contains(logs.String(), "privacy policy sent") {
		t.Fatalf("the success line was logged for an answer that sent nothing: %q", logs.String())
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
