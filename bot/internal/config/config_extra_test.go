package config

import (
	"testing"
)

// withCleanEnv resets every variable Load reads, so each case starts from a
// blank slate rather than inheriting the developer's shell.
func withCleanEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"DATABASE_URL", "MIGRATION_DATABASE_URL", "TELEGRAM_BOT_TOKEN",
		"OWNER_TELEGRAM_USER_ID", "MEDIA_DIR", "MEDIA_PURGE_DRY_RUN", "HEALTH_ADDR",
	} {
		t.Setenv(key, "")
	}
}

func withValidBase(t *testing.T) {
	t.Helper()
	withCleanEnv(t)
	t.Setenv("DATABASE_URL", "postgres://app:secret@db:5432/undelete")
	t.Setenv("MIGRATION_DATABASE_URL", "postgres://owner:secret@db:5432/undelete")
	t.Setenv("TELEGRAM_BOT_TOKEN", "token")
}

// TestLoadRejectsMissingRequiredVars pins the three startup guards: without
// any one of them the bot must refuse to start, plainly.
func TestLoadRejectsMissingRequiredVars(t *testing.T) {
	tests := []struct {
		name  string
		blank string
		want  string
	}{
		{name: "DATABASE_URL", blank: "DATABASE_URL", want: "DATABASE_URL is required"},
		{name: "MIGRATION_DATABASE_URL", blank: "MIGRATION_DATABASE_URL", want: "MIGRATION_DATABASE_URL is required"},
		{name: "TELEGRAM_BOT_TOKEN", blank: "TELEGRAM_BOT_TOKEN", want: "TELEGRAM_BOT_TOKEN is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withValidBase(t)
			t.Setenv(tt.blank, "")
			_, err := Load()
			if err == nil || err.Error() != tt.want {
				t.Fatalf("Load() error = %v, want %q", err, tt.want)
			}
		})
	}
}

// TestLoadRejectsInvalidOwnerGuard pins the mono-tenant guard parsing: a
// non-numeric OWNER_TELEGRAM_USER_ID must fail startup, never silently
// disable the filter.
func TestLoadRejectsInvalidOwnerGuard(t *testing.T) {
	for _, raw := range []string{"abc", "12.5", "7x7"} {
		t.Run(raw, func(t *testing.T) {
			withValidBase(t)
			t.Setenv("OWNER_TELEGRAM_USER_ID", raw)
			if _, err := Load(); err == nil {
				t.Fatalf("Load() accepted OWNER_TELEGRAM_USER_ID=%q", raw)
			}
		})
	}
}

// TestLoadOwnerGuardZeroMeansNoRestriction pins the dev-mode convention: an
// explicitly empty value (and the unset variable) leaves the filter off.
func TestLoadOwnerGuardZeroMeansNoRestriction(t *testing.T) {
	withValidBase(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if cfg.OwnerTelegramUserID != 0 {
		t.Fatalf("OwnerTelegramUserID = %d, want 0 (no restriction)", cfg.OwnerTelegramUserID)
	}
}

// TestLoadMediaDirDefaultsAndTrims pins the storage-root handling: empty
// means the default (never the working directory), and surrounding spaces
// are trimmed rather than becoming part of a path.
func TestLoadMediaDirDefaultsAndTrims(t *testing.T) {
	t.Run("empty means default", func(t *testing.T) {
		withValidBase(t)
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load(): %v", err)
		}
		if cfg.MediaDir != "media" {
			t.Fatalf("MediaDir = %q, want default %q", cfg.MediaDir, "media")
		}
	})
	t.Run("trims spaces", func(t *testing.T) {
		withValidBase(t)
		t.Setenv("MEDIA_DIR", "  custom-dir  ")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load(): %v", err)
		}
		if cfg.MediaDir != "custom-dir" {
			t.Fatalf("MediaDir = %q, want trimmed custom-dir", cfg.MediaDir)
		}
	})
	t.Run("blank means default", func(t *testing.T) {
		withValidBase(t)
		t.Setenv("MEDIA_DIR", "   ")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load(): %v", err)
		}
		if cfg.MediaDir != "media" {
			t.Fatalf("MediaDir = %q, want default", cfg.MediaDir)
		}
	})
}

// TestLoadDryRunParsing pins the conservative boolean parsing: "1"/"0" and
// any casing ParseBool accepts work, while a typo ("yes") fails startup
// instead of silently leaving retention enabled.
func TestLoadDryRunParsing(t *testing.T) {
	accepted := map[string]bool{"true": true, "True": true, "TRUE": true, "1": true, "false": false, "0": false, " true ": true}
	for raw, want := range accepted {
		t.Run("accept "+raw, func(t *testing.T) {
			withValidBase(t)
			t.Setenv("MEDIA_PURGE_DRY_RUN", raw)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load(): %v", err)
			}
			if cfg.MediaPurgeDryRun != want {
				t.Fatalf("MediaPurgeDryRun = %t, want %t for %q", cfg.MediaPurgeDryRun, want, raw)
			}
		})
	}
	for _, raw := range []string{"yes", "on", "2"} {
		t.Run("reject "+raw, func(t *testing.T) {
			withValidBase(t)
			t.Setenv("MEDIA_PURGE_DRY_RUN", raw)
			if _, err := Load(); err == nil {
				t.Fatalf("Load() accepted MEDIA_PURGE_DRY_RUN=%q", raw)
			}
		})
	}
}

// TestLoadHealthAddrEdges pins the probe-address validation: IPv6 and
// host-less forms are accepted, a missing port is rejected at startup.
func TestLoadHealthAddrEdges(t *testing.T) {
	t.Run("ipv6 accepted", func(t *testing.T) {
		withValidBase(t)
		t.Setenv("HEALTH_ADDR", "[::1]:9090")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load(): %v", err)
		}
		if cfg.HealthAddr != "[::1]:9090" {
			t.Fatalf("HealthAddr = %q", cfg.HealthAddr)
		}
	})
	t.Run("missing port rejected", func(t *testing.T) {
		withValidBase(t)
		t.Setenv("HEALTH_ADDR", "127.0.0.1")
		if _, err := Load(); err == nil {
			t.Fatal("Load() accepted HEALTH_ADDR without a port")
		}
	})
	t.Run("explicit empty disables", func(t *testing.T) {
		withValidBase(t)
		t.Setenv("HEALTH_ADDR", "")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load(): %v", err)
		}
		if cfg.HealthAddr != "" {
			t.Fatalf("HealthAddr = %q, want empty (disabled)", cfg.HealthAddr)
		}
	})
}
