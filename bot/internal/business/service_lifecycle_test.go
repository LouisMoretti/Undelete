package business

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/LouisMoretti/Undelete/bot/internal/telegram"
	"github.com/LouisMoretti/Undelete/bot/internal/users"
)

// TestSeveralOwnersOnboardIndependently pins the point of the phase: two
// distinct account holders connect the bot, each gets their own users row, and
// each connection resolves to its OWN owner. Nothing about resolving the first
// one may leak into the second.
func TestSeveralOwnersOnboardIndependently(t *testing.T) {
	pool := &fakePool{t: t, queryErr: pgx.ErrNoRows}
	api := &fakeAPI{}
	people := &sequentialUsers{}
	svc := NewService(pool, api, people, nil, testLogger())

	if err := svc.HandleBusinessConnection(context.Background(), *apiConn("bc-a", 700, true)); err != nil {
		t.Fatalf("onboarding owner 700: %v", err)
	}
	if err := svc.HandleBusinessConnection(context.Background(), *apiConn("bc-b", 800, true)); err != nil {
		t.Fatalf("onboarding owner 800: %v", err)
	}

	a, err := svc.Resolve(context.Background(), "bc-a")
	if err != nil {
		t.Fatalf("Resolve(bc-a): %v", err)
	}
	b, err := svc.Resolve(context.Background(), "bc-b")
	if err != nil {
		t.Fatalf("Resolve(bc-b): %v", err)
	}
	if a.OwnerUserID == b.OwnerUserID {
		t.Fatalf("two holders share one owner_user_id: %d", a.OwnerUserID)
	}
	if a.OwnerTelegramUserID != 700 || b.OwnerTelegramUserID != 800 {
		t.Fatalf("owners crossed: bc-a=%d bc-b=%d", a.OwnerTelegramUserID, b.OwnerTelegramUserID)
	}
	if len(api.requests) != 2 {
		t.Fatalf("welcomes sent = %d, want one per holder", len(api.requests))
	}
}

// TestSeveralConnectionsOfOneOwnerShareTheTenant pins the other half of AC1: an
// account holder may hold several Business connections at once (a second
// device, a reconnection issuing a new id), and all of them resolve to the SAME
// tenant -- otherwise their messages would land in two isolated halves and only
// one of them would be erased by /delete_my_data.
func TestSeveralConnectionsOfOneOwnerShareTheTenant(t *testing.T) {
	pool := &fakePool{t: t, queryErr: pgx.ErrNoRows}
	svc := NewService(pool, &fakeAPI{}, &fakeUsers{nextID: 7}, nil, testLogger())

	for _, id := range []string{"bc-1", "bc-2", "bc-3"} {
		if err := svc.HandleBusinessConnection(context.Background(), *apiConn(id, 700, true)); err != nil {
			t.Fatalf("onboarding %s: %v", id, err)
		}
		conn, err := svc.Resolve(context.Background(), id)
		if err != nil {
			t.Fatalf("Resolve(%s): %v", id, err)
		}
		if conn.OwnerUserID != 7 || conn.OwnerTelegramUserID != 700 {
			t.Fatalf("%s resolved to owner %d/%d, want the single tenant 7/700", id, conn.OwnerUserID, conn.OwnerTelegramUserID)
		}
	}
}

// TestConnectionLifecycleThroughUpdates walks the three transitions Telegram
// expresses through business_connection: deactivation stops the capture at
// once (cache included), and reactivation resumes it.
func TestConnectionLifecycleThroughUpdates(t *testing.T) {
	pool := &fakePool{t: t, queryErr: pgx.ErrNoRows}
	api := &fakeAPI{}
	svc := NewService(pool, api, &fakeUsers{nextID: 7}, nil, testLogger())
	ctx := context.Background()

	// Onboarding.
	if err := svc.HandleBusinessConnection(ctx, *apiConn("bc-life", 700, true)); err != nil {
		t.Fatalf("onboarding: %v", err)
	}
	conn, err := svc.Resolve(ctx, "bc-life")
	if err != nil || !conn.IsEnabled {
		t.Fatalf("after onboarding: (%+v, %v), want an enabled connection", conn, err)
	}
	if len(api.requests) != 1 {
		t.Fatalf("welcomes after onboarding = %d, want 1", len(api.requests))
	}

	// Deactivation: the holder switched the bot off, or removed it.
	if err := svc.HandleBusinessConnection(ctx, *apiConn("bc-life", 700, false)); err != nil {
		t.Fatalf("deactivation: %v", err)
	}
	conn, err = svc.Resolve(ctx, "bc-life")
	if err != nil {
		t.Fatalf("resolve after deactivation: %v", err)
	}
	if conn.IsEnabled {
		t.Fatal("the cache still serves a deactivated connection as enabled: capture would continue")
	}
	if len(api.requests) != 1 {
		t.Fatalf("a deactivation must not welcome anyone, welcomes = %d", len(api.requests))
	}

	// Reactivation.
	if err := svc.HandleBusinessConnection(ctx, *apiConn("bc-life", 700, true)); err != nil {
		t.Fatalf("reactivation: %v", err)
	}
	conn, err = svc.Resolve(ctx, "bc-life")
	if err != nil || !conn.IsEnabled {
		t.Fatalf("after reactivation: (%+v, %v), want an enabled connection", conn, err)
	}
}

// TestRevokedConnectionIsRefusedAndRememberedOnce pins the revocation branch:
// once Telegram answers "no such connection", the id is refused and the refusal
// is memoised, so the updates Telegram still had buffered for it cost one
// getBusinessConnection in total instead of one each.
func TestRevokedConnectionIsRefusedAndRememberedOnce(t *testing.T) {
	pool := &fakePool{t: t, queryErr: pgx.ErrNoRows}
	api := &fakeAPI{connErr: &telegram.APIError{
		Method:      "getBusinessConnection",
		Code:        http.StatusBadRequest,
		Description: "Bad Request: business connection not found",
	}}
	svc := NewService(pool, api, &fakeUsers{nextID: 7}, nil, testLogger())

	for attempt := 1; attempt <= 3; attempt++ {
		_, err := svc.Resolve(context.Background(), "bc-revoked")
		if !errors.Is(err, ErrConnectionUnknown) {
			t.Fatalf("attempt %d: Resolve = %v, want ErrConnectionUnknown", attempt, err)
		}
	}
	if api.connCalls != 1 {
		t.Fatalf("getBusinessConnection called %d times, want 1 (the refusal must be memoised)", api.connCalls)
	}
	if len(pool.executed) != 0 {
		t.Fatal("a revoked connection must not be upserted")
	}
}

// TestTransientAPIFailureIsNotARevocation pins the other side of that rule: a
// rate limit or a server error must stay a failure, retried by the next update.
// Treating one as a revocation would silently stop capturing for a live tenant.
func TestTransientAPIFailureIsNotARevocation(t *testing.T) {
	for _, apiErr := range []*telegram.APIError{
		{Method: "getBusinessConnection", Code: http.StatusTooManyRequests, RetryAfter: 30},
		{Method: "getBusinessConnection", Code: http.StatusBadGateway},
	} {
		t.Run(apiErr.Error(), func(t *testing.T) {
			pool := &fakePool{t: t, queryErr: pgx.ErrNoRows}
			api := &fakeAPI{connErr: apiErr}
			svc := NewService(pool, api, &fakeUsers{nextID: 7}, nil, testLogger())

			_, err := svc.Resolve(context.Background(), "bc-transient")
			if err == nil || errors.Is(err, ErrConnectionUnknown) {
				t.Fatalf("Resolve = %v, want a plain error (never a revocation)", err)
			}
			// Nothing memoised: the next update must ask again.
			if _, err := svc.Resolve(context.Background(), "bc-transient"); err == nil {
				t.Fatal("second Resolve = nil, want the failure to be retried")
			}
			if api.connCalls != 2 {
				t.Fatalf("getBusinessConnection called %d times, want 2 (no negative caching of a transient failure)", api.connCalls)
			}
		})
	}
}

// TestConnectionOwnerTakeoverIsRefused is the "usurped connection" case: an
// update claiming an existing connection for a different account holder must
// change nothing. Without the guard, tenant B would start writing into a
// connection tenant A's messages are attributed to.
func TestConnectionOwnerTakeoverIsRefused(t *testing.T) {
	// The upsert answers no row: that is what the WHERE on the ON CONFLICT
	// clause produces when the stored owner differs from the claimed one.
	pool := &fakePool{t: t, queryErr: pgx.ErrNoRows, upsertErr: pgx.ErrNoRows}
	api := &fakeAPI{}
	svc := NewService(pool, api, &fakeUsers{nextID: 9}, nil, testLogger())

	err := svc.HandleBusinessConnection(context.Background(), *apiConn("bc-stolen", 900, true))
	if !errors.Is(err, ErrConnectionOwnerConflict) {
		t.Fatalf("HandleBusinessConnection = %v, want ErrConnectionOwnerConflict", err)
	}
	if len(api.requests) != 0 {
		t.Fatal("a refused takeover must not welcome the claimant")
	}
	if _, cached := svc.cache.lookup("bc-stolen"); cached {
		t.Fatal("a refused takeover must not be cached: the next resolution must read the real owner from the database")
	}
}

// TestRestartRebuildsTheCacheFromTheDatabase pins the restart half of AC2: the
// cache is memory only, so a fresh process must reflect whatever the table says
// -- including a connection disabled while it was down.
func TestRestartRebuildsTheCacheFromTheDatabase(t *testing.T) {
	stored := Connection{ID: "bc-restart", OwnerUserID: 7, OwnerTelegramUserID: 700, IsEnabled: true}
	pool := &fakePool{t: t, queryConn: stored}
	before := NewService(pool, &fakeAPI{}, &fakeUsers{}, nil, testLogger())
	if conn, err := before.Resolve(context.Background(), "bc-restart"); err != nil || !conn.IsEnabled {
		t.Fatalf("before restart: (%+v, %v)", conn, err)
	}

	// The table changed under us (an operator, a maintenance script), and the
	// process restarted. The new Service starts with an empty cache.
	pool.queryConn = Connection{ID: "bc-restart", OwnerUserID: 7, OwnerTelegramUserID: 700, IsEnabled: false}
	after := NewService(pool, &fakeAPI{}, &fakeUsers{}, nil, testLogger())
	conn, err := after.Resolve(context.Background(), "bc-restart")
	if err != nil {
		t.Fatalf("after restart: %v", err)
	}
	if conn.IsEnabled {
		t.Fatal("a restarted process served a stale enabled connection")
	}
}

// TestCacheRereadsTheDatabaseAfterTheTTL pins the running-process half of AC2:
// without a restart, a connection disabled out of band must stop being served
// as enabled once its entry ages past the TTL.
func TestCacheRereadsTheDatabaseAfterTheTTL(t *testing.T) {
	clock := newTestClock()
	stored := Connection{ID: "bc-ttl", OwnerUserID: 7, OwnerTelegramUserID: 700, IsEnabled: true}
	pool := &fakePool{t: t, queryConn: stored}
	svc := NewService(pool, &fakeAPI{}, &fakeUsers{}, nil, testLogger(), WithClock(clock.Now))

	if conn, err := svc.Resolve(context.Background(), "bc-ttl"); err != nil || !conn.IsEnabled {
		t.Fatalf("first Resolve: (%+v, %v)", conn, err)
	}
	// Disabled out of band, then the TTL elapses.
	pool.queryConn = Connection{ID: "bc-ttl", OwnerUserID: 7, OwnerTelegramUserID: 700, IsEnabled: false}
	clock.advance(connectionCacheTTL)

	conn, err := svc.Resolve(context.Background(), "bc-ttl")
	if err != nil {
		t.Fatalf("Resolve after the TTL: %v", err)
	}
	if conn.IsEnabled {
		t.Fatal("the cache served an enabled connection past its TTL, ignoring the database")
	}
	if len(pool.queried) != 2 {
		t.Fatalf("database queried %d times, want 2 (once per TTL window)", len(pool.queried))
	}
}

// TestDisableOwnerLeavesOtherTenantsAlone pins the tenant scope of the erasure
// step: DisableOwner must touch exactly one tenant's cache entries, whatever
// the UPDATE reports.
func TestDisableOwnerLeavesOtherTenantsAlone(t *testing.T) {
	pool := &fakePool{t: t, execTag: pgconn.NewCommandTag("UPDATE 1")}
	svc := NewService(pool, &fakeAPI{}, &fakeUsers{}, nil, testLogger())
	svc.cache.store(Connection{ID: "bc-a", OwnerUserID: 7, OwnerTelegramUserID: 700, IsEnabled: true})
	svc.cache.store(Connection{ID: "bc-b", OwnerUserID: 8, OwnerTelegramUserID: 800, IsEnabled: true})
	svc.cache.store(Connection{ID: "bc-c", OwnerUserID: 9, OwnerTelegramUserID: 900, IsEnabled: true})

	if _, err := svc.DisableOwner(context.Background(), 8); err != nil {
		t.Fatalf("DisableOwner: %v", err)
	}

	for _, tc := range []struct {
		id          string
		wantEnabled bool
	}{
		{id: "bc-a", wantEnabled: true},
		{id: "bc-b", wantEnabled: false},
		{id: "bc-c", wantEnabled: true},
	} {
		conn, err := svc.Resolve(context.Background(), tc.id)
		if err != nil {
			t.Fatalf("Resolve(%s): %v", tc.id, err)
		}
		if conn.IsEnabled != tc.wantEnabled {
			t.Fatalf("Resolve(%s).IsEnabled = %t, want %t", tc.id, conn.IsEnabled, tc.wantEnabled)
		}
	}
}

// TestResolveRejectsAnAnswerForAnotherConnection pins a defensive check on the
// API fallback: caching an answer under a different id would leave the
// requested one permanently unresolved and re-ask Telegram for every update
// carrying it.
func TestResolveRejectsAnAnswerForAnotherConnection(t *testing.T) {
	pool := &fakePool{t: t, queryErr: pgx.ErrNoRows}
	api := &fakeAPI{conn: apiConn("bc-other", 700, true)}
	svc := NewService(pool, api, &fakeUsers{nextID: 7}, nil, testLogger())

	if _, err := svc.Resolve(context.Background(), "bc-asked"); err == nil {
		t.Fatal("Resolve accepted an answer for another connection id")
	}
	if len(pool.executed) != 0 {
		t.Fatal("a mismatched answer must not be upserted")
	}
}

// sequentialUsers hands out a distinct internal id per Telegram account holder,
// as the users table does. fakeUsers returns a fixed one, which is enough for
// single-tenant cases but would hide a crossed tenant here.
type sequentialUsers struct {
	byTelegramID map[int64]int64
	next         int64
}

func (u *sequentialUsers) UpsertByTelegramID(_ context.Context, telegramUserID int64) (*users.User, error) {
	if u.byTelegramID == nil {
		u.byTelegramID = make(map[int64]int64)
	}
	id, ok := u.byTelegramID[telegramUserID]
	if !ok {
		u.next++
		id = u.next
		u.byTelegramID[telegramUserID] = id
	}
	return &users.User{ID: id, TelegramUserID: telegramUserID}, nil
}
