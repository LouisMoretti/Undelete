package telegram

import (
	"strings"
	"testing"
	"unicode/utf16"
)

func TestBuildDeletionMessageRequestsPreservesContentAndUTF16Limit(t *testing.T) {
	content := strings.Repeat("a", 4095) + "😀" + strings.Repeat("é", 10)
	requests := BuildDeletionMessageRequests(700001, 800001, content)

	if len(requests) != 2 {
		t.Fatalf("nombre de requêtes = %d, attendu 2", len(requests))
	}
	const prefix = "Message supprimé récupéré (chat 800001) :\n\n"
	texts := make([]string, 0, len(requests))
	for i, request := range requests {
		if request.ChatID != 700001 {
			t.Fatalf("requête %d: chat_id = %d, attendu 700001", i, request.ChatID)
		}
		texts = append(texts, request.Text)
		units := 0
		for _, r := range request.Text {
			units += utf16.RuneLen(r)
		}
		if units > telegramTextLimit {
			t.Fatalf("requête %d contient %d unités UTF-16", i, units)
		}
	}
	if strings.Join(texts, "") != prefix+content {
		t.Fatal("le découpage a modifié ou tronqué la notification")
	}
}

func TestSplitTelegramTextRejectsInvalidInput(t *testing.T) {
	if chunks := splitTelegramText("", telegramTextLimit); chunks != nil {
		t.Fatalf("texte vide: chunks = %#v, attendu nil", chunks)
	}
	if chunks := splitTelegramText("test", 0); chunks != nil {
		t.Fatalf("limite invalide: chunks = %#v, attendu nil", chunks)
	}
}
