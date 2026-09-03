package app

import (
	"testing"

	"github.com/LouisMoretti/Undelete/bot/internal/telegram"
)

// TestChatTitleCoversAllTelegramChatShapes pins down the derivation of the
// label persisted in chats: Telegram only fills title for chats that have
// one, and nothing at all for some private chats. An empty label is a
// legitimate result -- the alert then falls back on "chat <id>" rather than
// an invented value.
func TestChatTitleCoversAllTelegramChatShapes(t *testing.T) {
	tests := []struct {
		name string
		chat telegram.Chat
		want string
	}{
		{
			name: "private chat first name only",
			chat: telegram.Chat{ID: 800001, Type: "private", FirstName: "Anaïs"},
			want: "Anaïs",
		},
		{
			name: "private chat first and last name",
			chat: telegram.Chat{ID: 800001, Type: "private", FirstName: "Zoë", LastName: "Test"},
			want: "Zoë Test",
		},
		{
			name: "last name without first name",
			chat: telegram.Chat{ID: 800001, Type: "private", LastName: "Test"},
			want: "Test",
		},
		{
			name: "title takes precedence",
			chat: telegram.Chat{ID: 800001, Type: "group", Title: "Team 🚀", FirstName: "unused"},
			want: "Team 🚀",
		},
		{
			name: "no label available",
			chat: telegram.Chat{ID: 800001, Type: "private", Username: "fixture_sender"},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := chatTitle(tt.chat); got != tt.want {
				t.Fatalf("chatTitle() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDisplayNameCoversOptionalFields(t *testing.T) {
	tests := []struct {
		name string
		user telegram.User
		want string
	}{
		{name: "first name only", user: telegram.User{FirstName: "Anaïs"}, want: "Anaïs"},
		{name: "first and last name", user: telegram.User{FirstName: "Zoë", LastName: "Test"}, want: "Zoë Test"},
		{
			name: "with username",
			user: telegram.User{FirstName: "Anaïs", Username: "fixture_sender"},
			want: "Anaïs (@fixture_sender)",
		},
		{name: "user without any name", user: telegram.User{ID: 42}, want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			user := tt.user
			if got := displayName(&user); got != tt.want {
				t.Fatalf("displayName() = %q, want %q", got, tt.want)
			}
		})
	}
}
