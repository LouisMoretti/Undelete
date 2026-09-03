// Package telegramtest provides the helpers shared by the Bot API contract
// tests. It lives apart from package telegram so it can be imported by the
// calling packages (app, business), which must be able to exercise the real
// production paths -- alert construction included -- against the same
// fixtures as the client.
//
// Comparison convention (a single one, valid for all fixtures): the raw
// bytes of the HTTP body are compared to those of the file via bytes.Equal.
// The ONLY normalization applied is the removal of the single terminal
// storage "\n" required by Fixture; no space, no indentation, no CRLF is
// tolerated or cleaned elsewhere.
package telegramtest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/LouisMoretti/Undelete/bot/internal/telegram"
)

// Version is the fixture directory pinned for the covered Bot API version
// (cf. testdata/README.md).
const Version = "bot-api-10.3"

// OKEnvelopeFixture is a TRANSPORT stub, not a contract: it only verifies
// that the client accepts a {"ok": true} envelope. Its empty `result`
// describes no scenario and must never be read as the expected response for
// a given sendMessage (SendMessage ignores `result`).
const OKEnvelopeFixture = "send-message-ok-envelope.json"

// Token is the dummy token used in the verified URL path.
const Token = "test-token"

// fixtureDir locates testdata via this package's source file rather than via
// the current directory: the calling tests (app, business) run from their own
// package directory.
func fixtureDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "testdata", Version)
}

// Fixture reads a fixture and returns its bytes, stripped of the single
// terminal LF storage newline. Any other termination (absent, doubled, CRLF)
// is an error: that is what makes the byte-by-byte comparison exact rather
// than approximate (cf. package comment).
func Fixture(t testing.TB, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(fixtureDir(), name))
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	if !bytes.HasSuffix(data, []byte("\n")) || bytes.HasSuffix(data, []byte("\n\n")) || bytes.HasSuffix(data, []byte("\r\n")) {
		t.Fatalf("fixture %s: want exactly one trailing LF newline", name)
	}
	return data[:len(data)-1]
}

// Call describes an expected HTTP call, in order.
type Call struct {
	// Method is the expected Bot API method (getUpdates, sendMessage, ...).
	Method string
	// RequestFixture is compared byte by byte to the sent body.
	RequestFixture string
	// ResponseFixture is returned as-is to the client.
	ResponseFixture string
}

// NewClient returns a real telegram.Client pointed at an httptest server
// that verifies each call against calls, in order, and fails at cleanup if
// not all expected calls happened.
func NewClient(t testing.TB, calls ...Call) *telegram.Client {
	t.Helper()

	var (
		mu sync.Mutex
		n  int
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		index := n
		n++
		mu.Unlock()

		if index >= len(calls) {
			t.Errorf("unexpected HTTP call #%d (%s), %d expected", index+1, r.URL.Path, len(calls))
			// 4xx: definitive on the client side, so no retry or wait.
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"ok":false,"error_code":400,"description":"unexpected call"}`))
			return
		}
		call := calls[index]

		if r.Method != http.MethodPost {
			t.Errorf("call %d: HTTP method = %s, want POST", index+1, r.Method)
		}
		if want := "/bot" + Token + "/" + call.Method; r.URL.Path != want {
			t.Errorf("call %d: path = %s, want %s", index+1, r.URL.Path, want)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("call %d: Content-Type = %q, want application/json", index+1, got)
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("call %d: reading request: %v", index+1, err)
			return
		}
		if want := Fixture(t, call.RequestFixture); !bytes.Equal(body, want) {
			t.Errorf("call %d: JSON %s = %s, want %s", index+1, call.Method, body, want)
		}

		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write(Fixture(t, call.ResponseFixture)); err != nil {
			t.Errorf("call %d: writing response: %v", index+1, err)
		}
	}))

	t.Cleanup(func() {
		server.Close()
		mu.Lock()
		defer mu.Unlock()
		if n != len(calls) {
			t.Errorf("number of HTTP calls = %d, want %d", n, len(calls))
		}
	})

	return telegram.NewClient(Token, time.Second, telegram.WithBaseURL(server.URL+"/bot"))
}

// NewRecordingClient returns a real telegram.Client that always answers the
// success envelope and records the emitted request bodies, in order.
//
// Use it when what matters is the NUMBER and the SEQUENCE of calls produced
// by a production path (a notification split into several sendMessage calls,
// for example) rather than conformance to a pinned fixture.
func NewRecordingClient(t testing.TB) (*telegram.Client, func() [][]byte) {
	t.Helper()

	var (
		mu     sync.Mutex
		bodies [][]byte
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading recorded request: %v", err)
			return
		}
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write(Fixture(t, OKEnvelopeFixture)); err != nil {
			t.Errorf("writing response: %v", err)
		}
	}))
	t.Cleanup(server.Close)

	client := telegram.NewClient(Token, time.Second, telegram.WithBaseURL(server.URL+"/bot"))
	return client, func() [][]byte {
		mu.Lock()
		defer mu.Unlock()
		return append([][]byte(nil), bodies...)
	}
}

// AssertNoBusinessConnectionID fails if the serialized payload exposes, at
// any depth, a business_connection_id KEY.
//
// Constraint 7, verified on the JSON actually emitted: no reference to a Go
// field name, so the guard survives any evolution of the SendMessageRequest
// type (renaming as well as field removal). The check targets keys, not a
// substring: a deleted message whose text would contain these words remains
// a legitimate notification.
func AssertNoBusinessConnectionID(t testing.TB, payload []byte) {
	t.Helper()
	var decoded any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("unreadable sendMessage payload: %v", err)
	}
	if path := findKey(decoded, "business_connection_id", ""); path != "" {
		t.Fatalf("sendMessage payload exposing business_connection_id at %s: %s", path, payload)
	}
}

// findKey returns the path of the first occurrence of key, or "".
func findKey(value any, key, path string) string {
	switch v := value.(type) {
	case map[string]any:
		for name, child := range v {
			childPath := path + "." + name
			if name == key {
				return childPath
			}
			if found := findKey(child, key, childPath); found != "" {
				return found
			}
		}
	case []any:
		for i, child := range v {
			if found := findKey(child, key, fmt.Sprintf("%s[%d]", path, i)); found != "" {
				return found
			}
		}
	}
	return ""
}
