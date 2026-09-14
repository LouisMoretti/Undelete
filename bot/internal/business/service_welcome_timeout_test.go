package business

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/LouisMoretti/Undelete/bot/internal/telegram"
)

// blockingWelcomeAPI simulates a Telegram that never answers the welcome
// send (a stuck connection, or a 429 wait outliving the test): SendMessage
// blocks until its context is done, like the real client parking on backoff.
type blockingWelcomeAPI struct {
	entered chan struct{}
	once    sync.Once
}

func (a *blockingWelcomeAPI) GetBusinessConnection(_ context.Context, id string) (*telegram.BusinessConnection, error) {
	return nil, ErrConnectionUnknown
}

func (a *blockingWelcomeAPI) SendMessage(ctx context.Context, _ telegram.SendMessageRequest) error {
	a.once.Do(func() { close(a.entered) })
	<-ctx.Done()
	return ctx.Err()
}

// TestWelcomeSendIsBounded pins the C2 ceiling: establishing a connection
// while Telegram never answers the welcome must return within the welcome
// timeout instead of parking the shard partition for the whole SendMessage
// retry budget. The connection itself is still stored (only the send is
// cut), and the update never fails because of it.
func TestWelcomeSendIsBounded(t *testing.T) {
	api := &blockingWelcomeAPI{entered: make(chan struct{})}
	pool := &fakePool{t: t}
	svc := NewService(pool, api, &fakeUsers{nextID: 7}, nil, testLogger(), WithWelcomeTimeout(200*time.Millisecond))

	done := make(chan error, 1)
	start := time.Now()
	go func() {
		done <- svc.HandleBusinessConnection(context.Background(), *apiConn("bc-welcome", 700, true))
	}()

	select {
	case <-api.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("welcome send never attempted")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("HandleBusinessConnection = %v, want nil (a lost welcome must not fail the update)", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("HandleBusinessConnection stuck past the welcome timeout: the welcome path has no ceiling")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("HandleBusinessConnection took %v, want it bounded by the welcome timeout", elapsed)
	}

	got, err := svc.Resolve(context.Background(), "bc-welcome")
	if err != nil {
		t.Fatalf("Resolve after bounded welcome = %v, want nil", err)
	}
	if !got.IsEnabled {
		t.Fatal("the connection must be stored enabled even when its welcome send is cut short")
	}
}
