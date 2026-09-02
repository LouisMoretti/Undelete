// Package health exposes the bot's supervision probes: /livez, /readyz
// and /metrics, served by a dedicated HTTP server (HEALTH_ADDR).
//
// No response from this package contains user content. In particular, /readyz
// NEVER returns the raw error message of a PostgreSQL ping: it contains the
// DSN (hence a password) and sometimes query fragments. Degraded checks are
// described by a FIXED set of short reasons, defined here.
package health

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/LouisMoretti/Undelete/bot/internal/metrics"
)

// pollerFreshnessThreshold bounds the acceptable age of the last successful
// getUpdates. Long polling uses a 50s timeout: a healthy poller returns at
// least every 50s (plus processing time). 90s therefore leaves the margin of
// one full cycle without ever marking a poller simply waiting for the
// Telegram response as degraded.
const pollerFreshnessThreshold = 90 * time.Second

// pingTimeout bounds the database check: an unreachable database must answer
// "degraded" quickly, not drag the probe until the orchestrator's timeout.
const pingTimeout = 2 * time.Second

// Possible reasons for a check. Closed list: nothing from an update, a
// database error or a Telegram response can slip in.
const (
	reasonOK             = "ok"
	reasonDBUnreachable  = "unreachable"
	reasonPollerNoPoll   = "no_successful_poll_yet"
	reasonPollerStale    = "stale"
	statusOK             = "ok"
	statusDegraded       = "degraded"
	checkNameDatabase    = "database"
	checkNamePoller      = "poller"
	defaultShutdownGrace = 5 * time.Second
)

// Pinger is the database check. *pgxpool.Pool satisfies it as is; tests
// provide a double that returns the desired error.
type Pinger interface {
	Ping(ctx context.Context) error
}

// FreshnessSource provides the time of the last successful getUpdates. The
// zero value means "no successful poll since startup". telegram.Poller
// implements it via LastSuccessfulPoll.
type FreshnessSource interface {
	LastSuccessfulPoll() time.Time
}

// Handler serves the three routes. Dependencies are injected via interfaces:
// the degraded states (dead database, frozen poller) can be tested without a
// network or a database.
type Handler struct {
	db       Pinger
	poller   FreshnessSource
	counters *metrics.Counters
	now      func() time.Time
}

// NewHandler assembles the handler. now may be nil: the system clock is then
// used (tests inject a frozen clock).
func NewHandler(db Pinger, poller FreshnessSource, counters *metrics.Counters, now func() time.Time) *Handler {
	if now == nil {
		now = time.Now
	}
	if counters == nil {
		counters = metrics.Default()
	}
	return &Handler{db: db, poller: poller, counters: counters, now: now}
}

// Mux builds the probes router.
func (h *Handler) Mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/livez", h.handleLive)
	mux.HandleFunc("/readyz", h.handleReady)
	mux.HandleFunc("/metrics", h.handleMetrics)
	return mux
}

// handleLive answers 200 as soon as the process serves HTTP: liveness must not
// depend on any external dependency, otherwise a momentarily unavailable
// database would make an otherwise healthy bot restart in a loop.
func (h *Handler) handleLive(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": statusOK})
}

// handleReady answers 200 if the database responds AND the poller has
// succeeded a getUpdates recently; 503 with per-check detail otherwise.
func (h *Handler) handleReady(w http.ResponseWriter, r *http.Request) {
	checks := map[string]string{
		checkNameDatabase: h.checkDatabase(r.Context()),
		checkNamePoller:   h.checkPoller(),
	}

	status, code := statusOK, http.StatusOK
	for _, reason := range checks {
		if reason != reasonOK {
			status, code = statusDegraded, http.StatusServiceUnavailable
			break
		}
	}

	writeJSON(w, code, map[string]any{"status": status, "checks": checks})
}

func (h *Handler) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", metrics.ContentType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(h.counters.RenderPrometheus()))
}

func (h *Handler) checkDatabase(ctx context.Context) string {
	if h.db == nil {
		return reasonDBUnreachable
	}
	ctx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	if err := h.db.Ping(ctx); err != nil {
		// err is intentionally ignored in the response: it contains the
		// application DSN, password included.
		return reasonDBUnreachable
	}
	return reasonOK
}

func (h *Handler) checkPoller() string {
	if h.poller == nil {
		return reasonPollerNoPoll
	}
	last := h.poller.LastSuccessfulPoll()
	if last.IsZero() {
		return reasonPollerNoPoll
	}
	if h.now().Sub(last) > pollerFreshnessThreshold {
		return reasonPollerStale
	}
	return reasonOK
}

func writeJSON(w http.ResponseWriter, code int, body map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

// Serve starts the probes server on addr and shuts it down cleanly when ctx
// is cancelled. Empty addr = server disabled (immediate return without
// error), for environments where no port must be opened.
func Serve(ctx context.Context, addr string, handler *Handler, logger *slog.Logger) error {
	if addr == "" {
		logger.Info("health server disabled (empty HEALTH_ADDR)")
		return nil
	}

	// Full timeouts: the probes answer in milliseconds (the only blocking work is
	// a ping bounded at 2 s), so there is no reason to let a scrape connection
	// linger indefinitely.
	srv := &http.Server{
		Handler:           handler.Mux(),
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	// The effective address is logged (useful when addr is ":0"), never the bot
	// token or a DSN.
	logger.Info("health server started", slog.String("addr", ln.Addr().String()))

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), defaultShutdownGrace)
	defer cancel()
	err = srv.Shutdown(shutdownCtx)
	logger.Info("health server stopped")
	return err
}
