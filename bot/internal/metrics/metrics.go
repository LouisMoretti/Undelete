// Package metrics exposes aggregated counters in the Prometheus text format.
//
// Non-negotiable rule: NO user content enters here. Neither
// telegram_user_id, nor chat_id, nor message_id, nor display name, nor
// message text, nor bot token may appear -- neither as a value, nor as a
// series name, nor above all as a LABEL. A metric labeled by chat_id would
// reconstruct the list of monitored conversations in any scrape, and a
// /metrics endpoint is by nature less protected than the database.
//
// Deliberate corollary: the exposed series have NO labels, the list of names
// is fixed and hardcoded in RenderPrometheus. Cardinality is therefore
// bounded by construction (one series per name, ten in total), without any
// runtime guardrail being necessary.
package metrics

import (
	"strconv"
	"strings"
	"sync/atomic"
)

// Counters holds the state of the counters. The type is exported so tests can
// instantiate an isolated set of counters; application code uses the default
// instance through the package functions.
type Counters struct {
	updates       atomic.Int64
	updateErrors  atomic.Int64
	outboxRetries atomic.Int64
	outboxFailed  atomic.Int64
	deletions     atomic.Int64
	outboxBacklog atomic.Int64
	// quotaDrops counts captures dropped by a per-tenant quota (issue #19):
	// messages, media attachments and media downloads refused past a limit.
	quotaDrops atomic.Int64
	// quotaWarnings counts pre-saturation alerts of the volume quotas (stored
	// messages, catalogued media files, stored media bytes): usages crossing
	// the warn threshold, and fresh volume refusals. The capture rate never
	// warns (a flood must not emit a warning per refusal). At most one per
	// crossing and one per fresh block -- never per update under sustained
	// saturation.
	quotaWarnings atomic.Int64
	// updateRetries counts in-place retries of an update whose handling
	// failed transiently (database blip, Telegram 429/5xx).
	updateRetries atomic.Int64
	// updatesDroppedTransient counts updates still failing transiently once
	// their retry budget was spent: the poller advances past them, so each
	// one is a capture lost to an outage rather than to a bug.
	updatesDroppedTransient atomic.Int64
}

// std is the instance used by the binary. The counters are atomic: the poller
// runs its shard workers, and the outbox and the backlog loop run in their
// own goroutines, all writing concurrently.
var std = &Counters{}

// Default returns the package instance, to pass to the health server.
func Default() *Counters { return std }

func (c *Counters) AddUpdates(n int64)       { c.updates.Add(n) }
func (c *Counters) AddUpdateErrors(n int64)  { c.updateErrors.Add(n) }
func (c *Counters) AddOutboxRetries(n int64) { c.outboxRetries.Add(n) }
func (c *Counters) AddDeletions(n int64)     { c.deletions.Add(n) }

// AddQuotaDrops counts captures refused past a per-tenant quota. No label
// carries the tenant or the quota kind (see the package rule): the breakdown
// lives in the logs, which quote ids and quota names without user content.
func (c *Counters) AddQuotaDrops(n int64) { c.quotaDrops.Add(n) }

// AddQuotaWarnings counts volume-quota pre-saturation alerts (threshold
// crossings and fresh volume refusals, never rate refusals), the operator
// signal that a tenant is about to -- or has just started to -- lose captures.
func (c *Counters) AddQuotaWarnings(n int64) { c.quotaWarnings.Add(n) }

// AddOutboxFailed counts alerts that exhausted the fast lane and entered
// the slow lane (deferred with a fresh budget after 6h, never abandoned).
// Without this series, a slow-lane alert leaves no metric trace: it stays in
// undelete_outbox_backlog (which counts pending/processing/failed) without
// being delivered, and a spike of deferred alerts would read as plain backlog.
func (c *Counters) AddOutboxFailed(n int64) { c.outboxFailed.Add(n) }

func (c *Counters) AddUpdateRetries(n int64)           { c.updateRetries.Add(n) }
func (c *Counters) AddUpdatesDroppedTransient(n int64) { c.updatesDroppedTransient.Add(n) }

// SetOutboxBacklog publishes the number of outbox rows still to be delivered.
// It is a gauge: it goes up and down, unlike counters.
func (c *Counters) SetOutboxBacklog(n int64) { c.outboxBacklog.Store(n) }

// AddUpdates and friends, default instance versions.
func AddUpdates(n int64)       { std.AddUpdates(n) }
func AddUpdateErrors(n int64)  { std.AddUpdateErrors(n) }
func AddOutboxRetries(n int64) { std.AddOutboxRetries(n) }
func AddOutboxFailed(n int64)  { std.AddOutboxFailed(n) }
func AddDeletions(n int64)     { std.AddDeletions(n) }
func SetOutboxBacklog(n int64) { std.SetOutboxBacklog(n) }
func AddQuotaDrops(n int64)    { std.AddQuotaDrops(n) }
func AddQuotaWarnings(n int64) { std.AddQuotaWarnings(n) }
func AddUpdateRetries(n int64) { std.AddUpdateRetries(n) }

func AddUpdatesDroppedTransient(n int64) { std.AddUpdatesDroppedTransient(n) }

// ContentType is the MIME type of the Prometheus text exposition.
const ContentType = "text/plain; version=0.0.4; charset=utf-8"

// series describes an exposed series. The array is the EXHAUSTIVE list of
// possible names: adding a series happens here, never dynamically from an
// update's data.
type series struct {
	name  string
	help  string
	kind  string // "counter" or "gauge"
	value func(*Counters) int64
}

var allSeries = []series{
	{
		name:  "undelete_updates_total",
		help:  "Total number of Telegram updates received and delivered to the handler.",
		kind:  "counter",
		value: func(c *Counters) int64 { return c.updates.Load() },
	},
	{
		name:  "undelete_update_errors_total",
		help:  "Total number of update fetch or processing errors.",
		kind:  "counter",
		value: func(c *Counters) int64 { return c.updateErrors.Load() },
	},
	{
		name:  "undelete_outbox_retries_total",
		help:  "Total number of outbox alert reschedules.",
		kind:  "counter",
		value: func(c *Counters) int64 { return c.outboxRetries.Load() },
	},
	{
		name:  "undelete_outbox_failed_total",
		help:  "Total number of outbox alerts that exhausted the fast lane and entered the slow lane (deferred with a fresh budget, never abandoned).",
		kind:  "counter",
		value: func(c *Counters) int64 { return c.outboxFailed.Load() },
	},
	{
		name:  "undelete_deletions_total",
		help:  "Total number of deleted messages recovered and marked in the database.",
		kind:  "counter",
		value: func(c *Counters) int64 { return c.deletions.Load() },
	},
	{
		name:  "undelete_outbox_backlog",
		help:  "Number of outbox alerts still to be delivered (pending, processing or failed).",
		kind:  "gauge",
		value: func(c *Counters) int64 { return c.outboxBacklog.Load() },
	},
	{
		name:  "undelete_quota_drops_total",
		help:  "Total number of captures dropped past a per-tenant quota (messages, media attachments, media downloads).",
		kind:  "counter",
		value: func(c *Counters) int64 { return c.quotaDrops.Load() },
	},
	{
		name:  "undelete_quota_warnings_total",
		help:  "Total number of per-tenant volume-quota pre-saturation alerts (threshold crossings and fresh volume refusals, never rate refusals).",
		kind:  "counter",
		value: func(c *Counters) int64 { return c.quotaWarnings.Load() },
	},
	{
		name:  "undelete_update_retries_total",
		help:  "Total number of in-place retries of an update whose handling failed transiently (database connection, Telegram 429/5xx).",
		kind:  "counter",
		value: func(c *Counters) int64 { return c.updateRetries.Load() },
	},
	{
		name:  "undelete_updates_dropped_transient_total",
		help:  "Total number of updates skipped while still failing transiently after their retry budget (captures lost to an outage).",
		kind:  "counter",
		value: func(c *Counters) int64 { return c.updatesDroppedTransient.Load() },
	},
}

// RenderPrometheus renders the text exposition: a HELP/TYPE block then the
// value for each series. No label is emitted, so no value coming from an
// update can end up in the output.
func (c *Counters) RenderPrometheus() string {
	var b strings.Builder
	for _, s := range allSeries {
		b.WriteString("# HELP ")
		b.WriteString(s.name)
		b.WriteString(" ")
		b.WriteString(s.help)
		b.WriteString("\n# TYPE ")
		b.WriteString(s.name)
		b.WriteString(" ")
		b.WriteString(s.kind)
		b.WriteString("\n")
		b.WriteString(s.name)
		b.WriteString(" ")
		b.WriteString(strconv.FormatInt(s.value(c), 10))
		b.WriteString("\n")
	}
	return b.String()
}
