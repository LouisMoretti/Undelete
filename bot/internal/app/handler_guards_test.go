package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/LouisMoretti/Undelete/bot/internal/business"
	"github.com/LouisMoretti/Undelete/bot/internal/messages"
	"github.com/LouisMoretti/Undelete/bot/internal/telegram"
	"github.com/LouisMoretti/Undelete/bot/internal/tenantexcl"
)

// scriptedBusiness replays one Resolve result per call, in order. The handler
// resolves twice on the save path (control command, then save) and twice more
// under the tenant guard, so a flaky second resolution is scripted
// deterministically without any timing.
type scriptedBusiness struct {
	results []scriptedResolve
	calls   int
}

type scriptedResolve struct {
	conn *business.Connection
	err  error
}

func (f *scriptedBusiness) Resolve(_ context.Context, _ string) (*business.Connection, error) {
	if f.calls >= len(f.results) {
		return nil, errors.New("scriptedBusiness: no more scripted results")
	}
	r := f.results[f.calls]
	f.calls++
	return r.conn, r.err
}

func (f *scriptedBusiness) HandleBusinessConnection(_ context.Context, _ telegram.BusinessConnection) error {
	return nil
}

func scriptedOK() scriptedResolve { return scriptedResolve{conn: enabledConn()} }

func scriptedDisabled() scriptedResolve {
	disabled := enabledConn()
	disabled.IsEnabled = false
	return scriptedResolve{conn: disabled}
}

// countingDeleter records how many deletion marks reached the store.
type countingDeleter struct {
	*fakeMessages
	calls int
}

func (r *countingDeleter) MarkDeleted(ctx context.Context, ownerUserID, ownerTelegramUserID int64, businessConnectionID string, chatID int64, messageIDs []int64) ([]messages.DeletedRecord, error) {
	r.calls++
	return r.fakeMessages.MarkDeleted(ctx, ownerUserID, ownerTelegramUserID, businessConnectionID, chatID, messageIDs)
}

// TestEditedConfirmEmptyGuardSkipsResolve pins the dropConfirmEdit guard: an
// edited message without routing ids is not a command and must never reach
// Resolve, let alone the store.
func TestEditedConfirmEmptyGuardSkipsResolve(t *testing.T) {
	biz := &countingBusiness{fakeBusiness: fakeBusiness{connections: map[string]*business.Connection{"bc-1": enabledConn()}}}
	msgs := &fakeMessages{}
	h := NewHandler(biz, msgs, &fakeMedia{}, testLogger())

	msg := ownerMessage("/delete_my_data ABCD2345")
	msg.BusinessConnectionID = ""
	if err := h.HandleUpdate(context.Background(), telegram.Update{UpdateID: 1, EditedBusinessMessage: msg}); err != nil {
		t.Fatalf("HandleUpdate: %v", err)
	}
	if biz.resolveCalls != 0 {
		t.Fatalf("Resolve called %d times, want 0: empty ids must short-circuit before any lookup", biz.resolveCalls)
	}
	if len(msgs.saved) != 0 {
		t.Fatal("nothing must be saved for empty ids")
	}
}

// TestEditedConfirmRefusedDropsWithoutSave pins the secrecy half of
// dropConfirmEdit: an edited confirmation on a refused connection is dropped
// silently -- the code is never stored, the eraser never runs.
func TestEditedConfirmRefusedDropsWithoutSave(t *testing.T) {
	biz := &fakeBusiness{resolveErr: map[string]error{"bc-1": business.ErrOwnerNotAllowed}}
	msgs := &fakeMessages{}
	h := NewHandler(biz, msgs, &fakeMedia{}, testLogger())

	if err := h.HandleUpdate(context.Background(), telegram.Update{
		UpdateID: 1, EditedBusinessMessage: ownerMessage("/delete_my_data ABCD2345"),
	}); err != nil {
		t.Fatalf("refused edited confirmation must be swallowed, got %v", err)
	}
	if len(msgs.saved) != 0 {
		t.Fatal("a refused edited confirmation must save nothing: a code is never stored")
	}
}

// TestEditedConfirmResolveErrorPropagates pins the failure half: a transient
// resolution failure while inspecting an edited confirmation fails the update
// (the poller logs it), never silently drops it.
func TestEditedConfirmResolveErrorPropagates(t *testing.T) {
	biz := &fakeBusiness{resolveErr: map[string]error{"bc-1": errors.New("db down")}}
	h := NewHandler(biz, &fakeMessages{}, &fakeMedia{}, testLogger())

	err := h.HandleUpdate(context.Background(), telegram.Update{
		UpdateID: 1, EditedBusinessMessage: ownerMessage("/delete_my_data ABCD2345"),
	})
	if err == nil || !strings.Contains(err.Error(), "edited command") {
		t.Fatalf("want resolution error wrapped as edited command, got %v", err)
	}
}

// TestSaveSecondResolveRefusedSwallowed pins that saveMessage authenticates
// its own resolution: the control path already admitted this update, but the
// save's lookup refuses it (allowlist change between the two calls) -- the
// update is then dropped silently, exactly like a refused connection.
func TestSaveSecondResolveRefusedSwallowed(t *testing.T) {
	biz := &scriptedBusiness{results: []scriptedResolve{scriptedOK(), {conn: nil, err: business.ErrOwnerNotAllowed}}}
	msgs := &fakeMessages{}
	h := NewHandler(biz, msgs, nil, testLogger())

	if err := h.HandleUpdate(context.Background(), telegram.Update{UpdateID: 1, BusinessMessage: testMessage()}); err != nil {
		t.Fatalf("second-resolve refusal must be swallowed, got %v", err)
	}
	if len(msgs.saved) != 0 {
		t.Fatal("a save refused on its own lookup must persist nothing")
	}
}

// TestSaveSecondResolveErrorPropagates pins the transient twin: the save's
// own lookup fails after the control path succeeded -- the update fails so
// the outage stays visible, with the save (not control) attribution.
func TestSaveSecondResolveErrorPropagates(t *testing.T) {
	biz := &scriptedBusiness{results: []scriptedResolve{scriptedOK(), {conn: nil, err: errors.New("db down")}}}
	h := NewHandler(biz, &fakeMessages{}, nil, testLogger())

	err := h.HandleUpdate(context.Background(), telegram.Update{UpdateID: 1, BusinessMessage: testMessage()})
	if err == nil || !strings.Contains(err.Error(), "for save") {
		t.Fatalf("want save-attributed resolution error, got %v", err)
	}
}

// TestSaveMediaCatalogueFailureFailsUpdate pins the ordering: the message row
// is written first, so a catalogue failure surfaces as an update error rather
// than a silent catalogue hole behind a message claiming attachments.
func TestSaveMediaCatalogueFailureFailsUpdate(t *testing.T) {
	biz := &fakeBusiness{connections: map[string]*business.Connection{"bc-1": enabledConn()}}
	msgs := &fakeMessages{}
	med := &fakeMedia{saveErr: errors.New("db down")}
	h := NewHandler(biz, msgs, med, testLogger())

	msg := testMessage()
	msg.Text = ""
	msg.Caption = "look"
	msg.Photo = []telegram.PhotoSize{{FileID: "f-1", FileUniqueID: "u-1", Width: 10, Height: 10}}
	err := h.HandleUpdate(context.Background(), telegram.Update{UpdateID: 1, BusinessMessage: msg})
	if err == nil || !strings.Contains(err.Error(), "media catalogue") {
		t.Fatalf("want media catalogue error, got %v", err)
	}
	if len(msgs.saved) != 1 {
		t.Fatalf("saved = %d, want 1: the message precedes the catalogue write", len(msgs.saved))
	}
}

// TestSaveSkippedWhenDisabledUnderGuard pins the recheck: the connection was
// enabled on both pre-guard lookups but disabled when re-read under the
// tenant exclusion -- the save waits, sees disabled, and skips without
// writing.
func TestSaveSkippedWhenDisabledUnderGuard(t *testing.T) {
	biz := &scriptedBusiness{results: []scriptedResolve{scriptedOK(), scriptedOK(), scriptedDisabled()}}
	msgs := &fakeMessages{}
	h := NewHandler(biz, msgs, nil, testLogger(), WithTenantGuard(tenantexcl.New()))

	if err := h.HandleUpdate(context.Background(), telegram.Update{UpdateID: 1, BusinessMessage: testMessage()}); err != nil {
		t.Fatalf("disabled-under-guard must be skipped with nil, got %v", err)
	}
	if len(msgs.saved) != 0 {
		t.Fatal("a save disabled under the exclusion must persist nothing")
	}
}

// TestSaveSkippedWhenRefusedUnderGuard pins the same skip for a refusal that
// lands between the pre-guard lookups and the guarded re-resolution.
func TestSaveSkippedWhenRefusedUnderGuard(t *testing.T) {
	biz := &scriptedBusiness{results: []scriptedResolve{scriptedOK(), scriptedOK(), {conn: nil, err: business.ErrOwnerNotAllowed}}}
	msgs := &fakeMessages{}
	h := NewHandler(biz, msgs, nil, testLogger(), WithTenantGuard(tenantexcl.New()))

	if err := h.HandleUpdate(context.Background(), telegram.Update{UpdateID: 1, BusinessMessage: testMessage()}); err != nil {
		t.Fatalf("refused-under-guard must be skipped with nil, got %v", err)
	}
	if len(msgs.saved) != 0 {
		t.Fatal("a save refused under the exclusion must persist nothing")
	}
}

// TestSaveReresolutionErrorPropagatesUnderGuard pins the transient twin under
// the exclusion: the guarded re-resolution fails -- the update fails with the
// re-resolution attribution, never a silent skip.
func TestSaveReresolutionErrorPropagatesUnderGuard(t *testing.T) {
	biz := &scriptedBusiness{results: []scriptedResolve{scriptedOK(), scriptedOK(), {conn: nil, err: errors.New("db down")}}}
	h := NewHandler(biz, &fakeMessages{}, nil, testLogger(), WithTenantGuard(tenantexcl.New()))

	err := h.HandleUpdate(context.Background(), telegram.Update{UpdateID: 1, BusinessMessage: testMessage()})
	if err == nil || !strings.Contains(err.Error(), "re-resolution") {
		t.Fatalf("want re-resolution error, got %v", err)
	}
}

// TestSaveCancelledGuardSharedFails pins that a cancelled context aborts the
// save unit at the exclusion: no write, and the cancellation surfaces so the
// shutdown stays visible.
func TestSaveCancelledGuardSharedFails(t *testing.T) {
	biz := &fakeBusiness{connections: map[string]*business.Connection{"bc-1": enabledConn()}}
	msgs := &fakeMessages{}
	h := NewHandler(biz, msgs, nil, testLogger(), WithTenantGuard(tenantexcl.New()))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := h.HandleUpdate(ctx, telegram.Update{UpdateID: 1, BusinessMessage: testMessage()}); err == nil {
		t.Fatal("a cancelled guard acquisition must surface as an error")
	}
	if len(msgs.saved) != 0 {
		t.Fatal("a cancelled save unit must persist nothing")
	}
}

// TestDeletionServedWithGuard pins the guarded happy path: with the exclusion
// wired, a deletion still reaches MarkDeleted exactly once.
func TestDeletionServedWithGuard(t *testing.T) {
	biz := &scriptedBusiness{results: []scriptedResolve{scriptedOK(), scriptedOK()}}
	msgs := &fakeMessages{deleted: []messages.DeletedRecord{{ChatID: 77, MessageID: 3}}}
	rec := &countingDeleter{fakeMessages: msgs}
	h := NewHandler(biz, rec, nil, testLogger(), WithTenantGuard(tenantexcl.New()))

	del := &telegram.BusinessMessagesDeleted{BusinessConnectionID: "bc-1", Chat: telegram.Chat{ID: 77}, MessageIDs: []int64{3}}
	if err := h.HandleUpdate(context.Background(), telegram.Update{UpdateID: 1, DeletedBusinessMessages: del}); err != nil {
		t.Fatalf("guarded deletion: %v", err)
	}
	if rec.calls != 1 {
		t.Fatalf("MarkDeleted calls = %d, want 1", rec.calls)
	}
}

// TestDeletionDisabledUnderGuardStillServed pins that a deletion mark is not
// a capture: even re-resolved as disabled under the exclusion, it is still
// recorded -- unlike a save, which would skip.
func TestDeletionDisabledUnderGuardStillServed(t *testing.T) {
	biz := &scriptedBusiness{results: []scriptedResolve{scriptedOK(), scriptedDisabled()}}
	msgs := &fakeMessages{}
	rec := &countingDeleter{fakeMessages: msgs}
	h := NewHandler(biz, rec, nil, testLogger(), WithTenantGuard(tenantexcl.New()))

	del := &telegram.BusinessMessagesDeleted{BusinessConnectionID: "bc-1", Chat: telegram.Chat{ID: 77}, MessageIDs: []int64{3}}
	if err := h.HandleUpdate(context.Background(), telegram.Update{UpdateID: 1, DeletedBusinessMessages: del}); err != nil {
		t.Fatalf("disabled-under-guard deletion: %v", err)
	}
	if rec.calls != 1 {
		t.Fatalf("MarkDeleted calls = %d, want 1: a deletion mark is served even when disabled", rec.calls)
	}
}

// TestDeletionRefusedUnderGuardSkipsMark pins the serialised refusal: the
// connection is refused on the guarded re-resolution -- the mark is skipped
// silently, exactly as without the guard.
func TestDeletionRefusedUnderGuardSkipsMark(t *testing.T) {
	biz := &scriptedBusiness{results: []scriptedResolve{scriptedOK(), {conn: nil, err: business.ErrConnectionUnknown}}}
	msgs := &fakeMessages{}
	rec := &countingDeleter{fakeMessages: msgs}
	h := NewHandler(biz, rec, nil, testLogger(), WithTenantGuard(tenantexcl.New()))

	del := &telegram.BusinessMessagesDeleted{BusinessConnectionID: "bc-1", Chat: telegram.Chat{ID: 77}, MessageIDs: []int64{3}}
	if err := h.HandleUpdate(context.Background(), telegram.Update{UpdateID: 1, DeletedBusinessMessages: del}); err != nil {
		t.Fatalf("refused-under-guard must be skipped with nil, got %v", err)
	}
	if rec.calls != 0 {
		t.Fatal("a deletion refused under the exclusion must not reach the store")
	}
}

// TestDeletionReresolutionErrorPropagates pins the transient twin on the
// deletion path: the guarded re-resolution fails -- the update fails with the
// re-resolution attribution.
func TestDeletionReresolutionErrorPropagates(t *testing.T) {
	biz := &scriptedBusiness{results: []scriptedResolve{scriptedOK(), {conn: nil, err: errors.New("db down")}}}
	h := NewHandler(biz, &fakeMessages{}, nil, testLogger(), WithTenantGuard(tenantexcl.New()))

	del := &telegram.BusinessMessagesDeleted{BusinessConnectionID: "bc-1", Chat: telegram.Chat{ID: 77}, MessageIDs: []int64{3}}
	err := h.HandleUpdate(context.Background(), telegram.Update{UpdateID: 1, DeletedBusinessMessages: del})
	if err == nil || !strings.Contains(err.Error(), "re-resolution") {
		t.Fatalf("want re-resolution error, got %v", err)
	}
}

// TestDeletionCancelledGuardSharedFails pins that a cancelled context aborts
// the deletion unit at the exclusion before any mark.
func TestDeletionCancelledGuardSharedFails(t *testing.T) {
	biz := &fakeBusiness{connections: map[string]*business.Connection{"bc-1": enabledConn()}}
	msgs := &fakeMessages{}
	rec := &countingDeleter{fakeMessages: msgs}
	h := NewHandler(biz, rec, nil, testLogger(), WithTenantGuard(tenantexcl.New()))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	del := &telegram.BusinessMessagesDeleted{BusinessConnectionID: "bc-1", Chat: telegram.Chat{ID: 77}, MessageIDs: []int64{3}}
	if err := h.HandleUpdate(ctx, telegram.Update{UpdateID: 1, DeletedBusinessMessages: del}); err == nil {
		t.Fatal("a cancelled guard acquisition must surface as an error")
	}
	if rec.calls != 0 {
		t.Fatal("a cancelled deletion unit must not reach the store")
	}
}

// TestNilInputsAreNoOps pins the defensive guards: neither the save nor the
// deletion path panics on a nil payload, both report "nothing to do".
func TestNilInputsAreNoOps(t *testing.T) {
	h := NewHandler(&fakeBusiness{}, &fakeMessages{}, nil, testLogger())
	if conn, err := h.saveMessage(context.Background(), nil, false); err != nil || conn != nil {
		t.Fatalf("saveMessage(nil) = (%v, %v), want (nil, nil)", conn, err)
	}
	if err := h.handleDeleted(context.Background(), nil); err != nil {
		t.Fatalf("handleDeleted(nil) = %v, want nil", err)
	}
}
