package business

import (
	"context"
	"testing"

	"github.com/LouisMoretti/Undelete/bot/internal/telegram"
)

func bc(id string, userID int64) telegram.BusinessConnection {
	return telegram.BusinessConnection{ID: id, User: telegram.User{ID: userID}}
}

func TestResolveRejectsEmptyConnectionID(t *testing.T) {
	svc := &Service{ownerFilter: 0, cache: make(map[string]Connection)}
	if _, err := svc.Resolve(context.Background(), ""); err == nil {
		t.Fatalf("Resolve(\"\") = nil, want error (must not hit DB/API)")
	}
}

func TestUpsertFromTelegramRejectsZeroUser(t *testing.T) {
	svc := &Service{ownerFilter: 0, cache: make(map[string]Connection)}
	if _, err := svc.upsertFromTelegram(context.Background(), bc("bc-1", 0)); err == nil {
		t.Fatalf("upsert with zero user id = nil, want error (would pollute users with telegram_user_id=0)")
	}
	if _, err := svc.upsertFromTelegram(context.Background(), bc("", 42)); err == nil {
		t.Fatalf("upsert with empty connection id = nil, want error")
	}
}
