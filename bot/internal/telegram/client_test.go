package telegram

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestTransportErrorDoesNotLeakBotToken(t *testing.T) {
	const token = "not-a-real-bot-token"

	client := NewClient(token, time.Second)
	client.baseURL = "https://example.invalid/bot"
	client.httpClient.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("network unavailable")
	})

	_, err := client.GetUpdates(context.Background(), 0, 50)
	if err == nil {
		t.Fatal("GetUpdates() should have failed")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("error exposes the bot token: %q", err)
	}
	if strings.Contains(err.Error(), "example.invalid") {
		t.Fatalf("error exposes the Bot API URL: %q", err)
	}
}
