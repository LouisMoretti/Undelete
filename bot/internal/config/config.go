// Package config loads and validates the bot configuration from
// environment variables.
package config

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"unicode"
)

// Config holds the bot's runtime configuration.
type Config struct {
	// DatabaseURL is the application DSN, connected with the undelete_app
	// role (NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS). It is the ONLY
	// DSN used after boot, once migrations are applied.
	DatabaseURL string

	// MigrationDatabaseURL is the owner DSN (POSTGRES_USER, superuser in the
	// official Postgres image). Used ONLY at boot, to apply migrations, never
	// for runtime traffic.
	MigrationDatabaseURL string

	// TelegramBotToken is the bot token, as provided by BotFather.
	TelegramBotToken string

	// AllowedOwnerTelegramUserIDs is the onboarding allowlist, read from
	// OWNER_ALLOWLIST_TELEGRAM_USER_IDS. It replaces the mono-tenant
	// OWNER_TELEGRAM_USER_ID guard of Phase 1.
	//
	// EMPTY means open onboarding: any Telegram Business account holder may
	// connect the bot, and each one becomes a tenant of their own. A non-empty
	// list admits exactly those Telegram user ids -- which is how a deployment
	// that used to be mono-tenant keeps precisely the guarantee it had, by
	// listing its single holder.
	//
	// This is ADMISSION control, never isolation: whatever it contains, every
	// tenant's data stays behind the same RLS policies keyed on owner_user_id.
	AllowedOwnerTelegramUserIDs []int64

	// MediaDir is the storage root of the downloaded attachments ("media" by
	// default, bind-mounted to /app/media by Compose). Every path stored in
	// media_files is RELATIVE to it, so moving the root does not invalidate a
	// single row.
	MediaDir string

	// MediaPurgeDryRun switches the media retention purge to a mode where it
	// logs every file it would delete and removes nothing. Meant for the
	// first runs on a real storage tree, where the cost of a wrong deletion
	// (a blob is gone for good) is not symmetric with the cost of keeping it
	// one more day. Defaults to false: retention that does not run is a
	// silent breach of the promise made to the owner.
	MediaPurgeDryRun bool

	// BackupRetentionDays mirrors the BACKUP_RETENTION_DAYS used by
	// scripts/backup.sh to purge the daily dumps. The bot never reads or writes
	// a backup; it needs the value for one sentence: the confirmation of
	// /delete_my_data states the maximum residual survival of the data in the
	// archives already written, and quoting a hardcoded 14 on a deployment that
	// keeps them for 90 would be a false statement about deleted data.
	// Defaults to defaultBackupRetentionDays, the same default backup.sh
	// applies when the variable is absent.
	BackupRetentionDays int

	// HealthAddr is the listen address for the /livez, /readyz and
	// /metrics probes. Defaults to defaultHealthAddr if HEALTH_ADDR is not
	// set; an explicitly EMPTY value disables the server (no port opened).
	// These endpoints expose no user content, but they remain intended for
	// the internal network: do not publish them as-is.
	HealthAddr string
}

// defaultHealthAddr: dedicated monitoring port, distinct from any
// application traffic (the bot listens on nothing else, it is in outgoing
// long polling).
const defaultHealthAddr = ":9090"

// defaultMediaDir matches the volume mounted by docker-compose (./media ->
// /app/media), the bot running with /app as its working directory.
const defaultMediaDir = "media"

// defaultBackupRetentionDays is the default of scripts/backup.sh, repeated here
// because the bot has no way to ask the script what it applies. The two must
// stay equal: this is the number the owner is told their deleted data can
// survive in a dump.
const defaultBackupRetentionDays = 14

// Load reads the configuration from the environment and validates it.
//
// Refuses to start if DatabaseURL == MigrationDatabaseURL: if the two DSNs
// point to the same role, the application would run with the superuser
// privileges of the migration role, and FORCE ROW LEVEL SECURITY on
// messages would become purely decorative (a superuser bypasses RLS in
// practice via implicit BYPASSRLS / table ownership). This is the project's
// most silent security constraint: nothing visibly breaks, the table is
// just completely open.
func Load() (*Config, error) {
	cfg := &Config{
		DatabaseURL:          os.Getenv("DATABASE_URL"),
		MigrationDatabaseURL: os.Getenv("MIGRATION_DATABASE_URL"),
		TelegramBotToken:     os.Getenv("TELEGRAM_BOT_TOKEN"),
		HealthAddr:           defaultHealthAddr,
		MediaDir:             defaultMediaDir,
		BackupRetentionDays:  defaultBackupRetentionDays,
	}

	// Same parse rule as preflight.sh applies to the same variable: an integer,
	// and a strictly positive one. A malformed or zero value would make the
	// erasure confirmation state a survival time that matches nothing the
	// backups actually do, so it fails at startup rather than being rounded to
	// a default that contradicts the operator's intent.
	if raw := strings.TrimSpace(os.Getenv("BACKUP_RETENTION_DAYS")); raw != "" {
		days, err := strconv.Atoi(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid BACKUP_RETENTION_DAYS (expected an integer number of days): %w", err)
		}
		if days <= 0 {
			return nil, fmt.Errorf("invalid BACKUP_RETENTION_DAYS: %d days; expected a positive number of days", days)
		}
		cfg.BackupRetentionDays = days
	}

	// An empty MEDIA_DIR is not a way to disable anything: it would resolve
	// every relative path against the working directory. The default applies
	// instead, and only a non-empty value overrides it.
	if raw := strings.TrimSpace(os.Getenv("MEDIA_DIR")); raw != "" {
		cfg.MediaDir = raw
	}

	// Only an explicit, unambiguous opt-in enables the dry run. A typo
	// ("yes", "on") must not silently disable retention: it falls through to
	// the parse error below rather than to false.
	if raw := strings.TrimSpace(os.Getenv("MEDIA_PURGE_DRY_RUN")); raw != "" {
		dryRun, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid MEDIA_PURGE_DRY_RUN (expected a boolean, e.g. \"true\" or \"false\"): %w", err)
		}
		cfg.MediaPurgeDryRun = dryRun
	}

	// LookupEnv rather than Getenv: "variable absent" (we want the default)
	// and "variable set empty" (we want to disable the server) are two
	// different intentions.
	if raw, ok := os.LookupEnv("HEALTH_ADDR"); ok {
		cfg.HealthAddr = raw
	}

	// Validated here, not just at net.Listen: a malformed value ("9090"
	// without a colon) would let the bot start normally and then lose ALL of
	// its probes and metrics on a single Error log, with nothing else moving.
	// A silent monitoring is exactly what issue #6 seeks to eliminate: we
	// fail at startup, plainly.
	if cfg.HealthAddr != "" {
		if _, _, err := net.SplitHostPort(cfg.HealthAddr); err != nil {
			return nil, fmt.Errorf("invalid HEALTH_ADDR (expected \"host:port\", e.g. %q; empty to disable the health server): %w", defaultHealthAddr, err)
		}
	}

	if cfg.DatabaseURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}
	if cfg.MigrationDatabaseURL == "" {
		return nil, fmt.Errorf("MIGRATION_DATABASE_URL is required")
	}
	if cfg.TelegramBotToken == "" {
		return nil, fmt.Errorf("TELEGRAM_BOT_TOKEN is required")
	}

	if cfg.DatabaseURL == cfg.MigrationDatabaseURL {
		return nil, fmt.Errorf("DATABASE_URL and MIGRATION_DATABASE_URL are identical: " +
			"the application would run with the owner role (superuser) and FORCE ROW LEVEL SECURITY " +
			"on messages would be decorative; use the restricted undelete_app role for DATABASE_URL")
	}

	// OWNER_TELEGRAM_USER_ID was the Phase 1 mono-tenant guard. Ignoring a
	// leftover value would be the one silent failure mode this change can
	// have: the operator would still believe a single account holder is
	// admitted, while the bot had in fact switched to open onboarding. It is
	// therefore a startup error, not a warning.
	//
	// "Set but empty" is tolerated on purpose: docker compose passes every
	// declared variable through, so an unset OWNER_TELEGRAM_USER_ID reaches the
	// container as an empty string. An empty value meant "no guard" before and
	// means "open onboarding" now -- the same thing, so there is nothing to
	// correct.
	if raw := strings.TrimSpace(os.Getenv("OWNER_TELEGRAM_USER_ID")); raw != "" {
		return nil, fmt.Errorf("OWNER_TELEGRAM_USER_ID is no longer supported: " +
			"move that id into OWNER_ALLOWLIST_TELEGRAM_USER_IDS (comma-separated Telegram user ids, " +
			"empty for open onboarding) and unset OWNER_TELEGRAM_USER_ID")
	}

	allowed, err := parseOwnerAllowlist(os.Getenv("OWNER_ALLOWLIST_TELEGRAM_USER_IDS"))
	if err != nil {
		return nil, err
	}
	cfg.AllowedOwnerTelegramUserIDs = allowed

	return cfg, nil
}

// parseOwnerAllowlist reads OWNER_ALLOWLIST_TELEGRAM_USER_IDS into the list of
// Telegram user ids allowed to onboard.
//
// Accepted separators are commas and any whitespace, so a list can be written
// on one line or wrapped. Each token must be a canonical, strictly positive
// decimal integer: no sign, no leading zero, nothing else (same strictness as
// users.ParseRetentionDays). A malformed entry is refused rather than skipped
// -- an allowlist silently missing an id would look like it admits an owner it
// does not, and one silently gaining an id would be worse.
//
// An empty (or absent) value yields nil: open onboarding.
func parseOwnerAllowlist(raw string) ([]int64, error) {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || unicode.IsSpace(r)
	})
	if len(fields) == 0 {
		return nil, nil
	}

	allowed := make([]int64, 0, len(fields))
	seen := make(map[int64]struct{}, len(fields))
	for _, token := range fields {
		if !isCanonicalPositiveDecimal(token) {
			return nil, fmt.Errorf("invalid OWNER_ALLOWLIST_TELEGRAM_USER_IDS entry %q: "+
				"expected a comma-separated list of positive Telegram user ids", token)
		}
		id, err := strconv.ParseInt(token, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid OWNER_ALLOWLIST_TELEGRAM_USER_IDS entry %q: %w", token, err)
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		allowed = append(allowed, id)
	}
	return allowed, nil
}

// isCanonicalPositiveDecimal reports whether token is a strictly positive
// decimal integer written without sign, leading zero or separator.
func isCanonicalPositiveDecimal(token string) bool {
	if token == "" || token == "0" {
		return false
	}
	if token[0] == '0' {
		return false
	}
	for i := 0; i < len(token); i++ {
		if token[i] < '0' || token[i] > '9' {
			return false
		}
	}
	return true
}
