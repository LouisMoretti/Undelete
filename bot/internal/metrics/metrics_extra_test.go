package metrics

import (
	"strings"
	"sync"
	"testing"
)

// TestPackageWrappersTouchTheDefaultInstance pins that the six package-level
// helpers write to std (the instance the binary serves): a wrapper silently
// detached from Default() would make production dashboards freeze.
func TestPackageWrappersTouchTheDefaultInstance(t *testing.T) {
	before := Default().RenderPrometheus()
	AddUpdates(2)
	AddUpdateErrors(3)
	AddOutboxRetries(4)
	AddOutboxFailed(5)
	AddDeletions(6)
	SetOutboxBacklog(7)
	after := Default().RenderPrometheus()
	if before == after {
		t.Fatal("package wrappers left the default instance unchanged")
	}
	for _, want := range []string{
		"undelete_updates_total 2",
		"undelete_update_errors_total 3",
		"undelete_outbox_retries_total 4",
		"undelete_outbox_failed_total 5",
		"undelete_deletions_total 6",
		"undelete_outbox_backlog 7",
	} {
		if !strings.Contains(after, want) {
			t.Fatalf("after wrappers, exposition misses %q:\n%s", want, after)
		}
	}
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
