package app

import (
	"testing"

	"github.com/LouisMoretti/Undelete/bot/internal/telegram"
)

// TestChatTitleCouvreLesFormesDeChatTelegram fige la dérivation du libellé
// persisté dans chats : Telegram ne renseigne title que pour les chats qui en
// ont un, et rien du tout sur certains chats privés. Un libellé vide est un
// résultat légitime -- l'alerte retombe alors sur « chat <id> » plutôt que sur
// une valeur inventée.
func TestChatTitleCouvreLesFormesDeChatTelegram(t *testing.T) {
	tests := []struct {
		name string
		chat telegram.Chat
		want string
	}{
		{
			name: "chat privé prénom seul",
			chat: telegram.Chat{ID: 800001, Type: "private", FirstName: "Anaïs"},
			want: "Anaïs",
		},
		{
			name: "chat privé prénom et nom",
			chat: telegram.Chat{ID: 800001, Type: "private", FirstName: "Zoë", LastName: "Test"},
			want: "Zoë Test",
		},
		{
			name: "nom sans prénom",
			chat: telegram.Chat{ID: 800001, Type: "private", LastName: "Test"},
			want: "Test",
		},
		{
			name: "title prioritaire",
			chat: telegram.Chat{ID: 800001, Type: "group", Title: "Équipe 🚀", FirstName: "ignoré"},
			want: "Équipe 🚀",
		},
		{
			name: "aucun libellé disponible",
			chat: telegram.Chat{ID: 800001, Type: "private", Username: "fixture_sender"},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := chatTitle(tt.chat); got != tt.want {
				t.Fatalf("chatTitle() = %q, attendu %q", got, tt.want)
			}
		})
	}
}

func TestDisplayNameCouvreLesChampsOptionnels(t *testing.T) {
	tests := []struct {
		name string
		user telegram.User
		want string
	}{
		{name: "prénom seul", user: telegram.User{FirstName: "Anaïs"}, want: "Anaïs"},
		{name: "prénom et nom", user: telegram.User{FirstName: "Zoë", LastName: "Test"}, want: "Zoë Test"},
		{
			name: "avec username",
			user: telegram.User{FirstName: "Anaïs", Username: "fixture_sender"},
			want: "Anaïs (@fixture_sender)",
		},
		{name: "utilisateur sans aucun nom", user: telegram.User{ID: 42}, want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			user := tt.user
			if got := displayName(&user); got != tt.want {
				t.Fatalf("displayName() = %q, attendu %q", got, tt.want)
			}
		})
	}
}
