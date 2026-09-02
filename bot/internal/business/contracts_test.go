package business

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/LouisMoretti/Undelete/bot/internal/telegram"
	"github.com/LouisMoretti/Undelete/bot/internal/telegram/telegramtest"
)

// TestWelcomeAlertContract exercises the production path of the welcome
// alert -- not a SendMessageRequest rebuilt by the test -- and compares the
// HTTP request emitted against the fixture, byte by byte.
//
// What is only verifiable here, and not from the telegram package: that
// Service passes user_chat_id (700002) and not user.id (700001) as chat_id,
// and that the payload carries no business_connection_id even though the
// connection provides one (constraint #7).
func TestWelcomeAlertContract(t *testing.T) {
	client := telegramtest.NewClient(t, telegramtest.Call{
		Method:          "sendMessage",
		RequestFixture:  "send-message-welcome-request.json",
		ResponseFixture: telegramtest.OKEnvelopeFixture,
	})
	service := &Service{
		client: client,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	service.notifyWelcome(context.Background(), telegram.BusinessConnection{
		ID:         "bc_fixture_001",
		User:       telegram.User{ID: 700001, FirstName: "Zoë"},
		UserChatID: 700002,
		Rights:     &telegram.BusinessBotRights{CanReply: true},
		IsEnabled:  true,
	})
}

// TestWelcomeAlertFallsBackToUserID covers the defensive fallback: without
// user_chat_id (older Telegram responses), the owner must remain reachable
// via user.id rather than receiving a sendMessage to chat 0.
func TestWelcomeAlertFallsBackToUserID(t *testing.T) {
	if got := telegram.BuildWelcomeMessageRequest(0, 700001).ChatID; got != 700001 {
		t.Fatalf("fallback chat_id = %d, expected 700001", got)
	}
}
