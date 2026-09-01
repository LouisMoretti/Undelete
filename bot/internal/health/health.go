// Package health expose les probes de supervision du bot : /livez, /readyz
// et /metrics, servis par un serveur HTTP dédié (HEALTH_ADDR).
//
// Aucune réponse de ce paquet ne contient de contenu utilisateur. En
// particulier, /readyz ne renvoie JAMAIS le message d'erreur brut d'un ping
// PostgreSQL : celui-ci contient le DSN (donc un mot de passe) et parfois des
// fragments de requête. Les checks dégradés sont décrits par un jeu FIXE de
// raisons courtes, définies ici.
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

// pollerFreshnessThreshold borne l'ancienneté acceptable du dernier
// getUpdates réussi. Le long polling utilise un timeout de 50s : un poller
// sain rend la main au moins toutes les 50s (plus le temps de traitement).
// 90s laisse donc la marge d'un cycle complet sans jamais signaler dégradé un
// poller simplement en attente de la réponse Telegram.
const pollerFreshnessThreshold = 90 * time.Second

// pingTimeout borne le check base : une base injoignable doit répondre
// « degraded » vite, pas faire traîner la probe jusqu'au timeout de l'orchestrateur.
const pingTimeout = 2 * time.Second

// Raisons possibles pour un check. Liste fermée : rien d'issu d'un update,
// d'une erreur base ou d'une réponse Telegram ne peut s'y glisser.
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

// Pinger est le check base. *pgxpool.Pool le satisfait tel quel ; les tests
// fournissent un double qui renvoie l'erreur voulue.
type Pinger interface {
	Ping(ctx context.Context) error
}

// FreshnessSource fournit la date du dernier getUpdates réussi. La valeur
// zéro signifie « aucun poll réussi depuis le démarrage ». telegram.Poller
// l'implémente via LastSuccessfulPoll.
type FreshnessSource interface {
	LastSuccessfulPoll() time.Time
}

// Handler sert les trois routes. Les dépendances sont injectées par
// interfaces : les états dégradés (base morte, poller figé) se testent sans
// réseau ni base.
type Handler struct {
	db       Pinger
	poller   FreshnessSource
	counters *metrics.Counters
	now      func() time.Time
}

// NewHandler assemble le handler. now peut être nil : l'horloge système est
// alors utilisée (les tests injectent une horloge figée).
func NewHandler(db Pinger, poller FreshnessSource, counters *metrics.Counters, now func() time.Time) *Handler {
	if now == nil {
		now = time.Now
	}
	if counters == nil {
		counters = metrics.Default()
	}
	return &Handler{db: db, poller: poller, counters: counters, now: now}
}

// Mux construit le routeur des probes.
func (h *Handler) Mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/livez", h.handleLive)
	mux.HandleFunc("/readyz", h.handleReady)
	mux.HandleFunc("/metrics", h.handleMetrics)
	return mux
}

// handleLive répond 200 dès que le processus sert du HTTP : la liveness ne
// doit dépendre d'aucune dépendance externe, sinon une base momentanément
// indisponible ferait redémarrer en boucle un bot par ailleurs sain.
func (h *Handler) handleLive(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": statusOK})
}

// handleReady répond 200 si la base répond ET si le poller a réussi un
// getUpdates récemment ; 503 avec le détail par check sinon.
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
		// err est volontairement ignorée dans la réponse : elle contient le
		// DSN applicatif, mot de passe compris.
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

// Serve démarre le serveur de probes sur addr et le coupe proprement quand
// ctx est annulé. addr vide = serveur désactivé (retour immédiat sans
// erreur), pour les environnements où aucun port ne doit être ouvert.
func Serve(ctx context.Context, addr string, handler *Handler, logger *slog.Logger) error {
	if addr == "" {
		logger.Info("serveur de santé désactivé (HEALTH_ADDR vide)")
		return nil
	}

	// Timeouts complets : les probes répondent en millisecondes (le seul
	// travail bloquant est un ping borné à 2 s), donc aucune raison de
	// laisser une connexion de scrape traîner indéfiniment.
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
	// L'adresse effective est loggée (utile quand addr vaut ":0"), jamais le
	// jeton du bot ni un DSN.
	logger.Info("serveur de santé démarré", slog.String("addr", ln.Addr().String()))

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
	logger.Info("serveur de santé arrêté")
	return err
}
