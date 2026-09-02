// Package config loads and validates the bot configuration from
// environment variables.
package config

import (
	"fmt"
	"net"
	"os"
	"strconv"
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

	// OwnerTelegramUserID, if non-zero, restricts the bot to a single
	// Telegram Business owner (mono-tenant guard Phase 1). A Business
	// connection from a different telegram_user_id is refused.
	// 0 = no restriction (not recommended outside local dev).
	OwnerTelegramUserID int64

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

	if raw := os.Getenv("OWNER_TELEGRAM_USER_ID"); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid OWNER_TELEGRAM_USER_ID: %w", err)
		}
		cfg.OwnerTelegramUserID = id
	}

	return cfg, nil
}
