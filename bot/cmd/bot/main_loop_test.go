package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/LouisMoretti/Undelete/bot/internal/media/purge"
	"github.com/LouisMoretti/Undelete/bot/internal/users"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

func testTenants(ids ...int64) []users.TenantRetention {
	tenants := make([]users.TenantRetention, 0, len(ids))
	for _, id := range ids {
		tenants = append(tenants, users.TenantRetention{OwnerUserID: id, RetentionDays: 7})
	}
	return tenants
}

type fakeTenantLister struct {
	tenants []users.TenantRetention
	err     error
	calls   int
}

func (f *fakeTenantLister) ListTenantsForRetention(context.Context) ([]users.TenantRetention, error) {
	f.calls++
	return f.tenants, f.err
}

type fakeOutboxDeliverer struct {
	// process runs on every ProcessOne call; it may cancel the context to
	// stop the loop under test.
	process func(ctx context.Context, ownerUserID int64) (bool, error)
	calls   []int64
}

func (f *fakeOutboxDeliverer) ProcessOne(ctx context.Context, ownerUserID int64) (bool, error) {
	f.calls = append(f.calls, ownerUserID)
	return f.process(ctx, ownerUserID)
}

type fakeMediaFetcher struct {
	calls  []int64
	stored int
	err    error
}

func (f *fakeMediaFetcher) ProcessTenant(_ context.Context, ownerUserID int64) (int, error) {
	f.calls = append(f.calls, ownerUserID)
	return f.stored, f.err
}

type fakeBacklogCounter struct {
	calls int
	value int64
	err   error
}

func (f *fakeBacklogCounter) CountBacklog(_ context.Context, _ []users.TenantRetention) (int64, error) {
	f.calls++
	return f.value, f.err
}

type phaseRecorder struct {
	calls []string
	// errOn names the phase that fails: "messages", "outbox" or "media".
	errOn string
}

func (p *phaseRecorder) PurgeExpired(context.Context, []users.TenantRetention) (int64, error) {
	p.calls = append(p.calls, "purgeExpired")
	if p.errOn == "purgeExpired" {
		return 3, errors.New("purge exploded")
	}
	return 5, nil
}

type fakeMediaRetention struct {
	calls []string
	err   error
}

func (f *fakeMediaRetention) Run(context.Context, []users.TenantRetention) (purge.Stats, error) {
	f.calls = append(f.calls, "run")
	return purge.Stats{FilesDeleted: 2}, f.err
}

// TestRunOutboxLoopVisitsEveryTenantThenStops pins the loop contract: one
// iteration visits every tenant in order, drains each queue until it reports
// idle, then waits for the next tick -- or exits on shutdown. A pre-cancelled
// context runs exactly one iteration, deterministically.
func TestRunOutboxLoopVisitsEveryTenantThenStops(t *testing.T) {
	tenants := &fakeTenantLister{tenants: testTenants(11, 22)}
	worker := &fakeOutboxDeliverer{process: func(context.Context, int64) (bool, error) {
		return false, nil // idle immediately: one call per tenant
	}}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runOutboxLoop(ctx, tenants, worker, discardLogger())

	if tenants.calls != 1 {
		t.Fatalf("tenants listed %d times, want 1 iteration", tenants.calls)
	}
	if len(worker.calls) != 2 || worker.calls[0] != 11 || worker.calls[1] != 22 {
		t.Fatalf("worker visited %v, want [11 22] in order", worker.calls)
	}
}

// TestRunOutboxLoopCapsJobsPerTenant pins the fairness bound: a tenant whose
// queue never reports idle gets at most maxJobsPerTenantPerTick deliveries
// per tick, so one huge backlog cannot starve the other tenants.
func TestRunOutboxLoopCapsJobsPerTenant(t *testing.T) {
	tenants := &fakeTenantLister{tenants: testTenants(11)}
	worker := &fakeOutboxDeliverer{process: func(context.Context, int64) (bool, error) {
		return true, nil // never idle
	}}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runOutboxLoop(ctx, tenants, worker, discardLogger())

	if len(worker.calls) != maxJobsPerTenantPerTick {
		t.Fatalf("deliveries = %d, want the %d cap", len(worker.calls), maxJobsPerTenantPerTick)
	}
}

// TestRunOutboxLoopSurvivesTenantListingFailure pins the failure mode: when
// the tenant listing fails, the loop logs and waits for the next tick instead
// of dying -- and the worker is never touched without a tenant.
func TestRunOutboxLoopSurvivesTenantListingFailure(t *testing.T) {
	tenants := &fakeTenantLister{err: errors.New("database away")}
	worker := &fakeOutboxDeliverer{process: func(context.Context, int64) (bool, error) {
		t.Error("worker must not run without a tenant list")
		return false, nil
	}}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runOutboxLoop(ctx, tenants, worker, discardLogger())

	if tenants.calls != 1 || len(worker.calls) != 0 {
		t.Fatalf("listings = %d, worker calls = %d; want 1 and 0", tenants.calls, len(worker.calls))
	}
}

// TestRunMediaLoopFetchesPerTenant pins the fetch pacing: one ProcessTenant
// per listed tenant per iteration, then stop on shutdown.
func TestRunMediaLoopFetchesPerTenant(t *testing.T) {
	tenants := &fakeTenantLister{tenants: testTenants(11, 22)}
	fetcher := &fakeMediaFetcher{stored: 3}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runMediaLoop(ctx, tenants, fetcher, discardLogger())

	if len(fetcher.calls) != 2 || fetcher.calls[0] != 11 || fetcher.calls[1] != 22 {
		t.Fatalf("fetcher visited %v, want [11 22] in order", fetcher.calls)
	}
}

// TestRunBacklogLoopCountsOnce pins the gauge refresh: one count per
// iteration with the full tenant list, then stop on shutdown.
func TestRunBacklogLoopCountsOnce(t *testing.T) {
	tenants := &fakeTenantLister{tenants: testTenants(11)}
	counter := &fakeBacklogCounter{value: 42}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runBacklogLoop(ctx, tenants, counter, discardLogger())

	if counter.calls != 1 {
		t.Fatalf("backlog counted %d times, want 1 iteration", counter.calls)
	}
}

// TestRunRetentionLoopTicksCyclesAndStops pins the loop mechanics: every tick
// runs a full cycle, and shutdown stops the loop promptly.
func TestRunRetentionLoopTicksCyclesAndStops(t *testing.T) {
	tenants := &fakeTenantLister{tenants: testTenants(11)}
	msgs := &phaseRecorder{}
	ob := &phaseRecorder{}
	media := &fakeMediaRetention{}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runRetentionLoop(ctx, tenants, msgs, ob, media, discardLogger(), 10*time.Millisecond)
	}()
	// Let a few ticks fire, then stop: the loop must exit, not hang on the
	// ticker.
	time.Sleep(55 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runRetentionLoop did not stop on shutdown")
	}
	if len(msgs.calls) == 0 {
		t.Fatal("no retention cycle ran across several ticks")
	}
}

// TestRunFailsFastWithoutConfiguration pins the startup contract: without
// any environment, run returns the configuration error instead of booting
// half-wired (no migrations, no pool, no Telegram client).
func TestRunFailsFastWithoutConfiguration(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("MIGRATION_DATABASE_URL", "")
	t.Setenv("TELEGRAM_BOT_TOKEN", "")
	if err := run(discardLogger()); err == nil {
		t.Fatal("run() without configuration = nil, want the config error")
	}
}

// TestRunRetentionOnceRunsPhasesInOrder pins the cycle: tenants, then text,
// then outbox payloads, then the media tree -- the only I/O-bound phase runs
// last.
func TestRunRetentionOnceRunsPhasesInOrder(t *testing.T) {
	tenants := &fakeTenantLister{tenants: testTenants(11)}
	msgs := &phaseRecorder{}
	ob := &phaseRecorder{}
	media := &fakeMediaRetention{}

	runRetentionOnce(context.Background(), tenants, msgs, ob, media, discardLogger())

	want := []string{"purgeExpired", "purgeExpired", "run"}
	got := append(append([]string{}, msgs.calls...), ob.calls...)
	got = append(got, media.calls...)
	if len(got) != len(want) {
		t.Fatalf("phases = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("phases = %v, want %v", got, want)
		}
	}
}

// TestRunRetentionOnceSkipsLaterPhasesOnFailure pins the short-circuit: a
// failing text purge skips the outbox and media phases (its own error is
// already logged), and a failing outbox purge skips the media phase. The
// media phase failure itself still closes the cycle.
func TestRunRetentionOnceSkipsLaterPhasesOnFailure(t *testing.T) {
	t.Run("messages failure skips outbox and media", func(t *testing.T) {
		tenants := &fakeTenantLister{tenants: testTenants(11)}
		msgs := &phaseRecorder{errOn: "purgeExpired"}
		ob := &phaseRecorder{}
		media := &fakeMediaRetention{}

		runRetentionOnce(context.Background(), tenants, msgs, ob, media, discardLogger())

		if len(ob.calls) != 0 || len(media.calls) != 0 {
			t.Fatalf("outbox calls = %v, media calls = %v; want none after a messages failure", ob.calls, media.calls)
		}
	})

	t.Run("outbox failure skips media", func(t *testing.T) {
		tenants := &fakeTenantLister{tenants: testTenants(11)}
		ob := &failingOutbox{}
		media := &fakeMediaRetention{}

		runRetentionOnce(context.Background(), tenants, &phaseRecorder{}, ob, media, discardLogger())

		if len(media.calls) != 0 {
			t.Fatalf("media calls = %v; want none after an outbox failure", media.calls)
		}
	})

	t.Run("media failure still closes the cycle", func(t *testing.T) {
		tenants := &fakeTenantLister{tenants: testTenants(11)}
		msgs := &phaseRecorder{}
		ob := &phaseRecorder{}
		media := &fakeMediaRetention{err: errors.New("disk away")}

		// Must not panic, hang or skip the summary: it only logs.
		runRetentionOnce(context.Background(), tenants, msgs, ob, media, discardLogger())

		if len(msgs.calls) != 1 || len(ob.calls) != 1 || len(media.calls) != 1 {
			t.Fatal("every phase must have run despite the media failure")
		}
	})
}

type failingOutbox struct{ calls []string }

func (f *failingOutbox) PurgeExpired(context.Context, []users.TenantRetention) (int64, error) {
	f.calls = append(f.calls, "purgeExpired")
	return 0, errors.New("outbox purge exploded")
}

// TestRunRetentionOnceSurvivesTenantListingFailure pins the first failure
// mode: no tenant list, no phase runs, no panic.
func TestRunRetentionOnceSurvivesTenantListingFailure(t *testing.T) {
	tenants := &fakeTenantLister{err: errors.New("database away")}
	msgs := &phaseRecorder{}
	ob := &phaseRecorder{}
	media := &fakeMediaRetention{}

	runRetentionOnce(context.Background(), tenants, msgs, ob, media, discardLogger())

	if len(msgs.calls) != 0 || len(ob.calls) != 0 || len(media.calls) != 0 {
		t.Fatal("no phase may run without a tenant list")
	}
}
