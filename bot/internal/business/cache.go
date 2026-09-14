package business

import (
	"container/list"
	"sync"
	"time"
)

// connectionCacheTTL bounds how long a resolution may be answered from memory
// without re-reading business_connections.
//
// Mono-tenant could afford an entry that never expired: the only writer of
// that table was this very process, so the map could not disagree with the
// database. Multi-tenant cannot. A connection disabled out of band -- by an
// operator running an UPDATE during an incident, by a maintenance script, by a
// second process during a rollout overlap -- would otherwise keep being served
// as enabled for the lifetime of the process, and the bot would go on capturing
// messages for a tenant whose capture was switched off. The TTL is what bounds
// that window.
//
// One minute is short enough that a disable takes effect within a poll cycle or
// two, and cheap enough to be irrelevant: re-reading costs one primary-key
// lookup per connection per minute, whatever the message rate.
const connectionCacheTTL = time.Minute

// connectionCacheMaxEntries bounds the cache in memory.
//
// Mono-tenant bounded it implicitly (one holder, a handful of connections).
// Open onboarding does not: the number of distinct connection ids this process
// sees is now driven by how many accounts connect the bot, plus every id
// Telegram ever delivered and that no longer exists. An unbounded map is a slow
// leak keyed by strangers. Entries past the bound are evicted least-recently-
// used, which costs at worst a database read to resolve them again.
const connectionCacheMaxEntries = 4096

// entryKind tells apart the three things a resolution can settle on. Both
// negative kinds exist for the same reason -- a decision that will not change
// within a TTL must not be re-taken for every update Telegram delivers -- but
// they are distinct verdicts and the caller answers them with distinct errors.
type entryKind uint8

const (
	// entryResolved is a connection: conn is the resolution.
	entryResolved entryKind = iota
	// entryUnknown is a NEGATIVE entry: Telegram answered
	// getBusinessConnection for this id with "no such connection". Remembering
	// the refusal is what keeps a revoked connection from costing one API call
	// per queued update -- the updates Telegram had buffered for it keep
	// arriving after the owner removed the bot, and each one would otherwise
	// re-ask.
	entryUnknown
	// entryRefused is the other NEGATIVE entry: the id resolves to an account
	// holder the onboarding allowlist does not admit. Without it, every update
	// from an unadmitted holder re-runs the whole chain -- one
	// business_connections read and, for an id no row matches, one
	// getBusinessConnection -- on a shard worker of the poller and on the
	// bot's shared Telegram rate budget.
	entryRefused
)

// cacheEntry is one memoised resolution.
type cacheEntry struct {
	id   string
	kind entryKind
	// conn is meaningful only for entryResolved.
	conn Connection
	// refusedOwnerTelegramUserID is meaningful only for entryRefused: the
	// account holder the id resolved to, kept so the memo can be re-checked
	// against the allowlist on lookup instead of being served on trust. The
	// internal owner_user_id is deliberately NOT kept: a refused holder is not
	// a tenant, and no entry that cannot be resolved may carry a tenant key.
	refusedOwnerTelegramUserID int64
	expiresAt                  time.Time
}

// connectionCache is a bounded, expiring, LRU cache of resolved connections.
//
// It is deliberately a type of its own rather than a map field on Service: the
// eviction and expiry rules are what keep the cache honest about a table it
// does not own, and they are worth testing without a resolution chain around
// them.
type connectionCache struct {
	mu   sync.Mutex
	ttl  time.Duration
	max  int
	now  func() time.Time
	byID map[string]*list.Element
	// order holds *cacheEntry, most recently used at the front. The eviction
	// victim is therefore always order.Back().
	order *list.List
	// disabledOwners remembers the owners DisableOwner turned off, so that
	// a stale database read (predating the disable) cannot resurrect their
	// connections as enabled even when no cache entry existed to patch --
	// disableOwner patches entries, but an entry only exists for a
	// connection resolved within the TTL. A fresh enabled state (a
	// reconnect, which Telegram reports now) clears the marker through
	// store. One entry per erased owner, cleared on reconnect: erasure is a
	// rare deliberate act, so the set stays tiny.
	disabledOwners map[int64]struct{}
}

func newConnectionCache(ttl time.Duration, max int, now func() time.Time) *connectionCache {
	return &connectionCache{
		ttl:            ttl,
		max:            max,
		now:            now,
		byID:           make(map[string]*list.Element),
		order:          list.New(),
		disabledOwners: make(map[int64]struct{}),
	}
}

// lookup returns the entry cached for id, if there is a live one. An expired
// entry is dropped and reported as absent: the caller must go back to the
// database rather than serve a resolution older than the TTL.
func (c *connectionCache) lookup(id string) (cacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	element, ok := c.byID[id]
	if !ok {
		return cacheEntry{}, false
	}
	entry := element.Value.(*cacheEntry)
	if !c.now().Before(entry.expiresAt) {
		c.remove(element)
		return cacheEntry{}, false
	}
	c.order.MoveToFront(element)
	return *entry, true
}

// store memoises a resolved connection.
//
// Fresh states only: the caller describes the connection as Telegram reports
// it right now (HandleBusinessConnection, or the API upsert of a database
// miss), including a reconnect that legitimately re-enables a connection an
// erasure disabled. A connection re-read from the database must go through
// storeDB instead: that read may predate a DisableOwner.
//
// An enabled store clears the owner's disabled marker (a reconnect is a fresh
// user intent, newer than any disable); a disabled store records it, so the
// marker also tracks deactivations Telegram reports, not just erasures.
func (c *connectionCache) store(conn Connection) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if conn.IsEnabled {
		delete(c.disabledOwners, conn.OwnerUserID)
	} else {
		c.disabledOwners[conn.OwnerUserID] = struct{}{}
	}
	c.putLocked(cacheEntry{id: conn.ID, conn: conn})
}

// storeDB memoises a connection freshly read from the database. Unlike
// store, it never lets a disabled connection resolve as enabled again:
//
//   - it refuses the enabled-over-disabled transition on an existing entry;
//   - it downgrades an enabled row to disabled when the owner carries a
//     disabled marker but no entry existed to patch.
//
// Both cover the same TOCTOU: the Resolve chain is cache lookup -> database
// read -> store, and the database read may return the pre-disable row while
// a DisableOwner commits between the read and the store. Storing
// unconditionally would resurrect an enabled entry (or create one) that every
// later Resolve would be served until the TTL, while the table says disabled.
// No mutex closes this: it is a read-modify-write ordering, not a data race.
// A genuine reconnect still re-enables through store (its state comes from
// Telegram now, not from a predating read).
func (c *connectionCache) storeDB(conn Connection) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !conn.IsEnabled {
		c.putLocked(cacheEntry{id: conn.ID, conn: conn})
		return
	}
	if element, ok := c.byID[conn.ID]; ok {
		if existing := element.Value.(*cacheEntry); existing.kind == entryResolved && !existing.conn.IsEnabled {
			return
		}
	}
	if _, marked := c.disabledOwners[conn.OwnerUserID]; marked {
		conn.IsEnabled = false
	}
	c.putLocked(cacheEntry{id: conn.ID, conn: conn})
}

// storeUnknown memoises the fact that Telegram does not recognise id.
func (c *connectionCache) storeUnknown(id string) {
	c.put(cacheEntry{id: id, kind: entryUnknown})
}

// storeRefused memoises the fact that id belongs to an account holder the
// onboarding allowlist does not admit. ownerTelegramUserID is the holder the id
// resolved to, so the refusal is re-checked on lookup rather than trusted.
func (c *connectionCache) storeRefused(id string, ownerTelegramUserID int64) {
	c.put(cacheEntry{id: id, kind: entryRefused, refusedOwnerTelegramUserID: ownerTelegramUserID})
}

func (c *connectionCache) put(entry cacheEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.putLocked(entry)
}

// putLocked inserts or replaces one entry. The caller holds the mutex.
func (c *connectionCache) putLocked(entry cacheEntry) {
	entry.expiresAt = c.now().Add(c.ttl)
	if element, ok := c.byID[entry.id]; ok {
		*element.Value.(*cacheEntry) = entry
		c.order.MoveToFront(element)
		return
	}
	c.byID[entry.id] = c.order.PushFront(&entry)
	// One eviction per insertion is enough to hold the bound: the map only
	// ever grows one entry at a time.
	if c.order.Len() > c.max {
		c.remove(c.order.Back())
	}
}

// disableOwner marks every cached connection of one owner as disabled, without
// evicting them, and remembers the owner as disabled so that a stale database
// read predating this call cannot resurrect their connections as enabled
// through storeDB -- even for connections this cache never held an entry for.
//
// Not an optimisation: Resolve answers from this cache before reading the
// database, so a connection disabled in PostgreSQL alone would keep resolving
// as enabled until its entry expired -- and the erasure that disabled it cannot
// afford a minute of further capture. Patching rather than evicting also keeps
// the control-command path (which must still resolve a disabled connection to
// authenticate /delete_my_data) answerable without a database round trip.
func (c *connectionCache) disableOwner(ownerUserID int64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.disabledOwners[ownerUserID] = struct{}{}
	for _, element := range c.byID {
		entry := element.Value.(*cacheEntry)
		if entry.kind == entryResolved && entry.conn.OwnerUserID == ownerUserID {
			entry.conn.IsEnabled = false
		}
	}
}

// len reports how many entries are held, expired ones included (they are
// dropped on lookup, not by a sweeper).
func (c *connectionCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}

// remove unlinks one element from both the map and the recency list. The
// caller holds the mutex.
func (c *connectionCache) remove(element *list.Element) {
	entry := element.Value.(*cacheEntry)
	delete(c.byID, entry.id)
	c.order.Remove(element)
}
