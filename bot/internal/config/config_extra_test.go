package config

import (
	"strings"
	"testing"
)

// withCleanEnv resets every variable Load reads, so each case starts from a
// blank slate rather than inheriting the developer's shell.
func withCleanEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"DATABASE_URL", "MIGRATION_DATABASE_URL", "TELEGRAM_BOT_TOKEN",
		"OWNER_TELEGRAM_USER_ID", "OWNER_ALLOWLIST_TELEGRAM_USER_IDS",
		"MEDIA_DIR", "MEDIA_PURGE_DRY_RUN", "HEALTH_ADDR",
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

// TestLoadRejectsInvalidOwnerAllowlist pins the allowlist parsing: a malformed
// entry must fail startup, never be skipped. An allowlist silently missing an
// id would lock its owner out; one silently gaining an id would admit a
// stranger.
func TestLoadRejectsInvalidOwnerAllowlist(t *testing.T) {
	for _, raw := range []string{"abc", "12.5", "7x7", "-42", "+42", "007", "0", "42,abc", "42 abc"} {
		t.Run(raw, func(t *testing.T) {
			withValidBase(t)
			t.Setenv("OWNER_ALLOWLIST_TELEGRAM_USER_IDS", raw)
			if _, err := Load(); err == nil {
				t.Fatalf("Load() accepted OWNER_ALLOWLIST_TELEGRAM_USER_IDS=%q", raw)
			}
		})
	}
}

// TestLoadEmptyOwnerAllowlistIsOpenOnboarding pins the multi-tenant default: an
// explicitly empty value (and the unset variable) admits every account holder.
func TestLoadEmptyOwnerAllowlistIsOpenOnboarding(t *testing.T) {
	withValidBase(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if len(cfg.AllowedOwnerTelegramUserIDs) != 0 {
		t.Fatalf("AllowedOwnerTelegramUserIDs = %v, want empty (open onboarding)", cfg.AllowedOwnerTelegramUserIDs)
	}
}

// TestLoadRejectsObsoleteOwnerGuard is the upgrade guard of issue #17: an
// operator who upgrades with OWNER_TELEGRAM_USER_ID still in their .env would
// otherwise keep believing a single account holder is admitted, while the bot
// had silently switched to open onboarding. Startup must fail and name the
// replacement.
//
// "Set but empty" is the one tolerated shape: docker compose forwards every
// declared variable, so an unset guard reaches the container as an empty
// string, and empty meant "no guard" before exactly as it means "open
// onboarding" now.
func TestLoadRejectsObsoleteOwnerGuard(t *testing.T) {
	t.Run("a leftover value fails startup", func(t *testing.T) {
		withValidBase(t)
		t.Setenv("OWNER_TELEGRAM_USER_ID", "123456789")
		_, err := Load()
		if err == nil {
			t.Fatal("Load() accepted the obsolete OWNER_TELEGRAM_USER_ID")
		}
		if !strings.Contains(err.Error(), "OWNER_ALLOWLIST_TELEGRAM_USER_IDS") {
			t.Fatalf("Load() error = %v, want it to name the replacement variable", err)
		}
	})

	t.Run("set but empty is tolerated", func(t *testing.T) {
		withValidBase(t)
		t.Setenv("OWNER_TELEGRAM_USER_ID", "")
		if _, err := Load(); err != nil {
			t.Fatalf("Load() with an empty OWNER_TELEGRAM_USER_ID: %v", err)
		}
	})
}

// TestLoadParsesOwnerAllowlist pins the accepted shapes of the list: commas,
// whitespace, both mixed, and duplicates collapsed rather than refused.
func TestLoadParsesOwnerAllowlist(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []int64
	}{
		{name: "single id", raw: "123456789", want: []int64{123456789}},
		{name: "comma separated", raw: "1,2,3", want: []int64{1, 2, 3}},
		{name: "comma and spaces", raw: " 1, 2 ,3 ", want: []int64{1, 2, 3}},
		{name: "whitespace separated", raw: "1 2\t3", want: []int64{1, 2, 3}},
		{name: "empty entries skipped", raw: "1,,2", want: []int64{1, 2}},
		{name: "duplicates collapsed", raw: "1,2,1", want: []int64{1, 2}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withValidBase(t)
			t.Setenv("OWNER_ALLOWLIST_TELEGRAM_USER_IDS", tt.raw)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load(): %v", err)
			}
			if len(cfg.AllowedOwnerTelegramUserIDs) != len(tt.want) {
				t.Fatalf("AllowedOwnerTelegramUserIDs = %v, want %v", cfg.AllowedOwnerTelegramUserIDs, tt.want)
			}
			for i, id := range tt.want {
				if cfg.AllowedOwnerTelegramUserIDs[i] != id {
					t.Fatalf("AllowedOwnerTelegramUserIDs = %v, want %v", cfg.AllowedOwnerTelegramUserIDs, tt.want)
				}
			}
		})
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
