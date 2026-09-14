//go:build integration

package integration_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/LouisMoretti/Undelete/bot/internal/business"
	"github.com/LouisMoretti/Undelete/bot/internal/erasure"
	"github.com/LouisMoretti/Undelete/bot/internal/media"
	"github.com/LouisMoretti/Undelete/bot/internal/messages"
	"github.com/LouisMoretti/Undelete/bot/internal/storage"
	"github.com/LouisMoretti/Undelete/bot/internal/telegram"
	"github.com/LouisMoretti/Undelete/bot/internal/users"
)

// The three Telegram account holders of this suite. Distinct from every other
// integration test's ids so the suites never collide in the shared database.
const (
	tenantATelegramID int64 = 94101
	tenantBTelegramID int64 = 94102
	tenantCTelegramID int64 = 94103
)

// rlsTenantTables are the five tables under FORCE ROW LEVEL SECURITY. The
// isolation subtests below walk all of them rather than a representative one:
// a policy is per table, and the table that gets forgotten is the one that
// leaks.
var rlsTenantTables = []string{
	"messages",
	"chats",
	"notification_outbox",
	"media_files",
	"data_erasure_requests",
}

// TestPostgreSQL16MultiTenantLifecycleAndIsolation is the acceptance test of
// issue #17, on a real PostgreSQL 16 with THREE tenants.
//
// Three is the smallest number that catches the isolation bug two tenants
// hide: a policy that leaks "everything except my own rows" reads as correct
// with two tenants seeded symmetrically, and a purge or a count that loops over
// "the other tenant" instead of "this tenant" passes just as well. With three,
// every cross-read has two wrong answers available and both are checked.
func TestPostgreSQL16MultiTenantLifecycleAndIsolation(t *testing.T) {
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
	db, err := storage.NewPool(ctx, runtimeDSN)
	if err != nil {
		t.Fatalf("open runtime pool: %v", err)
	}
	defer db.Close()

	t.Cleanup(func() {
		// ON DELETE CASCADE from users clears every tenant table at once.
		_, _ = admin.Exec(context.Background(),
			`DELETE FROM users WHERE telegram_user_id IN ($1, $2, $3)`,
			tenantATelegramID, tenantBTelegramID, tenantCTelegramID)
	})

	// The scripted Bot API answers getBusinessConnection for the ids it knows
	// and refuses the rest with a 400 -- which is exactly how Telegram reports
	// a connection the holder has removed.
	api := &scriptedBotAPI{t: t, conn: map[string]any{
		"bc-mt-a1": businessConn("bc-mt-a1", tenantATelegramID, true),
	}}
	server := httptest.NewServer(http.HandlerFunc(api.handler))
	defer server.Close()
	newClient := func() *telegram.Client {
		return telegram.NewClient("test-token", 5*time.Second, telegram.WithBaseURL(server.URL+"/bot"))
	}
	userRepo := users.NewRepository(db.Pool)
	// Open onboarding: an empty allowlist is the multi-tenant default this
	// issue introduces. Every refusal proved below therefore comes from the
	// schema or from the connection lifecycle, never from an admission filter.
	newService := func(opts ...business.Option) *business.Service {
		return business.NewService(db.Pool, newClient(), userRepo, nil, logger, opts...)
	}

	messagesRepo := messages.NewRepository(db)
	mediaRepo := media.NewRepository(db)
	challenges := erasure.NewRepository(db)

	// --- Onboarding: three holders, four connections ------------------------
	//
	// Tenant B deliberately holds TWO connections: an account holder that
	// reconnects (or connects from a second Business account) gets a new
	// connection id, and both must resolve to the same tenant. If they did not,
	// half their messages would sit in a tenant nothing ever erases.
	svc := newService()
	onboarding := []struct {
		connection string
		telegramID int64
	}{
		{connection: "bc-mt-a1", telegramID: tenantATelegramID},
		{connection: "bc-mt-b1", telegramID: tenantBTelegramID},
		{connection: "bc-mt-b2", telegramID: tenantBTelegramID},
		{connection: "bc-mt-c1", telegramID: tenantCTelegramID},
	}
	for _, step := range onboarding {
		if err := svc.HandleBusinessConnection(ctx, telegramConnection(step.connection, step.telegramID, true)); err != nil {
			t.Fatalf("onboarding %s: %v", step.connection, err)
		}
	}

	owners := map[int64]int64{} // telegram user id -> internal owner_user_id
	for _, telegramID := range []int64{tenantATelegramID, tenantBTelegramID, tenantCTelegramID} {
		u, err := userRepo.UpsertByTelegramID(ctx, telegramID)
		if err != nil {
			t.Fatalf("read tenant %d: %v", telegramID, err)
		}
		owners[telegramID] = u.ID
	}
	ownerA, ownerB, ownerC := owners[tenantATelegramID], owners[tenantBTelegramID], owners[tenantCTelegramID]
	if ownerA == ownerB || ownerB == ownerC || ownerA == ownerC {
		t.Fatalf("three holders collapsed into fewer tenants: %d %d %d", ownerA, ownerB, ownerC)
	}

	t.Run("several connections of one holder resolve to one tenant", func(t *testing.T) {
		ctx := phaseContext(t)
		for _, connection := range []string{"bc-mt-b1", "bc-mt-b2"} {
			conn, err := svc.Resolve(ctx, connection)
			if err != nil {
				t.Fatalf("Resolve(%s): %v", connection, err)
			}
			if conn.OwnerUserID != ownerB || conn.OwnerTelegramUserID != tenantBTelegramID {
				t.Fatalf("%s resolved to owner %d/%d, want %d/%d",
					connection, conn.OwnerUserID, conn.OwnerTelegramUserID, ownerB, tenantBTelegramID)
			}
		}
		var connections int
		if err := admin.QueryRow(ctx,
			`SELECT count(*) FROM business_connections WHERE owner_user_id = $1`, ownerB).Scan(&connections); err != nil {
			t.Fatalf("count connections of tenant B: %v", err)
		}
		if connections != 2 {
			t.Fatalf("tenant B holds %d connections, want 2", connections)
		}
	})

	// --- Seed: every tenant gets a full set of rows in every RLS table ------
	seeded := map[int64]int{}
	for _, tenant := range []struct {
		ownerUserID int64
		telegramID  int64
		connection  string
		messages    int
	}{
		{ownerUserID: ownerA, telegramID: tenantATelegramID, connection: "bc-mt-a1", messages: 2},
		{ownerUserID: ownerB, telegramID: tenantBTelegramID, connection: "bc-mt-b1", messages: 3},
		{ownerUserID: ownerC, telegramID: tenantCTelegramID, connection: "bc-mt-c1", messages: 4},
	} {
		seedTenant(ctx, t, messagesRepo, mediaRepo, challenges, tenant.ownerUserID, tenant.telegramID, tenant.connection, tenant.messages)
		seeded[tenant.ownerUserID] = tenant.messages
	}

	t.Run("a tenant context reads its own rows and nothing else", func(t *testing.T) {
		ctx := phaseContext(t)
		for _, table := range rlsTenantTables {
			for _, ownerUserID := range []int64{ownerA, ownerB, ownerC} {
				// Count WITHOUT a WHERE clause: the policy is the only thing
				// standing between this query and the other two tenants' rows.
				visible := countInTenant(ctx, t, db, ownerUserID, table)
				own := countAsAdmin(ctx, t, admin, table, ownerUserID)
				if own == 0 {
					t.Fatalf("%s: tenant %d was seeded with no row, the isolation check would be vacuous", table, ownerUserID)
				}
				if visible != own {
					t.Fatalf("%s: tenant %d sees %d rows but owns %d -- the policy leaks or hides", table, ownerUserID, visible, own)
				}
			}
		}
	})

	t.Run("a tenant cannot read a named row of another tenant", func(t *testing.T) {
		ctx := phaseContext(t)
		// The sharper form of the check above: ask explicitly for the other
		// tenants' rows by owner_user_id. A policy that filtered on something
		// else than the context would answer this one.
		for _, table := range rlsTenantTables {
			for _, pair := range crossTenantPairs(ownerA, ownerB, ownerC) {
				var seen int
				err := db.InTenant(ctx, pair.reader, func(tx pgx.Tx) error {
					return tx.QueryRow(ctx,
						fmt.Sprintf(`SELECT count(*) FROM %s WHERE owner_user_id = $1`, table), pair.target).Scan(&seen)
				})
				if err != nil {
					t.Fatalf("%s: cross read %d -> %d: %v", table, pair.reader, pair.target, err)
				}
				if seen != 0 {
					t.Fatalf("%s: tenant %d sees %d rows of tenant %d", table, pair.reader, seen, pair.target)
				}
			}
		}
	})

	t.Run("a tenant cannot write, update or delete another tenant's rows", func(t *testing.T) {
		ctx := phaseContext(t)
		for _, pair := range crossTenantPairs(ownerA, ownerB, ownerC) {
			// INSERT: refused by WITH CHECK, loudly (a policy violation error),
			// not silently ignored.
			err := db.InTenant(ctx, pair.reader, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `
					INSERT INTO messages (owner_user_id, business_connection_id, chat_id, message_id, message_type, telegram_date)
					VALUES ($1, 'bc-forged', 42, 4242, 'text', 1788019201)
				`, pair.target)
				return err
			})
			if err == nil {
				t.Fatalf("tenant %d inserted a row owned by tenant %d", pair.reader, pair.target)
			}

			// UPDATE and DELETE: refused by USING, which makes the other
			// tenant's rows invisible -- so they match nothing and the
			// statement reports zero rows rather than failing.
			for _, statement := range []string{
				`UPDATE messages SET text_content = 'tampered' WHERE owner_user_id = $1`,
				`DELETE FROM messages WHERE owner_user_id = $1`,
			} {
				var affected int64
				err := db.InTenant(ctx, pair.reader, func(tx pgx.Tx) error {
					tag, err := tx.Exec(ctx, statement, pair.target)
					affected = tag.RowsAffected()
					return err
				})
				if err != nil {
					t.Fatalf("cross statement %q (%d -> %d): %v", statement, pair.reader, pair.target, err)
				}
				if affected != 0 {
					t.Fatalf("tenant %d changed %d rows of tenant %d with %q", pair.reader, affected, pair.target, statement)
				}
			}
		}
		// Nothing above may have destroyed anything: re-check the seeded counts.
		for ownerUserID, want := range seeded {
			if got := countAsAdmin(ctx, t, admin, "messages", ownerUserID); got != want {
				t.Fatalf("tenant %d holds %d messages after the cross-tenant attempts, want %d", ownerUserID, got, want)
			}
		}
	})

	t.Run("an usurped connection is refused", func(t *testing.T) {
		ctx := phaseContext(t)
		// Tenant C claims tenant A's connection id. Telegram never reuses one
		// across holders, so this is either a contract we misread or an attempt
		// to have A's connection start feeding C's rows. Either way: refused,
		// and the stored row is left exactly as it was.
		err := svc.HandleBusinessConnection(ctx, telegramConnection("bc-mt-a1", tenantCTelegramID, true))
		if err == nil {
			t.Fatal("a connection takeover was accepted")
		}
		if !errors.Is(err, business.ErrConnectionOwnerConflict) {
			t.Fatalf("takeover error = %v, want ErrConnectionOwnerConflict", err)
		}

		var storedOwner int64
		if err := admin.QueryRow(ctx,
			`SELECT owner_user_id FROM business_connections WHERE id = 'bc-mt-a1'`).Scan(&storedOwner); err != nil {
			t.Fatalf("re-read the usurped connection: %v", err)
		}
		if storedOwner != ownerA {
			t.Fatalf("bc-mt-a1 now belongs to owner %d, want %d", storedOwner, ownerA)
		}

		// And a FRESH process (empty cache) must still resolve it to A.
		conn, err := newService().Resolve(ctx, "bc-mt-a1")
		if err != nil {
			t.Fatalf("Resolve after the refused takeover: %v", err)
		}
		if conn.OwnerUserID != ownerA {
			t.Fatalf("bc-mt-a1 resolves to owner %d, want %d", conn.OwnerUserID, ownerA)
		}
	})

	t.Run("deactivation and reactivation take effect immediately", func(t *testing.T) {
		ctx := phaseContext(t)
		if err := svc.HandleBusinessConnection(ctx, telegramConnection("bc-mt-c1", tenantCTelegramID, false)); err != nil {
			t.Fatalf("deactivation: %v", err)
		}
		conn, err := svc.Resolve(ctx, "bc-mt-c1")
		if err != nil {
			t.Fatalf("resolve after deactivation: %v", err)
		}
		if conn.IsEnabled {
			t.Fatal("a deactivated connection still resolves as enabled: capture would continue")
		}
		var enabled bool
		if err := admin.QueryRow(ctx, `SELECT is_enabled FROM business_connections WHERE id = 'bc-mt-c1'`).Scan(&enabled); err != nil {
			t.Fatalf("read is_enabled: %v", err)
		}
		if enabled {
			t.Fatal("the deactivation was not persisted")
		}
		// The other tenants are untouched by it.
		for _, connection := range []string{"bc-mt-a1", "bc-mt-b1", "bc-mt-b2"} {
			other, err := svc.Resolve(ctx, connection)
			if err != nil || !other.IsEnabled {
				t.Fatalf("%s disturbed by another tenant's deactivation: (%+v, %v)", connection, other, err)
			}
		}

		if err := svc.HandleBusinessConnection(ctx, telegramConnection("bc-mt-c1", tenantCTelegramID, true)); err != nil {
			t.Fatalf("reactivation: %v", err)
		}
		conn, err = svc.Resolve(ctx, "bc-mt-c1")
		if err != nil || !conn.IsEnabled {
			t.Fatalf("after reactivation: (%+v, %v), want an enabled connection", conn, err)
		}
	})

	t.Run("a revoked connection is refused and remembered", func(t *testing.T) {
		ctx := phaseContext(t)
		before := api.calls()
		fresh := newService()
		for attempt := 1; attempt <= 3; attempt++ {
			_, err := fresh.Resolve(ctx, "bc-mt-revoked")
			if err == nil || !errors.Is(err, business.ErrConnectionUnknown) {
				t.Fatalf("attempt %d: Resolve of a revoked connection = %v, want ErrConnectionUnknown", attempt, err)
			}
		}
		if calls := api.calls() - before; calls != 1 {
			t.Fatalf("getBusinessConnection called %d times for one revoked id, want 1", calls)
		}
		var rows int
		if err := admin.QueryRow(ctx,
			`SELECT count(*) FROM business_connections WHERE id = 'bc-mt-revoked'`).Scan(&rows); err != nil {
			t.Fatalf("count the revoked connection: %v", err)
		}
		if rows != 0 {
			t.Fatal("a connection Telegram does not recognise must not be persisted")
		}
	})

	t.Run("the cache follows the database across a restart and past its TTL", func(t *testing.T) {
		ctx := phaseContext(t)
		// Disabled out of band, as an operator would during an incident.
		if _, err := admin.Exec(ctx, `UPDATE business_connections SET is_enabled = false WHERE id = 'bc-mt-b2'`); err != nil {
			t.Fatalf("disable out of band: %v", err)
		}
		t.Cleanup(func() {
			_, _ = admin.Exec(context.Background(), `UPDATE business_connections SET is_enabled = true WHERE id = 'bc-mt-b2'`)
		})

		// A restarted process starts with an empty cache and must see it.
		restarted := newService()
		conn, err := restarted.Resolve(ctx, "bc-mt-b2")
		if err != nil {
			t.Fatalf("resolve after restart: %v", err)
		}
		if conn.IsEnabled {
			t.Fatal("a restarted process served a stale enabled connection")
		}

		// A RUNNING process that cached it as enabled must see it too, once the
		// entry ages past the TTL.
		clock := &steppingClock{now: time.Now()}
		running := newService(business.WithClock(clock.Now))
		if _, err := admin.Exec(ctx, `UPDATE business_connections SET is_enabled = true WHERE id = 'bc-mt-b2'`); err != nil {
			t.Fatalf("re-enable before the TTL check: %v", err)
		}
		if cached, err := running.Resolve(ctx, "bc-mt-b2"); err != nil || !cached.IsEnabled {
			t.Fatalf("prime the cache: (%+v, %v)", cached, err)
		}
		if _, err := admin.Exec(ctx, `UPDATE business_connections SET is_enabled = false WHERE id = 'bc-mt-b2'`); err != nil {
			t.Fatalf("disable behind the cache: %v", err)
		}
		if stale, err := running.Resolve(ctx, "bc-mt-b2"); err != nil || !stale.IsEnabled {
			t.Fatalf("within the TTL the cached value is expected: (%+v, %v)", stale, err)
		}
		clock.advance(2 * time.Minute)
		refreshed, err := running.Resolve(ctx, "bc-mt-b2")
		if err != nil {
			t.Fatalf("resolve past the TTL: %v", err)
		}
		if refreshed.IsEnabled {
			t.Fatal("the cache kept serving an enabled connection past its TTL, ignoring the database")
		}
	})

	t.Run("a backup taken with the owner role covers every tenant", func(t *testing.T) {
		ctx := phaseContext(t)
		// scripts/backup.sh dumps with MIGRATION_DATABASE_URL, i.e. the owner
		// role, which bypasses RLS. That is precisely why the dump stays
		// exhaustive once the database holds several tenants -- and why a dump
		// taken with DATABASE_URL would silently produce an empty backup. Both
		// halves are asserted here, because the failure mode of the second one
		// is a backup that restores without error and contains nothing.
		for _, table := range rlsTenantTables {
			var total int
			if err := admin.QueryRow(ctx, fmt.Sprintf(
				`SELECT count(*) FROM %s WHERE owner_user_id IN ($1, $2, $3)`, table),
				ownerA, ownerB, ownerC).Scan(&total); err != nil {
				t.Fatalf("owner-role count of %s: %v", table, err)
			}
			sum := countAsAdmin(ctx, t, admin, table, ownerA) +
				countAsAdmin(ctx, t, admin, table, ownerB) +
				countAsAdmin(ctx, t, admin, table, ownerC)
			if total != sum || total == 0 {
				t.Fatalf("%s: the owner role sees %d of the %d seeded rows", table, total, sum)
			}

			var visibleWithoutContext int
			if err := db.Pool.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %s`, table)).Scan(&visibleWithoutContext); err != nil {
				t.Fatalf("app-role count of %s: %v", table, err)
			}
			if visibleWithoutContext != 0 {
				t.Fatalf("%s: the app role sees %d rows with no tenant context; a dump taken with DATABASE_URL would not be empty, it would be arbitrary",
					table, visibleWithoutContext)
			}
		}
	})
}

// telegramConnection builds the business_connection update Telegram would send.
func telegramConnection(id string, telegramUserID int64, enabled bool) telegram.BusinessConnection {
	return telegram.BusinessConnection{
		ID:         id,
		User:       telegram.User{ID: telegramUserID, FirstName: "Tenant"},
		UserChatID: telegramUserID,
		Date:       1788019201,
		IsEnabled:  enabled,
		Rights:     &telegram.BusinessBotRights{CanReply: true},
	}
}

// seedTenant gives one tenant a row in every RLS-protected table: messages (one
// of them deleted, which writes the chat label and the alert chunks), a
// catalogued attachment, and a pending erasure challenge.
func seedTenant(
	ctx context.Context,
	t *testing.T,
	messagesRepo *messages.Repository,
	mediaRepo *media.Repository,
	challenges *erasure.Repository,
	ownerUserID, telegramUserID int64,
	connection string,
	messageCount int,
) {
	t.Helper()

	for id := int64(1); id <= int64(messageCount); id++ {
		if err := messagesRepo.Save(ctx, ownerUserID, messages.Record{
			BusinessConnectionID: connection,
			ChatID:               770,
			MessageID:            id,
			FromDisplay:          "multi-tenant integration",
			MessageType:          "text",
			TextContent:          fmt.Sprintf("%s message %d", connection, id),
			TelegramDate:         1788019201,
			ChatTitle:            "chat " + connection,
			ChatType:             "private",
		}, false); err != nil {
			t.Fatalf("seed message %d of %s: %v", id, connection, err)
		}
	}
	if _, err := messagesRepo.MarkDeleted(ctx, ownerUserID, telegramUserID, connection, 770, []int64{1}); err != nil {
		t.Fatalf("seed outbox of %s: %v", connection, err)
	}
	if _, err := mediaRepo.Save(ctx, ownerUserID, media.Record{
		BusinessConnectionID: connection,
		ChatID:               770,
		MessageID:            1,
		TelegramFileID:       "file-handle-" + connection,
		TelegramFileUniqueID: "unique-" + connection,
		MediaType:            media.TypePhoto,
	}); err != nil {
		t.Fatalf("seed media of %s: %v", connection, err)
	}

	sum := sha256.Sum256([]byte(connection))
	if _, err := challenges.Issue(ctx, erasure.Tenant{
		OwnerUserID:          ownerUserID,
		OwnerTelegramUserID:  telegramUserID,
		BusinessConnectionID: connection,
	}, hex.EncodeToString(sum[:]), time.Hour); err != nil {
		t.Fatalf("seed erasure challenge of %s: %v", connection, err)
	}
}

// countInTenant counts a table from INSIDE one tenant's RLS context, with no
// WHERE clause: whatever comes back is exactly what that tenant can see.
func countInTenant(ctx context.Context, t *testing.T, db *storage.DB, ownerUserID int64, table string) int {
	t.Helper()
	var count int
	if err := db.InTenant(ctx, ownerUserID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %s`, table)).Scan(&count)
	}); err != nil {
		t.Fatalf("count %s in tenant %d: %v", table, ownerUserID, err)
	}
	return count
}

// countAsAdmin counts a tenant's rows through the owner role, which bypasses
// RLS: it is the independent reference the tenant-context counts are compared
// against.
func countAsAdmin(ctx context.Context, t *testing.T, admin *pgx.Conn, table string, ownerUserID int64) int {
	t.Helper()
	var count int
	if err := admin.QueryRow(ctx,
		fmt.Sprintf(`SELECT count(*) FROM %s WHERE owner_user_id = $1`, table), ownerUserID).Scan(&count); err != nil {
		t.Fatalf("owner-role count of %s for tenant %d: %v", table, ownerUserID, err)
	}
	return count
}

type tenantPair struct{ reader, target int64 }

// crossTenantPairs enumerates the six ordered pairs of three tenants: every
// tenant as a reader against both of the others.
func crossTenantPairs(owners ...int64) []tenantPair {
	var pairs []tenantPair
	for _, reader := range owners {
		for _, target := range owners {
			if reader != target {
				pairs = append(pairs, tenantPair{reader: reader, target: target})
			}
		}
	}
	return pairs
}

// steppingClock is a manual clock, so the TTL of the connection cache can be
// crossed without the test sleeping through it.
type steppingClock struct{ now time.Time }

func (c *steppingClock) Now() time.Time          { return c.now }
func (c *steppingClock) advance(d time.Duration) { c.now = c.now.Add(d) }
