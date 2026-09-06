package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/LouisMoretti/Undelete/bot/internal/business"
	"github.com/LouisMoretti/Undelete/bot/internal/media"
	"github.com/LouisMoretti/Undelete/bot/internal/messages"
	"github.com/LouisMoretti/Undelete/bot/internal/telegram"
)

// fakeBusiness implements businessService without a database.
type fakeBusiness struct {
	connections map[string]*business.Connection
	resolveErr  map[string]error
	handled     []telegram.BusinessConnection
	handleErr   error
}

func (f *fakeBusiness) Resolve(_ context.Context, id string) (*business.Connection, error) {
	if err, ok := f.resolveErr[id]; ok {
		return nil, err
	}
	if c, ok := f.connections[id]; ok {
		return c, nil
	}
	return nil, business.ErrOwnerMismatch
}

func (f *fakeBusiness) HandleBusinessConnection(_ context.Context, tc telegram.BusinessConnection) error {
	if f.handleErr != nil {
		return f.handleErr
	}
	f.handled = append(f.handled, tc)
	return nil
}

// fakeMessages implements messageStore in memory.
type fakeMessages struct {
	saved      []messages.Record
	saveErr    error
	deleted    []messages.DeletedRecord
	markErr    error
	editedSeen []bool
}

func (f *fakeMessages) Save(_ context.Context, _ int64, m messages.Record, edited bool) error {
	if f.saveErr != nil {
		return f.saveErr
	}
	f.saved = append(f.saved, m)
	f.editedSeen = append(f.editedSeen, edited)
	return nil
}

func (f *fakeMessages) MarkDeleted(_ context.Context, _, _ int64, _ string, _ int64, _ []int64) ([]messages.DeletedRecord, error) {
	if f.markErr != nil {
		return nil, f.markErr
	}
	return f.deleted, nil
}

// fakeMedia implements mediaCatalogue in memory.
type fakeMedia struct {
	saved   []media.Record
	saveErr error
}

func (f *fakeMedia) Save(_ context.Context, _ int64, m media.Record) (int64, error) {
	if f.saveErr != nil {
		return 0, f.saveErr
	}
	f.saved = append(f.saved, m)
	return int64(len(f.saved)), nil
}

func testLogger() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

func enabledConn() *business.Connection {
	return &business.Connection{ID: "bc-1", OwnerUserID: 11, OwnerTelegramUserID: 700001, CanReply: true, IsEnabled: true}
}

func testMessage() *telegram.Message {
	return &telegram.Message{
		MessageID:            3,
		BusinessConnectionID: "bc-1",
		Chat:                 telegram.Chat{ID: 77, Type: "private", FirstName: "Anaïs"},
		From:                 &telegram.User{ID: 700002, FirstName: "Zoë", LastName: "Test"},
		Text:                 "hello",
		Date:                 1700000000,
	}
}

// TestHandleUpdateDispatchesEveryBusinessType pins the router: each of the
// four business_* updates reaches its handler, and an unknown update is
// ignored (nil) rather than failing the poller loop.
func TestHandleUpdateDispatchesEveryBusinessType(t *testing.T) {
	biz := &fakeBusiness{connections: map[string]*business.Connection{"bc-1": enabledConn()}}
	msgs := &fakeMessages{}
	med := &fakeMedia{}
	h := NewHandler(biz, msgs, med, testLogger())
	ctx := context.Background()

	if err := h.HandleUpdate(ctx, telegram.Update{UpdateID: 1, BusinessConnection: &telegram.BusinessConnection{ID: "bc-1"}}); err != nil {
		t.Fatalf("BusinessConnection: %v", err)
	}
	if len(biz.handled) != 1 {
		t.Fatalf("HandleBusinessConnection calls = %d, want 1", len(biz.handled))
	}
	if err := h.HandleUpdate(ctx, telegram.Update{UpdateID: 2, BusinessMessage: testMessage()}); err != nil {
		t.Fatalf("BusinessMessage: %v", err)
	}
	if len(msgs.saved) != 1 || msgs.editedSeen[0] {
		t.Fatalf("business_message must save with edited=false: %+v", msgs.editedSeen)
	}
	if err := h.HandleUpdate(ctx, telegram.Update{UpdateID: 3, EditedBusinessMessage: testMessage()}); err != nil {
		t.Fatalf("EditedBusinessMessage: %v", err)
	}
	if len(msgs.saved) != 2 || !msgs.editedSeen[1] {
		t.Fatalf("edited_business_message must save with edited=true: %+v", msgs.editedSeen)
	}
	msgs.deleted = []messages.DeletedRecord{{ChatID: 77, MessageID: 3}}
	if err := h.HandleUpdate(ctx, telegram.Update{UpdateID: 4, DeletedBusinessMessages: &telegram.BusinessMessagesDeleted{
		BusinessConnectionID: "bc-1", Chat: telegram.Chat{ID: 77}, MessageIDs: []int64{3},
	}}); err != nil {
		t.Fatalf("DeletedBusinessMessages: %v", err)
	}
	if err := h.HandleUpdate(ctx, telegram.Update{UpdateID: 5}); err != nil {
		t.Fatalf("unknown update must be ignored with nil, got %v", err)
	}
}

// TestSaveMessageGuards pins the three save filters: a refused connection is
// swallowed (nil), a disabled connection is ignored, and a resolution error
// is wrapped (the poller logs it and advances the offset).
func TestSaveMessageGuards(t *testing.T) {
	ctx := context.Background()

	t.Run("owner mismatch swallowed", func(t *testing.T) {
		biz := &fakeBusiness{}
		msgs := &fakeMessages{}
		h := NewHandler(biz, msgs, nil, testLogger())
		if err := h.HandleUpdate(ctx, telegram.Update{BusinessMessage: testMessage()}); err != nil {
			t.Fatalf("ErrOwnerMismatch must be swallowed, got %v", err)
		}
		if len(msgs.saved) != 0 {
			t.Fatal("refused connection must save nothing")
		}
	})

	t.Run("disabled connection ignored", func(t *testing.T) {
		disabled := enabledConn()
		disabled.IsEnabled = false
		biz := &fakeBusiness{connections: map[string]*business.Connection{"bc-1": disabled}}
		msgs := &fakeMessages{}
		h := NewHandler(biz, msgs, nil, testLogger())
		if err := h.HandleUpdate(ctx, telegram.Update{BusinessMessage: testMessage()}); err != nil {
			t.Fatalf("disabled connection must be ignored with nil, got %v", err)
		}
		if len(msgs.saved) != 0 {
			t.Fatal("disabled connection must save nothing")
		}
	})

	t.Run("resolution error wrapped", func(t *testing.T) {
		biz := &fakeBusiness{resolveErr: map[string]error{"bc-1": errors.New("db down")}}
		h := NewHandler(biz, &fakeMessages{}, nil, testLogger())
		if err := h.HandleUpdate(ctx, telegram.Update{BusinessMessage: testMessage()}); err == nil {
			t.Fatal("resolution error must propagate")
		}
	})

	t.Run("save error wrapped", func(t *testing.T) {
		biz := &fakeBusiness{connections: map[string]*business.Connection{"bc-1": enabledConn()}}
		msgs := &fakeMessages{saveErr: errors.New("constraint")}
		h := NewHandler(biz, msgs, nil, testLogger())
		if err := h.HandleUpdate(ctx, telegram.Update{BusinessMessage: testMessage()}); err == nil {
			t.Fatal("save error must propagate")
		}
	})
}

// TestSaveMessageWithoutSender pins the sender-less path (channels, service
// messages): From==nil must save with empty display and nil id, never panic.
func TestSaveMessageWithoutSender(t *testing.T) {
	biz := &fakeBusiness{connections: map[string]*business.Connection{"bc-1": enabledConn()}}
	msgs := &fakeMessages{}
	h := NewHandler(biz, msgs, nil, testLogger())
	msg := testMessage()
	msg.From = nil
	if err := h.HandleUpdate(context.Background(), telegram.Update{BusinessMessage: msg}); err != nil {
		t.Fatalf("sender-less message: %v", err)
	}
	if len(msgs.saved) != 1 {
		t.Fatal("sender-less message must still be saved")
	}
	if msgs.saved[0].FromUserID != nil || msgs.saved[0].FromDisplay != "" {
		t.Fatalf("sender-less save must carry no identity: %+v", msgs.saved[0])
	}
}

// TestSaveMediaNilDisablesCapture pins text-only mode: with a nil catalogue
// the message is still saved and no media error can occur.
func TestSaveMediaNilDisablesCapture(t *testing.T) {
	biz := &fakeBusiness{connections: map[string]*business.Connection{"bc-1": enabledConn()}}
	msgs := &fakeMessages{}
	h := NewHandler(biz, msgs, nil, testLogger())
	if err := h.saveMedia(context.Background(), 11, testMessage(), []telegram.MediaAttachment{{Type: "photo", FileID: "x"}}); err != nil {
		t.Fatalf("nil media catalogue must be a no-op, got %v", err)
	}
}

// TestSaveMediaIndexesAttachments pins the file_index contract: the position
// in ExtractMedia's deterministic list is what makes a Telegram redelivery
// hit the upsert instead of duplicating the file.
func TestSaveMediaIndexesAttachments(t *testing.T) {
	biz := &fakeBusiness{connections: map[string]*business.Connection{"bc-1": enabledConn()}}
	msgs := &fakeMessages{}
	med := &fakeMedia{}
	h := NewHandler(biz, msgs, med, testLogger())
	attachments := []telegram.MediaAttachment{
		{Type: "photo", FileID: "f1", FileUniqueID: "u1", ByteSize: 1024, Width: 800, Height: 600},
		{Type: "video", FileID: "f2", FileUniqueID: "u2"},
	}
	if err := h.saveMedia(context.Background(), 11, testMessage(), attachments); err != nil {
		t.Fatalf("saveMedia: %v", err)
	}
	if len(med.saved) != 2 || med.saved[0].FileIndex != 0 || med.saved[1].FileIndex != 1 {
		t.Fatalf("file_index must follow declaration order: %+v", med.saved)
	}
	if med.saved[0].ByteSize == nil || *med.saved[0].ByteSize != 1024 {
		t.Fatalf("non-zero metadata must survive: %+v", med.saved[0])
	}
	if med.saved[1].ByteSize != nil || med.saved[1].Width != nil {
		t.Fatalf("zero metadata must stay NULL, never 0: %+v", med.saved[1])
	}
}

// TestSaveMediaErrorPropagates pins catalogue failures: a media.Save error
// fails the update (the poller logs it) rather than silently dropping the
// catalogue row while the message claims attachments.
func TestSaveMediaErrorPropagates(t *testing.T) {
	med := &fakeMedia{saveErr: errors.New("db down")}
	h := NewHandler(nil, nil, med, testLogger())
	err := h.saveMedia(context.Background(), 11, testMessage(), []telegram.MediaAttachment{{Type: "photo"}})
	if err == nil {
		t.Fatal("catalogue error must propagate")
	}
}

// TestHandleDeletedPartialBatch pins constraint #6's degraded path: ids that
// predate the connection (or were purged) are missing from `found` -- logged
// and skipped, never an error -- while the counter only counts recovered
// messages.
func TestHandleDeletedPartialBatch(t *testing.T) {
	biz := &fakeBusiness{connections: map[string]*business.Connection{"bc-1": enabledConn()}}
	msgs := &fakeMessages{deleted: []messages.DeletedRecord{{ChatID: 77, MessageID: 3}}}
	h := NewHandler(biz, msgs, nil, testLogger())
	del := &telegram.BusinessMessagesDeleted{
		BusinessConnectionID: "bc-1", Chat: telegram.Chat{ID: 77}, MessageIDs: []int64{3, 999},
	}
	if err := h.handleDeleted(context.Background(), del); err != nil {
		t.Fatalf("partial batch must succeed, got %v", err)
	}
}

// TestHandleDeletedErrors pins the failure paths: resolution errors propagate
// (except the guard refusal), and MarkDeleted errors are wrapped.
func TestHandleDeletedErrors(t *testing.T) {
	ctx := context.Background()
	del := &telegram.BusinessMessagesDeleted{
		BusinessConnectionID: "bc-1", Chat: telegram.Chat{ID: 77}, MessageIDs: []int64{3},
	}

	t.Run("owner mismatch swallowed", func(t *testing.T) {
		h := NewHandler(&fakeBusiness{}, &fakeMessages{}, nil, testLogger())
		if err := h.handleDeleted(ctx, del); err != nil {
			t.Fatalf("guard refusal must be swallowed, got %v", err)
		}
	})
	t.Run("resolution error wrapped", func(t *testing.T) {
		biz := &fakeBusiness{resolveErr: map[string]error{"bc-1": errors.New("db down")}}
		h := NewHandler(biz, &fakeMessages{}, nil, testLogger())
		if err := h.handleDeleted(ctx, del); err == nil {
			t.Fatal("resolution error must propagate")
		}
	})
	t.Run("mark error wrapped", func(t *testing.T) {
		biz := &fakeBusiness{connections: map[string]*business.Connection{"bc-1": enabledConn()}}
		msgs := &fakeMessages{markErr: errors.New("tx failed")}
		h := NewHandler(biz, msgs, nil, testLogger())
		if err := h.handleDeleted(ctx, del); err == nil {
			t.Fatal("MarkDeleted error must propagate")
		}
	})
}

// TestMessageHelpers pins the pure mapping: type of the FIRST attachment
// wins, and text-vs-caption follows the Telegram invariant (never both).
func TestMessageHelpers(t *testing.T) {
	if got := messageType(nil); got != "text" {
		t.Fatalf("messageType(nil) = %q, want text", got)
	}
	attachments := []telegram.MediaAttachment{{Type: "photo"}, {Type: "video"}}
	if got := messageType(attachments); got != "photo" {
		t.Fatalf("first attachment must win, got %q", got)
	}
	if got := messageText(&telegram.Message{Text: "plain"}); got != "plain" {
		t.Fatalf("plain message must read Text, got %q", got)
	}
	if got := messageText(&telegram.Message{Caption: "cap"}); got != "cap" {
		t.Fatalf("media message must read Caption, got %q", got)
	}
	if got := messageText(&telegram.Message{Text: "t", Caption: "c"}); got != "t" {
		t.Fatalf("both filled must prefer Text, got %q", got)
	}
	if got := messageText(&telegram.Message{}); got != "" {
		t.Fatalf("empty message must read empty, got %q", got)
	}
	if optional(0) != nil {
		t.Fatal("optional(0) must be nil (NULL, never 0)")
	}
	if v := optional(7); v == nil || *v != 7 {
		t.Fatalf("optional(7) must round-trip, got %v", v)
	}
}

// TestDisplayNameLastNameOnlyHasNoLeadingSpace pins the fixed asymmetry with
// chatTitle: a last name without first name must not produce " Test".
func TestDisplayNameLastNameOnlyHasNoLeadingSpace(t *testing.T) {
	user := telegram.User{LastName: "Test"}
	if got := displayName(&user); got != "Test" {
		t.Fatalf("displayName(last only) = %q, want %q", got, "Test")
	}
	user = telegram.User{LastName: "Test", Username: "neo"}
	if got := displayName(&user); got != "Test (@neo)" {
		t.Fatalf("displayName(last+username) = %q, want %q", got, "Test (@neo)")
	}
}

// TestNewHandlerWiresDependencies pins construction: no nil logger panic on
// the ignored-update path, and the returned handler is usable.
func TestNewHandlerWiresDependencies(t *testing.T) {
	h := NewHandler(
		&fakeBusiness{connections: map[string]*business.Connection{"bc-1": enabledConn()}},
		&fakeMessages{},
		&fakeMedia{},
		testLogger(),
	)
	if h == nil {
		t.Fatal("NewHandler returned nil")
	}
	if err := h.HandleUpdate(context.Background(), telegram.Update{UpdateID: 9}); err != nil {
		t.Fatalf("unknown update on a wired handler: %v", err)
	}
}
