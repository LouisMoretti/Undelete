package business

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/LouisMoretti/Undelete/bot/internal/telegram"
	"github.com/LouisMoretti/Undelete/bot/internal/users"
)

// fakeRow scans a scripted Connection (or error) into the five destinations
// of the resolution SELECT, in column order.
type fakeRow struct {
	conn Connection
	err  error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != 5 {
		return fmt.Errorf("fakeRow: %d destinations, want 5", len(dest))
	}
	id, ok1 := dest[0].(*string)
	owner, ok2 := dest[1].(*int64)
	ownerTg, ok3 := dest[2].(*int64)
	canReply, ok4 := dest[3].(*bool)
	enabled, ok5 := dest[4].(*bool)
	if !(ok1 && ok2 && ok3 && ok4 && ok5) {
		return fmt.Errorf("fakeRow: unexpected destination types %T", dest)
	}
	*id, *owner, *ownerTg, *canReply, *enabled =
		r.conn.ID, r.conn.OwnerUserID, r.conn.OwnerTelegramUserID, r.conn.CanReply, r.conn.IsEnabled
	return nil
}

// fakePool scripts the two statements the service issues: the resolution
// SELECT (queryConn/queryErr) and the writes (execTag/execErr). Any
// unexpected call fails the test, so a test that must not touch the database
// simply leaves the zero value.
type fakePool struct {
	t         *testing.T
	queryConn Connection
	queryErr  error
	queried   []string
	execTag   pgconn.CommandTag
	execErr   error
	executed  []string
}

func (p *fakePool) QueryRow(_ context.Context, _ string, args ...any) pgx.Row {
	if len(args) != 1 {
		p.t.Fatalf("QueryRow args = %v, want the connection id", args)
	}
	id, _ := args[0].(string)
	p.queried = append(p.queried, id)
	return fakeRow{conn: p.queryConn, err: p.queryErr}
}

func (p *fakePool) Exec(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
	p.executed = append(p.executed, "write")
	return p.execTag, p.execErr
}

type fakeUsers struct {
	nextID int64
	seen   []int64
}

func (u *fakeUsers) UpsertByTelegramID(_ context.Context, telegramUserID int64) (*users.User, error) {
	u.seen = append(u.seen, telegramUserID)
	return &users.User{ID: u.nextID, TelegramUserID: telegramUserID}, nil
}

type fakeAPI struct {
	conn     *telegram.BusinessConnection
	connErr  error
	requests []telegram.SendMessageRequest
	sendErr  error
}

func (a *fakeAPI) GetBusinessConnection(_ context.Context, id string) (*telegram.BusinessConnection, error) {
	if a.connErr != nil {
		return nil, a.connErr
	}
	if a.conn == nil {
		return nil, fmt.Errorf("fakeAPI: no connection scripted for %s", id)
	}
	return a.conn, nil
}

func (a *fakeAPI) SendMessage(_ context.Context, req telegram.SendMessageRequest) error {
	a.requests = append(a.requests, req)
	return a.sendErr
}

func testLogger() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

func apiConn(id string, userID int64, enabled bool) *telegram.BusinessConnection {
	return &telegram.BusinessConnection{
		ID: id, User: telegram.User{ID: userID}, IsEnabled: enabled,
		UserChatID: userID,
	}
}

// TestResolveServesCacheWithoutTouchingDBOrAPI pins level 1 of the chain: a
// cached connection resolves with no database or network traffic.
func TestResolveServesCacheWithoutTouchingDBOrAPI(t *testing.T) {
	pool := &fakePool{t: t, queryErr: errors.New("database must not be touched")}
	api := &fakeAPI{connErr: errors.New("API must not be touched")}
	svc := NewService(pool, api, &fakeUsers{}, 0, testLogger())
	svc.storeInCache(Connection{ID: "bc-cached", OwnerUserID: 7, OwnerTelegramUserID: 700, CanReply: true, IsEnabled: true})

	conn, err := svc.Resolve(context.Background(), "bc-cached")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if conn.ID != "bc-cached" || !conn.IsEnabled {
		t.Fatalf("unexpected connection: %+v", conn)
	}
	if len(pool.queried) != 0 {
		t.Fatalf("database queried for a cached connection: %v", pool.queried)
	}
}

// TestResolveFallsBackToDBThenCaches pins level 2: a database hit is stored
// in cache, so the second resolution costs nothing.
func TestResolveFallsBackToDBThenCaches(t *testing.T) {
	stored := Connection{ID: "bc-db", OwnerUserID: 7, OwnerTelegramUserID: 700, CanReply: true, IsEnabled: true}
	pool := &fakePool{t: t, queryConn: stored}
	api := &fakeAPI{connErr: errors.New("API must not be touched")}
	svc := NewService(pool, api, &fakeUsers{}, 0, testLogger())

	first, err := svc.Resolve(context.Background(), "bc-db")
	if err != nil || first.OwnerUserID != 7 {
		t.Fatalf("Resolve = (%+v, %v)", first, err)
	}
	second, err := svc.Resolve(context.Background(), "bc-db")
	if err != nil {
		t.Fatalf("second Resolve: %v", err)
	}
	if len(pool.queried) != 1 {
		t.Fatalf("database queried %d times, want 1 (second hit must come from cache)", len(pool.queried))
	}
	_ = second
}

// TestResolveFallsBackToAPIUpsertsAndCaches pins level 3: a connection the
// database never saw is resolved via getBusinessConnection, upserted, and
// cached. A restart that lost the cache but kept the database would find it
// at level 2 next time.
func TestResolveFallsBackToAPIUpsertsAndCaches(t *testing.T) {
	pool := &fakePool{t: t, queryErr: pgx.ErrNoRows, execTag: pgconn.NewCommandTag("INSERT 0 1")}
	people := &fakeUsers{nextID: 7}
	api := &fakeAPI{conn: apiConn("bc-new", 700, true)}
	svc := NewService(pool, api, people, 0, testLogger())

	conn, err := svc.Resolve(context.Background(), "bc-new")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if conn.OwnerUserID != 7 || conn.OwnerTelegramUserID != 700 || !conn.IsEnabled {
		t.Fatalf("unexpected resolved connection: %+v", conn)
	}
	if len(people.seen) != 1 || people.seen[0] != 700 {
		t.Fatalf("owner upserted as %v, want [700]", people.seen)
	}
	if len(pool.executed) != 1 {
		t.Fatalf("connection upsert executed %d times, want 1", len(pool.executed))
	}
	// Second resolution must come from the cache: no more queries, no API.
	pool.queryErr = errors.New("database must not be touched again")
	api.connErr = errors.New("API must not be touched again")
	if _, err := svc.Resolve(context.Background(), "bc-new"); err != nil {
		t.Fatalf("cached Resolve after API fallback: %v", err)
	}
}

// TestMonoTenantGuardAppliesAtEveryLevel pins the filter on all three
// resolution paths: cache, database and API. A foreign connection created
// before the guardrail was enabled must not become authorized after a
// restart.
func TestMonoTenantGuardAppliesAtEveryLevel(t *testing.T) {
	foreign := Connection{ID: "bc-foreign", OwnerUserID: 9, OwnerTelegramUserID: 900, IsEnabled: true}

	t.Run("cache", func(t *testing.T) {
		pool := &fakePool{t: t, queryErr: errors.New("database must not be touched")}
		svc := NewService(pool, &fakeAPI{}, &fakeUsers{}, 700, testLogger())
		svc.storeInCache(foreign)
		if _, err := svc.Resolve(context.Background(), "bc-foreign"); !errors.Is(err, ErrOwnerMismatch) {
			t.Fatalf("cached foreign Resolve = %v, want ErrOwnerMismatch", err)
		}
	})

	t.Run("database", func(t *testing.T) {
		pool := &fakePool{t: t, queryConn: foreign}
		svc := NewService(pool, &fakeAPI{connErr: errors.New("API must not be touched")}, &fakeUsers{}, 700, testLogger())
		if _, err := svc.Resolve(context.Background(), "bc-foreign"); !errors.Is(err, ErrOwnerMismatch) {
			t.Fatalf("database foreign Resolve = %v, want ErrOwnerMismatch", err)
		}
		if len(pool.executed) != 0 {
			t.Fatal("a refused connection must not be upserted")
		}
	})

	t.Run("api", func(t *testing.T) {
		pool := &fakePool{t: t, queryErr: pgx.ErrNoRows}
		people := &fakeUsers{nextID: 9}
		api := &fakeAPI{conn: apiConn("bc-foreign", 900, true)}
		svc := NewService(pool, api, people, 700, testLogger())
		if _, err := svc.Resolve(context.Background(), "bc-foreign"); !errors.Is(err, ErrOwnerMismatch) {
			t.Fatalf("API foreign Resolve = %v, want ErrOwnerMismatch", err)
		}
		if len(people.seen) != 0 || len(pool.executed) != 0 {
			t.Fatal("a refused connection must create neither user nor connection row")
		}
	})
}

// TestServiceSurfacesDependencyFailures pins the error paths the happy-path
// tests above do not: a database read/write failure aborts resolution, and a
// welcome failure is logged without failing the connection that is already
// persisted (losing a welcome must not replay the update).
func TestServiceSurfacesDependencyFailures(t *testing.T) {
	t.Run("database read error aborts resolution", func(t *testing.T) {
		pool := &fakePool{t: t, queryErr: errors.New("connection reset")}
		api := &fakeAPI{connErr: errors.New("API must not be reached")}
		svc := NewService(pool, api, &fakeUsers{}, 0, testLogger())
		if _, err := svc.Resolve(context.Background(), "bc-x"); err == nil {
			t.Fatal("Resolve over a broken database = nil, want error")
		}
	})

	t.Run("database write error aborts API fallback", func(t *testing.T) {
		pool := &fakePool{t: t, queryErr: pgx.ErrNoRows, execErr: errors.New("disk full")}
		api := &fakeAPI{conn: apiConn("bc-new", 700, true)}
		svc := NewService(pool, api, &fakeUsers{nextID: 7}, 0, testLogger())
		if _, err := svc.Resolve(context.Background(), "bc-new"); err == nil {
			t.Fatal("Resolve with a failing upsert = nil, want error")
		}
	})

	t.Run("welcome failure does not fail the connection", func(t *testing.T) {
		pool := &fakePool{t: t, execTag: pgconn.NewCommandTag("INSERT 0 1")}
		api := &fakeAPI{sendErr: errors.New("chat not found")}
		svc := NewService(pool, api, &fakeUsers{nextID: 7}, 0, testLogger())
		if err := svc.HandleBusinessConnection(context.Background(), *apiConn("bc-w", 700, true)); err != nil {
			t.Fatalf("a lost welcome must not fail processing, got %v", err)
		}
	})
}

// TestDisableOwnerDisablesCache pins the erasure-critical half of
// DisableOwner: Resolve answers from cache first, so the cache must agree
// with the UPDATE, entry by entry, while other owners are untouched.
func TestDisableOwnerDisablesCache(t *testing.T) {
	pool := &fakePool{t: t, execTag: pgconn.NewCommandTag("UPDATE 2")}
	svc := NewService(pool, &fakeAPI{}, &fakeUsers{}, 0, testLogger())
	svc.storeInCache(Connection{ID: "bc-a1", OwnerUserID: 7, OwnerTelegramUserID: 700, IsEnabled: true})
	svc.storeInCache(Connection{ID: "bc-a2", OwnerUserID: 7, OwnerTelegramUserID: 700, IsEnabled: true})
	svc.storeInCache(Connection{ID: "bc-b1", OwnerUserID: 8, OwnerTelegramUserID: 800, IsEnabled: true})

	changed, err := svc.DisableOwner(context.Background(), 7)
	if err != nil || changed != 2 {
		t.Fatalf("DisableOwner = (%d, %v), want (2, nil)", changed, err)
	}
	for _, id := range []string{"bc-a1", "bc-a2"} {
		conn, err := svc.Resolve(context.Background(), id)
		if err != nil {
			t.Fatalf("Resolve(%s): %v", id, err)
		}
		if conn.IsEnabled {
			t.Fatalf("Resolve(%s) still enabled after DisableOwner", id)
		}
	}
	other, err := svc.Resolve(context.Background(), "bc-b1")
	if err != nil || !other.IsEnabled {
		t.Fatalf("other owner's connection disturbed: (%+v, %v)", other, err)
	}
}

// TestHandleBusinessConnectionWelcomesOnlyWhenEnabled pins the welcome
// discipline: an enabled connection gets exactly one welcome as a direct
// message (never via a business connection), a disabled one gets none, and a
// refused one is a silent nil.
func TestHandleBusinessConnectionWelcomesOnlyWhenEnabled(t *testing.T) {
	t.Run("enabled welcomes once", func(t *testing.T) {
		pool := &fakePool{t: t, execTag: pgconn.NewCommandTag("INSERT 0 1")}
		api := &fakeAPI{}
		svc := NewService(pool, api, &fakeUsers{nextID: 7}, 0, testLogger())

		if err := svc.HandleBusinessConnection(context.Background(), *apiConn("bc-w", 700, true)); err != nil {
			t.Fatalf("HandleBusinessConnection: %v", err)
		}
		if len(api.requests) != 1 {
			t.Fatalf("welcome sent %d times, want 1", len(api.requests))
		}
		if got := api.requests[0].ChatID; got != 700 {
			t.Fatalf("welcome ChatID = %d, want the owner's user id 700", got)
		}
	})

	t.Run("disabled sends nothing", func(t *testing.T) {
		pool := &fakePool{t: t, execTag: pgconn.NewCommandTag("INSERT 0 1")}
		api := &fakeAPI{}
		svc := NewService(pool, api, &fakeUsers{nextID: 7}, 0, testLogger())

		if err := svc.HandleBusinessConnection(context.Background(), *apiConn("bc-d", 700, false)); err != nil {
			t.Fatalf("HandleBusinessConnection: %v", err)
		}
		if len(api.requests) != 0 {
			t.Fatalf("disabled connection welcomed %d times, want 0", len(api.requests))
		}
	})

	t.Run("refused is silent", func(t *testing.T) {
		pool := &fakePool{t: t}
		api := &fakeAPI{}
		svc := NewService(pool, api, &fakeUsers{}, 700, testLogger())

		if err := svc.HandleBusinessConnection(context.Background(), *apiConn("bc-x", 900, true)); err != nil {
			t.Fatalf("refused connection must be a silent nil, got %v", err)
		}
		if len(api.requests) != 0 || len(pool.executed) != 0 {
			t.Fatal("refused connection must send nothing and store nothing")
		}
	})
}
