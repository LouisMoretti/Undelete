package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/LouisMoretti/Undelete/bot/internal/messages"
	"github.com/LouisMoretti/Undelete/bot/internal/telegram/telegramtest"
)

func testHandler(t *testing.T, calls ...telegramtest.Call) *Handler {
	t.Helper()
	// business et messages restent nil : notifyDeletion n'emprunte que le
	// client et le logger. Un nil-pointer ici serait un test qui ment sur le
	// chemin réellement exercé, pas un faux positif.
	return NewHandler(nil, nil, telegramtest.NewClient(t, calls...), slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// TestDeletionAlertContract exerce le chemin de production de la notification
// de suppression -- Handler.notifyDeletion, à partir de l'enregistrement rendu
// par messages.MarkDeleted -- et compare la requête HTTP émise à la fixture,
// octet par octet.
//
// Ce qui n'est vérifiable qu'ici, et pas depuis le package telegram : que le
// handler notifie bien le telegram_user_id du owner (700001) et non le chat
// surveillé (800001), et que la charge utile n'emporte aucun
// business_connection_id (contrainte n°7).
func TestDeletionAlertContract(t *testing.T) {
	handler := testHandler(t, telegramtest.Call{
		Method:          "sendMessage",
		RequestFixture:  "send-message-deletion-request.json",
		ResponseFixture: telegramtest.OKEnvelopeFixture,
	})

	handler.notifyDeletion(context.Background(), 700001, messages.DeletedRecord{
		ChatID:      800001,
		MessageID:   501,
		FromDisplay: "Anaïs (@fixture_sender)",
		MessageType: "text",
		TextContent: "Bonjour, café ☕ — déjà vu ?",
	})
}

// TestDeletionAlertSendsEveryChunk vérifie sur le chemin de production qu'un
// contenu dépassant la limite Telegram part en PLUSIEURS sendMessage
// successifs, tous adressés au owner, sans perte de contenu : un découpage
// correct mais dont le handler n'enverrait que le premier morceau passerait
// inaperçu au niveau du builder seul.
func TestDeletionAlertSendsEveryChunk(t *testing.T) {
	client, recorded := telegramtest.NewRecordingClient(t)
	handler := NewHandler(nil, nil, client, slog.New(slog.NewTextHandler(io.Discard, nil)))

	content := strings.Repeat("a", 4100)
	handler.notifyDeletion(context.Background(), 700001, messages.DeletedRecord{
		ChatID:      800001,
		MessageID:   501,
		MessageType: "text",
		TextContent: content,
	})

	bodies := recorded()
	if len(bodies) != 2 {
		t.Fatalf("nombre de sendMessage = %d, attendu 2", len(bodies))
	}

	var joined strings.Builder
	for i, body := range bodies {
		telegramtest.AssertNoBusinessConnectionID(t, body)
		var sent struct {
			ChatID int64  `json:"chat_id"`
			Text   string `json:"text"`
		}
		if err := json.Unmarshal(body, &sent); err != nil {
			t.Fatalf("décodage sendMessage %d: %v", i+1, err)
		}
		if sent.ChatID != 700001 {
			t.Fatalf("sendMessage %d: chat_id = %d, attendu le owner 700001", i+1, sent.ChatID)
		}
		joined.WriteString(sent.Text)
	}
	if want := "Message supprimé récupéré (chat 800001) :\n\n" + content; joined.String() != want {
		t.Fatal("le découpage a modifié ou tronqué la notification")
	}
}
