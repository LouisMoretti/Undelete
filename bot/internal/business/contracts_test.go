package business

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/LouisMoretti/Undelete/bot/internal/telegram"
	"github.com/LouisMoretti/Undelete/bot/internal/telegram/telegramtest"
)

// TestWelcomeAlertContract exerce le chemin de production de l'alerte de
// bienvenue -- pas une SendMessageRequest reconstruite par le test -- et
// compare la requête HTTP émise à la fixture, octet par octet.
//
// Ce qui n'est vérifiable qu'ici, et pas depuis le package telegram : que
// Service passe bien user_chat_id (700002) et non user.id (700001) comme
// chat_id, et que la charge utile n'emporte aucun business_connection_id
// alors même que la connexion en fournit un (contrainte n°7).
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

// TestWelcomeAlertFallsBackToUserID couvre le repli défensif : sans
// user_chat_id (anciennes réponses Telegram), le owner doit rester joignable
// via user.id plutôt que de recevoir un sendMessage vers le chat 0.
func TestWelcomeAlertFallsBackToUserID(t *testing.T) {
	if got := telegram.BuildWelcomeMessageRequest(0, 700001).ChatID; got != 700001 {
		t.Fatalf("chat_id de repli = %d, attendu 700001", got)
	}
}
