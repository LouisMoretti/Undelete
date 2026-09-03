package metrics

import (
	"strings"
	"testing"
)

// expectedSeries freezes the exposition contract: this list IS the total
// cardinality of /metrics. Adding a series must be a deliberate choice,
// visible in review, not a side effect.
var expectedSeries = []string{
	"undelete_updates_total",
	"undelete_update_errors_total",
	"undelete_outbox_retries_total",
	"undelete_outbox_failed_total",
	"undelete_deletions_total",
	"undelete_outbox_backlog",
}

func TestRenderPrometheusExposesExpectedSeries(t *testing.T) {
	c := &Counters{}
	c.AddUpdates(3)
	c.AddUpdateErrors(1)
	c.AddOutboxRetries(2)
	c.AddOutboxFailed(6)
	c.AddDeletions(7)
	c.SetOutboxBacklog(4)

	out := c.RenderPrometheus()

	for _, want := range []string{
		"undelete_updates_total 3",
		"undelete_update_errors_total 1",
		"undelete_outbox_retries_total 2",
		"undelete_outbox_failed_total 6",
		"undelete_deletions_total 7",
		"undelete_outbox_backlog 4",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output without %q:\n%s", want, out)
		}
	}

	for _, name := range expectedSeries {
		if !strings.Contains(out, "# HELP "+name+" ") || !strings.Contains(out, "# TYPE "+name+" ") {
			t.Fatalf("series %q without HELP/TYPE:\n%s", name, out)
		}
	}
}

// TestRenderPrometheusEmitsNoLabel is the confidentiality guardrail: no label
// is emitted, so no id, name, text or token can slip into the exposition. A
// `{` on a value line would signal the appearance of a dynamic dimension.
func TestRenderPrometheusEmitsNoLabel(t *testing.T) {
	c := &Counters{}
	c.AddUpdates(1)

	out := c.RenderPrometheus()
	names := map[string]bool{}

	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		if strings.ContainsAny(line, "{}") {
			t.Fatalf("label detected in line %q", line)
		}
		name, _, found := strings.Cut(line, " ")
		if !found {
			t.Fatalf("malformed metric line: %q", line)
		}
		if names[name] {
			t.Fatalf("series %q exposed twice", name)
		}
		names[name] = true
	}

	if len(names) != len(expectedSeries) {
		t.Fatalf("number of series = %d, expected %d (bounded cardinality)", len(names), len(expectedSeries))
	}
	for _, want := range expectedSeries {
		if !names[want] {
			t.Fatalf("series %q missing from the exposition", want)
		}
	}
}

func TestCountersAreIndependentOfDefaultInstance(t *testing.T) {
	before := Default().RenderPrometheus()

	c := &Counters{}
	c.AddDeletions(42)

	if Default().RenderPrometheus() != before {
		t.Fatal("an isolated set of counters modified the default instance")
	}
}
