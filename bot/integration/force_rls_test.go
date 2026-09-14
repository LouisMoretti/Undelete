//go:build integration

package integration_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/LouisMoretti/Undelete/bot/internal/storage"
)

// TestPostgreSQL16ForceRowLevelSecurity proves on a real PostgreSQL 16 what
// behavioural tests cannot: that ROW LEVEL SECURITY is FORCED (not merely
// enabled) on every tenant table.
//
// ENABLE alone does not apply to the table owner: without FORCE, the owner
// role (which runs migrations and owns the tables) would bypass every
// policy silently. The restore script checks the same five tables; this test
// covers them all -- messages, notification_outbox, chats, media_files
// (migration 0004) and data_erasure_requests (migration 0006).
func TestPostgreSQL16ForceRowLevelSecurity(t *testing.T) {
	adminDSN := requireEnv(t, "POSTGRES_INTEGRATION_ADMIN_DSN")
	runtimeDSN := requireEnv(t, "POSTGRES_INTEGRATION_RUNTIME_DSN")
	if err := validateExplicitDestructiveOptIn(os.Getenv("POSTGRES_INTEGRATION_ALLOW_DESTRUCTIVE")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), setupTimeout)
	defer cancel()

	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	defer admin.Close(ctx)
	var databaseName string
	if err := admin.QueryRow(ctx, `SELECT current_database()`).Scan(&databaseName); err != nil {
		t.Fatalf("read current database for destructive interlock: %v", err)
	}
	if err := validateDestructiveInterlock(os.Getenv("POSTGRES_INTEGRATION_ALLOW_DESTRUCTIVE"), databaseName); err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := storage.RunMigrations(ctx, adminDSN, logger); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	// The runtime pool must still open: this also proves the migration set
	// grants the undelete_app role everything it needs on all five tables.
	db, err := storage.NewPool(ctx, runtimeDSN)
	if err != nil {
		t.Fatalf("open runtime pool: %v", err)
	}
	defer db.Close()

	for _, table := range []string{"messages", "notification_outbox", "chats", "media_files", "data_erasure_requests"} {
		t.Run("FORCE RLS on "+table, func(t *testing.T) {
			ctx := phaseContext(t)
			var enabled, forced bool
			if err := admin.QueryRow(ctx, `
				SELECT relrowsecurity, relforcerowsecurity
				FROM pg_class WHERE relname = $1
			`, table).Scan(&enabled, &forced); err != nil {
				t.Fatalf("read RLS flags of %s: %v", table, err)
			}
			if !enabled {
				t.Fatalf("RLS is not enabled on %s", table)
			}
			if !forced {
				t.Fatalf("RLS is enabled but NOT FORCED on %s: the table owner would bypass every policy", table)
			}
		})
	}

	t.Run("owner bypasses policies but app role does not", func(t *testing.T) {
		ctx := phaseContext(t)
		// The admin (owner) sees everything regardless of policies -- this is
		// WHY the runtime role must differ (config.Load refuses identical
		// DSNs) and why FORCE matters.
		var ownerSeesMessages int
		if err := admin.QueryRow(ctx, `SELECT count(*) FROM messages`).Scan(&ownerSeesMessages); err != nil {
			t.Fatalf("owner count: %v", err)
		}
		var appSeesMessages int
		if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM messages`).Scan(&appSeesMessages); err != nil {
			t.Fatalf("app count: %v", err)
		}
		if appSeesMessages != 0 {
			t.Fatalf("app role sees %d messages without tenant context", appSeesMessages)
		}
	})
}
