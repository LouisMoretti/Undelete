// External test (package telegram_test) like the contracts: the poller is
// exercised through its public API, the one the readiness consumes.
package telegram_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/LouisMoretti/Undelete/bot/internal/telegram"
)

func newDiscardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// TestLastSuccessfulPollIsZeroBeforeAnyPoll pins the contract read by the
// readiness: before the first successful getUpdates, the time is the zero
// value (not "now"), otherwise a bot that has never reached Telegram would be
// declared ready throughout its first cycle.
func TestLastSuccessfulPollIsZeroBeforeAnyPoll(t *testing.T) {
	poller := telegram.NewPoller(telegram.NewClient("test-token", time.Second), nil)

	if last := poller.LastSuccessfulPoll(); !last.IsZero() {
		t.Fatalf("LastSuccessfulPoll() = %v, want the zero value", last)
	}
}

func TestLastSuccessfulPollAdvancesAfterSuccessfulGetUpdates(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":[{"update_id":42}]}`))
	}))
	defer server.Close()

	client := telegram.NewClient("test-token", time.Second, telegram.WithBaseURL(server.URL+"/bot"))
	poller := telegram.NewPoller(client, newDiscardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	before := time.Now()
	// The handler cuts the loop as soon as the first update arrives: the
	// freshness timestamp is already set at that point (it is set right after
	// getUpdates, before delivery to the handler).
	err := poller.Run(ctx, func(context.Context, telegram.Update) error {
		cancel()
		return nil
	})
	if err == nil {
		t.Fatal("Run() = nil, want the context cancellation error")
	}

	last := poller.LastSuccessfulPoll()
	if last.Before(before) || last.After(time.Now()) {
		t.Fatalf("LastSuccessfulPoll() = %v, want a time in [%v, now]", last, before)
	}
}
