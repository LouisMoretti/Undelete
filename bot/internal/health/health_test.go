package health

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/LouisMoretti/Undelete/bot/internal/metrics"
)

// fakePinger joue la base : c'est le seul moyen de tester l'état dégradé
// « base injoignable » sans PostgreSQL.
type fakePinger struct{ err error }

func (p fakePinger) Ping(context.Context) error { return p.err }

type fakePoller struct{ last time.Time }

func (p fakePoller) LastSuccessfulPoll() time.Time { return p.last }

// now est l'horloge figée des tests : la fraîcheur du poller se teste par
// arithmétique, jamais par une attente réelle.
var now = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func newTestHandler(dbErr error, lastPoll time.Time) *Handler {
	return NewHandler(fakePinger{err: dbErr}, fakePoller{last: lastPoll}, &metrics.Counters{}, func() time.Time { return now })
}

func do(t *testing.T, h *Handler, path string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.Mux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	body, err := io.ReadAll(rec.Result().Body)
	if err != nil {
		t.Fatalf("lecture du corps: %v", err)
	}
	return rec.Code, string(body)
}

func decodeChecks(t *testing.T, body string) (string, map[string]string) {
	t.Helper()
	var parsed struct {
		Status string            `json:"status"`
		Checks map[string]string `json:"checks"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("réponse JSON invalide %q: %v", body, err)
	}
	return parsed.Status, parsed.Checks
}

func TestLivezRepondOKSansDependance(t *testing.T) {
	// Base morte et poller jamais parti : la liveness reste verte, sinon
	// l'orchestrateur redémarrerait un processus parfaitement vivant.
	code, body := do(t, newTestHandler(errors.New("connexion refusée"), time.Time{}), "/livez")

	if code != http.StatusOK {
		t.Fatalf("code = %d, attendu %d", code, http.StatusOK)
	}
	status, _ := decodeChecks(t, body)
	if status != "ok" {
		t.Fatalf("status = %q, attendu \"ok\"", status)
	}
}

func TestReadyzOKQuandBaseEtPollerSains(t *testing.T) {
	code, body := do(t, newTestHandler(nil, now.Add(-10*time.Second)), "/readyz")

	if code != http.StatusOK {
		t.Fatalf("code = %d, attendu %d (corps %s)", code, http.StatusOK, body)
	}
	status, checks := decodeChecks(t, body)
	if status != "ok" || checks["database"] != "ok" || checks["poller"] != "ok" {
		t.Fatalf("readyz sain attendu, obtenu status=%q checks=%v", status, checks)
	}
}

func TestReadyzDegradeQuandBaseInjoignable(t *testing.T) {
	// L'erreur porte un DSN complet : la réponse ne doit RIEN en reprendre.
	dbErr := errors.New("dial postgres://undelete_app:s3cret@postgres:5432/undelete: connection refused")
	code, body := do(t, newTestHandler(dbErr, now.Add(-time.Second)), "/readyz")

	if code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, attendu %d", code, http.StatusServiceUnavailable)
	}
	status, checks := decodeChecks(t, body)
	if status != "degraded" || checks["database"] != "unreachable" {
		t.Fatalf("status=%q checks=%v", status, checks)
	}
	if checks["poller"] != "ok" {
		t.Fatalf("le poller devait rester sain, checks=%v", checks)
	}
	for _, leak := range []string{"s3cret", "postgres://", "connection refused"} {
		if strings.Contains(body, leak) {
			t.Fatalf("fuite de %q dans la réponse: %s", leak, body)
		}
	}
}

func TestReadyzDegradeQuandPollerPerime(t *testing.T) {
	code, body := do(t, newTestHandler(nil, now.Add(-pollerFreshnessThreshold-time.Second)), "/readyz")

	if code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, attendu %d", code, http.StatusServiceUnavailable)
	}
	status, checks := decodeChecks(t, body)
	if status != "degraded" || checks["poller"] != "stale" {
		t.Fatalf("status=%q checks=%v", status, checks)
	}
	if checks["database"] != "ok" {
		t.Fatalf("la base devait rester saine, checks=%v", checks)
	}
}

// Le seuil est une borne stricte : un poll pile à 90s reste acceptable, un
// poll de 50s (le timeout de long polling) doit toujours passer.
func TestReadyzToleranceExacteDuSeuil(t *testing.T) {
	code, _ := do(t, newTestHandler(nil, now.Add(-pollerFreshnessThreshold)), "/readyz")
	if code != http.StatusOK {
		t.Fatalf("poll pile au seuil: code = %d, attendu %d", code, http.StatusOK)
	}
}

func TestReadyzDegradeAvantLePremierPollReussi(t *testing.T) {
	code, body := do(t, newTestHandler(nil, time.Time{}), "/readyz")

	if code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, attendu %d", code, http.StatusServiceUnavailable)
	}
	if _, checks := decodeChecks(t, body); checks["poller"] != "no_successful_poll_yet" {
		t.Fatalf("checks = %v", checks)
	}
}

func TestMetricsSertLExpositionPrometheus(t *testing.T) {
	counters := &metrics.Counters{}
	counters.AddUpdates(5)
	h := NewHandler(fakePinger{}, fakePoller{last: now}, counters, func() time.Time { return now })

	rec := httptest.NewRecorder()
	h.Mux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != metrics.ContentType {
		t.Fatalf("Content-Type = %q, attendu %q", got, metrics.ContentType)
	}
	if !strings.Contains(rec.Body.String(), "undelete_updates_total 5") {
		t.Fatalf("exposition inattendue:\n%s", rec.Body.String())
	}
}

// Serve doit rendre la main sur annulation du contexte, sinon l'extinction
// propre du binaire resterait bloquée sur wg.Wait().
func TestServeSarreteSurAnnulationDuContexte(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	done := make(chan error, 1)

	go func() { done <- Serve(ctx, "127.0.0.1:0", newTestHandler(nil, now), logger) }()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve() erreur = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve() ne s'est pas arrêté après annulation du contexte")
	}
}

func TestServeDesactiveSiAdresseVide(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	if err := Serve(context.Background(), "", newTestHandler(nil, now), logger); err != nil {
		t.Fatalf("Serve(\"\") erreur = %v, attendu nil", err)
	}
}
