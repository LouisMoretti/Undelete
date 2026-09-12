package config

import (
	"os"
	"strings"
	"testing"
)

func validEnv(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://undelete_app:app@postgres/undelete")
	t.Setenv("MIGRATION_DATABASE_URL", "postgres://owner:owner@postgres/undelete")
	t.Setenv("TELEGRAM_BOT_TOKEN", "not-a-real-bot-token")
	t.Setenv("OWNER_TELEGRAM_USER_ID", "")
	t.Setenv("HEALTH_ADDR", defaultHealthAddr)
}

func TestLoadHealthAddr(t *testing.T) {
	cases := map[string]struct {
		env   string
		unset bool
		want  string
	}{
		"explicit value":            {env: "127.0.0.1:9999", want: "127.0.0.1:9999"},
		"empty disables the server": {env: "", want: ""},
		"default value if unset":    {unset: true, want: defaultHealthAddr},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			validEnv(t)
			t.Setenv("HEALTH_ADDR", tc.env)
			if tc.unset {
				// validEnv/t.Setenv has already registered the restoration: we
				// can unset the variable to test the "absent" case, distinct
				// from the "set empty" case.
				os.Unsetenv("HEALTH_ADDR")
			}

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load() unexpected error: %v", err)
			}
			if cfg.HealthAddr != tc.want {
				t.Fatalf("HealthAddr = %q, want %q", cfg.HealthAddr, tc.want)
			}
		})
	}
}

// A malformed address must fail at startup: otherwise the bot keeps running
// normally, net.Listen fails in its goroutine and monitoring is silently
// lost, on a single log line.
func TestLoadRejectsMalformedHealthAddr(t *testing.T) {
	for _, addr := range []string{"9090", "127.0.0.1", ":"} {
		t.Run(addr, func(t *testing.T) {
			validEnv(t)
			t.Setenv("HEALTH_ADDR", addr)

			cfg, err := Load()
			if addr == ":" {
				// An empty ":port" is still a valid port per SplitHostPort
				// (listens on an ephemeral port): we do not invent a rule
				// stricter than net.Listen.
				if err != nil {
					t.Fatalf("HEALTH_ADDR=%q wrongly rejected: %v", addr, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("HEALTH_ADDR=%q accepted, expected an error (HealthAddr=%q)", addr, cfg.HealthAddr)
			}
			if !strings.Contains(err.Error(), "HEALTH_ADDR") {
				t.Fatalf("uninformative error: %v", err)
			}
		})
	}
}

func TestLoadRejectsIdenticalDatabaseURLs(t *testing.T) {
	validEnv(t)
	t.Setenv("MIGRATION_DATABASE_URL", "postgres://undelete_app:app@postgres/undelete")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "identical") {
		t.Fatalf("Load() error = %v, expected rejection of identical DSNs", err)
	}
}

func TestLoadParsesOwnerGuard(t *testing.T) {
	validEnv(t)
	t.Setenv("OWNER_TELEGRAM_USER_ID", "123456789")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	if cfg.OwnerTelegramUserID != 123456789 {
		t.Fatalf("OwnerTelegramUserID = %d", cfg.OwnerTelegramUserID)
	}
}

// The dry run is an opt-in, and an ambiguous value must not silently disable
// retention: a purge that never runs breaks the promise made to the owner
// without a single error in the logs.
func TestLoadParsesMediaPurgeDryRun(t *testing.T) {
	cases := map[string]struct {
		env     string
		want    bool
		wantErr bool
	}{
		"absent defaults to a real purge": {env: "", want: false},
		"explicit true":                   {env: "true", want: true},
		"numeric true":                    {env: "1", want: true},
		"explicit false":                  {env: "false", want: false},
		"ambiguous value is refused":      {env: "yes", wantErr: true},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			validEnv(t)
			t.Setenv("MEDIA_PURGE_DRY_RUN", tc.env)

			cfg, err := Load()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Load() accepted MEDIA_PURGE_DRY_RUN=%q", tc.env)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() unexpected error: %v", err)
			}
			if cfg.MediaPurgeDryRun != tc.want {
				t.Fatalf("MediaPurgeDryRun = %t, want %t", cfg.MediaPurgeDryRun, tc.want)
			}
		})
	}
}

// TestLoadParsesBackupRetentionDays: the value is quoted verbatim in the
// /delete_my_data confirmation as the maximum residual survival of the deleted
// data. A malformed or non-positive one must fail at startup rather than be
// rounded into a default that contradicts what backup.sh actually does — the
// owner would be told a number that matches nothing.
func TestLoadParsesBackupRetentionDays(t *testing.T) {
	cases := map[string]struct {
		env     string
		want    int
		wantErr bool
	}{
		"absent matches the backup.sh default": {env: "", want: 14},
		"explicit value":                       {env: "90", want: 90},
		"padded value":                         {env: " 7 ", want: 7},
		"not a number is refused":              {env: "fortnight", wantErr: true},
		"zero is refused":                      {env: "0", wantErr: true},
		"negative is refused":                  {env: "-1", wantErr: true},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			validEnv(t)
			t.Setenv("BACKUP_RETENTION_DAYS", tc.env)

			cfg, err := Load()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Load() accepted BACKUP_RETENTION_DAYS=%q", tc.env)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() unexpected error: %v", err)
			}
			if cfg.BackupRetentionDays != tc.want {
				t.Fatalf("BackupRetentionDays = %d, want %d", cfg.BackupRetentionDays, tc.want)
			}
		})
	}
}
