package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

const apiBaseURL = "https://api.telegram.org/bot"

// Client est un client HTTP minimal pour la Bot API. Volontairement sans
// dépendance à une lib Telegram tierce (cf. commentaire de package) : ce
// client n'expose que les méthodes utilisées par le bot.
type Client struct {
	token      string
	httpClient *http.Client
	baseURL    string
}

// NewClient construit un client. httpTimeout doit rester strictement
// supérieur au timeout de long-polling passé à GetUpdates (50s) : sinon le
// client HTTP coupe la requête avant que Telegram n'ait eu la chance de
// répondre "pas d'update" au bout des 50s.
func NewClient(token string, httpTimeout time.Duration) *Client {
	return &Client{
		token:      token,
		httpClient: &http.Client{Timeout: httpTimeout},
		baseURL:    apiBaseURL,
	}
}

func (c *Client) call(ctx context.Context, method string, params any, out any) error {
	body, err := marshal(params)
	if err != nil {
		return fmt.Errorf("sérialisation requête %s: %w", method, err)
	}

	endpoint := c.baseURL + c.token + "/" + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("construction requête %s: %w", method, err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// net/http encapsule les erreurs de transport dans *url.Error, dont
		// Error() contient l'URL complète. L'URL Bot API inclut le token dans
		// son chemin ; propager ce wrapper jusque dans slog ferait fuiter le
		// secret à la première erreur DNS/TLS/timeout. On ne conserve que la
		// cause réseau, qui ne contient pas l'URL.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			return fmt.Errorf("appel %s: %w", method, urlErr.Err)
		}
		return fmt.Errorf("appel %s: erreur de transport", method)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("lecture réponse %s: %w", method, err)
	}

	var env apiResponse[json.RawMessage]
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("décodage enveloppe %s (status %d): %w", method, resp.StatusCode, err)
	}

	if !env.OK {
		apiErr := &APIError{
			Method:      method,
			Code:        env.ErrorCode,
			Description: env.Description,
		}
		if env.Parameters != nil {
			apiErr.RetryAfter = env.Parameters.RetryAfter
		}
		return apiErr
	}

	if out != nil {
		if err := json.Unmarshal(env.Result, out); err != nil {
			return fmt.Errorf("décodage résultat %s: %w", method, err)
		}
	}
	return nil
}

// APIError représente une réponse {"ok": false, ...} de la Bot API.
type APIError struct {
	Method      string
	Code        int
	Description string
	// RetryAfter (secondes) n'est renseigné que sur les erreurs 429 : le
	// poller doit dormir exactement ce temps-là avant de réessayer, cf.
	// contrainte "respect de retry_after sur 429".
	RetryAfter int
}

func (e *APIError) Error() string {
	return fmt.Sprintf("telegram %s: %d %s", e.Method, e.Code, e.Description)
}

// IsRateLimited signale une erreur 429 avec un retry_after exploitable.
func (e *APIError) IsRateLimited() bool {
	return e.Code == http.StatusTooManyRequests && e.RetryAfter > 0
}

// GetUpdates effectue un appel long-polling. timeoutSeconds doit être <
// c.httpClient.Timeout (voir NewClient).
func (c *Client) GetUpdates(ctx context.Context, offset int64, timeoutSeconds int) ([]Update, error) {
	req := getUpdatesRequest{
		Offset:         offset,
		Timeout:        timeoutSeconds,
		AllowedUpdates: AllowedUpdates, // contrainte n°1 : jamais omis
	}
	var updates []Update
	if err := c.call(ctx, "getUpdates", req, &updates); err != nil {
		return nil, err
	}
	return updates, nil
}

const sendMessageAttempts = 3

// SendMessage conserve les retries bornés pour les envois non persistés
// (par exemple le message de bienvenue). Les alertes outbox appellent
// SendMessageOnce afin que leur backoff soit persisté en PostgreSQL.
func (c *Client) SendMessage(ctx context.Context, req SendMessageRequest) error {
	backoff := time.Second
	var lastErr error
	for attempt := 1; attempt <= sendMessageAttempts; attempt++ {
		lastErr = c.SendMessageOnce(ctx, req)
		if lastErr == nil {
			return nil
		}
		wait := backoff
		var apiErr *APIError
		if errors.As(lastErr, &apiErr) {
			switch {
			case apiErr.IsRateLimited():
				wait = time.Duration(apiErr.RetryAfter) * time.Second
			case apiErr.Code < http.StatusInternalServerError:
				return lastErr
			}
		}
		if attempt == sendMessageAttempts {
			break
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		backoff *= 2
	}
	return fmt.Errorf("sendMessage après %d tentatives: %w", sendMessageAttempts, lastErr)
}

// SendMessageOnce effectue une seule tentative. Le worker outbox est seul
// responsable de la replanification durable des alertes de suppression.
func (c *Client) SendMessageOnce(ctx context.Context, req SendMessageRequest) error {
	return c.call(ctx, "sendMessage", req, nil)
}

// GetBusinessConnection interroge l'API pour une connexion Business par son
// id. Utilisé comme dernier niveau de résolution (cache -> base -> API) :
// après un redémarrage du bot, une connexion établie pendant l'indisponibilité
// n'est ni en cache ni en base, seul cet appel permet de la récupérer.
func (c *Client) GetBusinessConnection(ctx context.Context, connectionID string) (*BusinessConnection, error) {
	var conn BusinessConnection
	if err := c.call(ctx, "getBusinessConnection", map[string]string{"business_connection_id": connectionID}, &conn); err != nil {
		return nil, err
	}
	return &conn, nil
}
