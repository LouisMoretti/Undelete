package app

import (
	"context"
	"testing"

	"github.com/LouisMoretti/Undelete/bot/internal/business"
	"github.com/LouisMoretti/Undelete/bot/internal/messages"
	"github.com/LouisMoretti/Undelete/bot/internal/quotas"
	"github.com/LouisMoretti/Undelete/bot/internal/telegram"
)

// quotaLimits returns tight quotas for tests: 2 messages, 1 media file,
// effectively unlimited bytes and rate, warn at 50%.
func quotaLimits() quotas.Limits {
	return quotas.Limits{
		MaxMessages:       2,
		MaxMediaFiles:     1,
		MaxMediaBytes:     1 << 30,
		CapturesPerMinute: 100000,
		WarnPercent:       50,
	}
}

func quotaTracker(t *testing.T) *quotas.Tracker {
	t.Helper()
	tr, err := quotas.NewTracker(quotaLimits(), nil, nil)
	if err != nil {
		t.Fatalf("NewTracker: %v", err)
	}
	return tr
}

func messageFor(connID string, chatID, messageID int64) *telegram.Message {
	msg := testMessage()
	msg.BusinessConnectionID = connID
	msg.Chat.ID = chatID
	msg.MessageID = messageID
	return msg
}

// TestQuotaDropsMessagesPastLimit pins the volume behaviour: past the limit
// the capture is dropped explicitly -- nil error (the poller advances past
// the update like past a refused connection), nothing saved.
func TestQuotaDropsMessagesPastLimit(t *testing.T) {
	biz := &fakeBusiness{connections: map[string]*business.Connection{"bc-1": enabledConn()}}
	msgs := &fakeMessages{}
	h := NewHandler(biz, msgs, nil, testLogger(), WithQuota(quotaTracker(t)))
	ctx := context.Background()

	for i := int64(0); i < 2; i++ {
		if err := h.HandleUpdate(ctx, telegram.Update{BusinessMessage: messageFor("bc-1", 77, 10+i)}); err != nil {
			t.Fatalf("admit %d: %v", i, err)
		}
	}
	if len(msgs.saved) != 2 {
		t.Fatalf("saved = %d, want 2", len(msgs.saved))
	}
	if err := h.HandleUpdate(ctx, telegram.Update{BusinessMessage: messageFor("bc-1", 77, 12)}); err != nil {
		t.Fatalf("over-quota update must not fail the poller, got %v", err)
	}
	if len(msgs.saved) != 2 {
		t.Fatalf("saved = %d after the drop, want still 2", len(msgs.saved))
	}
}

// TestQuotaDropIsTenantWideNotAChatSelection pins constraint #8 under quota:
// the drop applies to the tenant as a whole. Past the limit, messages from
// another chat of the same connection are dropped too -- while a message from
// another tenant is still saved.
func TestQuotaDropIsTenantWideNotAChatSelection(t *testing.T) {
	other := enabledConn()
	other.ID = "bc-2"
	other.OwnerUserID = 22
	other.OwnerTelegramUserID = 700009
	biz := &fakeBusiness{connections: map[string]*business.Connection{
		"bc-1": enabledConn(),
		"bc-2": other,
	}}
	msgs := &fakeMessages{}
	h := NewHandler(biz, msgs, nil, testLogger(), WithQuota(quotaTracker(t)))
	ctx := context.Background()

	for i := int64(0); i < 2; i++ {
		if err := h.HandleUpdate(ctx, telegram.Update{BusinessMessage: messageFor("bc-1", 77, 10+i)}); err != nil {
			t.Fatal(err)
		}
	}
	// Same tenant, different chat: dropped too -- the quota knows no chats.
	if err := h.HandleUpdate(ctx, telegram.Update{BusinessMessage: messageFor("bc-1", 999, 20)}); err != nil {
		t.Fatal(err)
	}
	// Another tenant: unaffected.
	if err := h.HandleUpdate(ctx, telegram.Update{BusinessMessage: messageFor("bc-2", 77, 30)}); err != nil {
		t.Fatal(err)
	}
	if len(msgs.saved) != 3 {
		t.Fatalf("saved = %d, want 2 (tenant 11) + 1 (tenant 22)", len(msgs.saved))
	}
}

// TestQuotaKeyedByResolvedOwner pins the anti-spoofing shape: two connections
// of one owner share a single budget (keyed by the resolved OwnerUserID,
// never by anything read from the update), while another owner is isolated.
func TestQuotaKeyedByResolvedOwner(t *testing.T) {
	second := enabledConn()
	second.ID = "bc-1b"
	biz := &fakeBusiness{connections: map[string]*business.Connection{
		"bc-1":  enabledConn(),
		"bc-1b": second,
	}}
	msgs := &fakeMessages{}
	h := NewHandler(biz, msgs, nil, testLogger(), WithQuota(quotaTracker(t)))
	ctx := context.Background()

	if err := h.HandleUpdate(ctx, telegram.Update{BusinessMessage: messageFor("bc-1", 77, 10)}); err != nil {
		t.Fatal(err)
	}
	if err := h.HandleUpdate(ctx, telegram.Update{BusinessMessage: messageFor("bc-1b", 77, 11)}); err != nil {
		t.Fatal(err)
	}
	// Both connections resolved to owner 11: the shared budget is spent.
	if err := h.HandleUpdate(ctx, telegram.Update{BusinessMessage: messageFor("bc-1b", 77, 12)}); err != nil {
		t.Fatal(err)
	}
	if len(msgs.saved) != 2 {
		t.Fatalf("saved = %d, want 2: the two connections share owner 11's budget", len(msgs.saved))
	}
}

// TestQuotaDoesNotGateDeletions pins that a deletion mark is not a capture:
// a tenant past its message quota still has its deletions recorded (and the
// alerts still go out through the outbox).
// recordingDeleter wraps fakeMessages to prove the deletion path ran.
type recordingDeleter struct {
	*fakeMessages
	calls int
}

func (r *recordingDeleter) MarkDeleted(ctx context.Context, ownerUserID, ownerTelegramUserID int64, businessConnectionID string, chatID int64, messageIDs []int64) ([]messages.DeletedRecord, error) {
	r.calls++
	return r.fakeMessages.MarkDeleted(ctx, ownerUserID, ownerTelegramUserID, businessConnectionID, chatID, messageIDs)
}

func TestQuotaDoesNotGateDeletions(t *testing.T) {
	biz := &fakeBusiness{connections: map[string]*business.Connection{"bc-1": enabledConn()}}
	msgs := &fakeMessages{deleted: []messages.DeletedRecord{{ChatID: 77, MessageID: 3}}}
	rec := &recordingDeleter{fakeMessages: msgs}
	h := NewHandler(biz, rec, nil, testLogger(), WithQuota(quotaTracker(t)))
	ctx := context.Background()

	for i := int64(0); i < 2; i++ {
		if err := h.HandleUpdate(ctx, telegram.Update{BusinessMessage: messageFor("bc-1", 77, 10+i)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.HandleUpdate(ctx, telegram.Update{DeletedBusinessMessages: &telegram.BusinessMessagesDeleted{
		BusinessConnectionID: "bc-1", Chat: telegram.Chat{ID: 77}, MessageIDs: []int64{3},
	}}); err != nil {
		t.Fatalf("deletion past quota: %v", err)
	}
	if rec.calls != 1 {
		t.Fatalf("MarkDeleted calls = %d, want 1: the deletion mark is not a capture", rec.calls)
	}
}

// TestQuotaCapsAttachmentsKeepsText pins the media-file behaviour: past the
// attachment quota the text stays saved and only the file rows are missing --
// the alert still says a media existed, exactly like a download Telegram
// never hands over.
func TestQuotaCapsAttachmentsKeepsText(t *testing.T) {
	biz := &fakeBusiness{connections: map[string]*business.Connection{"bc-1": enabledConn()}}
	msgs := &fakeMessages{}
	med := &fakeMedia{}
	h := NewHandler(biz, msgs, med, testLogger(), WithQuota(quotaTracker(t)))
	ctx := context.Background()

	photo := func(fileID string) *telegram.Message {
		msg := messageFor("bc-1", 77, 10)
		msg.Text = ""
		msg.Caption = "look"
		msg.Photo = []telegram.PhotoSize{{FileID: fileID, FileUniqueID: "u-" + fileID, Width: 800, Height: 600}}
		return msg
	}
	for _, fileID := range []string{"f1", "f2"} {
		if err := h.HandleUpdate(ctx, telegram.Update{BusinessMessage: photo(fileID)}); err != nil {
			t.Fatal(err)
		}
	}
	if len(msgs.saved) != 2 {
		t.Fatalf("saved texts = %d, want 2: the message quota is not the one biting", len(msgs.saved))
	}
	if len(med.saved) != 1 {
		t.Fatalf("catalogued = %d, want 1: the second attachment is past the quota", len(med.saved))
	}
}

// TestRateQuotaDropsBursts pins the throughput behaviour: a burst past the
// per-minute rate is dropped explicitly, and the drop is not an error.
func TestRateQuotaDropsBursts(t *testing.T) {
	limits := quotaLimits()
	limits.CapturesPerMinute = 2
	tr, err := quotas.NewTracker(limits, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	biz := &fakeBusiness{connections: map[string]*business.Connection{"bc-1": enabledConn()}}
	msgs := &fakeMessages{}
	h := NewHandler(biz, msgs, nil, testLogger(), WithQuota(tr))
	ctx := context.Background()

	for i := int64(0); i < 2; i++ {
		if err := h.HandleUpdate(ctx, telegram.Update{BusinessMessage: messageFor("bc-1", 77, 10+i)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.HandleUpdate(ctx, telegram.Update{BusinessMessage: messageFor("bc-1", 77, 12)}); err != nil {
		t.Fatalf("rate-dropped update must not fail the poller, got %v", err)
	}
	if len(msgs.saved) != 2 {
		t.Fatalf("saved = %d, want 2: the burst past the rate is dropped", len(msgs.saved))
	}
}
