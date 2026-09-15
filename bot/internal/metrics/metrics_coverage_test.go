package metrics

import (
	"strconv"
	"strings"
	"sync"
	"testing"
)

// TestContentTypeIsPrometheusTextExposition pins the MIME type the health
// handler serves on /metrics: a scrape must be parsed as Prometheus text
// (version 0.0.4), not as JSON or a generic text dump.
func TestContentTypeIsPrometheusTextExposition(t *testing.T) {
	const want = "text/plain; version=0.0.4; charset=utf-8"
	if ContentType != want {
		t.Fatalf("ContentType = %q, want %q", ContentType, want)
	}
}

// TestRenderPrometheusDeclaresCounterAndGaugeKinds pins the TYPE contract
// from the README: every series is a counter except undelete_outbox_backlog,
// which is a gauge (a COUNT(*) that goes up AND down). A backlog wrongly
// declared counter would make rate() dashboards silently wrong.
func TestRenderPrometheusDeclaresCounterAndGaugeKinds(t *testing.T) {
	out := (&Counters{}).RenderPrometheus()
	kinds := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		name, kind, found := strings.Cut(strings.TrimPrefix(line, "# TYPE "), " ")
		if !strings.HasPrefix(line, "# TYPE ") || !found {
			continue
		}
		kinds[name] = kind
	}
	for _, name := range expectedSeries {
		kind, ok := kinds[name]
		if !ok {
			t.Fatalf("series %q has no # TYPE line", name)
		}
		want := "counter"
		if name == "undelete_outbox_backlog" {
			want = "gauge"
		}
		if kind != want {
			t.Fatalf("# TYPE %s = %q, want %q", name, kind, want)
		}
	}
}

// TestFreshCountersRenderZerosInFixedOrder pins the scrape baseline: a fresh
// instance exposes every series at 0, in the hardcoded expectedSeries order,
// with exactly one HELP/TYPE/value triple per series. Order stability keeps
// scrapes diffable and makes an accidental series insertion visible.
func TestFreshCountersRenderZerosInFixedOrder(t *testing.T) {
	out := (&Counters{}).RenderPrometheus()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != len(expectedSeries)*3 {
		t.Fatalf("exposition lines = %d, want %d (one HELP/TYPE/value triple per series):\n%s",
			len(lines), len(expectedSeries)*3, out)
	}
	var values []string
	for _, line := range lines {
		if strings.HasPrefix(line, "#") {
			continue
		}
		values = append(values, line)
	}
	if len(values) != len(expectedSeries) {
		t.Fatalf("value lines = %d, want %d:\n%s", len(values), len(expectedSeries), out)
	}
	for i, want := range expectedSeries {
		if values[i] != want+" 0" {
			t.Fatalf("value line %d = %q, want %q", i, values[i], want+" 0")
		}
	}
}

// TestOutboxFailedLeavesBacklogUntouched documents the boundary between the
// two slow-lane signals: AddOutboxFailed bumps undelete_outbox_failed_total
// but must NOT move undelete_outbox_backlog. The gauge only moves via
// SetOutboxBacklog, fed in production by CountBacklog's COUNT(*) over
// pending/processing/failed -- so a failed row parked in the slow lane stays
// visible in the gauge until it is actually delivered, and the failed counter
// can never mask a stuck backlog by accident.
func TestOutboxFailedLeavesBacklogUntouched(t *testing.T) {
	c := &Counters{}
	c.SetOutboxBacklog(5)
	c.AddOutboxFailed(3)
	got := prometheusValues(t, c.RenderPrometheus())
	if got["undelete_outbox_failed_total"] != 3 {
		t.Fatalf("failed_total = %d, want 3", got["undelete_outbox_failed_total"])
	}
	if got["undelete_outbox_backlog"] != 5 {
		t.Fatalf("backlog = %d, want 5 (AddOutboxFailed must not move the gauge)", got["undelete_outbox_backlog"])
	}

	// The slow-lane row is still undelivered work: the next COUNT(*) that
	// includes failed rows republishes it explicitly via the gauge, while the
	// failed counter stays put.
	c.SetOutboxBacklog(8)
	got = prometheusValues(t, c.RenderPrometheus())
	if got["undelete_outbox_backlog"] != 8 {
		t.Fatalf("backlog = %d, want 8 after explicit Set", got["undelete_outbox_backlog"])
	}
	if got["undelete_outbox_failed_total"] != 3 {
		t.Fatalf("failed_total = %d, want 3 (SetOutboxBacklog must not move the counter)", got["undelete_outbox_failed_total"])
	}

	// From zero: entering the slow lane alone never lifts the gauge by itself.
	alone := &Counters{}
	alone.AddOutboxFailed(1)
	aloneGot := prometheusValues(t, alone.RenderPrometheus())
	if aloneGot["undelete_outbox_backlog"] != 0 {
		t.Fatalf("backlog = %d, want 0 (failed counter must not imply backlog)", aloneGot["undelete_outbox_backlog"])
	}
}

// TestRenderPrometheusValueLinesAreLabelFreeIntegers is the strict shape of
// the confidentiality guardrail: every value line is exactly "<known series>
// <int64>", with no label syntax ('{', '}', '=', '"') through which a chat,
// user, message id or token could slip. HELP lines are checked too: the fixed
// help texts must not smuggle an identifier either.
func TestRenderPrometheusValueLinesAreLabelFreeIntegers(t *testing.T) {
	c := &Counters{}
	c.AddUpdates(1)
	c.AddUpdateErrors(2)
	c.AddOutboxRetries(3)
	c.AddOutboxFailed(4)
	c.AddDeletions(5)
	c.SetOutboxBacklog(6)
	c.AddQuotaDrops(7)
	c.AddQuotaWarnings(8)

	known := map[string]bool{}
	for _, name := range expectedSeries {
		known[name] = true
	}
	for _, line := range strings.Split(strings.TrimSpace(c.RenderPrometheus()), "\n") {
		if strings.HasPrefix(line, "# HELP ") || strings.HasPrefix(line, "# TYPE ") {
			for _, forbidden := range []string{"{", "}", "chat_id", "user_id", "message_id", "token", "business_connection"} {
				if strings.Contains(line, forbidden) {
					t.Fatalf("HELP/TYPE line carries %q: %q", forbidden, line)
				}
			}
			continue
		}
		if strings.ContainsAny(line, "{}=\"") {
			t.Fatalf("label syntax detected in value line %q", line)
		}
		name, raw, found := strings.Cut(line, " ")
		if !found || !known[name] {
			t.Fatalf("value line with unknown series: %q", line)
		}
		if strings.Contains(raw, " ") {
			t.Fatalf("value line carries extra fields: %q", line)
		}
		if _, err := strconv.ParseInt(raw, 10, 64); err != nil {
			t.Fatalf("value line %q has non-integer value: %v", line, err)
		}
	}
}

// TestQuotaCountersAreSafeUnderConcurrency covers the two quota paths the
// main concurrency test omits: per-tenant quota drops and volume-quota
// warnings are incremented from the capture path while the poller shards run.
// A concurrent reader exercises the atomic Load side under -race; only the
// final totals are asserted.
func TestQuotaCountersAreSafeUnderConcurrency(t *testing.T) {
	c := &Counters{}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 250; j++ {
				c.AddQuotaDrops(1)
				c.AddQuotaWarnings(1)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			_ = c.RenderPrometheus()
		}
	}()
	wg.Wait()
	got := prometheusValues(t, c.RenderPrometheus())
	if got["undelete_quota_drops_total"] != 2000 {
		t.Fatalf("quota_drops_total = %d, want 2000 (concurrent increments lost)", got["undelete_quota_drops_total"])
	}
	if got["undelete_quota_warnings_total"] != 2000 {
		t.Fatalf("quota_warnings_total = %d, want 2000 (concurrent increments lost)", got["undelete_quota_warnings_total"])
	}
}

// TestDefaultReturnsStableSingleton pins the wiring the health server relies
// on: Default() is non-nil and always the same instance, distinct from any
// isolated test instance.
func TestDefaultReturnsStableSingleton(t *testing.T) {
	if Default() == nil {
		t.Fatal("Default() = nil")
	}
	if Default() != Default() {
		t.Fatal("Default() returned two different instances")
	}
	if Default() == (&Counters{}) {
		t.Fatal("isolated instance must never alias the default one")
	}
}
