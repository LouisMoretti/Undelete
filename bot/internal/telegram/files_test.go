package telegram

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestGetFileResolvesDownloadPath(t *testing.T) {
	const token = "not-a-real-bot-token"

	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"file_id":"AgAC","file_unique_id":"AQADuniq","file_size":1234,"file_path":"photos/file_0.jpg"}}`))
	}))
	defer srv.Close()

	client := NewClient(token, time.Second, WithBaseURL(srv.URL+"/bot"))
	file, err := client.GetFile(context.Background(), "AgAC")
	if err != nil {
		t.Fatalf("GetFile() = %v", err)
	}
	if want := "/bot" + token + "/getFile"; gotPath != want {
		t.Fatalf("path = %q, want %q", gotPath, want)
	}
	if file.FileUniqueID != "AQADuniq" || file.FilePath != "photos/file_0.jpg" || file.FileSize != 1234 {
		t.Fatalf("unexpected file: %+v", file)
	}
}

// A file older than one hour returns 400: the error must surface as an
// APIError so the caller can distinguish it from a network failure, and it
// must not carry the token.
func TestGetFileAPIErrorDoesNotLeakToken(t *testing.T) {
	const token = "not-a-real-bot-token"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: file is temporarily unavailable"}`))
	}))
	defer srv.Close()

	client := NewClient(token, time.Second, WithBaseURL(srv.URL+"/bot"))
	_, err := client.GetFile(context.Background(), "AgAC")
	if err == nil {
		t.Fatal("GetFile() should have failed")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("error exposes the bot token: %q", err)
	}
}
