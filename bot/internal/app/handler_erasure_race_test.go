package app

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/LouisMoretti/Undelete/bot/internal/business"
	"github.com/LouisMoretti/Undelete/bot/internal/messages"
	"github.com/LouisMoretti/Undelete/bot/internal/telegram"
	"github.com/LouisMoretti/Undelete/bot/internal/tenantexcl"
)

// raceBusiness resolves the connection as enabled until the scripted erasure
// disables it, then as disabled -- the real Service behaves this way once its
// cache patch lands (cf. business.Service.DisableOwner). The first call
// closes enteredWindow, proving the save resolved before the disable.
type raceBusiness struct {
	disabled      atomic.Bool
	enteredWindow chan struct{}
	enterOnce     sync.Once
}

func (f *raceBusiness) Resolve(_ context.Context, id string) (*business.Connection, error) {
	f.enterOnce.Do(func() { close(f.enteredWindow) })
	return &business.Connection{
		ID: id, OwnerUserID: 42, OwnerTelegramUserID: 4242,
		CanReply: true, IsEnabled: !f.disabled.Load(),
	}, nil
}

func (f *raceBusiness) HandleBusinessConnection(_ context.Context, _ telegram.BusinessConnection) error {
	return nil
}

// raceMessages records the commit order of the save unit.
type raceMessages struct {
	mu     sync.Mutex
	events []string
}

func (f *raceMessages) Save(_ context.Context, _ int64, _ messages.Record, _ bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, "saved")
	return nil
}

func (f *raceMessages) MarkDeleted(_ context.Context, _, _ int64, _ string, _ int64, _ []int64) ([]messages.DeletedRecord, error) {
	return nil, nil
}

func (f *raceMessages) ordered() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.events...)
}

// TestSaveCannotLandAfterErasureDelete pins the B2 invariant: the schedule
// [disabled deleted saved] must never occur.
//
// The scripted erasure holds the real tenant exclusion around its whole run
// (disable, then delete) and forces the delete to land while the save is
// inside its resolve-to-write window (it waits for the save's first Resolve
// before deleting). The save therefore either commits before the delete or
// waits on the exclusion, re-reads the now-disabled connection and skips --
// both orders satisfy the invariant. Without the exclusion the same schedule
// commits behind the delete and resurrects data the owner was told is gone.
func TestSaveCannotLandAfterErasureDelete(t *testing.T) {
	guard := tenantexcl.New()
	biz := &raceBusiness{enteredWindow: make(chan struct{})}
	msgs := &raceMessages{}
	h := NewHandler(biz, msgs, nil, testLogger(), WithTenantGuard(guard))

	msg := &telegram.Message{
		MessageID:            5,
		BusinessConnectionID: "bc-race",
		Chat:                 telegram.Chat{ID: 7, Type: "private"},
		From:                 &telegram.User{ID: 4242, FirstName: "Owner"},
		Text:                 "hi",
		Date:                 1700000000,
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		// Scripted erasure: the exclusive side around disable + delete.
		release, err := guard.Exclusive(context.Background(), 42)
		if err != nil {
			t.Errorf("Exclusive = %v, want nil", err)
			return
		}
		defer release()
		// Force the delete to land while the save is in its window: the
		// save resolved enabled (first Resolve done) but has not committed.
		// Waiting before disabling makes that order deterministic rather
		// than racy: the flag flips strictly after the save's first Resolve
		// returned enabled.
		<-biz.enteredWindow
		msgs.mu.Lock()
		msgs.events = append(msgs.events, "disabled")
		msgs.mu.Unlock()
		biz.disabled.Store(true)
		msgs.mu.Lock()
		msgs.events = append(msgs.events, "deleted")
		msgs.mu.Unlock()
	}()
	go func() {
		defer wg.Done()
		if _, err := h.saveMessage(context.Background(), msg, false); err != nil {
			t.Errorf("saveMessage = %v, want nil", err)
		}
	}()
	wg.Wait()

	events := msgs.ordered()
	saved, deleted := -1, -1
	for i, e := range events {
		switch e {
		case "saved":
			if saved == -1 {
				saved = i
			}
		case "deleted":
			deleted = i
		}
	}
	if deleted == -1 {
		t.Fatalf("events = %v, want the scripted delete to have run", events)
	}
	if saved != -1 && saved > deleted {
		t.Fatalf("events = %v, want [disabled deleted saved] never: the save committed after the erasure's delete step", events)
	}
}
