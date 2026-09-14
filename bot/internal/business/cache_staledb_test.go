package business

import (
	"testing"
	"time"
)

// TestStoreDBDowngradesStaleEnabledAfterDisable pins the cache half of the B1
// invariant at the entry level: after disableOwner, a database re-read that
// still reports the connection as enabled (its snapshot predates the disable)
// is stored as disabled, whether or not an entry existed to patch.
func TestStoreDBDowngradesStaleEnabledAfterDisable(t *testing.T) {
	clock := newTestClock()
	cache := newConnectionCache(time.Minute, 8, clock.Now)

	// Hardest case first: no entry at disable time, so the patch hits
	// nothing and only the disabled-owner marker stands between the stale
	// read and the served resolution.
	cache.disableOwner(7)
	cache.storeDB(cachedConn("bc-never-cached", 7))

	entry, ok := cache.lookup("bc-never-cached")
	if !ok {
		t.Fatal("storeDB dropped the connection: the control-command path must still resolve disabled connections")
	}
	if entry.conn.IsEnabled {
		t.Fatal("storeDB served a stale enabled row after disableOwner with no entry to patch")
	}

	// Patched-entry case: the entry exists, the patch disabled it, the stale
	// read must not resurrect it.
	cache2 := newConnectionCache(time.Minute, 8, clock.Now)
	cache2.store(cachedConn("bc-cached", 7))
	cache2.disableOwner(7)
	cache2.storeDB(cachedConn("bc-cached", 7))
	entry, ok = cache2.lookup("bc-cached")
	if !ok || entry.conn.IsEnabled {
		t.Fatalf("storeDB resurrected a patched entry: (%+v, %t), want disabled", entry, ok)
	}

	// A genuine reconnect still re-enables: a fresh enabled state comes from
	// Telegram now, clears the marker, and later database reads serve it.
	cache2.store(cachedConn("bc-cached", 7))
	entry, ok = cache2.lookup("bc-cached")
	if !ok || !entry.conn.IsEnabled {
		t.Fatalf("a reconnect through store must re-enable, got (%+v, %t)", entry, ok)
	}
	cache2.storeDB(cachedConn("bc-cached", 7))
	entry, ok = cache2.lookup("bc-cached")
	if !ok || !entry.conn.IsEnabled {
		t.Fatalf("storeDB after a reconnect must serve enabled, got (%+v, %t)", entry, ok)
	}

	// Other tenants are never downgraded by someone else's disable.
	cache2.disableOwner(7)
	cache2.storeDB(cachedConn("bc-other", 8))
	other, ok := cache2.lookup("bc-other")
	if !ok || !other.conn.IsEnabled {
		t.Fatalf("another tenant's connection was downgraded, got (%+v, %t)", other, ok)
	}
}
