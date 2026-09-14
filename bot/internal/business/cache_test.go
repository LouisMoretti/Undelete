package business

import (
	"fmt"
	"testing"
	"time"
)

// testClock is a manual clock: the cache ages its entries on it, so a test can
// cross the TTL without sleeping.
type testClock struct{ now time.Time }

func (c *testClock) Now() time.Time          { return c.now }
func (c *testClock) advance(d time.Duration) { c.now = c.now.Add(d) }

func newTestClock() *testClock {
	return &testClock{now: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
}

func cachedConn(id string, ownerUserID int64) Connection {
	return Connection{
		ID:                  id,
		OwnerUserID:         ownerUserID,
		OwnerTelegramUserID: ownerUserID * 100,
		IsEnabled:           true,
	}
}

// TestCacheEntriesExpire pins the coherence half of the cache: an entry older
// than the TTL must not be served. Multi-tenant cannot assume this process is
// the only writer of business_connections, so a memoised resolution has to go
// stale on its own.
func TestCacheEntriesExpire(t *testing.T) {
	clock := newTestClock()
	cache := newConnectionCache(time.Minute, 8, clock.Now)
	cache.store(cachedConn("bc-1", 7))

	if _, ok := cache.lookup("bc-1"); !ok {
		t.Fatal("a freshly stored entry must be served from cache")
	}
	clock.advance(59 * time.Second)
	if _, ok := cache.lookup("bc-1"); !ok {
		t.Fatal("an entry within the TTL must still be served")
	}
	clock.advance(time.Second)
	if _, ok := cache.lookup("bc-1"); ok {
		t.Fatal("an entry that reached the TTL must be reported absent, so the caller re-reads the database")
	}
	if cache.len() != 0 {
		t.Fatalf("expired entry left behind: cache holds %d entries", cache.len())
	}
}

// TestCacheIsBoundedAndEvictsLeastRecentlyUsed pins the memory half: open
// onboarding means the number of distinct connection ids this process sees is
// driven by strangers, so the map must not be allowed to grow with them.
func TestCacheIsBoundedAndEvictsLeastRecentlyUsed(t *testing.T) {
	clock := newTestClock()
	const max = 4
	cache := newConnectionCache(time.Hour, max, clock.Now)

	for i := 1; i <= max; i++ {
		cache.store(cachedConn(fmt.Sprintf("bc-%d", i), int64(i)))
	}
	// Touch bc-1 so it is no longer the least recently used; bc-2 becomes the
	// eviction victim of the next insertion.
	if _, ok := cache.lookup("bc-1"); !ok {
		t.Fatal("bc-1 must still be cached before the overflow")
	}
	cache.store(cachedConn("bc-5", 5))

	if cache.len() != max {
		t.Fatalf("cache holds %d entries, want the bound %d", cache.len(), max)
	}
	if _, ok := cache.lookup("bc-2"); ok {
		t.Fatal("bc-2 was least recently used and must have been evicted")
	}
	for _, id := range []string{"bc-1", "bc-3", "bc-4", "bc-5"} {
		if _, ok := cache.lookup(id); !ok {
			t.Fatalf("%s must still be cached", id)
		}
	}
}

// TestCacheOverwriteDoesNotGrowTheCache pins that re-storing a known id
// replaces its entry instead of adding one: a connection re-resolved every
// minute for a year must not count as half a million entries.
func TestCacheOverwriteDoesNotGrowTheCache(t *testing.T) {
	clock := newTestClock()
	cache := newConnectionCache(time.Minute, 8, clock.Now)

	for i := 0; i < 100; i++ {
		cache.store(cachedConn("bc-1", 7))
		clock.advance(30 * time.Second)
	}
	if cache.len() != 1 {
		t.Fatalf("cache holds %d entries for one connection, want 1", cache.len())
	}
}

// TestCacheDisableOwnerPatchesOnlyThatOwner pins the erasure-critical
// invalidation: the tenant's entries must agree with the UPDATE immediately,
// and no other tenant may be touched by it.
func TestCacheDisableOwnerPatchesOnlyThatOwner(t *testing.T) {
	clock := newTestClock()
	cache := newConnectionCache(time.Minute, 8, clock.Now)
	cache.store(cachedConn("bc-a1", 7))
	cache.store(cachedConn("bc-a2", 7))
	cache.store(cachedConn("bc-b1", 8))
	cache.storeUnknown("bc-gone")
	cache.storeRefused("bc-stranger", 900)

	cache.disableOwner(7)

	for _, id := range []string{"bc-a1", "bc-a2"} {
		entry, ok := cache.lookup(id)
		if !ok {
			t.Fatalf("%s must stay cached: the control-command path resolves disabled connections", id)
		}
		if entry.conn.IsEnabled {
			t.Fatalf("%s still cached as enabled after disableOwner", id)
		}
	}
	other, ok := cache.lookup("bc-b1")
	if !ok || !other.conn.IsEnabled {
		t.Fatal("another tenant's connection was disturbed by disableOwner")
	}
	gone, ok := cache.lookup("bc-gone")
	if !ok || gone.kind != entryUnknown {
		t.Fatal("a negative entry must survive disableOwner unchanged")
	}
	refused, ok := cache.lookup("bc-stranger")
	if !ok || refused.kind != entryRefused || refused.refusedOwnerTelegramUserID != 900 {
		t.Fatalf("a refusal memo must survive disableOwner unchanged, got (%+v, %t)", refused, ok)
	}
}

// TestCacheNegativeEntriesAreServedAndExpire pins the revocation memo: a
// connection Telegram does not recognise is remembered as unknown, so the
// updates it still has buffered cost one API call in total, and the memo ages
// out like any other entry.
func TestCacheNegativeEntriesAreServedAndExpire(t *testing.T) {
	clock := newTestClock()
	cache := newConnectionCache(time.Minute, 8, clock.Now)
	cache.storeUnknown("bc-gone")

	entry, ok := cache.lookup("bc-gone")
	if !ok || entry.kind != entryUnknown {
		t.Fatalf("lookup of a negative entry = (%+v, %t), want an unknown entry", entry, ok)
	}
	clock.advance(time.Minute)
	if _, ok := cache.lookup("bc-gone"); ok {
		t.Fatal("a negative entry must expire like any other")
	}
}
