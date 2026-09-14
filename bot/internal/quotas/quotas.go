// Package quotas bounds how much of the instance one tenant can consume:
// captured messages, catalogued media files, stored media bytes and capture
// rate. Without it, open onboarding lets a single tenant fill the disk, the
// database and the Telegram rate budget the instance shares (issue #19).
//
// # Keying
//
// Every admission is keyed by the INTERNAL owner_user_id the connection
// resolved to (business.Service.Resolve), never by anything read from the
// update itself -- a sender id or a chat id from the message cannot move
// usage from one tenant to another, because they never reach this package.
//
// # Enforcement shape
//
// The tracker is in-memory and process-local; the database is the source of
// truth it reconciles against:
//
//   - the fast path is pure memory (a counter compare, a sliding-window
//     prune), so the steady state costs no query per update;
//   - the first touch of a tenant seeds its counters from a UsageSource (the
//     repositories, in production), which is also what makes a restart safe:
//     a new tracker re-reads the same numbers instead of starting from zero;
//   - a tenant the ledger calls over quota is re-verified against the source
//     before being refused, then memoised as blocked for recheckInterval. The
//     re-verification is what absorbs retention purges and erasures: they
//     shrink the database behind the tracker's back, and the next admission
//     resyncs instead of refusing forever. The memoisation is what keeps a
//     genuinely saturated tenant from paying COUNT/SUM queries per update.
//
// All database reads run OUTSIDE the tracker's lock, so one tenant's slow
// source never delays another tenant's admission. Overshoot is bounded by
// construction: the message and media-file counters move under the lock (no
// overshoot), the byte counter moves after the download it accounts for
// (overshoot bounded by one fetch batch -- enforcement at batch granularity).
//
// A nil UsageSource is pure-memory mode (unit tests): seeding marks the
// ledger seeded with the counters at zero.
//
// # Behaviour on refusal
//
// The tracker only answers; it never logs, never sends, never fails the
// update. The caller drops the capture explicitly (log with ids only, quota
// metric) and advances: a refused update is not an error, exactly like a
// refused connection. Deletion marks, commands and lifecycle paths never
// consult the tracker -- a deletion mark is not a capture, and an erasure
// must work precisely when the tenant is over quota.
package quotas

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// QuotaKind names the quota an admission decision was taken against. It is
// what the logs and the (label-free) metrics break down by at the call site.
type QuotaKind string

const (
	// QuotaMessages bounds the stored rows of messages per tenant.
	QuotaMessages QuotaKind = "messages"
	// QuotaMediaFiles bounds the catalogued rows of media_files per tenant.
	QuotaMediaFiles QuotaKind = "media_files"
	// QuotaMediaBytes bounds the stored blob bytes on disk per tenant.
	QuotaMediaBytes QuotaKind = "media_bytes"
	// QuotaCaptureRate bounds the captured messages per minute per tenant.
	QuotaCaptureRate QuotaKind = "capture_rate"
)

// rateWindow is the sliding window the capture rate is measured over. One
// minute: short enough that a burst cannot spend the shared Telegram budget
// for long, long enough that a busy group does not trip on a lively minute.
const rateWindow = time.Minute

// recheckInterval memoises a verified refusal: a saturated tenant is
// re-verified against the database at most once a minute, instead of paying
// COUNT/SUM queries per update. Same order as the business resolution cache
// TTL, for the same reason (stranger-shaped load must cost O(1) per minute,
// not O(1) per update).
const recheckInterval = time.Minute

// maxTenants bounds the tracker's memory: past it the least recently seen
// tenant is evicted. An evicted tenant simply re-seeds from the source on its
// next admission -- correct, at the price of fresh queries. Same bound as the
// business resolution cache.
const maxTenants = 4096

// Limits holds the per-tenant quotas. Every field is strictly positive;
// there is no "unlimited" -- config.Load refuses a non-positive value at
// startup rather than running an unbounded tenant silently.
type Limits struct {
	// MaxMessages caps the stored message rows of one tenant.
	MaxMessages int64
	// MaxMediaFiles caps the catalogued media rows of one tenant.
	MaxMediaFiles int64
	// MaxMediaBytes caps the stored blob bytes of one tenant.
	MaxMediaBytes int64
	// CapturesPerMinute caps the captured messages per sliding minute of one
	// tenant.
	CapturesPerMinute int64
	// WarnPercent is the usage percentage (1-99) at which the pre-saturation
	// alert fires once per crossing.
	WarnPercent int
}

// DefaultLimits is the starting point shipped in .env.example: generous for a
// personal instance, and every field operator-tunable. The numbers are
// capacity planning, not an ADR: ~years of message capture, gigabytes of
// media, a rate far above what a human group produces but far below what
// would spend the shared Telegram budget.
func DefaultLimits() Limits {
	return Limits{
		MaxMessages:       100000,
		MaxMediaFiles:     10000,
		MaxMediaBytes:     5 << 30,
		CapturesPerMinute: 300,
		WarnPercent:       80,
	}
}

// Validate reports whether the limits can run a tracker.
func (l Limits) Validate() error {
	if l.MaxMessages <= 0 {
		return fmt.Errorf("invalid quota: MaxMessages = %d, want a positive number", l.MaxMessages)
	}
	if l.MaxMediaFiles <= 0 {
		return fmt.Errorf("invalid quota: MaxMediaFiles = %d, want a positive number", l.MaxMediaFiles)
	}
	if l.MaxMediaBytes <= 0 {
		return fmt.Errorf("invalid quota: MaxMediaBytes = %d, want a positive number", l.MaxMediaBytes)
	}
	if l.CapturesPerMinute <= 0 {
		return fmt.Errorf("invalid quota: CapturesPerMinute = %d, want a positive number", l.CapturesPerMinute)
	}
	if l.WarnPercent <= 0 || l.WarnPercent >= 100 {
		return fmt.Errorf("invalid quota: WarnPercent = %d, want between 1 and 99", l.WarnPercent)
	}
	return nil
}

// warnThreshold returns the usage at which the pre-saturation alert fires.
// Exact integer math (quotient and remainder handled separately) so neither
// an absurd limit overflows the product nor a small limit floors to zero.
func (l Limits) warnThreshold(limit int64) int64 {
	percent := int64(l.WarnPercent)
	return limit/100*percent + (limit%100)*percent/100
}

// UsageSource is the database truth the tracker seeds and re-verifies from.
// In production it is the messages and media repositories (tenant-scoped
// reads through InTenant); in tests a fake.
type UsageSource interface {
	CountMessages(ctx context.Context, ownerUserID int64) (int64, error)
	CountMediaFiles(ctx context.Context, ownerUserID int64) (int64, error)
	SumStoredMediaBytes(ctx context.Context, ownerUserID int64) (int64, error)
}

// Admission is the answer to one admission check.
type Admission struct {
	// Allowed reports whether the capture may proceed. False drops the update
	// explicitly at the call site -- never an error to the poller.
	Allowed bool
	// Warn tells the caller to emit the pre-saturation alert NOW (log with
	// ids only, quota metric): the usage just crossed the warn threshold, or
	// this is a fresh (non-memoised) refusal. True at most once per crossing
	// and at most once per recheckInterval per blocked quota -- never per
	// update under sustained saturation.
	Warn bool
	// Quota names the quota the decision was taken against.
	Quota QuotaKind
	// Usage and Limit are the numbers the alert quotes.
	Usage int64
	Limit int64
}

// tenant is one tenant's ledger. Only touched under Tracker.mu.
type tenant struct {
	messages   int64
	mediaFiles int64
	mediaBytes int64
	seeded     bool
	// rate holds the capture timestamps inside the current sliding window,
	// oldest first. Bounded by CapturesPerMinute by construction: a timestamp
	// is only appended when the window has room.
	rate []time.Time
	// warned records the quotas whose pre-saturation alert already fired for
	// the current crossing. Cleared when the usage drops back under the
	// threshold, so the next climb alerts again.
	warned map[QuotaKind]bool
	// blocked memoises a verified refusal until the instant it carries: a
	// saturated tenant costs no database read before then.
	blocked  map[QuotaKind]time.Time
	lastSeen time.Time
}

// Tracker admits per-tenant captures against Limits. Safe for concurrent use
// by the poller shard workers: the critical sections never block on I/O.
type Tracker struct {
	limits Limits
	source UsageSource
	now    func() time.Time

	mu      sync.Mutex
	tenants map[int64]*tenant
}

// NewTracker builds a Tracker. limits must be Validate-clean (config.Load
// guarantees it in production); now defaults to time.Now when nil so tests
// can drive the sliding window and the memoisation deterministically.
func NewTracker(limits Limits, source UsageSource, now func() time.Time) (*Tracker, error) {
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	if now == nil {
		now = time.Now
	}
	return &Tracker{
		limits:  limits,
		source:  source,
		now:     now,
		tenants: make(map[int64]*tenant),
	}, nil
}

// AdmitCapture admits one captured message: rate first (the cheapest check,
// and the one that sheds a flood before any database is touched), then
// message volume. A refused volume does not refund the rate budget: the
// update was load either way.
func (t *Tracker) AdmitCapture(ctx context.Context, ownerUserID int64) Admission {
	if ownerUserID <= 0 {
		return Admission{Allowed: false, Warn: true, Quota: QuotaMessages, Usage: 0, Limit: t.limits.MaxMessages}
	}
	now := t.now()

	t.mu.Lock()
	te := t.tenantLocked(ownerUserID, now)
	te.rate = pruneBefore(te.rate, now.Add(-rateWindow))
	if int64(len(te.rate)) >= t.limits.CapturesPerMinute {
		adm := Admission{Allowed: false, Quota: QuotaCaptureRate, Usage: int64(len(te.rate)), Limit: t.limits.CapturesPerMinute}
		t.mu.Unlock()
		return adm
	}
	te.rate = append(te.rate, now)
	t.mu.Unlock()

	return t.admitVolume(ctx, ownerUserID, now, QuotaMessages, true)
}

// AdmitMediaFile admits one catalogued media attachment against the
// media-file count.
func (t *Tracker) AdmitMediaFile(ctx context.Context, ownerUserID int64) Admission {
	if ownerUserID <= 0 {
		return Admission{Allowed: false, Warn: true, Quota: QuotaMediaFiles, Usage: 0, Limit: t.limits.MaxMediaFiles}
	}
	return t.admitVolume(ctx, ownerUserID, t.now(), QuotaMediaFiles, true)
}

// AdmitMediaBytes gates one media download against the stored-bytes quota. It
// reserves nothing: the bytes are only known after the download, so the fetch
// loop accounts them afterwards with AddMediaBytes. Enforcement is therefore
// at batch granularity -- a download that starts under quota may push the
// tenant over it, and the next download is then refused.
func (t *Tracker) AdmitMediaBytes(ctx context.Context, ownerUserID int64) Admission {
	if ownerUserID <= 0 {
		return Admission{Allowed: false, Warn: true, Quota: QuotaMediaBytes, Usage: 0, Limit: t.limits.MaxMediaBytes}
	}
	return t.admitVolume(ctx, ownerUserID, t.now(), QuotaMediaBytes, false)
}

// AddMediaBytes accounts bytes just stored on disk. Allowed is always true:
// the download already happened, so there is nothing left to refuse -- the
// gate stood before it (AdmitMediaBytes). Warn fires on the crossing call so
// the saturation alert still precedes the next refusal.
func (t *Tracker) AddMediaBytes(ctx context.Context, ownerUserID int64, n int64) Admission {
	if ownerUserID <= 0 {
		return Admission{Allowed: true, Quota: QuotaMediaBytes, Limit: t.limits.MaxMediaBytes}
	}
	if n < 0 {
		n = 0
	}
	now := t.now()

	t.mu.Lock()
	te := t.tenantLocked(ownerUserID, now)
	needSeed := !te.seeded
	t.mu.Unlock()
	if needSeed {
		t.resync(ctx, ownerUserID)
		t.mu.Lock()
		te = t.tenantLocked(ownerUserID, now)
		t.mu.Unlock()
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	te = t.tenantLocked(ownerUserID, now)
	te.mediaBytes += n
	warn := false
	if threshold := t.limits.warnThreshold(t.limits.MaxMediaBytes); te.mediaBytes >= threshold && !te.warned[QuotaMediaBytes] {
		te.warned[QuotaMediaBytes] = true
		warn = true
	}
	return Admission{Allowed: true, Warn: warn, Quota: QuotaMediaBytes, Usage: te.mediaBytes, Limit: t.limits.MaxMediaBytes}
}

// Usage reports the ledger of a tenant for tests and operators. The numbers
// are the tracker's view, seeded from the database -- not a live COUNT(*).
func (t *Tracker) Usage(ownerUserID int64) (messages, mediaFiles, mediaBytes int64, seeded bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	te, ok := t.tenants[ownerUserID]
	if !ok {
		return 0, 0, 0, false
	}
	return te.messages, te.mediaFiles, te.mediaBytes, te.seeded
}

// admitVolume enforces one volume quota (message count, media-file count or
// media bytes). inc counts one unit on admission (message and media-file
// reservations); the bytes gate passes false and leaves accounting to
// AddMediaBytes.
//
// The database is only ever touched with t.mu released (resync runs
// lock-free), so a slow source for one tenant never stalls another tenant's
// admission. A failing source fails OPEN: the write that follows fails loudly
// on its own if the database is truly away, while a blip costs no data loss.
func (t *Tracker) admitVolume(ctx context.Context, ownerUserID int64, now time.Time, kind QuotaKind, inc bool) Admission {
	limit := t.limitOf(kind)
	threshold := t.limits.warnThreshold(limit)

	t.mu.Lock()
	te := t.tenantLocked(ownerUserID, now)
	if !te.seeded {
		t.mu.Unlock()
		t.resync(ctx, ownerUserID)
		t.mu.Lock()
		te = t.tenantLocked(ownerUserID, now)
	}
	if !te.seeded || t.currentOf(te, kind) < limit {
		adm := allowLocked(te, kind, limit, threshold, inc)
		t.mu.Unlock()
		return adm
	}
	usage := t.currentOf(te, kind)
	if until, ok := te.blocked[kind]; ok && now.Before(until) {
		adm := Admission{Allowed: false, Quota: kind, Usage: usage, Limit: limit}
		t.mu.Unlock()
		return adm
	}
	// Over quota by the ledger: re-verify against the source before refusing
	// (retention or an erasure may have shrunk the truth behind our back).
	t.mu.Unlock()
	t.resync(ctx, ownerUserID)
	t.mu.Lock()
	te = t.tenantLocked(ownerUserID, now)
	usage = t.currentOf(te, kind)
	if !te.seeded || usage < limit {
		delete(te.blocked, kind)
		adm := allowLocked(te, kind, limit, threshold, inc)
		t.mu.Unlock()
		return adm
	}
	te.blocked[kind] = now.Add(recheckInterval)
	// A fresh block always alerts: it is the "now dropping" event, distinct
	// from the "approaching" alert the crossing already emitted (or never
	// reached, when the process seeded straight into saturation). Repeats
	// inside the memo stay silent.
	te.warned[kind] = true
	adm := Admission{Allowed: false, Warn: true, Quota: kind, Usage: usage, Limit: limit}
	t.mu.Unlock()
	return adm
}

// allowLocked counts one unit (when inc) and answers the admission. The Warn
// flag fires on the crossing call only: usage at or above the threshold with
// the alert not yet emitted for this climb. Callers hold t.mu.
func allowLocked(te *tenant, kind QuotaKind, limit, threshold int64, inc bool) Admission {
	if inc {
		switch kind {
		case QuotaMediaFiles:
			te.mediaFiles++
		case QuotaMediaBytes:
			te.mediaBytes++
		default:
			te.messages++
		}
	}
	var usage int64
	switch kind {
	case QuotaMediaFiles:
		usage = te.mediaFiles
	case QuotaMediaBytes:
		usage = te.mediaBytes
	default:
		usage = te.messages
	}
	warn := false
	if usage >= threshold && !te.warned[kind] {
		te.warned[kind] = true
		warn = true
	}
	return Admission{Allowed: true, Warn: warn, Quota: kind, Usage: usage, Limit: limit}
}

// resync re-reads one tenant's usage from the source and applies it. It never
// holds t.mu across the queries; a failing source leaves the ledger untouched
// (the caller fails open). A shrink behind our back (retention purge,
// erasure) clears the warn and block state it made stale, so the next
// admission re-alerts honestly instead of refusing forever.
func (t *Tracker) resync(ctx context.Context, ownerUserID int64) {
	if t.source == nil {
		t.mu.Lock()
		defer t.mu.Unlock()
		if te, ok := t.tenants[ownerUserID]; ok {
			te.seeded = true
		}
		return
	}
	messages, errM := t.source.CountMessages(ctx, ownerUserID)
	files, errF := t.source.CountMediaFiles(ctx, ownerUserID)
	bytes, errB := t.source.SumStoredMediaBytes(ctx, ownerUserID)
	if errM != nil || errF != nil || errB != nil {
		return
	}
	if messages < 0 {
		messages = 0
	}
	if files < 0 {
		files = 0
	}
	if bytes < 0 {
		bytes = 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	te, ok := t.tenants[ownerUserID]
	if !ok {
		return // evicted while the queries ran; the next admission retries
	}
	te.messages = messages
	te.mediaFiles = files
	te.mediaBytes = bytes
	te.seeded = true
	for _, kind := range []QuotaKind{QuotaMessages, QuotaMediaFiles, QuotaMediaBytes} {
		if t.currentOf(te, kind) < t.limitOf(kind) {
			delete(te.blocked, kind)
		}
		if t.currentOf(te, kind) < t.limits.warnThreshold(t.limitOf(kind)) {
			te.warned[kind] = false
		}
	}
}

// tenantLocked returns the ledger of a tenant, inserting (and bounding the
// map) when new. Callers hold t.mu.
func (t *Tracker) tenantLocked(ownerUserID int64, now time.Time) *tenant {
	if te, ok := t.tenants[ownerUserID]; ok {
		te.lastSeen = now
		return te
	}
	if len(t.tenants) >= maxTenants {
		t.evictLocked()
	}
	te := &tenant{
		warned:   make(map[QuotaKind]bool),
		blocked:  make(map[QuotaKind]time.Time),
		lastSeen: now,
	}
	t.tenants[ownerUserID] = te
	return te
}

// evictLocked drops the least recently seen tenant. Its next admission
// re-seeds from the source -- correct, at the price of fresh queries.
// Callers hold t.mu.
func (t *Tracker) evictLocked() {
	var victim int64
	var oldest time.Time
	first := true
	for id, te := range t.tenants {
		if first || te.lastSeen.Before(oldest) {
			victim, oldest, first = id, te.lastSeen, false
		}
	}
	delete(t.tenants, victim)
}

// currentOf reads the ledger field behind a quota kind. Callers hold t.mu.
func (t *Tracker) currentOf(te *tenant, kind QuotaKind) int64 {
	switch kind {
	case QuotaMediaFiles:
		return te.mediaFiles
	case QuotaMediaBytes:
		return te.mediaBytes
	default:
		return te.messages
	}
}

// limitOf returns the configured limit behind a quota kind.
func (t *Tracker) limitOf(kind QuotaKind) int64 {
	switch kind {
	case QuotaMediaFiles:
		return t.limits.MaxMediaFiles
	case QuotaMediaBytes:
		return t.limits.MaxMediaBytes
	default:
		return t.limits.MaxMessages
	}
}

// pruneBefore drops the timestamps at or before the cutoff, oldest first.
func pruneBefore(stamps []time.Time, cutoff time.Time) []time.Time {
	kept := stamps[:0]
	for _, ts := range stamps {
		if ts.After(cutoff) {
			kept = append(kept, ts)
		}
	}
	// Clear the tail so a burst that just aged out does not pin its backing
	// array: the slice is bounded by CapturesPerMinute, but the array behind
	// it would otherwise only grow.
	for i := len(kept); i < len(stamps); i++ {
		stamps[i] = time.Time{}
	}
	return kept
}
