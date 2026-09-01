// Test externe (package telegram_test) comme les contrats : le poller est
// exercé par son API publique, celle que consomme la readiness.
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

// TestLastSuccessfulPollEstZeroAvantToutPoll fige le contrat lu par la
// readiness : avant le premier getUpdates réussi, la date est la valeur zéro
// (et non "maintenant"), sinon un bot qui n'a jamais joint Telegram serait
// déclaré prêt pendant tout son premier cycle.
func TestLastSuccessfulPollEstZeroAvantToutPoll(t *testing.T) {
	poller := telegram.NewPoller(telegram.NewClient("test-token", time.Second), nil)

	if last := poller.LastSuccessfulPoll(); !last.IsZero() {
		t.Fatalf("LastSuccessfulPoll() = %v, attendu la valeur zéro", last)
	}
}

func TestLastSuccessfulPollAvanceApresUnGetUpdatesReussi(t *testing.T) {
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
	// Le handler coupe la boucle dès le premier update : le timestamp de
	// fraîcheur est déjà posé à ce stade (il l'est juste après le
	// getUpdates, avant la remise au handler).
	err := poller.Run(ctx, func(context.Context, telegram.Update) error {
		cancel()
		return nil
	})
	if err == nil {
		t.Fatal("Run() = nil, attendu l'erreur d'annulation du contexte")
	}

	last := poller.LastSuccessfulPoll()
	if last.Before(before) || last.After(time.Now()) {
		t.Fatalf("LastSuccessfulPoll() = %v, attendu une date dans [%v, maintenant]", last, before)
	}
}
