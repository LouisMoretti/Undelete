package config

import (
	"strings"
	"testing"
)

func validEnv(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://undelete_app:app@postgres/undelete")
	t.Setenv("MIGRATION_DATABASE_URL", "postgres://owner:owner@postgres/undelete")
	t.Setenv("TELEGRAM_BOT_TOKEN", "not-a-real-bot-token")
	t.Setenv("OWNER_TELEGRAM_USER_ID", "")
}

func TestLoadRejectsIdenticalDatabaseURLs(t *testing.T) {
	validEnv(t)
	t.Setenv("MIGRATION_DATABASE_URL", "postgres://undelete_app:app@postgres/undelete")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "identiques") {
		t.Fatalf("Load() erreur = %v, attendu refus des DSN identiques", err)
	}
}

func TestLoadParsesOwnerGuard(t *testing.T) {
	validEnv(t)
	t.Setenv("OWNER_TELEGRAM_USER_ID", "123456789")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() erreur inattendue: %v", err)
	}
	if cfg.OwnerTelegramUserID != 123456789 {
		t.Fatalf("OwnerTelegramUserID = %d", cfg.OwnerTelegramUserID)
	}
}
