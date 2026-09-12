package telegram

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestClientRefusesOversizedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true,"result":`))
		// Stream more than the limit without holding it all in memory.
		chunk := strings.Repeat("x", 1<<20)
		for i := 0; i < 12; i++ {
			_, _ = w.Write([]byte(chunk))
		}
		_, _ = w.Write([]byte(`}`))
	}))
	defer srv.Close()

	c := NewClient("token", 5*time.Second, WithBaseURL(srv.URL+"/bot"))
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/bot"+"token/getUpdates", nil)
	if err := c.do(req, "getUpdates", nil); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("expected oversize error, got %v", err)
	}
}
