package outbox

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/LouisMoretti/Undelete/bot/internal/telegram"
)

type fakeStore struct {
	job       *Job
	claimedAt time.Time
	lease     time.Duration
	sent      bool
	failed    bool
	retryAt   time.Time
	attempts  int
}

func (s *fakeStore) Claim(_ context.Context, _ int64, now time.Time, lease time.Duration) (*Job, error) {
	if s.job == nil || s.sent || s.failed || (!s.retryAt.IsZero() && now.Before(s.retryAt)) || (!s.claimedAt.IsZero() && now.Before(s.claimedAt.Add(s.lease))) {
		return nil, nil
	}
	s.claimedAt, s.lease = now, lease
	copy := *s.job
	copy.Attempts = s.attempts
	return &copy, nil
}
func (s *fakeStore) MarkSent(context.Context, int64, int64, string, time.Time) error {
	s.sent = true
	return nil
}
func (s *fakeStore) MarkRetry(_ context.Context, _, _ int64, _ string, next time.Time, _ string) error {
	s.attempts++
	s.retryAt, s.claimedAt = next, time.Time{}
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
	return &Job{ID: 7, OwnerUserID: 11, OwnerTelegramUserID: 42, BusinessConnectionID: "bc-secret", ChatID: 99, MessageID: 3, EventType: EventDeletedMessage, Text: "contenu privé"}
}

func TestWorkerSuccessSendsAsBotAndMarksSent(t *testing.T) {
	store := &fakeStore{job: testJob()}
	sender := &fakeSender{}
	var logs bytes.Buffer
	worker := newTestWorker(store, sender, &logs)

	processed, err := worker.ProcessOne(context.Background(), 11, time.Unix(100, 0))
	if err != nil || !processed {
		t.Fatalf("ProcessOne = (%t, %v), attendu (true, nil)", processed, err)
	}
	if !store.sent || len(sender.requests) != 1 {
		t.Fatalf("sent=%t requests=%d", store.sent, len(sender.requests))
	}
	raw, err := json.Marshal(sender.requests[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "business_connection_id") {
		t.Fatalf("business_connection_id ne doit jamais être sérialisé pour une alerte: %s", raw)
	}
	if sender.requests[0].ChatID != 42 || !strings.Contains(sender.requests[0].Text, "contenu privé") {
		t.Fatalf("requête inattendue: %#v", sender.requests[0])
	}
	if strings.Contains(logs.String(), "contenu privé") || strings.Contains(logs.String(), "bc-secret") {
		t.Fatalf("fuite de contenu ou identifiant dans les logs: %s", logs.String())
	}
}

func TestWorker429UsesRetryAfter(t *testing.T) {
	store := &fakeStore{job: testJob()}
	sender := &fakeSender{err: &telegram.APIError{Method: "sendMessage", Code: 429, RetryAfter: 17}}
	worker := newTestWorker(store, sender, &bytes.Buffer{})
	now := time.Unix(100, 0)

	processed, err := worker.ProcessOne(context.Background(), 11, now)
	if err != nil || !processed {
		t.Fatalf("ProcessOne = (%t, %v)", processed, err)
	}
	if want := now.Add(17 * time.Second); !store.retryAt.Equal(want) {
		t.Fatalf("next_attempt_at=%v, attendu %v", store.retryAt, want)
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
			now := time.Unix(100, 0)
			if _, err := worker.ProcessOne(context.Background(), 11, now); err != nil {
				t.Fatal(err)
			}
			if want := now.Add(4 * time.Second); !store.retryAt.Equal(want) {
				t.Fatalf("next_attempt_at=%v, attendu %v", store.retryAt, want)
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
			if _, err := worker.ProcessOne(context.Background(), 11, time.Unix(100, 0)); err != nil {
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
	if _, err := worker.ProcessOne(context.Background(), 11, time.Unix(100, 0)); err != nil {
		t.Fatal(err)
	}
	if !store.failed || !store.retryAt.IsZero() {
		t.Fatalf("failed=%t retryAt=%v", store.failed, store.retryAt)
	}
}

func TestWorkerShutdownCancellationLeavesJobToLeaseExpiry(t *testing.T) {
	store := &fakeStore{job: testJob()}
	worker := newTestWorker(store, &fakeSender{err: context.Canceled}, &bytes.Buffer{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	processed, err := worker.ProcessOne(ctx, 11, time.Unix(100, 0))
	if err != nil || processed {
		t.Fatalf("ProcessOne = (%t, %v), attendu (false, nil)", processed, err)
	}
	if store.failed || !store.retryAt.IsZero() || store.attempts != 0 {
		t.Fatalf("aucun marquage attendu avec un contexte mort: failed=%t retryAt=%v attempts=%d", store.failed, store.retryAt, store.attempts)
	}
}

func TestRetryDelaySaturatesInsteadOfOverflowing(t *testing.T) {
	for _, attempts := range []int{0, 3, 64, 1024} {
		got := retryDelay(attempts)
		if got <= 0 || got > maxBackoff {
			t.Fatalf("retryDelay(%d) = %v, attendu dans ]0, %v]", attempts, got, maxBackoff)
		}
	}
	if got := retryDelay(2); got != 4*time.Second {
		t.Fatalf("retryDelay(2) = %v, attendu 4s", got)
	}
}

func TestClaimLeaseAllowsRecoveryAfterCrash(t *testing.T) {
	store := &fakeStore{job: testJob()}
	now := time.Unix(100, 0)
	claimed, err := store.Claim(context.Background(), 11, now, time.Minute)
	if err != nil || claimed == nil {
		t.Fatal("première réservation impossible")
	}
	if again, _ := store.Claim(context.Background(), 11, now.Add(30*time.Second), time.Minute); again != nil {
		t.Fatal("le job ne doit pas être repris avant expiration du lease")
	}
	if recovered, _ := store.Claim(context.Background(), 11, now.Add(time.Minute), time.Minute); recovered == nil {
		t.Fatal("le job doit être repris après un crash et expiration du lease")
	}
}
