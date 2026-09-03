package outbox

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/LouisMoretti/Undelete/bot/internal/metrics"
	"github.com/LouisMoretti/Undelete/bot/internal/telegram"
)

// fakeStore keeps the clock that the real Repository delegates to PostgreSQL:
// the worker only passes durations, so it is the store that timestamps the
// transitions. Tests advance `clock` explicitly.
type fakeStore struct {
	job       *Job
	clock     time.Time
	claimedAt time.Time
	lease     time.Duration
	sent      bool
	failed    bool
	retryAt   time.Time
	retryIn   time.Duration
	attempts  int
}

func (s *fakeStore) Claim(_ context.Context, _ int64, lease time.Duration) (*Job, error) {
	if s.job == nil || s.sent || s.failed || (!s.retryAt.IsZero() && s.clock.Before(s.retryAt)) || (!s.claimedAt.IsZero() && s.clock.Before(s.claimedAt.Add(s.lease))) {
		return nil, nil
	}
	s.claimedAt, s.lease = s.clock, lease
	copy := *s.job
	copy.Attempts = s.attempts
	return &copy, nil
}
func (s *fakeStore) MarkSent(context.Context, int64, int64, string) error {
	s.sent = true
	return nil
}
func (s *fakeStore) MarkRetry(_ context.Context, _, _ int64, _ string, wait time.Duration, _ string) error {
	s.attempts++
	s.retryIn = wait
	s.retryAt, s.claimedAt = s.clock.Add(wait), time.Time{}
	return nil
}
func (s *fakeStore) MarkFailed(context.Context, int64, int64, string, string) error {
	s.failed = true
	return nil
}

type fakeSender struct {
	err      error
	requests []telegram.SendMessageRequest
}

func (s *fakeSender) SendMessageOnce(_ context.Context, req telegram.SendMessageRequest) error {
	s.requests = append(s.requests, req)
	return s.err
}

func newTestWorker(store Store, sender Sender, logBuffer *bytes.Buffer) *Worker {
	return NewWorker(store, sender, slog.New(slog.NewJSONHandler(logBuffer, nil)))
}

func testJob() *Job {
	return &Job{ID: 7, OwnerUserID: 11, OwnerTelegramUserID: 42, BusinessConnectionID: "bc-secret", ChatID: 99, MessageID: 3, EventType: EventDeletedMessage, Text: "private content"}
}

func TestWorkerSuccessSendsAsBotAndMarksSent(t *testing.T) {
	store := &fakeStore{job: testJob()}
	sender := &fakeSender{}
	var logs bytes.Buffer
	worker := newTestWorker(store, sender, &logs)

	processed, err := worker.ProcessOne(context.Background(), 11)
	if err != nil || !processed {
		t.Fatalf("ProcessOne = (%t, %v), expected (true, nil)", processed, err)
	}
	if !store.sent || len(sender.requests) != 1 {
		t.Fatalf("sent=%t requests=%d", store.sent, len(sender.requests))
	}
	raw, err := json.Marshal(sender.requests[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "business_connection_id") {
		t.Fatalf("business_connection_id must never be serialized for an alert: %s", raw)
	}
	if sender.requests[0].ChatID != 42 || !strings.Contains(sender.requests[0].Text, "private content") {
		t.Fatalf("unexpected request: %#v", sender.requests[0])
	}
	if strings.Contains(logs.String(), "private content") || strings.Contains(logs.String(), "bc-secret") {
		t.Fatalf("content or identifier leak in logs: %s", logs.String())
	}
}

func TestWorker429UsesRetryAfter(t *testing.T) {
	store := &fakeStore{job: testJob()}
	sender := &fakeSender{err: &telegram.APIError{Method: "sendMessage", Code: 429, RetryAfter: 17}}
	worker := newTestWorker(store, sender, &bytes.Buffer{})

	processed, err := worker.ProcessOne(context.Background(), 11)
	if err != nil || !processed {
		t.Fatalf("ProcessOne = (%t, %v)", processed, err)
	}
	if store.retryIn != 17*time.Second {
		t.Fatalf("retry delay=%v, expected 17s", store.retryIn)
	}
}

func TestWorker5xxAndTimeoutUseExponentialBackoff(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"5xx", &telegram.APIError{Method: "sendMessage", Code: 503}},
		{"timeout", context.DeadlineExceeded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			job := testJob()
			job.Attempts = 2
			store := &fakeStore{job: job, attempts: 2}
			sender := &fakeSender{err: tc.err}
			worker := newTestWorker(store, sender, &bytes.Buffer{})
			if _, err := worker.ProcessOne(context.Background(), 11); err != nil {
				t.Fatal(err)
			}
			if store.retryIn != 4*time.Second {
				t.Fatalf("retry delay=%v, expected 4s", store.retryIn)
			}
		})
	}
}

func TestWorker5xxAndTimeoutReachFailedAfterMaxAttempts(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"5xx", &telegram.APIError{Method: "sendMessage", Code: 503}},
		{"timeout", context.DeadlineExceeded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeStore{job: testJob(), attempts: maxDeliveryAttempts - 1}
			worker := newTestWorker(store, &fakeSender{err: tc.err}, &bytes.Buffer{})
			if _, err := worker.ProcessOne(context.Background(), 11); err != nil {
				t.Fatal(err)
			}
			if !store.failed || !store.retryAt.IsZero() {
				t.Fatalf("failed=%t retryAt=%v", store.failed, store.retryAt)
			}
		})
	}
}

func TestWorkerPermanent4xxMarksFailed(t *testing.T) {
	store := &fakeStore{job: testJob()}
	sender := &fakeSender{err: &telegram.APIError{Method: "sendMessage", Code: 400}}
	worker := newTestWorker(store, sender, &bytes.Buffer{})
	if _, err := worker.ProcessOne(context.Background(), 11); err != nil {
		t.Fatal(err)
	}
	if !store.failed || !store.retryAt.IsZero() {
		t.Fatalf("failed=%t retryAt=%v", store.failed, store.retryAt)
	}
}

// outboxFailedCount reads the exposed series rather than an internal field: it
// is the value a scrape will actually see.
func outboxFailedCount(t *testing.T) int64 {
	t.Helper()
	const name = "undelete_outbox_failed_total"
	for _, line := range strings.Split(metrics.Default().RenderPrometheus(), "\n") {
		value, found := strings.CutPrefix(line, name+" ")
		if !found {
			continue
		}
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			t.Fatalf("unreadable value for %s: %q", name, value)
		}
		return n
	}
	t.Fatalf("series %s missing from the exposition", name)
	return 0
}

// An abandoned alert leaves the backlog without having been delivered:
// without this counter, both MarkFailed paths would leave no metric trace and
// the failure would only be visible in the logs.
func TestWorkerCountsAbandonedAlerts(t *testing.T) {
	cases := map[string]*fakeSender{
		"permanent 4xx":      {err: &telegram.APIError{Method: "sendMessage", Code: 400}},
		"attempts exhausted": {err: &telegram.APIError{Method: "sendMessage", Code: 500}},
	}

	for name, sender := range cases {
		t.Run(name, func(t *testing.T) {
			// attempts on the store: it is Claim that stamps the attempt.
			store := &fakeStore{job: testJob(), attempts: maxDeliveryAttempts - 1}
			worker := newTestWorker(store, sender, &bytes.Buffer{})

			before := outboxFailedCount(t)
			if _, err := worker.ProcessOne(context.Background(), 11); err != nil {
				t.Fatal(err)
			}
			if !store.failed {
				t.Fatal("the job must have been marked failed")
			}
			if got := outboxFailedCount(t) - before; got != 1 {
				t.Fatalf("undelete_outbox_failed_total progressed by %d, expected 1", got)
			}
		})
	}
}

// A retry is NOT an abandonment: the counter must not move while the job
// remains replayable, otherwise the metric would lose all alerting value.
func TestWorkerDoesNotCountRetryAsAbandonment(t *testing.T) {
	store := &fakeStore{job: testJob()}
	sender := &fakeSender{err: &telegram.APIError{Method: "sendMessage", Code: 500}}
	worker := newTestWorker(store, sender, &bytes.Buffer{})

	before := outboxFailedCount(t)
	if _, err := worker.ProcessOne(context.Background(), 11); err != nil {
		t.Fatal(err)
	}
	if store.failed {
		t.Fatal("a 500 on a first attempt must be rescheduled, not abandoned")
	}
	if got := outboxFailedCount(t); got != before {
		t.Fatalf("undelete_outbox_failed_total = %d, expected unchanged (%d)", got, before)
	}
}

func TestWorkerShutdownCancellationLeavesJobToLeaseExpiry(t *testing.T) {
	store := &fakeStore{job: testJob()}
	worker := newTestWorker(store, &fakeSender{err: context.Canceled}, &bytes.Buffer{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	processed, err := worker.ProcessOne(ctx, 11)
	if err != nil || processed {
		t.Fatalf("ProcessOne = (%t, %v), expected (false, nil)", processed, err)
	}
	if store.failed || !store.retryAt.IsZero() || store.attempts != 0 {
		t.Fatalf("no marking expected with a dead context: failed=%t retryAt=%v attempts=%d", store.failed, store.retryAt, store.attempts)
	}
}

func TestRetryDelaySaturatesInsteadOfOverflowing(t *testing.T) {
	for _, attempts := range []int{0, 3, 64, 1024} {
		got := retryDelay(attempts)
		if got <= 0 || got > maxBackoff {
			t.Fatalf("retryDelay(%d) = %v, expected in ]0, %v]", attempts, got, maxBackoff)
		}
	}
	if got := retryDelay(2); got != 4*time.Second {
		t.Fatalf("retryDelay(2) = %v, expected 4s", got)
	}
}

func TestClaimLeaseAllowsRecoveryAfterCrash(t *testing.T) {
	store := &fakeStore{job: testJob(), clock: time.Unix(100, 0)}
	claimed, err := store.Claim(context.Background(), 11, time.Minute)
	if err != nil || claimed == nil {
		t.Fatal("first reservation impossible")
	}
	store.clock = store.clock.Add(30 * time.Second)
	if again, _ := store.Claim(context.Background(), 11, time.Minute); again != nil {
		t.Fatal("the job must not be picked up before lease expiry")
	}
	store.clock = store.clock.Add(30 * time.Second)
	if recovered, _ := store.Claim(context.Background(), 11, time.Minute); recovered == nil {
		t.Fatal("the job must be picked up after a crash and lease expiry")
	}
}

// sequentialStore delivers the jobs in order and without failure: sufficient
// to verify the relay of all chunks of a single message.
type sequentialStore struct {
	jobs []*Job
	i    int
	sent int
}

func (s *sequentialStore) Claim(context.Context, int64, time.Duration) (*Job, error) {
	if s.i >= len(s.jobs) {
		return nil, nil
	}
	job := s.jobs[s.i]
	s.i++
	return job, nil
}

func (s *sequentialStore) MarkSent(context.Context, int64, int64, string) error {
	s.sent++
	return nil
}

func (s *sequentialStore) MarkRetry(context.Context, int64, int64, string, time.Duration, string) error {
	return nil
}

func (s *sequentialStore) MarkFailed(context.Context, int64, int64, string, string) error {
	return nil
}

// TestWorkerSendsEveryChunkInOrderToOwner is the deletion contract on the
// current production path: the chunks are written to the outbox by
// messages.MarkDeleted (via telegram.BuildDeletionMessageRequests) and the
// worker must relay ALL of them, in order, to the owner, without ever
// serializing business_connection_id.
func TestWorkerSendsEveryChunkInOrderToOwner(t *testing.T) {
	store := &sequentialStore{jobs: []*Job{
		{ID: 1, OwnerUserID: 11, OwnerTelegramUserID: 42, BusinessConnectionID: "bc-secret", ChatID: 99, MessageID: 3, EventType: EventDeletedMessage, Text: "part-one"},
		{ID: 2, OwnerUserID: 11, OwnerTelegramUserID: 42, BusinessConnectionID: "bc-secret", ChatID: 99, MessageID: 3, EventType: EventDeletedMessage, Text: "part-two"},
	}}
	sender := &fakeSender{}
	var logs bytes.Buffer
	worker := newTestWorker(store, sender, &logs)

	for i := 0; i < len(store.jobs); i++ {
		processed, err := worker.ProcessOne(context.Background(), 11)
		if err != nil || !processed {
			t.Fatalf("ProcessOne %d = (%t, %v), expected (true, nil)", i, processed, err)
		}
	}

	if len(sender.requests) != 2 || store.sent != 2 {
		t.Fatalf("requests=%d sent=%d, expected 2/2", len(sender.requests), store.sent)
	}
	for i, req := range sender.requests {
		raw, err := json.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "business_connection_id") {
			t.Fatalf("business_connection_id serialized on chunk %d: %s", i, raw)
		}
		if req.ChatID != 42 {
			t.Fatalf("chunk %d addressed to chat %d, expected owner 42", i, req.ChatID)
		}
	}
	if sender.requests[0].Text != "part-one" || sender.requests[1].Text != "part-two" {
		t.Fatalf("chunks relayed out of order: %q then %q", sender.requests[0].Text, sender.requests[1].Text)
	}
	if strings.Contains(logs.String(), "part-one") || strings.Contains(logs.String(), "bc-secret") {
		t.Fatalf("content or identifier leak in logs: %s", logs.String())
	}
}
