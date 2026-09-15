package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/LouisMoretti/Undelete/bot/internal/media/purge"
	"github.com/LouisMoretti/Undelete/bot/internal/metrics"
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
	// afterRun, when set, runs at the end of every Run (e.g. a shutdown).
	afterRun func()
}

func (f *fakeMediaRetention) Run(context.Context, []users.TenantRetention) (purge.Stats, error) {
	f.calls = append(f.calls, "run")
	if f.afterRun != nil {
		f.afterRun()
	}
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

// TestRunOutboxLoopServesQuietTenantPastSaturatedOne pins the issue #19
// fairness half for notifications: a tenant whose queue never reports idle
// (saturated, over quota, huge backlog) is capped per tick, so a quiet
// tenant listed after it is still served in the same iteration -- a
// saturated tenant never blocks the others.
func TestRunOutboxLoopServesQuietTenantPastSaturatedOne(t *testing.T) {
	tenants := &fakeTenantLister{tenants: testTenants(11, 22)}
	served := map[int64]int{}
	worker := &fakeOutboxDeliverer{process: func(_ context.Context, ownerUserID int64) (bool, error) {
		served[ownerUserID]++
		if ownerUserID == 11 {
			return true, nil // saturated: never idle
		}
		return false, nil // quiet: idle at once
	}}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runOutboxLoop(ctx, tenants, worker, discardLogger())

	if served[11] != maxJobsPerTenantPerTick {
		t.Fatalf("saturated tenant served %d, want the %d cap", served[11], maxJobsPerTenantPerTick)
	}
	if served[22] != 1 {
		t.Fatalf("quiet tenant served %d, want 1 in the same tick", served[22])
	}
}

// TestRunMediaLoopServesQuietTenantPastBusyOne pins the issue #19 fairness
// half for downloads: the fetch loop visits tenants in order whatever the
// previous one stored, so one tenant's burst never starves another's.
func TestRunMediaLoopServesQuietTenantPastBusyOne(t *testing.T) {
	tenants := &fakeTenantLister{tenants: testTenants(11, 22)}
	fetcher := &fakeMediaFetcher{stored: 20}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runMediaLoop(ctx, tenants, fetcher, discardLogger())

	if len(fetcher.calls) != 2 || fetcher.calls[0] != 11 || fetcher.calls[1] != 22 {
		t.Fatalf("fetcher visited %v, want [11 22] in order despite tenant 11's burst", fetcher.calls)
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

// TestRunRetentionLoopRunsACycleAtStart pins the boot pass: the first cycle
// runs before the first tick, so a process restarted more often than the
// interval still purges.
func TestRunRetentionLoopRunsACycleAtStart(t *testing.T) {
	tenants := &fakeTenantLister{tenants: testTenants(11)}
	msgs := &phaseRecorder{}
	ob := &phaseRecorder{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Shut down as soon as the first cycle ends: with an hour-long interval,
	// only a boot pass can have run by then.
	media := &fakeMediaRetention{afterRun: cancel}

	runRetentionLoop(ctx, tenants, msgs, ob, media, discardLogger(), time.Hour)

	if len(msgs.calls) != 1 || len(ob.calls) != 1 || len(media.calls) != 1 {
		t.Fatalf("cycles at start: messages %v, outbox %v, media %v; want exactly one each", msgs.calls, ob.calls, media.calls)
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

// TestRunRetentionOnceRunsEveryPhaseDespiteFailures pins the phase
// independence: a failing text purge must not starve the outbox and media
// phases of their daily pass, and a failing outbox purge must not starve the
// media phase. The media phase failure itself still closes the cycle.
func TestRunRetentionOnceRunsEveryPhaseDespiteFailures(t *testing.T) {
	t.Run("messages failure still runs outbox and media", func(t *testing.T) {
		tenants := &fakeTenantLister{tenants: testTenants(11)}
		msgs := &phaseRecorder{errOn: "purgeExpired"}
		ob := &phaseRecorder{}
		media := &fakeMediaRetention{}

		runRetentionOnce(context.Background(), tenants, msgs, ob, media, discardLogger())

		if len(ob.calls) != 1 || len(media.calls) != 1 {
			t.Fatalf("outbox calls = %v, media calls = %v; want one each after a messages failure", ob.calls, media.calls)
		}
	})

	t.Run("outbox failure still runs media", func(t *testing.T) {
		tenants := &fakeTenantLister{tenants: testTenants(11)}
		ob := &failingOutbox{}
		media := &fakeMediaRetention{}

		runRetentionOnce(context.Background(), tenants, &phaseRecorder{}, ob, media, discardLogger())

		if len(media.calls) != 1 {
			t.Fatalf("media calls = %v; want one after an outbox failure", media.calls)
		}
	})

	t.Run("shutdown between phases stops the cycle", func(t *testing.T) {
		tenants := &fakeTenantLister{tenants: testTenants(11)}
		ctx, cancel := context.WithCancel(context.Background())
		msgs := &cancellingPurge{cancel: cancel}
		ob := &phaseRecorder{}
		media := &fakeMediaRetention{}

		runRetentionOnce(ctx, tenants, msgs, ob, media, discardLogger())

		if len(ob.calls) != 0 || len(media.calls) != 0 {
			t.Fatalf("outbox calls = %v, media calls = %v; want none once shutdown began", ob.calls, media.calls)
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

// cancellingPurge simulates a shutdown arriving during the text purge.
type cancellingPurge struct{ cancel context.CancelFunc }

func (c *cancellingPurge) PurgeExpired(context.Context, []users.TenantRetention) (int64, error) {
	c.cancel()
	return 0, context.Canceled
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

// logCancellingHandler is a slog handler that records every logged message
// and cancels the context under test as soon as the watched message is
// logged. It turns "the loop logs the error, then stops at the next tick
// without hanging on it" into a deterministic sequence: the log call is
// synchronous in the loop's goroutine, so once Handle has cancelled, the
// following select can only see ctx.Done (the tickers are seconds away).
type logCancellingHandler struct {
	cancel context.CancelFunc
	watch  string
	msgs   []string
}

func (h *logCancellingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *logCancellingHandler) Handle(_ context.Context, r slog.Record) error {
	h.msgs = append(h.msgs, r.Message)
	if r.Message == h.watch {
		h.cancel()
	}
	return nil
}

func (h *logCancellingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *logCancellingHandler) WithGroup(string) slog.Handler      { return h }

func (h *logCancellingHandler) seen(msg string) bool {
	for _, m := range h.msgs {
		if m == msg {
			return true
		}
	}
	return false
}

// TestRunOutboxLoopBreaksTenantDrainOnErrorButServesNext pins the outbox
// failure mode: a ProcessOne error breaks THAT tenant's drain for the tick
// (its queue is retried on the next one), but the loop still moves on to the
// tenants listed after it -- one broken tenant never stops the whole delivery
// tick.
func TestRunOutboxLoopBreaksTenantDrainOnErrorButServesNext(t *testing.T) {
	tenants := &fakeTenantLister{tenants: testTenants(11, 22)}
	served := map[int64]int{}
	worker := &fakeOutboxDeliverer{process: func(_ context.Context, ownerUserID int64) (bool, error) {
		served[ownerUserID]++
		if ownerUserID == 11 {
			return true, errors.New("send away") // error mid-drain
		}
		return false, nil
	}}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runOutboxLoop(ctx, tenants, worker, discardLogger())

	if served[11] != 1 {
		t.Fatalf("failing tenant drained %d times, want the drain broken after 1 error", served[11])
	}
	if served[22] != 1 {
		t.Fatalf("next tenant served %d times, want 1 after the failing tenant", served[22])
	}
}

// TestRunMediaLoopLogsFetchErrorAndStillServesNextTenant pins the media
// failure mode: a tenant whose download fails is logged (the context is still
// live) and skipped for this iteration, and the tenants listed after it are
// still fetched in the same pass. The second tenant's call cancels the
// context, which stops the loop deterministically before the 5s tick.
func TestRunMediaLoopLogsFetchErrorAndStillServesNextTenant(t *testing.T) {
	tenants := &fakeTenantLister{tenants: testTenants(11, 22)}
	ctx, cancel := context.WithCancel(context.Background())
	fetcher := &scriptedMediaFetcher{cancel: cancel}

	done := make(chan struct{})
	go func() {
		defer close(done)
		runMediaLoop(ctx, tenants, fetcher, discardLogger())
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runMediaLoop did not stop after its iteration")
	}

	if len(fetcher.calls) != 2 || fetcher.calls[0] != 11 || fetcher.calls[1] != 22 {
		t.Fatalf("fetcher visited %v, want [11 22]: the failing tenant must not stop the pass", fetcher.calls)
	}
}

// scriptedMediaFetcher fails tenant 11's download and, once tenant 22 has been
// served (proving the pass continued), cancels the context to stop the loop.
type scriptedMediaFetcher struct {
	calls  []int64
	cancel context.CancelFunc
}

func (f *scriptedMediaFetcher) ProcessTenant(_ context.Context, ownerUserID int64) (int, error) {
	f.calls = append(f.calls, ownerUserID)
	if ownerUserID == 11 {
		return 0, errors.New("getFile away")
	}
	f.cancel()
	return 2, nil
}

// TestRunMediaLoopLogsListingFailureAndStops pins the media listing failure:
// the error is logged while the context is live (unlike the shutdown path,
// which stays silent), no tenant is fetched, and the loop stops on the
// shutdown instead of waiting out the media tick.
func TestRunMediaLoopLogsListingFailureAndStops(t *testing.T) {
	tenants := &fakeTenantLister{err: errors.New("database away")}
	fetcher := &fakeMediaFetcher{}
	ctx, cancel := context.WithCancel(context.Background())
	handler := &logCancellingHandler{cancel: cancel, watch: "media fetch: failed to list tenants"}

	done := make(chan struct{})
	go func() {
		defer close(done)
		runMediaLoop(ctx, tenants, fetcher, slog.New(handler))
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runMediaLoop did not stop after the listing failure")
	}

	if !handler.seen(handler.watch) {
		t.Fatalf("listing failure not logged; messages = %v", handler.msgs)
	}
	if len(fetcher.calls) != 0 {
		t.Fatal("fetcher must not run without a tenant list")
	}
}

// TestRunBacklogLoopLogsCountFailureAndStops pins the backlog failure mode:
// a failing COUNT(*) is logged while the context is live, leaves the gauge
// untouched, and the loop stops on the shutdown instead of waiting out the
// backlog tick.
func TestRunBacklogLoopLogsCountFailureAndStops(t *testing.T) {
	tenants := &fakeTenantLister{tenants: testTenants(11)}
	counter := &fakeBacklogCounter{err: errors.New("count away")}
	ctx, cancel := context.WithCancel(context.Background())
	handler := &logCancellingHandler{cancel: cancel, watch: "outbox backlog: count failed"}

	done := make(chan struct{})
	go func() {
		defer close(done)
		runBacklogLoop(ctx, tenants, counter, slog.New(handler))
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runBacklogLoop did not stop after the count failure")
	}

	if !handler.seen(handler.watch) {
		t.Fatalf("count failure not logged; messages = %v", handler.msgs)
	}
}

// TestRunBacklogLoopPublishesCountToGauge pins the observable end of the
// backlog loop: the value CountBacklog returns in an iteration is what the
// undelete_outbox_backlog gauge exposes -- the loop is not just calling the
// counter, its result reaches /metrics.
func TestRunBacklogLoopPublishesCountToGauge(t *testing.T) {
	tenants := &fakeTenantLister{tenants: testTenants(11)}
	counter := &fakeBacklogCounter{value: 37}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runBacklogLoop(ctx, tenants, counter, discardLogger())

	if !strings.Contains(metrics.Default().RenderPrometheus(), "undelete_outbox_backlog 37\n") {
		t.Fatal("undelete_outbox_backlog does not expose the counted backlog")
	}
}

// TestRunTakesTheInstanceLockFirst pins the boot order at the unit level:
// with a loadable configuration, run() takes the instance lock BEFORE
// anything else, migrations included (a new version must not migrate under
// an old one still serving) -- and an unreachable database fails the boot at
// once instead of waiting. Both DSNs point at unix socket directories that
// cannot exist, so the connections fail instantly and without any network.
func TestRunTakesTheInstanceLockFirst(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://app:app@/app?host=/tmp/opencode/no-such-app-socket&sslmode=disable")
	t.Setenv("MIGRATION_DATABASE_URL", "postgres://mig:mig@/mig?host=/tmp/opencode/no-such-socket&sslmode=disable")
	t.Setenv("TELEGRAM_BOT_TOKEN", "0:test-token")

	err := run(discardLogger())
	if err == nil {
		t.Fatal("run() with an unreachable database = nil, want the instance lock failure")
	}
	if !strings.Contains(err.Error(), "connecting for the instance lock") {
		t.Fatalf("run() error = %q, want the instance lock failure: it must precede the migrations", err)
	}
}
