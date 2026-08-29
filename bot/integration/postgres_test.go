//go:build integration

package integration_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/LouisMoretti/Undelete/bot/internal/messages"
	"github.com/LouisMoretti/Undelete/bot/internal/storage"
	"github.com/LouisMoretti/Undelete/bot/internal/users"
)

func TestPostgreSQL16SecurityAndRetention(t *testing.T) {
	adminDSN := requireEnv(t, "POSTGRES_INTEGRATION_ADMIN_DSN")
	runtimeDSN := requireEnv(t, "POSTGRES_INTEGRATION_RUNTIME_DSN")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	defer admin.Close(ctx)

	var serverVersionText string
	if err := admin.QueryRow(ctx, `SHOW server_version_num`).Scan(&serverVersionText); err != nil {
		t.Fatalf("read PostgreSQL version: %v", err)
	}
	serverVersion, err := strconv.Atoi(serverVersionText)
	if err != nil {
		t.Fatalf("parse PostgreSQL version %q: %v", serverVersionText, err)
	}
	if serverVersion < 160000 || serverVersion >= 170000 {
		t.Fatalf("expected PostgreSQL 16, got server_version_num=%d", serverVersion)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := storage.RunMigrations(ctx, adminDSN, logger); err != nil {
		t.Fatalf("first migration run: %v", err)
	}
	if err := storage.RunMigrations(ctx, adminDSN, logger); err != nil {
		t.Fatalf("migration rerun must be idempotent: %v", err)
	}
	var migrationCount int
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM schema_migrations WHERE version = 1`).Scan(&migrationCount); err != nil {
		t.Fatalf("read migration ledger: %v", err)
	}
	if migrationCount != 1 {
		t.Fatalf("migration 1 recorded %d times, want exactly once", migrationCount)
	}
	if _, err := admin.Exec(ctx, `TRUNCATE messages, business_connections, users RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("reset integration fixtures: %v", err)
	}

	t.Run("runtime role is least privileged", func(t *testing.T) {
		var superuser, createDB, createRole, bypassRLS bool
		if err := admin.QueryRow(ctx, `
			SELECT rolsuper, rolcreatedb, rolcreaterole, rolbypassrls
			FROM pg_roles WHERE rolname = 'undelete_app'
		`).Scan(&superuser, &createDB, &createRole, &bypassRLS); err != nil {
			t.Fatalf("read runtime role: %v", err)
		}
		if superuser || createDB || createRole || bypassRLS {
			t.Fatalf("dangerous runtime attributes: super=%t createdb=%t createrole=%t bypassrls=%t", superuser, createDB, createRole, bypassRLS)
		}
	})

	db, err := storage.NewPool(ctx, runtimeDSN)
	if err != nil {
		t.Fatalf("open runtime pool: %v", err)
	}
	defer db.Close()

	t.Run("dangerous and wrong roles are rejected", func(t *testing.T) {
		assertPoolRejected(t, ctx, adminDSN)
		for _, role := range []struct {
			name       string
			attributes string
		}{
			{name: "integration_wrong_role", attributes: "NOSUPERUSER NOBYPASSRLS"},
			{name: "integration_bypass_role", attributes: "NOSUPERUSER BYPASSRLS"},
		} {
			if _, err := admin.Exec(ctx, `DROP ROLE IF EXISTS `+pgx.Identifier{role.name}.Sanitize()); err != nil {
				t.Fatalf("drop test role %s: %v", role.name, err)
			}
			if _, err := admin.Exec(ctx, `CREATE ROLE `+pgx.Identifier{role.name}.Sanitize()+` LOGIN PASSWORD 'integration-only' `+role.attributes); err != nil {
				t.Fatalf("create test role %s: %v", role.name, err)
			}
			t.Cleanup(func() {
				_, _ = admin.Exec(context.Background(), `DROP ROLE IF EXISTS `+pgx.Identifier{role.name}.Sanitize())
			})
			assertPoolRejected(t, ctx, replaceDSNUser(runtimeDSN, role.name, "integration-only"))
		}
	})

	t.Run("runtime role cannot execute DDL", func(t *testing.T) {
		if _, err := db.Pool.Exec(ctx, `CREATE TABLE runtime_must_not_create_tables (id bigint)`); err == nil {
			t.Fatal("runtime DDL unexpectedly succeeded")
		}
	})

	userRepo := users.NewRepository(db.Pool)
	owner1, err := userRepo.UpsertByTelegramID(ctx, 91001)
	if err != nil {
		t.Fatalf("create owner 1: %v", err)
	}
	owner2, err := userRepo.UpsertByTelegramID(ctx, 91002)
	if err != nil {
		t.Fatalf("create owner 2: %v", err)
	}
	msgRepo := messages.NewRepository(db)

	messageFor := func(connection string, id int64, text string) messages.Record {
		return messages.Record{BusinessConnectionID: connection, ChatID: 77, MessageID: id, FromDisplay: "integration", MessageType: "text", TextContent: text, TelegramDate: time.Now().Unix()}
	}
	if err := msgRepo.Save(ctx, owner1.ID, messageFor("owner-1", 1, "owner one"), false); err != nil {
		t.Fatalf("save owner 1: %v", err)
	}
	if err := msgRepo.Save(ctx, owner2.ID, messageFor("owner-2", 1, "owner two"), false); err != nil {
		t.Fatalf("save owner 2: %v", err)
	}

	t.Run("RLS fails closed without tenant context", func(t *testing.T) {
		var count int
		if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM messages`).Scan(&count); err != nil {
			t.Fatalf("raw select: %v", err)
		}
		if count != 0 {
			t.Fatalf("raw select exposed %d messages without tenant context", count)
		}
		if _, err := db.Pool.Exec(ctx, `DELETE FROM messages`); err != nil {
			t.Fatalf("fail-closed raw delete should be a zero-row no-op: %v", err)
		}
		if _, err := db.Pool.Exec(ctx, `INSERT INTO messages (owner_user_id, business_connection_id, chat_id, message_id, message_type, telegram_date) VALUES ($1, 'raw', 1, 1, 'text', 1)`, owner1.ID); err == nil {
			t.Fatal("raw insert unexpectedly bypassed RLS WITH CHECK")
		}
	})

	t.Run("CRUD is isolated between tenants", func(t *testing.T) {
		wrongTenant, err := msgRepo.MarkDeleted(ctx, owner2.ID, "owner-1", 77, []int64{1})
		if err != nil {
			t.Fatalf("cross-tenant update: %v", err)
		}
		if len(wrongTenant) != 0 {
			t.Fatalf("cross-tenant update returned %d rows", len(wrongTenant))
		}
		ownTenant, err := msgRepo.MarkDeleted(ctx, owner1.ID, "owner-1", 77, []int64{1})
		if err != nil {
			t.Fatalf("own-tenant update: %v", err)
		}
		if len(ownTenant) != 1 || ownTenant[0].TextContent != "owner one" {
			t.Fatalf("unexpected own-tenant update result: %#v", ownTenant)
		}
		assertTenantCount(t, ctx, db, owner1.ID, 1)
		assertTenantCount(t, ctx, db, owner2.ID, 1)
		var leaked int
		if err := db.InTenant(ctx, owner1.ID, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM messages WHERE owner_user_id = $1`, owner2.ID).Scan(&leaked)
		}); err != nil {
			t.Fatalf("cross-tenant read: %v", err)
		}
		if leaked != 0 {
			t.Fatalf("owner 1 read %d owner 2 rows", leaked)
		}
	})

	t.Run("PurgeExpired purges tenant by tenant", func(t *testing.T) {
		for _, fixture := range []struct {
			ownerID int64
			conn    string
			id      int64
		}{
			{owner1.ID, "owner-1", 10}, {owner1.ID, "owner-1", 11},
			{owner2.ID, "owner-2", 10}, {owner2.ID, "owner-2", 11},
		} {
			if err := msgRepo.Save(ctx, fixture.ownerID, messageFor(fixture.conn, fixture.id, "retention"), false); err != nil {
				t.Fatalf("save retention fixture: %v", err)
			}
		}
		if _, err := admin.Exec(ctx, `UPDATE messages SET saved_at = now() - interval '10 days' WHERE message_id = 10`); err != nil {
			t.Fatalf("age fixtures: %v", err)
		}

		purged, err := msgRepo.PurgeExpired(ctx, []users.TenantRetention{{OwnerUserID: owner1.ID, RetentionDays: 7}})
		if err != nil {
			t.Fatalf("purge owner 1: %v", err)
		}
		if purged != 1 {
			t.Fatalf("purged owner 1 rows=%d, want 1", purged)
		}
		assertTenantCount(t, ctx, db, owner1.ID, 2)
		assertTenantCount(t, ctx, db, owner2.ID, 3)

		purged, err = msgRepo.PurgeExpired(ctx, []users.TenantRetention{{OwnerUserID: owner2.ID, RetentionDays: 7}})
		if err != nil {
			t.Fatalf("purge owner 2: %v", err)
		}
		if purged != 1 {
			t.Fatalf("purged owner 2 rows=%d, want 1", purged)
		}
		assertTenantCount(t, ctx, db, owner2.ID, 2)
	})
}

func requireEnv(t *testing.T, name string) string {
	t.Helper()
	value := os.Getenv(name)
	if value == "" {
		t.Fatalf("%s must be set; run `make test-integration` from the repository root", name)
	}
	return value
}

func assertPoolRejected(t *testing.T, ctx context.Context, dsn string) {
	t.Helper()
	db, err := storage.NewPool(ctx, dsn)
	if db != nil {
		db.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "rôle runtime PostgreSQL dangereux") {
		t.Fatalf("pool should reject role, got %v", err)
	}
}

func replaceDSNUser(dsn, user, password string) string {
	parts := strings.Split(dsn, "@")
	prefix := parts[0]
	if scheme := strings.Index(prefix, "://"); scheme >= 0 {
		prefix = prefix[:scheme+3] + user + ":" + password
	}
	return prefix + "@" + strings.Join(parts[1:], "@")
}

func assertTenantCount(t *testing.T, ctx context.Context, db *storage.DB, ownerID int64, want int) {
	t.Helper()
	var got int
	if err := db.InTenant(ctx, ownerID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM messages`).Scan(&got)
	}); err != nil {
		t.Fatalf("count tenant %d: %v", ownerID, err)
	}
	if got != want {
		t.Fatalf("tenant %d row count=%d, want %d", ownerID, got, want)
	}
}
