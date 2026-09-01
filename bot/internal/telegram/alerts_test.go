package telegram

import (
	"strings"
	"testing"
	"unicode/utf16"
)

// baseAlert est une alerte complète, que chaque test dégrade sur le seul point
// qu'il vérifie.
func baseAlert() DeletionAlert {
	senderID := int64(900123)
	return DeletionAlert{
		OwnerTelegramUserID: 700001,
		ChatID:              800001,
		ChatTitle:           "Anaïs",
		FromDisplay:         "Anaïs (@fixture_sender)",
		FromUserID:          &senderID,
		MessageType:         "text",
		TelegramDate:        1788019201,
		Content:             "Bonjour, café ☕ — déjà vu ?",
	}
}

func singleText(t *testing.T, alert DeletionAlert) string {
	t.Helper()
	requests := BuildDeletionMessageRequests(alert)
	if len(requests) != 1 {
		t.Fatalf("nombre de requêtes = %d, attendu 1", len(requests))
	}
	if requests[0].ChatID != alert.OwnerTelegramUserID {
		t.Fatalf("alerte adressée au chat %d, attendu le owner %d", requests[0].ChatID, alert.OwnerTelegramUserID)
	}
	return requests[0].Text
}

func TestBuildDeletionMessageRequestsIdentitéComplète(t *testing.T) {
	want := "Message supprimé récupéré\n" +
		"Chat : Anaïs (800001)\n" +
		"De : Anaïs (@fixture_sender) (900123)\n" +
		"Type : text\n" +
		"Date : 2026-08-29 16:00 UTC\n" +
		"\n" +
		"Bonjour, café ☕ — déjà vu ?"
	if got := singleText(t, baseAlert()); got != want {
		t.Fatalf("alerte =\n%s\nattendu\n%s", got, want)
	}
}

func TestBuildDeletionMessageRequestsRepliDesIdentitésInconnues(t *testing.T) {
	tests := []struct {
		name    string
		degrade func(*DeletionAlert)
		want    string
	}{
		{
			name:    "libellé de chat absent",
			degrade: func(a *DeletionAlert) { a.ChatTitle = "" },
			want:    "Chat : chat 800001",
		},
		{
			name:    "chat connu par son @username seul",
			degrade: func(a *DeletionAlert) { a.ChatTitle = ""; a.ChatUsername = "salon_test" },
			want:    "Chat : @salon_test (800001)",
		},
		{
			name:    "titre et @username",
			degrade: func(a *DeletionAlert) { a.ChatUsername = "salon_test" },
			want:    "Chat : Anaïs (@salon_test) (800001)",
		},
		{
			name:    "from_display vide",
			degrade: func(a *DeletionAlert) { a.FromDisplay = "" },
			want:    "De : inconnu (900123)",
		},
		{
			name:    "from_user_id NULL",
			degrade: func(a *DeletionAlert) { a.FromUserID = nil },
			want:    "De : Anaïs (@fixture_sender)",
		},
		{
			name:    "expéditeur totalement inconnu",
			degrade: func(a *DeletionAlert) { a.FromDisplay = ""; a.FromUserID = nil },
			want:    "De : inconnu",
		},
		{
			name:    "date absente",
			degrade: func(a *DeletionAlert) { a.TelegramDate = 0 },
			want:    "Date : inconnue",
		},
		{
			name:    "type absent",
			degrade: func(a *DeletionAlert) { a.MessageType = "" },
			want:    "Type : inconnu",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			alert := baseAlert()
			tt.degrade(&alert)
			text := singleText(t, alert)
			if !strings.Contains(text, tt.want+"\n") {
				t.Fatalf("alerte =\n%s\nattendu une ligne %q", text, tt.want)
			}
			// Le chat_id numérique reste visible quoi qu'il arrive : c'est le
			// seul identifiant que Telegram garantit.
			if !strings.Contains(text, "800001") {
				t.Fatalf("chat_id absent de l'alerte:\n%s", text)
			}
		})
	}
}

// TestBuildDeletionMessageRequestsSansContenu couvre les futures pièces
// jointes (Phase 2) : un message_type non textuel n'a pas de texte à
// restituer, l'en-tête doit alors se suffire à lui-même sans ligne vide
// pendante ni format cassé.
func TestBuildDeletionMessageRequestsSansContenu(t *testing.T) {
	alert := baseAlert()
	alert.MessageType = "photo"
	alert.Content = ""

	text := singleText(t, alert)
	if !strings.HasSuffix(text, "Date : 2026-08-29 16:00 UTC") {
		t.Fatalf("alerte sans contenu mal terminée:\n%q", text)
	}
	if !strings.Contains(text, "Type : photo\n") {
		t.Fatalf("type manquant dans l'alerte sans contenu:\n%s", text)
	}
}

// TestBuildDeletionMessageRequestsPreservesContentAndUTF16Limit vérifie que la
// limite s'applique au texte FINAL (en-tête d'identité compris) : c'est
// l'enrichissement qui décide du nombre de chunks, pas le seul contenu.
func TestBuildDeletionMessageRequestsPreservesContentAndUTF16Limit(t *testing.T) {
	content := strings.Repeat("a", 4095) + "😀" + strings.Repeat("é", 10)
	alert := baseAlert()
	alert.Content = content
	requests := BuildDeletionMessageRequests(alert)

	if len(requests) != 2 {
		t.Fatalf("nombre de requêtes = %d, attendu 2", len(requests))
	}
	header := "Message supprimé récupéré\n" +
		"Chat : Anaïs (800001)\n" +
		"De : Anaïs (@fixture_sender) (900123)\n" +
		"Type : text\n" +
		"Date : 2026-08-29 16:00 UTC\n\n"
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
	if strings.Join(texts, "") != header+content {
		t.Fatal("le découpage a modifié ou tronqué la notification")
	}
	// L'en-tête consomme des unités : le tout premier chunk se termine donc
	// AVANT la fin du contenu qui tenait seul dans 4096 unités.
	if strings.HasSuffix(texts[0], "😀") {
		t.Fatal("le découpage semble ignorer l'en-tête d'identité")
	}
}

// TestBuildDeletionMessageRequestsUnicodeEnTête vérifie que le découpage tient
// aussi quand ce sont les libellés d'identité, et non le contenu, qui portent
// des caractères hors BMP.
func TestBuildDeletionMessageRequestsUnicodeEnTête(t *testing.T) {
	alert := baseAlert()
	alert.ChatTitle = strings.Repeat("🐈", 2000)
	alert.FromDisplay = strings.Repeat("é", 500)
	alert.Content = strings.Repeat("😀", 200)

	requests := BuildDeletionMessageRequests(alert)
	if len(requests) < 2 {
		t.Fatalf("nombre de requêtes = %d, attendu au moins 2", len(requests))
	}
	var rebuilt strings.Builder
	for i, request := range requests {
		units := 0
		for _, r := range request.Text {
			units += utf16.RuneLen(r)
		}
		if units > telegramTextLimit {
			t.Fatalf("requête %d contient %d unités UTF-16", i, units)
		}
		rebuilt.WriteString(request.Text)
	}
	if !strings.HasSuffix(rebuilt.String(), alert.Content) {
		t.Fatal("le contenu Unicode a été tronqué par le découpage")
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
