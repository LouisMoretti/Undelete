package metrics

import (
	"strings"
	"sync"
	"testing"
)

// TestPackageWrappersTouchTheDefaultInstance pins that the ten package-level
// helpers write to std (the instance the binary serves): a wrapper silently
// detached from Default() would make production dashboards freeze. Deltas are
// asserted rather than absolute values so the test stays repeatable in-process
// (go test -count=3): the global instance accumulates across repetitions.
func TestPackageWrappersTouchTheDefaultInstance(t *testing.T) {
	before := prometheusValues(t, Default().RenderPrometheus())
	AddUpdates(2)
	AddUpdateErrors(3)
	AddOutboxRetries(4)
	AddOutboxFailed(5)
	AddDeletions(6)
	SetOutboxBacklog(7)
	AddQuotaDrops(8)
	AddQuotaWarnings(9)
	AddUpdateRetries(10)
	AddUpdatesDroppedTransient(11)
	after := prometheusValues(t, Default().RenderPrometheus())
	if equalMaps(before, after) {
		t.Fatal("package wrappers left the default instance unchanged")
	}
	for series, delta := range map[string]int64{
		"undelete_updates_total":        2,
		"undelete_update_errors_total":  3,
		"undelete_outbox_retries_total": 4,
		"undelete_outbox_failed_total":  5,
		"undelete_deletions_total":      6,
		"undelete_quota_drops_total":    8,
		"undelete_quota_warnings_total": 9,
		"undelete_update_retries_total": 10,

		"undelete_updates_dropped_transient_total": 11,
	} {
		if after[series]-before[series] != delta {
			t.Fatalf("wrapper delta for %q = %d, want +%d (before=%d after=%d)",
				series, after[series]-before[series], delta, before[series], after[series])
		}
	}
	// SetOutboxBacklog is a gauge (overwrite, not accumulate): it must read
	// exactly what was stored, whatever the previous repetitions left behind.
	if after["undelete_outbox_backlog"] != 7 {
		t.Fatalf("backlog gauge = %d, want 7", after["undelete_outbox_backlog"])
	}
}

// prometheusValues parses the value lines of a RenderPrometheus exposition
// into series -> value. HELP/TYPE lines carry no values and are skipped.
func prometheusValues(t *testing.T, exposition string) map[string]int64 {
	t.Helper()
	values := make(map[string]int64)
	for _, line := range strings.Split(strings.TrimSpace(exposition), "\n") {
		if strings.HasPrefix(line, "#") || strings.TrimSpace(line) == "" {
			continue
		}
		name, raw, found := strings.Cut(line, " ")
		if !found {
			t.Fatalf("malformed metric line: %q", line)
		}
		var n int64
		for _, r := range strings.TrimSpace(raw) {
			if r < '0' || r > '9' {
				t.Fatalf("non-numeric value in line %q", line)
			}
			n = n*10 + int64(r-'0')
		}
		values[name] = n
	}
	return values
}

func equalMaps(a, b map[string]int64) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// TestSetOutboxBacklogIsAGauge pins the overwrite semantics: the backlog goes
// up AND down (a COUNT(*), not a cumulative counter).
func TestSetOutboxBacklogIsAGauge(t *testing.T) {
	c := &Counters{}
	c.SetOutboxBacklog(10)
	c.SetOutboxBacklog(3)
	rendered := c.RenderPrometheus()
	if !strings.Contains(rendered, "undelete_outbox_backlog 3") {
		t.Fatalf("backlog gauge must reflect the last store, got:\n%s", rendered)
	}
}

// TestCountersAreSafeUnderConcurrency exercises the atomic contract under
// -race: poller, outbox and backlog loops write from different goroutines.
func TestCountersAreSafeUnderConcurrency(t *testing.T) {
	c := &Counters{}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 250; j++ {
				c.AddUpdates(1)
				c.AddUpdateErrors(1)
				c.AddOutboxRetries(1)
				c.AddOutboxFailed(1)
				c.AddDeletions(1)
				c.SetOutboxBacklog(int64(j))
			}
		}()
	}
	wg.Wait()
	rendered := c.RenderPrometheus()
	for _, want := range []string{
		"undelete_updates_total 2000",
		"undelete_update_errors_total 2000",
		"undelete_outbox_retries_total 2000",
		"undelete_outbox_failed_total 2000",
		"undelete_deletions_total 2000",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("concurrent increments lost, misses %q:\n%s", want, rendered)
		}
	}
}

// TestRenderPrometheusCarriesNoUserContent pins the privacy invariant on a
// fresh instance: no series name may ever look like an identifier smuggled
// in as a label or a suffix.
func TestRenderPrometheusCarriesNoUserContent(t *testing.T) {
	rendered := (&Counters{}).RenderPrometheus()
	for _, forbidden := range []string{"{", "}", "chat_id", "user_id", "message_id", "token"} {
		if strings.Contains(rendered, forbidden) {
			t.Fatalf("exposition must stay label-free, found %q in:\n%s", forbidden, rendered)
		}
	}
}
