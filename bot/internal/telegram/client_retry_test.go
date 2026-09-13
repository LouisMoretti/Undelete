package telegram

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// testServer points a real Client at an httptest server and counts calls.
func testServer(t *testing.T, handler http.HandlerFunc) (*Client, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return NewClient("test-token", 5*time.Second, WithBaseURL(srv.URL+"/bot")), &calls
}

func okEnvelope(t *testing.T, w http.ResponseWriter, result string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"ok":true,"result":%s}`, result)
}

func errEnvelope(w http.ResponseWriter, code int, description string, retryAfter int) {
	w.Header().Set("Content-Type", "application/json")
	params := ""
	if retryAfter > 0 {
		params = fmt.Sprintf(`,"parameters":{"retry_after":%d}`, retryAfter)
	}
	fmt.Fprintf(w, `{"ok":false,"error_code":%d,"description":%q%s}`, code, description, params)
}

// TestSendMessageSuccessFirstTry performs no retry on the happy path.
func TestSendMessageSuccessFirstTry(t *testing.T) {
	client, calls := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/sendMessage") {
			t.Errorf("unexpected method path %s", r.URL.Path)
		}
		okEnvelope(t, w, `{"message_id":1}`)
	})
	if err := client.SendMessage(context.Background(), SendMessageRequest{ChatID: 42, Text: "hi"}); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1 (no retry on success)", calls.Load())
	}
}

// TestSendMessage4xxReturnsImmediately pins the no-retry rule for definitive
// Telegram refusals: a 400 must surface after exactly one call.
func TestSendMessage4xxReturnsImmediately(t *testing.T) {
	client, calls := testServer(t, func(w http.ResponseWriter, _ *http.Request) {
		errEnvelope(w, 400, "chat not found", 0)
	})
	err := client.SendMessage(context.Background(), SendMessageRequest{ChatID: 42, Text: "hi"})
	if err == nil {
		t.Fatal("expected an error for a 400 response")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != 400 {
		t.Fatalf("expected a 400 APIError, got %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1 (4xx must not retry)", calls.Load())
	}
}

// TestSendMessage5xxRetriesThreeTimesThenGivesUp pins the bounded retry
// budget of non-persisted sends (welcome message): 3 attempts, then a wrapped
// error naming the attempt count.
func TestSendMessage5xxRetriesThreeTimesThenGivesUp(t *testing.T) {
	client, calls := testServer(t, func(w http.ResponseWriter, _ *http.Request) {
		errEnvelope(w, 503, "try again", 0)
	})
	start := time.Now()
	err := client.SendMessage(context.Background(), SendMessageRequest{ChatID: 42, Text: "hi"})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected an error after 3 attempts")
	}
	if !strings.Contains(err.Error(), "after 3 attempts") {
		t.Fatalf("error must name the attempt budget, got %v", err)
	}
	if calls.Load() != 3 {
		t.Fatalf("calls = %d, want 3", calls.Load())
	}
	// 1s + 2s backs off between the 3 attempts: the test would take ~3s if
	// the production backoff changed silently, bounding it loosely.
	if elapsed < 2*time.Second {
		t.Fatalf("expected ~3s of backoff, finished in %v", elapsed)
	}
}

// TestSendMessageWaitCapsAbusiveRetryAfter pins the poller-freeze fix: a 429
// carrying a huge retry_after resolves to sendMessageMaxWait, never to hours.
// Tested on the pure wait function: sleeping the real capped 60s here would
// make the suite unusable.
func TestSendMessageWaitCapsAbusiveRetryAfter(t *testing.T) {
	abusive := &APIError{Code: 429, RetryAfter: 3600}
	if got := sendMessageWait(abusive, time.Second); got != sendMessageMaxWait {
		t.Fatalf("sendMessageWait(429/retry_after=3600) = %v, want %v", got, sendMessageMaxWait)
	}
	// A reasonable retry_after passes through untouched: the server's recovery
	// timeline takes precedence over the client-side backoff.
	sane := &APIError{Code: 429, RetryAfter: 5}
	if got := sendMessageWait(sane, time.Second); got != 5*time.Second {
		t.Fatalf("sendMessageWait(429/retry_after=5) = %v, want 5s", got)
	}
	// Non-429 errors keep the exponential backoff.
	unavailable := &APIError{Code: 503}
	if got := sendMessageWait(unavailable, 4*time.Second); got != 4*time.Second {
		t.Fatalf("sendMessageWait(503) = %v, want the 4s backoff", got)
	}
	if got := sendMessageWait(nil, 2*time.Second); got != 2*time.Second {
		t.Fatalf("sendMessageWait(transport error) = %v, want the 2s backoff", got)
	}
}

// TestSendMessageCancelledDuringBackoff pins shutdown responsiveness: a
// cancelled context during the retry wait aborts immediately with ctx.Err.
func TestSendMessageCancelledDuringBackoff(t *testing.T) {
	client, _ := testServer(t, func(w http.ResponseWriter, _ *http.Request) {
		errEnvelope(w, 503, "try again", 0)
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	err := client.SendMessage(ctx, SendMessageRequest{ChatID: 42, Text: "hi"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

// TestGetBusinessConnectionRoundTrip pins the last-resort resolution level
// (cache -> DB -> API): the decoded shape must carry the identifiers the
// upsert needs.
func TestGetBusinessConnectionRoundTrip(t *testing.T) {
	client, _ := testServer(t, func(w http.ResponseWriter, _ *http.Request) {
		okEnvelope(t, w, `{"id":"bc-1","user":{"id":700002,"first_name":"Louis"},"user_chat_id":700002,"date":1700000000,"is_enabled":true,"rights":{"can_reply":true}}`)
	})
	conn, err := client.GetBusinessConnection(context.Background(), "bc-1")
	if err != nil {
		t.Fatalf("GetBusinessConnection: %v", err)
	}
	if conn.ID != "bc-1" || conn.User.ID != 700002 || !conn.CanReply() || !conn.IsEnabled {
		t.Fatalf("unexpected connection: %+v", conn)
	}
}

// TestGetBusinessConnectionAPIError pins error surfacing of the resolution
// fallback: an API refusal must wrap as APIError, not as a decode failure.
func TestGetBusinessConnectionAPIError(t *testing.T) {
	client, _ := testServer(t, func(w http.ResponseWriter, _ *http.Request) {
		errEnvelope(w, 400, "connection not found", 0)
	})
	_, err := client.GetBusinessConnection(context.Background(), "bc-missing")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != 400 {
		t.Fatalf("expected a 400 APIError, got %v", err)
	}
}

// TestDoRejectsUnreadableEnvelope pins fail-fast decoding: a non-JSON body
// must error without panicking, naming the method.
func TestDoRejectsUnreadableEnvelope(t *testing.T) {
	client, _ := testServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `not json {{{`)
	})
	err := client.SendMessageOnce(context.Background(), SendMessageRequest{ChatID: 42, Text: "hi"})
	if err == nil || !strings.Contains(err.Error(), "sendMessage") {
		t.Fatalf("expected a method-named decode error, got %v", err)
	}
}

// TestDoRejectsUnreadableResult pins the second decode stage: a valid
// envelope with a result of the wrong shape must error.
func TestDoRejectsUnreadableResult(t *testing.T) {
	client, _ := testServer(t, func(w http.ResponseWriter, _ *http.Request) {
		okEnvelope(t, w, `{"id":`)
	})
	err := client.SendMessageOnce(context.Background(), SendMessageRequest{ChatID: 42, Text: "hi"})
	if err == nil {
		t.Fatal("expected a result decode error")
	}
}

// TestNewClientDefaults ensures production construction never leaves the base
// URL empty (which would build a relative endpoint and fail obscurely).
func TestNewClientDefaults(t *testing.T) {
	c := NewClient("token-abc", 60*time.Second)
	if c.baseURL == "" || c.httpClient == nil {
		t.Fatalf("NewClient left zero fields: %+v", c)
	}
	if got := c.httpClient.Timeout; got != 60*time.Second {
		t.Fatalf("HTTP timeout = %v, want 60s", got)
	}
}
