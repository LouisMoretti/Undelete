package business

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/LouisMoretti/Undelete/bot/internal/users"
)

// failingUsers scripts the owner-upsert failure half of upsertFromTelegram:
// resolving a connection Telegram vouches for must not silently proceed when
// its owner row cannot be written.
type failingUsers struct {
	err  error
	seen []int64
}

func (u *failingUsers) UpsertByTelegramID(_ context.Context, telegramUserID int64) (*users.User, error) {
	u.seen = append(u.seen, telegramUserID)
	return nil, u.err
}

// TestDisableOwnerDatabaseFailureLeavesCacheUntouched pins the DB-first
// ordering of DisableOwner: a failed UPDATE must leave the cache describing
// what the table actually says (still enabled), not what the erasure wanted.
func TestDisableOwnerDatabaseFailureLeavesCacheUntouched(t *testing.T) {
	pool := &fakePool{t: t, execErr: errors.New("connection reset")}
	svc := NewService(pool, &fakeAPI{}, &fakeUsers{}, nil, testLogger())
	svc.cache.store(Connection{ID: "bc-a", OwnerUserID: 7, OwnerTelegramUserID: 700, IsEnabled: true})

	if _, err := svc.DisableOwner(context.Background(), 7); err == nil {
		t.Fatal("DisableOwner over a broken database = nil, want error")
	}
	entry, ok := svc.cache.lookup("bc-a")
	if !ok {
		t.Fatal("failed DisableOwner evicted the entry: the cache must still describe the table")
	}
	if !entry.conn.IsEnabled {
		t.Fatal("failed DisableOwner patched the cache: a failed UPDATE must not disable anything in memory")
	}
}

// TestOwnerUpsertFailureAbortsResolution pins the uncovered users branch of
// upsertFromTelegram on both entry points: the error is wrapped (never a
// refusal), nothing is upserted or cached, and the next update retries the
// whole chain instead of serving a verdict from memory.
func TestOwnerUpsertFailureAbortsResolution(t *testing.T) {
	t.Run("resolve retries the chain", func(t *testing.T) {
		pool := &fakePool{t: t, queryErr: pgx.ErrNoRows}
		api := &fakeAPI{conn: apiConn("bc-new", 700, true)}
		owners := &failingUsers{err: errors.New("users table unavailable")}
		svc := NewService(pool, api, owners, nil, testLogger())
		ctx := context.Background()

		for attempt := 1; attempt <= 2; attempt++ {
			_, err := svc.Resolve(ctx, "bc-new")
			if err == nil {
				t.Fatalf("attempt %d: Resolve = nil, want the owner-upsert failure", attempt)
			}
			if errors.Is(err, ErrOwnerNotAllowed) || errors.Is(err, ErrConnectionUnknown) || errors.Is(err, ErrConnectionOwnerConflict) {
				t.Fatalf("attempt %d: Resolve = %v, want a plain error (never a refusal)", attempt, err)
			}
			if !strings.Contains(err.Error(), "upsert owner") {
				t.Fatalf("attempt %d: Resolve = %v, want the wrapped owner failure", attempt, err)
			}
		}
		if len(pool.executed) != 0 {
			t.Fatal("a failed owner upsert must not reach the business_connections upsert")
		}
		if _, cached := svc.cache.lookup("bc-new"); cached {
			t.Fatal("a failed owner upsert must not be cached: the next update must retry")
		}
		if len(pool.queried) != 2 || api.connCalls != 2 {
			t.Fatalf("queried=%d apiCalls=%d, want 2/2 (a failure must be retried, not memoised)",
				len(pool.queried), api.connCalls)
		}
	})

	t.Run("handle surfaces the failure", func(t *testing.T) {
		pool := &fakePool{t: t}
		api := &fakeAPI{}
		owners := &failingUsers{err: errors.New("users table unavailable")}
		svc := NewService(pool, api, owners, nil, testLogger())

		err := svc.HandleBusinessConnection(context.Background(), *apiConn("bc-h", 700, true))
		if err == nil || !strings.Contains(err.Error(), "upsert owner") {
			t.Fatalf("HandleBusinessConnection = %v, want the wrapped owner failure", err)
		}
		if len(api.requests) != 0 {
			t.Fatal("a failed owner upsert must not welcome anyone")
		}
		if _, cached := svc.cache.lookup("bc-h"); cached {
			t.Fatal("a failed owner upsert must not be cached")
		}
	})
}

// TestNonAPIFailureIsNotARevocation pins the transport half of
// isUnknownConnection: only a 400 APIError means "no such connection". A
// plain transport failure stays a failure and is never memoised -- treating
// one as a revocation would silently stop capturing for a live tenant.
func TestNonAPIFailureIsNotARevocation(t *testing.T) {
	pool := &fakePool{t: t, queryErr: pgx.ErrNoRows}
	api := &fakeAPI{connErr: errors.New("dial tcp: connection reset by peer")}
	svc := NewService(pool, api, &fakeUsers{nextID: 7}, nil, testLogger())
	ctx := context.Background()

	for attempt := 1; attempt <= 2; attempt++ {
		_, err := svc.Resolve(ctx, "bc-flaky")
		if err == nil {
			t.Fatalf("attempt %d: Resolve = nil, want the transport failure", attempt)
		}
		if errors.Is(err, ErrConnectionUnknown) || errors.Is(err, ErrOwnerNotAllowed) {
			t.Fatalf("attempt %d: Resolve = %v, want a plain error (never a refusal)", attempt, err)
		}
	}
	if api.connCalls != 2 {
		t.Fatalf("getBusinessConnection called %d times, want 2 (a transport failure must be retried, not memoised)", api.connCalls)
	}
	if _, cached := svc.cache.lookup("bc-flaky"); cached {
		t.Fatal("a transport failure must not be cached at all")
	}
	if len(pool.executed) != 0 {
		t.Fatal("a failed API lookup must not be upserted")
	}
}

// TestCacheDisabledStoreDowngradesLaterStaleRead pins the second half of the
// disabled-owner marker: not only DisableOwner arms it, a deactivation
// Telegram reports (a disabled store) does too, so a stale enabled database
// read can never resurrect that connection through storeDB.
func TestCacheDisabledStoreDowngradesLaterStaleRead(t *testing.T) {
	clock := newTestClock()
	cache := newConnectionCache(time.Minute, 8, clock.Now)

	cache.store(Connection{ID: "bc-off", OwnerUserID: 7, OwnerTelegramUserID: 700, IsEnabled: false})
	cache.storeDB(cachedConn("bc-off", 7))

	entry, ok := cache.lookup("bc-off")
	if !ok {
		t.Fatal("storeDB dropped the connection: disabled connections must stay resolvable")
	}
	if entry.conn.IsEnabled {
		t.Fatal("storeDB resurrected a Telegram-deactivated connection as enabled")
	}

	other, ok := cache.lookup("bc-never-disabled")
	if ok {
		t.Fatalf("unexpected entry: %+v", other)
	}
	cache.storeDB(cachedConn("bc-other", 8))
	other, ok = cache.lookup("bc-other")
	if !ok || !other.conn.IsEnabled {
		t.Fatalf("another tenant's connection was downgraded, got (%+v, %t)", other, ok)
	}
}

// TestRefusedMemoForAdmittedHolderFallsThroughToDB pins that a refusal memo
// is re-checked, never served on trust: when the holder it names is admitted,
// the memo is ignored and the resolution falls through to the database.
func TestRefusedMemoForAdmittedHolderFallsThroughToDB(t *testing.T) {
	admitted := Connection{ID: "bc-x", OwnerUserID: 7, OwnerTelegramUserID: 700, IsEnabled: true}
	pool := &fakePool{t: t, queryConn: admitted}
	svc := NewService(pool, &fakeAPI{connErr: errors.New("API must not be touched")}, &fakeUsers{}, []int64{700}, testLogger())
	svc.cache.storeRefused("bc-x", 700)

	conn, err := svc.Resolve(context.Background(), "bc-x")
	if err != nil {
		t.Fatalf("Resolve over a stale refusal for an admitted holder = %v, want the database row", err)
	}
	if conn.OwnerUserID != 7 || !conn.IsEnabled {
		t.Fatalf("resolved %+v, want the admitted connection 7/enabled", conn)
	}
	if len(pool.queried) != 1 {
		t.Fatalf("database queried %d times, want 1 (the memo must be ignored, not served)", len(pool.queried))
	}
}

// TestHandleBusinessConnectionSurfacesUpsertFailure pins that a database
// write failure on the lifecycle path aborts the update like it does on the
// Resolve path: losing the write must not welcome anyone nor poison the
// cache with a connection the table never stored.
func TestHandleBusinessConnectionSurfacesUpsertFailure(t *testing.T) {
	pool := &fakePool{t: t, upsertErr: errors.New("disk full")}
	api := &fakeAPI{}
	svc := NewService(pool, api, &fakeUsers{nextID: 7}, nil, testLogger())

	if err := svc.HandleBusinessConnection(context.Background(), *apiConn("bc-w", 700, true)); err == nil {
		t.Fatal("HandleBusinessConnection with a failing upsert = nil, want error")
	}
	if len(api.requests) != 0 {
		t.Fatal("a failed upsert must not welcome anyone")
	}
	if _, cached := svc.cache.lookup("bc-w"); cached {
		t.Fatal("a failed upsert must not be cached")
	}
}
