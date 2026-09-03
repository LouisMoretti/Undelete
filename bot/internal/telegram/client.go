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

// Client is a minimal HTTP client for the Bot API. Deliberately free of any
// third-party Telegram library dependency (cf. package comment): this client
// only exposes the methods used by the bot.
type Client struct {
	token      string
	httpClient *http.Client
	baseURL    string
}

// Option tweaks a Client at construction time.
type Option func(*Client)

// WithBaseURL replaces the Bot API base URL (prefix, token excluded).
//
// Its only purpose: let contract tests point a real Client at an httptest
// server, including from the calling packages (app, business) that exercise
// the real production paths. Production never uses this option.
func WithBaseURL(baseURL string) Option {
	return func(c *Client) { c.baseURL = baseURL }
}

// NewClient builds a client. httpTimeout must stay strictly greater than the
// long-polling timeout passed to GetUpdates (50s): otherwise the HTTP client
// cuts the request before Telegram has had a chance to answer "no update"
// after 50s.
func NewClient(token string, httpTimeout time.Duration, opts ...Option) *Client {
	c := &Client{
		token:      token,
		httpClient: &http.Client{Timeout: httpTimeout},
		baseURL:    apiBaseURL,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

func (c *Client) call(ctx context.Context, method string, params any, out any) error {
	body, err := marshal(params)
	if err != nil {
		return fmt.Errorf("serializing request %s: %w", method, err)
	}

	endpoint := c.baseURL + c.token + "/" + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("building request %s: %w", method, err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// net/http wraps transport errors in *url.Error, whose Error()
		// contains the full URL. The Bot API URL includes the token in its
		// path; propagating this wrapper up into slog would leak the secret
		// at the first DNS/TLS/timeout error. We only keep the network
		// cause, which does not contain the URL.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			return fmt.Errorf("calling %s: %w", method, urlErr.Err)
		}
		return fmt.Errorf("calling %s: transport error", method)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading response %s: %w", method, err)
	}

	var env apiResponse[json.RawMessage]
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("decoding envelope %s (status %d): %w", method, resp.StatusCode, err)
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
			return fmt.Errorf("decoding result %s: %w", method, err)
		}
	}
	return nil
}

// APIError represents a {"ok": false, ...} response from the Bot API.
type APIError struct {
	Method      string
	Code        int
	Description string
	// RetryAfter (seconds) is only populated on 429 errors: the poller must
	// sleep exactly that long before retrying, cf. constraint "respect
	// retry_after on 429".
	RetryAfter int
}

func (e *APIError) Error() string {
	return fmt.Sprintf("telegram %s: %d %s", e.Method, e.Code, e.Description)
}

// IsRateLimited reports a 429 error with a usable retry_after.
func (e *APIError) IsRateLimited() bool {
	return e.Code == http.StatusTooManyRequests && e.RetryAfter > 0
}

// GetUpdates performs a long-polling call. timeoutSeconds must be <
// c.httpClient.Timeout (see NewClient).
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

// SendMessage keeps the bounded retries for non-persisted sends (for
// example the welcome message). The outbox alerts call SendMessageOnce so
// that their backoff is persisted in PostgreSQL.
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
	return fmt.Errorf("sendMessage after %d attempts: %w", sendMessageAttempts, lastErr)
}

// SendMessageOnce performs a single attempt. The outbox worker is solely
// responsible for durably rescheduling deletion alerts.
func (c *Client) SendMessageOnce(ctx context.Context, req SendMessageRequest) error {
	return c.call(ctx, "sendMessage", req, nil)
}

// GetBusinessConnection queries the API for a Business connection by its id.
// Used as the last resolution level (cache -> database -> API): after a bot
// restart, a connection established during the outage is neither in cache nor
// in the database, only this call can recover it.
func (c *Client) GetBusinessConnection(ctx context.Context, connectionID string) (*BusinessConnection, error) {
	var conn BusinessConnection
	if err := c.call(ctx, "getBusinessConnection", map[string]string{"business_connection_id": connectionID}, &conn); err != nil {
		return nil, err
	}
	return &conn, nil
}
