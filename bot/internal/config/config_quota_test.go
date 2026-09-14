package config

import (
	"testing"

	"github.com/LouisMoretti/Undelete/bot/internal/quotas"
)

// TestLoadQuotaDefaults pins the shipped per-tenant quotas: absent variables
// keep the generous defaults, which are quotas.DefaultLimits by construction.
func TestLoadQuotaDefaults(t *testing.T) {
	validEnv(t)
	for _, env := range []string{
		"QUOTA_MAX_MESSAGES_PER_TENANT",
		"QUOTA_MAX_MEDIA_FILES_PER_TENANT",
		"QUOTA_MAX_MEDIA_BYTES_PER_TENANT",
		"QUOTA_CAPTURES_PER_MINUTE_PER_TENANT",
		"QUOTA_WARN_PERCENT",
	} {
		t.Setenv(env, "")
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := quotas.DefaultLimits()
	if cfg.QuotaMaxMessages != want.MaxMessages ||
		cfg.QuotaMaxMediaFiles != want.MaxMediaFiles ||
		cfg.QuotaMaxMediaBytes != want.MaxMediaBytes ||
		cfg.QuotaCapturesPerMinute != want.CapturesPerMinute ||
		cfg.QuotaWarnPercent != want.WarnPercent {
		t.Fatalf("quota defaults = (%d,%d,%d,%d,%d), want %+v",
			cfg.QuotaMaxMessages, cfg.QuotaMaxMediaFiles, cfg.QuotaMaxMediaBytes,
			cfg.QuotaCapturesPerMinute, cfg.QuotaWarnPercent, want)
	}
}

// TestQuotaLimitsMatchDefaults pins the two defaults together: the config
// defaults and quotas.DefaultLimits must stay equal, and QuotaLimits must
// hand them to the tracker unchanged.
func TestQuotaLimitsMatchDefaults(t *testing.T) {
	validEnv(t)
	for _, env := range []string{
		"QUOTA_MAX_MESSAGES_PER_TENANT",
		"QUOTA_MAX_MEDIA_FILES_PER_TENANT",
		"QUOTA_MAX_MEDIA_BYTES_PER_TENANT",
		"QUOTA_CAPTURES_PER_MINUTE_PER_TENANT",
		"QUOTA_WARN_PERCENT",
	} {
		t.Setenv(env, "")
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.QuotaLimits(); got != quotas.DefaultLimits() {
		t.Fatalf("QuotaLimits() = %+v, want quotas.DefaultLimits() = %+v", got, quotas.DefaultLimits())
	}
}

// TestLoadQuotaOverrides pins the operator surface: every quota reads its own
// variable, and QuotaLimits carries the values to the tracker.
func TestLoadQuotaOverrides(t *testing.T) {
	validEnv(t)
	t.Setenv("QUOTA_MAX_MESSAGES_PER_TENANT", "1000")
	t.Setenv("QUOTA_MAX_MEDIA_FILES_PER_TENANT", "100")
	t.Setenv("QUOTA_MAX_MEDIA_BYTES_PER_TENANT", "1048576")
	t.Setenv("QUOTA_CAPTURES_PER_MINUTE_PER_TENANT", "60")
	t.Setenv("QUOTA_WARN_PERCENT", "90")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := cfg.QuotaLimits()
	want := quotas.Limits{
		MaxMessages:       1000,
		MaxMediaFiles:     100,
		MaxMediaBytes:     1048576,
		CapturesPerMinute: 60,
		WarnPercent:       90,
	}
	if got != want {
		t.Fatalf("QuotaLimits() = %+v, want %+v", got, want)
	}
}

// TestLoadRejectsInvalidQuotas pins the fail-fast contract: a malformed,
// zero, negative or out-of-range quota refuses to start rather than running
// an unbounded tenant silently.
func TestLoadRejectsInvalidQuotas(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  string
		val  string
	}{
		{"messages malformed", "QUOTA_MAX_MESSAGES_PER_TENANT", "a lot"},
		{"messages zero", "QUOTA_MAX_MESSAGES_PER_TENANT", "0"},
		{"messages negative", "QUOTA_MAX_MESSAGES_PER_TENANT", "-5"},
		{"media files zero", "QUOTA_MAX_MEDIA_FILES_PER_TENANT", "0"},
		{"media bytes malformed", "QUOTA_MAX_MEDIA_BYTES_PER_TENANT", "5GiB"},
		{"media bytes negative", "QUOTA_MAX_MEDIA_BYTES_PER_TENANT", "-1"},
		{"rate zero", "QUOTA_CAPTURES_PER_MINUTE_PER_TENANT", "0"},
		{"warn zero", "QUOTA_WARN_PERCENT", "0"},
		{"warn hundred", "QUOTA_WARN_PERCENT", "100"},
		{"warn malformed", "QUOTA_WARN_PERCENT", "eighty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			validEnv(t)
			t.Setenv(tc.env, tc.val)
			if _, err := Load(); err == nil {
				t.Fatalf("Load with %s=%q = nil, want a refusal", tc.env, tc.val)
			}
		})
	}
}
