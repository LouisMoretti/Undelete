package business

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/LouisMoretti/Undelete/bot/internal/telegram"
	"github.com/LouisMoretti/Undelete/bot/internal/users"
)

// This file pins the SECOND negative memo of the resolution chain: a connection
// whose account holder is not in the onboarding allowlist.
//
// The revocation memo (Telegram no longer knows the id) was already there; its
// sibling was not, so under a restricted allowlist every single update from an
// unadmitted holder re-ran the whole chain -- one business_connections read and
// one getBusinessConnection, forever, on the sequential poller goroutine and on
// the bot's shared Telegram rate budget. The refusal is a decision about the
// holder, not a transient failure, so it is memoised exactly like the
// revocation: bounded, expiring, and re-checked against the allowlist rather
// than taken on trust.

// TestAllowlistRefusalIsMemoisedOnTheAPIPath is the flooding case: a stranger
// connects the bot to their own Business account and starts typing. Their
// connection is in neither the cache nor the database, so every update used to
// cost one getBusinessConnection; it must cost exactly one in total.
func TestAllowlistRefusalIsMemoisedOnTheAPIPath(t *testing.T) {
	pool := &fakePool{t: t, queryErr: pgx.ErrNoRows}
	people := &fakeUsers{nextID: 9}
	api := &fakeAPI{conn: apiConn("bc-stranger", 900, true)}
	svc := NewService(pool, api, people, []int64{700}, testLogger())

	for attempt := 1; attempt <= 50; attempt++ {
		if _, err := svc.Resolve(context.Background(), "bc-stranger"); !errors.Is(err, ErrOwnerNotAllowed) {
			t.Fatalf("attempt %d: Resolve = %v, want ErrOwnerNotAllowed", attempt, err)
		}
	}

	if api.connCalls != 1 {
		t.Fatalf("getBusinessConnection called %d times, want 1 (the refusal must be memoised)", api.connCalls)
	}
	if len(pool.queried) != 1 {
		t.Fatalf("business_connections read %d times, want 1", len(pool.queried))
	}
	if len(people.seen) != 0 || len(pool.executed) != 0 {
		t.Fatal("a refused holder must create neither a users row nor a business_connections row")
	}
}

// TestAllowlistRefusalIsMemoisedOnTheDatabasePath is the historical-row case:
// the connection was persisted while onboarding was open and the deployment has
// since restricted it. The refusal is the same decision and must cost the same
// single database read.
func TestAllowlistRefusalIsMemoisedOnTheDatabasePath(t *testing.T) {
	stored := Connection{ID: "bc-historical", OwnerUserID: 9, OwnerTelegramUserID: 900, IsEnabled: true}
	pool := &fakePool{t: t, queryConn: stored}
	api := &fakeAPI{connErr: errors.New("API must not be touched")}
	svc := NewService(pool, api, &fakeUsers{}, []int64{700}, testLogger())

	for attempt := 1; attempt <= 50; attempt++ {
		if _, err := svc.Resolve(context.Background(), "bc-historical"); !errors.Is(err, ErrOwnerNotAllowed) {
			t.Fatalf("attempt %d: Resolve = %v, want ErrOwnerNotAllowed", attempt, err)
		}
	}

	if len(pool.queried) != 1 {
		t.Fatalf("business_connections read %d times, want 1 (the refusal must be memoised)", len(pool.queried))
	}
	if api.connCalls != 0 {
		t.Fatal("a connection the database knows must never reach the Telegram API")
	}
}

// TestAllowlistRefusalExpiresWithTheTTL pins that the memo is a cache entry and
// nothing more: it ages out like every other one, so the refusal is re-decided
// against the database (or the API) at most one TTL later and can never become
// a permanent verdict held in memory.
//
// Within one process the allowlist itself cannot change -- config.Load() reads
// OWNER_ALLOWLIST_TELEGRAM_USER_IDS once at boot and there is no reload -- so
// admitting a newly listed holder goes through a restart, which drops the whole
// cache. What the TTL guarantees is the other half: nothing about the holder is
// remembered longer than a resolution would have been.
func TestAllowlistRefusalExpiresWithTheTTL(t *testing.T) {
	clock := newTestClock()
	pool := &fakePool{t: t, queryErr: pgx.ErrNoRows}
	api := &fakeAPI{conn: apiConn("bc-stranger", 900, true)}
	svc := NewService(pool, api, &fakeUsers{nextID: 9}, []int64{700}, testLogger(), WithClock(clock.Now))

	for i := 0; i < 10; i++ {
		if _, err := svc.Resolve(context.Background(), "bc-stranger"); !errors.Is(err, ErrOwnerNotAllowed) {
			t.Fatalf("Resolve = %v, want ErrOwnerNotAllowed", err)
		}
		clock.advance(time.Second)
	}
	if api.connCalls != 1 {
		t.Fatalf("getBusinessConnection called %d times within the TTL, want 1", api.connCalls)
	}

	clock.advance(connectionCacheTTL)
	if _, err := svc.Resolve(context.Background(), "bc-stranger"); !errors.Is(err, ErrOwnerNotAllowed) {
		t.Fatalf("Resolve after the TTL = %v, want ErrOwnerNotAllowed", err)
	}
	if api.connCalls != 2 {
		t.Fatalf("getBusinessConnection called %d times across the TTL, want 2 (the memo must age out)", api.connCalls)
	}
}

// TestAdmittedLifecycleUpdateReplacesTheRefusalMemo pins that the memo never
// shadows the authoritative update: a business_connection update that IS
// admitted overwrites the entry in place, so the connection is live at the very
// next resolution instead of waiting out the TTL.
func TestAdmittedLifecycleUpdateReplacesTheRefusalMemo(t *testing.T) {
	pool := &fakePool{t: t, queryErr: pgx.ErrNoRows, execTag: pgconn.NewCommandTag("INSERT 0 1")}
	api := &fakeAPI{conn: apiConn("bc-shared", 900, true)}
	svc := NewService(pool, api, &fakeUsers{nextID: 7}, []int64{700}, testLogger())
	ctx := context.Background()

	if _, err := svc.Resolve(ctx, "bc-shared"); !errors.Is(err, ErrOwnerNotAllowed) {
		t.Fatalf("Resolve = %v, want ErrOwnerNotAllowed", err)
	}

	// The same id now arrives through a business_connection update carried by an
	// admitted holder. (Telegram does not reuse an id across holders; this is the
	// defence-in-depth half: the memo is an ordinary entry, replaced by the
	// lifecycle, never a verdict that outlives it.)
	if err := svc.HandleBusinessConnection(ctx, *apiConn("bc-shared", 700, true)); err != nil {
		t.Fatalf("HandleBusinessConnection: %v", err)
	}

	conn, err := svc.Resolve(ctx, "bc-shared")
	if err != nil {
		t.Fatalf("Resolve after the admitted update: %v", err)
	}
	if conn.OwnerTelegramUserID != 700 || !conn.IsEnabled {
		t.Fatalf("resolved %+v, want the admitted holder 700, enabled", conn)
	}
}

// TestRefusalMemosAreBoundedLikeAnyOtherEntry pins the memory half. Open
// onboarding already means the id space is driven by strangers; memoising
// refusals must not turn that into an unbounded map keyed by the very ids the
// deployment refuses.
func TestRefusalMemosAreBoundedLikeAnyOtherEntry(t *testing.T) {
	const max = 4
	clock := newTestClock()
	pool := &fakePool{t: t, queryErr: pgx.ErrNoRows}
	api := &fakeAPI{}
	svc := NewService(pool, api, &fakeUsers{nextID: 9}, []int64{700}, testLogger(), WithClock(clock.Now))
	// A cache bound of four instead of the production 4096, so the eviction is
	// reachable in a test.
	svc.cache = newConnectionCache(connectionCacheTTL, max, clock.Now)

	for i := 0; i < 10*max; i++ {
		id := fmt.Sprintf("bc-stranger-%d", i)
		api.conn = apiConn(id, int64(900+i), true)
		if _, err := svc.Resolve(context.Background(), id); !errors.Is(err, ErrOwnerNotAllowed) {
			t.Fatalf("Resolve(%s) = %v, want ErrOwnerNotAllowed", id, err)
		}
	}

	if got := svc.cache.len(); got != max {
		t.Fatalf("cache holds %d entries after %d refused ids, want the bound %d", got, 10*max, max)
	}
}

// TestRefusalMemoIsScopedToItsConnection pins the isolation half: remembering
// one holder's refusal must not disturb an admitted tenant's entries, and the
// erasure path must walk past a refusal memo without mistaking it for one of
// that tenant's connections.
func TestRefusalMemoIsScopedToItsConnection(t *testing.T) {
	admitted := Connection{ID: "bc-tenant", OwnerUserID: 7, OwnerTelegramUserID: 700, IsEnabled: true}
	pool := &fakePool{t: t, queryConn: admitted, execTag: pgconn.NewCommandTag("UPDATE 1")}
	api := &fakeAPI{connErr: errors.New("API must not be touched")}
	svc := NewService(pool, api, &fakeUsers{}, []int64{700}, testLogger())
	ctx := context.Background()

	// The admitted tenant resolves and is cached.
	if conn, err := svc.Resolve(ctx, "bc-tenant"); err != nil || !conn.IsEnabled {
		t.Fatalf("admitted Resolve = (%+v, %v)", conn, err)
	}

	// A refused holder, resolved from the same table.
	pool.queryConn = Connection{ID: "bc-stranger", OwnerUserID: 9, OwnerTelegramUserID: 900, IsEnabled: true}
	for i := 0; i < 5; i++ {
		if _, err := svc.Resolve(ctx, "bc-stranger"); !errors.Is(err, ErrOwnerNotAllowed) {
			t.Fatalf("refused Resolve = %v, want ErrOwnerNotAllowed", err)
		}
	}
	queriesAfterRefusals := len(pool.queried)
	if queriesAfterRefusals != 2 {
		t.Fatalf("business_connections read %d times, want 2 (one per connection)", queriesAfterRefusals)
	}

	// The admitted tenant is still served from cache, unchanged.
	conn, err := svc.Resolve(ctx, "bc-tenant")
	if err != nil || conn.OwnerUserID != 7 || !conn.IsEnabled {
		t.Fatalf("admitted connection disturbed by a refusal: (%+v, %v)", conn, err)
	}
	if len(pool.queried) != queriesAfterRefusals {
		t.Fatal("the admitted connection was re-read from the database: its cache entry was disturbed")
	}

	// An erasure disables that tenant. The refusal memo is not one of its
	// connections and must survive untouched -- still refusing, still memoised.
	if _, err := svc.DisableOwner(ctx, 7); err != nil {
		t.Fatalf("DisableOwner: %v", err)
	}
	if conn, err := svc.Resolve(ctx, "bc-tenant"); err != nil || conn.IsEnabled {
		t.Fatalf("after DisableOwner: (%+v, %v), want a disabled connection", conn, err)
	}
	if _, err := svc.Resolve(ctx, "bc-stranger"); !errors.Is(err, ErrOwnerNotAllowed) {
		t.Fatalf("refusal after DisableOwner = %v, want ErrOwnerNotAllowed", err)
	}
	if len(pool.queried) != queriesAfterRefusals {
		t.Fatalf("business_connections read %d times, want %d: DisableOwner must not evict the refusal memo",
			len(pool.queried), queriesAfterRefusals)
	}
}

// TestOwnerTakeoverIsNotMemoisedAsARefusal keeps the two refusals apart. A
// takeover is refused because the connection belongs to ANOTHER owner, which
// says nothing about the claimant's admission: memoising it would make the
// cache answer the next resolution -- the legitimate owner's -- with the wrong
// refusal, and the real row would never be read again.
func TestOwnerTakeoverIsNotMemoisedAsARefusal(t *testing.T) {
	// The upsert answers no row: what the WHERE on the ON CONFLICT clause
	// produces when the stored owner differs from the claimed one.
	pool := &fakePool{t: t, queryErr: pgx.ErrNoRows, upsertErr: pgx.ErrNoRows}
	svc := NewService(pool, &fakeAPI{}, &fakeUsers{nextID: 9}, nil, testLogger())
	ctx := context.Background()

	err := svc.HandleBusinessConnection(ctx, *apiConn("bc-stolen", 900, true))
	if !errors.Is(err, ErrConnectionOwnerConflict) {
		t.Fatalf("HandleBusinessConnection = %v, want ErrConnectionOwnerConflict", err)
	}
	if _, cached := svc.cache.lookup("bc-stolen"); cached {
		t.Fatal("a refused takeover must not be cached at all: the next resolution must read the real owner")
	}

	// And the next resolution does read it, and gets the legitimate owner.
	pool.queryErr = nil
	pool.queryConn = Connection{ID: "bc-stolen", OwnerUserID: 7, OwnerTelegramUserID: 700, IsEnabled: true}
	conn, err := svc.Resolve(ctx, "bc-stolen")
	if err != nil {
		t.Fatalf("Resolve after a refused takeover: %v", err)
	}
	if conn.OwnerUserID != 7 || conn.OwnerTelegramUserID != 700 {
		t.Fatalf("resolved %+v, want the legitimate owner 7/700", conn)
	}
}

// TestTransientFailureIsNotMemoisedAsARefusal pins the line between "this
// holder is not admitted" and "the lookup failed". A database outage or a 5xx
// says nothing about admission; memoising it would refuse an admitted tenant
// for a whole TTL because of one failed read.
func TestTransientFailureIsNotMemoisedAsARefusal(t *testing.T) {
	t.Run("database failure", func(t *testing.T) {
		pool := &fakePool{t: t, queryErr: errors.New("connection reset by peer")}
		api := &fakeAPI{connErr: errors.New("API must not be touched")}
		svc := NewService(pool, api, &fakeUsers{}, []int64{700}, testLogger())

		for attempt := 1; attempt <= 3; attempt++ {
			_, err := svc.Resolve(context.Background(), "bc-flaky")
			if err == nil || errors.Is(err, ErrOwnerNotAllowed) || errors.Is(err, ErrConnectionUnknown) {
				t.Fatalf("attempt %d: Resolve = %v, want a plain error (never a refusal)", attempt, err)
			}
		}
		if len(pool.queried) != 3 {
			t.Fatalf("business_connections read %d times, want 3 (a failure must be retried, not memoised)", len(pool.queried))
		}
	})

	t.Run("api failure", func(t *testing.T) {
		pool := &fakePool{t: t, queryErr: pgx.ErrNoRows}
		api := &fakeAPI{connErr: &telegram.APIError{Method: "getBusinessConnection", Code: http.StatusBadGateway}}
		svc := NewService(pool, api, &fakeUsers{}, []int64{700}, testLogger())

		for attempt := 1; attempt <= 3; attempt++ {
			_, err := svc.Resolve(context.Background(), "bc-flaky")
			if err == nil || errors.Is(err, ErrOwnerNotAllowed) || errors.Is(err, ErrConnectionUnknown) {
				t.Fatalf("attempt %d: Resolve = %v, want a plain error (never a refusal)", attempt, err)
			}
		}
		if api.connCalls != 3 {
			t.Fatalf("getBusinessConnection called %d times, want 3 (a failure must be retried, not memoised)", api.connCalls)
		}
	})
}

// TestRefusalMemoUnderConcurrentResolves hammers the memo from several
// goroutines. Production resolves on the poller goroutine alone, so this is
// defence in depth for the cache mutex: under -race, a concurrent burst must
// stay correct (every answer a refusal), must not cost more than one API call
// per goroutine, and must leave the id memoised afterwards.
func TestRefusalMemoUnderConcurrentResolves(t *testing.T) {
	const goroutines = 8
	const perGoroutine = 100

	pool := &lockedPool{rows: map[string]Connection{}}
	api := &lockedAPI{conn: apiConn("bc-stranger", 900, true)}
	svc := NewService(pool, api, &lockedUsers{}, []int64{700}, testLogger())

	var wg sync.WaitGroup
	errs := make(chan error, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				if _, err := svc.Resolve(context.Background(), "bc-stranger"); !errors.Is(err, ErrOwnerNotAllowed) {
					errs <- fmt.Errorf("Resolve = %v, want ErrOwnerNotAllowed", err)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	calls := api.calls()
	if calls == 0 || calls > goroutines {
		t.Fatalf("getBusinessConnection called %d times for %d goroutines x %d resolutions, want between 1 and %d",
			calls, goroutines, perGoroutine, goroutines)
	}
	// Memoised at the end of the burst: a further resolution costs nothing.
	if _, err := svc.Resolve(context.Background(), "bc-stranger"); !errors.Is(err, ErrOwnerNotAllowed) {
		t.Fatalf("Resolve after the burst = %v, want ErrOwnerNotAllowed", err)
	}
	if api.calls() != calls {
		t.Fatalf("getBusinessConnection called again after the burst (%d -> %d): the refusal was not memoised", calls, api.calls())
	}
}

// lockedPool, lockedAPI and lockedUsers are the concurrency-safe counterparts of
// the scripted fakes in service_chain_test.go, whose plain counters and slices
// would race under a parallel burst. They script the one shape this test needs:
// a connection no row matches.
type lockedPool struct {
	mu      sync.Mutex
	rows    map[string]Connection
	queries int
}

func (p *lockedPool) QueryRow(_ context.Context, _ string, args ...any) pgx.Row {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(args) != 1 {
		return fakeOwnerRow{err: errors.New("lockedPool: no upsert expected")}
	}
	p.queries++
	id, _ := args[0].(string)
	conn, ok := p.rows[id]
	if !ok {
		return fakeRow{err: pgx.ErrNoRows}
	}
	return fakeRow{conn: conn}
}

func (p *lockedPool) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errors.New("lockedPool: no write expected")
}

type lockedAPI struct {
	mu        sync.Mutex
	conn      *telegram.BusinessConnection
	connCalls int
}

func (a *lockedAPI) GetBusinessConnection(context.Context, string) (*telegram.BusinessConnection, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.connCalls++
	return a.conn, nil
}

func (a *lockedAPI) SendMessage(context.Context, telegram.SendMessageRequest) error { return nil }

func (a *lockedAPI) calls() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.connCalls
}

type lockedUsers struct{ mu sync.Mutex }

func (u *lockedUsers) UpsertByTelegramID(_ context.Context, telegramUserID int64) (*users.User, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return nil, fmt.Errorf("lockedUsers: a refused holder must never be upserted (telegram_user_id %d)", telegramUserID)
}
