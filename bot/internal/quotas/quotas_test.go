package quotas

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeSource is a controllable UsageSource: fixed values, injected errors,
// call counts proving the memoisation, and an optional gate proving the
// tracker's lock is never held across a database read.
type fakeSource struct {
	mu       sync.Mutex
	messages int64
	files    int64
	bytes    int64
	err      error
	calls    map[string]int
	// gate, when non-nil, blocks every query until closed.
	gate chan struct{}
}

func (f *fakeSource) counts() (int64, int64, int64, error) {
	if f.gate != nil {
		<-f.gate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls == nil {
		f.calls = make(map[string]int)
	}
	return f.messages, f.files, f.bytes, f.err
}

func (f *fakeSource) CountMessages(context.Context, int64) (int64, error) {
	m, _, _, err := f.counts()
	f.mu.Lock()
	f.calls["messages"]++
	f.mu.Unlock()
	return m, err
}

func (f *fakeSource) CountMediaFiles(context.Context, int64) (int64, error) {
	_, fi, _, err := f.counts()
	f.mu.Lock()
	f.calls["files"]++
	f.mu.Unlock()
	return fi, err
}

func (f *fakeSource) SumStoredMediaBytes(context.Context, int64) (int64, error) {
	_, _, b, err := f.counts()
	f.mu.Lock()
	f.calls["bytes"]++
	f.mu.Unlock()
	return b, err
}

func (f *fakeSource) totalCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls["messages"] + f.calls["files"] + f.calls["bytes"]
}

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func testLimits() Limits {
	return Limits{
		MaxMessages:       10,
		MaxMediaFiles:     5,
		MaxMediaBytes:     1000,
		CapturesPerMinute: 1000,
		WarnPercent:       50,
	}
}

func mustTracker(t *testing.T, limits Limits, source UsageSource, now func() time.Time) *Tracker {
	t.Helper()
	tr, err := NewTracker(limits, source, now)
	if err != nil {
		t.Fatalf("NewTracker: %v", err)
	}
	return tr
}

func TestLimitsValidate(t *testing.T) {
	if err := DefaultLimits().Validate(); err != nil {
		t.Fatalf("DefaultLimits invalid: %v", err)
	}
	bad := DefaultLimits()
	for _, mutate := range []func(*Limits){
		func(l *Limits) { l.MaxMessages = 0 },
		func(l *Limits) { l.MaxMediaFiles = -1 },
		func(l *Limits) { l.MaxMediaBytes = 0 },
		func(l *Limits) { l.CapturesPerMinute = 0 },
		func(l *Limits) { l.WarnPercent = 0 },
		func(l *Limits) { l.WarnPercent = 100 },
	} {
		limits := DefaultLimits()
		mutate(&limits)
		if err := limits.Validate(); err == nil {
			t.Fatalf("Validate(%+v) = nil, want an error", limits)
		}
		if _, err := NewTracker(limits, nil, nil); err == nil {
			t.Fatalf("NewTracker(%+v) = nil error, want a refusal", limits)
		}
	}
	_ = bad
}

func TestAdmitCaptureWarnsOnceThenRefuses(t *testing.T) {
	clock := &testClock{now: time.Now()}
	tr := mustTracker(t, testLimits(), nil, clock.Now)
	ctx := context.Background()

	// Threshold is 10/100*50 = 5: admits 1-4 silent, 5th warns once.
	for i := int64(1); i <= 4; i++ {
		adm := tr.AdmitCapture(ctx, 11)
		if !adm.Allowed || adm.Warn {
			t.Fatalf("admit %d = %+v, want allowed without warn", i, adm)
		}
	}
	if adm := tr.AdmitCapture(ctx, 11); !adm.Allowed || !adm.Warn {
		t.Fatalf("admit 5 = %+v, want allowed WITH the pre-saturation warn", adm)
	}
	for i := int64(6); i <= 10; i++ {
		adm := tr.AdmitCapture(ctx, 11)
		if !adm.Allowed || adm.Warn {
			t.Fatalf("admit %d = %+v, want allowed without a second warn", i, adm)
		}
	}
	// 11th: fresh refusal, warned again (the block is worth one alert).
	adm := tr.AdmitCapture(ctx, 11)
	if adm.Allowed || !adm.Warn || adm.Quota != QuotaMessages {
		t.Fatalf("admit 11 = %+v, want a fresh messages refusal with warn", adm)
	}
	if adm.Usage != 10 || adm.Limit != 10 {
		t.Fatalf("refusal quotes usage=%d limit=%d, want 10/10", adm.Usage, adm.Limit)
	}
	// 12th: memoised repeat -- refused, but silent.
	adm = tr.AdmitCapture(ctx, 11)
	if adm.Allowed || adm.Warn {
		t.Fatalf("repeat admit = %+v, want a silent memoised refusal", adm)
	}
}

func TestAdmitCaptureRateBoundsBursts(t *testing.T) {
	clock := &testClock{now: time.Now()}
	limits := testLimits()
	limits.CapturesPerMinute = 3
	tr := mustTracker(t, limits, nil, clock.Now)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if adm := tr.AdmitCapture(ctx, 11); !adm.Allowed {
			t.Fatalf("burst admit %d refused: %+v", i, adm)
		}
	}
	adm := tr.AdmitCapture(ctx, 11)
	if adm.Allowed || adm.Quota != QuotaCaptureRate {
		t.Fatalf("4th burst admit = %+v, want a capture_rate refusal", adm)
	}
	// The window slides: a minute later the same tenant is served again.
	clock.Advance(61 * time.Second)
	if adm := tr.AdmitCapture(ctx, 11); !adm.Allowed {
		t.Fatalf("admit after the window = %+v, want allowed", adm)
	}
}

func TestSeedsFromSourceOnFirstTouch(t *testing.T) {
	src := &fakeSource{messages: 7, files: 2, bytes: 100}
	tr := mustTracker(t, testLimits(), src, nil)
	ctx := context.Background()

	if _, _, _, seeded := tr.Usage(11); seeded {
		t.Fatal("untouched tenant reports seeded")
	}
	adm := tr.AdmitCapture(ctx, 11)
	if !adm.Allowed {
		t.Fatalf("seeded admit = %+v, want allowed", adm)
	}
	msgs, files, bytes, seeded := tr.Usage(11)
	if !seeded || msgs != 8 || files != 2 || bytes != 100 {
		t.Fatalf("usage = (%d,%d,%d,%v), want (8,2,100,true)", msgs, files, bytes, seeded)
	}
}

func TestRestartReseedsFromDatabase(t *testing.T) {
	src := &fakeSource{messages: 10, files: 5, bytes: 1000}
	ctx := context.Background()

	first := mustTracker(t, testLimits(), src, nil)
	if adm := first.AdmitCapture(ctx, 11); adm.Allowed {
		t.Fatalf("first tracker admit = %+v, want refused at the seeded limit", adm)
	}
	// A restart builds a new tracker over the same database: the very first
	// admission must refuse again, not grant a fresh budget.
	second := mustTracker(t, testLimits(), src, nil)
	adm := second.AdmitCapture(ctx, 11)
	if adm.Allowed {
		t.Fatalf("post-restart admit = %+v, want refused: the quota survived the restart", adm)
	}
	if msgs, _, _, seeded := second.Usage(11); !seeded || msgs != 10 {
		t.Fatalf("post-restart usage = (%d,%v), want (10,true)", msgs, seeded)
	}
}

func TestShrinkHealsBlockAfterRecheck(t *testing.T) {
	clock := &testClock{now: time.Now()}
	src := &fakeSource{messages: 10}
	tr := mustTracker(t, testLimits(), src, clock.Now)
	ctx := context.Background()

	if adm := tr.AdmitCapture(ctx, 11); adm.Allowed {
		t.Fatalf("saturated admit = %+v, want refused", adm)
	}
	queries := src.totalCalls()
	// Memoised: repeats refuse without touching the source.
	for i := 0; i < 5; i++ {
		if adm := tr.AdmitCapture(ctx, 11); adm.Allowed || adm.Warn {
			t.Fatalf("repeat %d = %+v, want a silent memoised refusal", i, adm)
		}
	}
	if got := src.totalCalls(); got != queries {
		t.Fatalf("memoised repeats cost %d source queries, want 0", got-queries)
	}
	// Meanwhile the retention purge (or an erasure) shrank the truth. Before
	// the memo expires the ledger still refuses -- the memo is the point.
	src.mu.Lock()
	src.messages = 3
	src.mu.Unlock()
	if adm := tr.AdmitCapture(ctx, 11); adm.Allowed {
		t.Fatalf("admit inside the memo = %+v, want refused until the recheck", adm)
	}
	// Past the recheck the admission re-verifies, resyncs and allows.
	clock.Advance(recheckInterval + time.Second)
	adm := tr.AdmitCapture(ctx, 11)
	if !adm.Allowed {
		t.Fatalf("admit after shrink + recheck = %+v, want allowed", adm)
	}
	if msgs, _, _, _ := tr.Usage(11); msgs != 4 {
		t.Fatalf("resynced usage = %d, want 3 + the admitted one", msgs)
	}
}

func TestSaturatedTenantDoesNotBlockOthers(t *testing.T) {
	clock := &testClock{now: time.Now()}
	src := &fakeSource{messages: 0}
	tr := mustTracker(t, testLimits(), src, clock.Now)
	ctx := context.Background()

	// Seed both tenants while the source is fast.
	if adm := tr.AdmitCapture(ctx, 11); !adm.Allowed {
		t.Fatalf("seed 11 = %+v, want allowed", adm)
	}
	if adm := tr.AdmitCapture(ctx, 22); !adm.Allowed {
		t.Fatalf("seed 22 = %+v, want allowed", adm)
	}
	// Saturate tenant 11 through its in-memory ledger.
	for i := 0; i < 9; i++ {
		if adm := tr.AdmitCapture(ctx, 11); !adm.Allowed {
			t.Fatalf("setup admit %d refused: %+v", i, adm)
		}
	}
	// The database agrees tenant 11 is saturated; its next admission must
	// take the verifying slow path. Gate the source: that query now hangs.
	src.mu.Lock()
	src.messages = 10
	src.mu.Unlock()
	src.gate = make(chan struct{})

	stuck := make(chan Admission, 1)
	go func() { stuck <- tr.AdmitCapture(ctx, 11) }()
	select {
	case <-stuck:
		t.Fatal("tenant 11 admission returned while its verification was gated")
	case <-time.After(200 * time.Millisecond):
	}
	// Tenant 22 is already seeded and under quota: its admission is pure
	// memory and must not wait behind tenant 11's query.
	before := src.totalCalls()
	select {
	case adm := <-func() chan Admission {
		fast := make(chan Admission, 1)
		go func() { fast <- tr.AdmitCapture(ctx, 22) }()
		return fast
	}():
		if !adm.Allowed {
			t.Fatalf("tenant 22 admit = %+v, want allowed while 11 is stuck", adm)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("tenant 22 admission stalled behind tenant 11: the lock is held across I/O")
	}
	if got := src.totalCalls(); got != before {
		t.Fatalf("tenant 22 fast path cost %d source queries, want 0", got-before)
	}
	close(src.gate)
	if adm := <-stuck; adm.Allowed {
		t.Fatalf("tenant 11 admit = %+v, want refused after its verification", adm)
	}
}

func TestSlowSourceNeverStallsAnotherTenant(t *testing.T) {
	release := make(chan struct{})
	src := &fakeSource{messages: 0, gate: release}
	tr := mustTracker(t, testLimits(), src, nil)
	ctx := context.Background()

	// Tenant 11 starts a seed that blocks inside the source (lock released).
	first := make(chan Admission, 1)
	go func() { first <- tr.AdmitCapture(ctx, 11) }()
	select {
	case <-first:
		t.Fatal("tenant 11 admission returned before the gate opened")
	case <-time.After(200 * time.Millisecond):
	}
	// Tenant 22's admission must not wait behind tenant 11's query... it also
	// needs the source for its own first touch, so it blocks on the GATE, not
	// on the tracker's lock: prove the lock is free by reading Usage.
	usageDone := make(chan struct{})
	go func() {
		defer close(usageDone)
		tr.Usage(22)
	}()
	select {
	case <-usageDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Usage blocked while another tenant seeded: the lock is held across I/O")
	}
	close(release)
	if adm := <-first; !adm.Allowed {
		t.Fatalf("tenant 11 admit = %+v, want allowed after the gate", adm)
	}
}

func TestConcurrentAdmitsAreExact(t *testing.T) {
	limits := testLimits()
	limits.MaxMessages = 200
	limits.CapturesPerMinute = 100000
	tr := mustTracker(t, limits, nil, nil)
	ctx := context.Background()

	var allowed atomic.Int64
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				if adm := tr.AdmitCapture(ctx, 11); adm.Allowed {
					allowed.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	if got := allowed.Load(); got != 200 {
		t.Fatalf("concurrent admits allowed %d, want exactly 200", got)
	}
	if msgs, _, _, _ := tr.Usage(11); msgs != 200 {
		t.Fatalf("ledger messages = %d, want 200", msgs)
	}
}

func TestTenantMapStaysBounded(t *testing.T) {
	tr := mustTracker(t, testLimits(), nil, nil)
	ctx := context.Background()
	for id := int64(1); id <= maxTenants+10; id++ {
		if adm := tr.AdmitCapture(ctx, id); !adm.Allowed {
			t.Fatalf("admit for tenant %d refused: %+v", id, adm)
		}
	}
	// The first tenants were evicted: they report unseeded and re-seed
	// cleanly on their next admission.
	if _, _, _, seeded := tr.Usage(1); seeded {
		t.Fatal("tenant 1 still tracked past the bound: the map grows without limit")
	}
	if adm := tr.AdmitCapture(ctx, 1); !adm.Allowed {
		t.Fatalf("evicted tenant re-admit = %+v, want allowed after re-seed", adm)
	}
}

func TestMediaBytesGateAndAccounting(t *testing.T) {
	clock := &testClock{now: time.Now()}
	tr := mustTracker(t, testLimits(), nil, clock.Now)
	ctx := context.Background()

	// Threshold is 1000/100*50 = 500.
	if adm := tr.AdmitMediaBytes(ctx, 11); !adm.Allowed || adm.Warn {
		t.Fatalf("bytes gate = %+v, want allowed without warn", adm)
	}
	if adm := tr.AddMediaBytes(ctx, 11, 499); !adm.Allowed || adm.Warn {
		t.Fatalf("add 499 = %+v, want allowed without warn", adm)
	}
	if adm := tr.AddMediaBytes(ctx, 11, 1); !adm.Allowed || !adm.Warn {
		t.Fatalf("add 1 = %+v, want allowed WITH the crossing warn", adm)
	}
	if adm := tr.AddMediaBytes(ctx, 11, 500); !adm.Allowed || adm.Warn {
		t.Fatalf("add 500 = %+v, want allowed without a second warn", adm)
	}
	// At exactly the limit the next download is refused (fresh, warned).
	if adm := tr.AdmitMediaBytes(ctx, 11); adm.Allowed || !adm.Warn {
		t.Fatalf("bytes gate at limit = %+v, want a fresh refusal with warn", adm)
	}
	if adm := tr.AdmitMediaBytes(ctx, 11); adm.Allowed || adm.Warn {
		t.Fatalf("bytes repeat = %+v, want a silent memoised refusal", adm)
	}
}

func TestMediaFileCountIsIndependent(t *testing.T) {
	tr := mustTracker(t, testLimits(), nil, nil)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if adm := tr.AdmitMediaFile(ctx, 11); !adm.Allowed {
			t.Fatalf("media file %d refused: %+v", i, adm)
		}
	}
	adm := tr.AdmitMediaFile(ctx, 11)
	if adm.Allowed || adm.Quota != QuotaMediaFiles {
		t.Fatalf("6th media file = %+v, want a media_files refusal", adm)
	}
	// Exhausting the media-file quota leaves the message quota untouched.
	if adm := tr.AdmitCapture(ctx, 11); !adm.Allowed {
		t.Fatalf("message admit = %+v, want allowed: quotas are independent", adm)
	}
}

func TestSourceFailureFailsOpen(t *testing.T) {
	src := &fakeSource{err: fmt.Errorf("database away")}
	tr := mustTracker(t, testLimits(), src, nil)
	if adm := tr.AdmitCapture(context.Background(), 11); !adm.Allowed {
		t.Fatalf("admit on source failure = %+v, want fail-open allowed", adm)
	}
}

func TestInvalidOwnerIsRefused(t *testing.T) {
	tr := mustTracker(t, testLimits(), nil, nil)
	ctx := context.Background()
	for _, owner := range []int64{0, -3} {
		if adm := tr.AdmitCapture(ctx, owner); adm.Allowed {
			t.Fatalf("AdmitCapture(%d) = %+v, want refused", owner, adm)
		}
		if adm := tr.AdmitMediaFile(ctx, owner); adm.Allowed {
			t.Fatalf("AdmitMediaFile(%d) = %+v, want refused", owner, adm)
		}
		if adm := tr.AdmitMediaBytes(ctx, owner); adm.Allowed {
			t.Fatalf("AdmitMediaBytes(%d) = %+v, want refused", owner, adm)
		}
	}
}
