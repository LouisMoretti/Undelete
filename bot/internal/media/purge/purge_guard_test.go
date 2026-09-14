package purge

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/LouisMoretti/Undelete/bot/internal/media"
	"github.com/LouisMoretti/Undelete/bot/internal/tenantexcl"
	"github.com/LouisMoretti/Undelete/bot/internal/users"
)

type guardRunResult struct {
	stats Stats
	err   error
}

func newGuardedPurger(t *testing.T, root string, cat Catalogue, guard *tenantexcl.Guard) *Purger {
	t.Helper()
	p, err := New(Config{MediaDir: root, Catalogue: cat, Now: fixedNow, Guard: guard})
	if err != nil {
		t.Fatalf("new guarded purger: %v", err)
	}
	return p
}

// TestRunWithGuardWaitsForErasure pins the exclusion contract of the
// retention path: while a /delete_my_data holds the exclusive side, the daily
// purge must not unlink or requeue -- it waits, then proceeds once the
// erasure releases.
func TestRunWithGuardWaitsForErasure(t *testing.T) {
	root := t.TempDir()
	cat := newCatalogue(fixedNow)
	rel := writeMedia(t, root, "42/2026-01/05/expired", 0)
	cat.add(1, media.StatusStored, rel, fixedNow().AddDate(0, 0, -30))

	guard := tenantexcl.New()
	releaseErasure, err := guard.Exclusive(context.Background(), testOwner)
	if err != nil {
		t.Fatalf("hold exclusive: %v", err)
	}

	done := make(chan guardRunResult, 1)
	go func() {
		stats, err := newGuardedPurger(t, root, cat, guard).Run(
			context.Background(), []users.TenantRetention{testTenant})
		done <- guardRunResult{stats: stats, err: err}
	}()

	select {
	case res := <-done:
		t.Fatalf("retention ran under an active erasure: %+v", res)
	case <-time.After(300 * time.Millisecond):
	}

	releaseErasure()
	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("Run after release: %v", res.err)
		}
		if res.stats.FilesDeleted != 1 {
			t.Fatalf("FilesDeleted = %d, want 1 (the expired blob)", res.stats.FilesDeleted)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not proceed after the erasure released")
	}
}

// TestRunAbortsGuardWaitOnShutdown pins the failure mode: a cancelled context
// aborts the exclusion wait with an error instead of running unguarded --
// "shutdown was requested" must not become a way to bypass the erasure.
func TestRunAbortsGuardWaitOnShutdown(t *testing.T) {
	root := t.TempDir()
	cat := newCatalogue(fixedNow)
	rel := writeMedia(t, root, "42/2026-01/05/expired", 0)
	cat.add(1, media.StatusStored, rel, fixedNow().AddDate(0, 0, -30))

	guard := tenantexcl.New()
	releaseErasure, err := guard.Exclusive(context.Background(), testOwner)
	if err != nil {
		t.Fatalf("hold exclusive: %v", err)
	}
	defer releaseErasure()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan guardRunResult, 1)
	go func() {
		stats, err := newGuardedPurger(t, root, cat, guard).Run(
			ctx, []users.TenantRetention{testTenant})
		done <- guardRunResult{stats: stats, err: err}
	}()
	// Let the run reach the guard wait before cancelling.
	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case res := <-done:
		// A cancelled wait surfaces as a quiet shutdown (Run suppresses
		// ctx-driven errors like every other loop): what matters is that
		// nothing ran unguarded. The error assertion lives one level down,
		// on runTenantGuarded.
		if res.stats.FilesDeleted != 0 {
			t.Fatalf("FilesDeleted = %d, want 0 (nothing runs unguarded)", res.stats.FilesDeleted)
		}
		if !exists(t, filepath.Join(root, rel)) {
			t.Fatal("expired blob deleted despite the active erasure")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not abort on cancellation")
	}

	// One level down, the wait itself reports the exclusion failure loudly:
	// callers that do not suppress shutdown noise must see it.
	if _, err := newGuardedPurger(t, root, cat, guard).runTenantGuarded(ctx, testTenant); err == nil {
		t.Fatal("runTenantGuarded with a cancelled guard wait = nil, want the exclusion error")
	}
}
