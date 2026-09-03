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
// bounded by construction (one series per name, five in total), without any
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
}

// std is the instance used by the binary. The counters are atomic: the poller
// is sequential, but the outbox and the backlog loop run in their own
// goroutines and write concurrently.
var std = &Counters{}

// Default returns the package instance, to pass to the health server.
func Default() *Counters { return std }

func (c *Counters) AddUpdates(n int64)       { c.updates.Add(n) }
func (c *Counters) AddUpdateErrors(n int64)  { c.updateErrors.Add(n) }
func (c *Counters) AddOutboxRetries(n int64) { c.outboxRetries.Add(n) }
func (c *Counters) AddDeletions(n int64)     { c.deletions.Add(n) }

// AddOutboxFailed counts alerts PERMANENTLY ABANDONED. Without this series, an
// alert in permanent failure leaves no metric trace: it leaves
// undelete_outbox_backlog (which only counts pending/processing) without
// being delivered, and a spike of 4xx would read as a simple backlog decline.
func (c *Counters) AddOutboxFailed(n int64) { c.outboxFailed.Add(n) }

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
		help:  "Total number of outbox alerts permanently abandoned (never delivered).",
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
		help:  "Number of outbox alerts still to be delivered (pending or processing).",
		kind:  "gauge",
		value: func(c *Counters) int64 { return c.outboxBacklog.Load() },
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
