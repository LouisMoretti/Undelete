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

// fakeStore tient l'horloge que le vrai Repository délègue à PostgreSQL : le
// worker ne passe plus que des durées, c'est donc le store qui date les
// transitions. Les tests avancent `clock` explicitement.
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
	return &Job{ID: 7, OwnerUserID: 11, OwnerTelegramUserID: 42, BusinessConnectionID: "bc-secret", ChatID: 99, MessageID: 3, EventType: EventDeletedMessage, Text: "contenu privé"}
}

func TestWorkerSuccessSendsAsBotAndMarksSent(t *testing.T) {
	store := &fakeStore{job: testJob()}
	sender := &fakeSender{}
	var logs bytes.Buffer
	worker := newTestWorker(store, sender, &logs)

	processed, err := worker.ProcessOne(context.Background(), 11)
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

	processed, err := worker.ProcessOne(context.Background(), 11)
	if err != nil || !processed {
		t.Fatalf("ProcessOne = (%t, %v)", processed, err)
	}
	if store.retryIn != 17*time.Second {
		t.Fatalf("délai de retry=%v, attendu 17s", store.retryIn)
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
				t.Fatalf("délai de retry=%v, attendu 4s", store.retryIn)
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

func TestWorkerShutdownCancellationLeavesJobToLeaseExpiry(t *testing.T) {
	store := &fakeStore{job: testJob()}
	worker := newTestWorker(store, &fakeSender{err: context.Canceled}, &bytes.Buffer{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	processed, err := worker.ProcessOne(ctx, 11)
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
	store := &fakeStore{job: testJob(), clock: time.Unix(100, 0)}
	claimed, err := store.Claim(context.Background(), 11, time.Minute)
	if err != nil || claimed == nil {
		t.Fatal("première réservation impossible")
	}
	store.clock = store.clock.Add(30 * time.Second)
	if again, _ := store.Claim(context.Background(), 11, time.Minute); again != nil {
		t.Fatal("le job ne doit pas être repris avant expiration du lease")
	}
	store.clock = store.clock.Add(30 * time.Second)
	if recovered, _ := store.Claim(context.Background(), 11, time.Minute); recovered == nil {
		t.Fatal("le job doit être repris après un crash et expiration du lease")
	}
}

// sequentialStore livre les jobs dans l'ordre et sans échec : suffisant pour
// vérifier le relais de tous les chunks d'un même message.
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

// TestWorkerSendsEveryChunkInOrderToOwner est le contrat de suppression sur
// le chemin de production actuel : les chunks sont écrits en outbox par
// messages.MarkDeleted (via telegram.BuildDeletionMessageRequests) et le
// worker doit les relayer TOUS, dans l'ordre, au owner, sans jamais
// sérialiser business_connection_id.
func TestWorkerSendsEveryChunkInOrderToOwner(t *testing.T) {
	store := &sequentialStore{jobs: []*Job{
		{ID: 1, OwnerUserID: 11, OwnerTelegramUserID: 42, BusinessConnectionID: "bc-secret", ChatID: 99, MessageID: 3, EventType: EventDeletedMessage, Text: "partie-un"},
		{ID: 2, OwnerUserID: 11, OwnerTelegramUserID: 42, BusinessConnectionID: "bc-secret", ChatID: 99, MessageID: 3, EventType: EventDeletedMessage, Text: "partie-deux"},
	}}
	sender := &fakeSender{}
	var logs bytes.Buffer
	worker := newTestWorker(store, sender, &logs)

	for i := 0; i < len(store.jobs); i++ {
		processed, err := worker.ProcessOne(context.Background(), 11)
		if err != nil || !processed {
			t.Fatalf("ProcessOne %d = (%t, %v), attendu (true, nil)", i, processed, err)
		}
	}

	if len(sender.requests) != 2 || store.sent != 2 {
		t.Fatalf("requests=%d sent=%d, attendu 2/2", len(sender.requests), store.sent)
	}
	for i, req := range sender.requests {
		raw, err := json.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "business_connection_id") {
			t.Fatalf("business_connection_id sérialisé sur le chunk %d: %s", i, raw)
		}
		if req.ChatID != 42 {
			t.Fatalf("chunk %d adressé au chat %d, attendu le owner 42", i, req.ChatID)
		}
	}
	if sender.requests[0].Text != "partie-un" || sender.requests[1].Text != "partie-deux" {
		t.Fatalf("chunks relayés hors ordre: %q puis %q", sender.requests[0].Text, sender.requests[1].Text)
	}
	if strings.Contains(logs.String(), "partie-un") || strings.Contains(logs.String(), "bc-secret") {
		t.Fatalf("fuite de contenu ou identifiant dans les logs: %s", logs.String())
	}
}
