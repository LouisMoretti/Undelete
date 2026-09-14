package telegram

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestRunAdvancesOverSubmittedDespiteCancel pins the other half of the C3
// contract: cancellation stops the offset only at updates no worker ever saw
// (ErrUpdateNotSubmitted, covered at the Dispatch level by
// TestDispatchMarksNeverSubmittedOnCancel). A submitted update whose handler
// observed the cancellation stays a per-update signal -- the offset advances
// past it exactly like past any other handler error, so a shutdown mid-batch
// never replays completed work.
func TestRunAdvancesOverSubmittedDespiteCancel(t *testing.T) {
	script := &scriptedPoller{t: t, bodies: []string{updateJSON(10)}}
	srv := newScriptServer(t, script)
	client := NewClient("token", 61*time.Second, WithBaseURL(srv.URL+"/bot"))
	poller := NewPoller(client, silentTestLogger())

	entered := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan error, 1)
	go func() {
		runDone <- poller.Run(ctx, func(ctx context.Context, u Update) error {
			close(entered)
			<-ctx.Done()
			return ctx.Err()
		})
	}()

	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("batch never reached the handler")
	}
	cancel()

	select {
	case err := <-runDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
	if poller.offset != 11 {
		t.Fatalf("offset = %d, want 11 (a submitted update advances even when its handler observed cancellation)", poller.offset)
	}
}
