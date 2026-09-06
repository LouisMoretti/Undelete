package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func silentTestLogger() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

func newScriptServer(t *testing.T, script *scriptedPoller) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(script.handler))
	t.Cleanup(srv.Close)
	return srv
}

func newRawServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

// orderRecorder records handling order from a single-threaded handler.
type orderRecorder struct {
	mu    sync.Mutex
	items []int
}

func (o *orderRecorder) add(v int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.items = append(o.items, v)
}

func (o *orderRecorder) snapshot() []int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]int(nil), o.items...)
}

// scriptedPoller serves a script of getUpdates responses, then blocks until
// the context dies. Offsets seen by the server are recorded for assertions.
type scriptedPoller struct {
	t       *testing.T
	bodies  []string
	offsets []int64
	calls   atomic.Int64
}

func (s *scriptedPoller) handler(w http.ResponseWriter, r *http.Request) {
	var req getUpdatesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	s.offsets = append(s.offsets, req.Offset)
	n := s.calls.Add(1)
	if int(n) <= len(s.bodies) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"ok":true,"result":[%s]}`, s.bodies[n-1])
		return
	}
	// No more script: hold the long poll until the test ends it.
	<-r.Context().Done()
}

func updateJSON(id int64) string {
	return fmt.Sprintf(`{"update_id":%d,"business_message":{"message_id":%d,"chat":{"id":77,"type":"private"},"date":1700000000,"text":"hi"}}`, id, id)
}

// TestRunAdvancesOffsetDespiteHandlerErrors pins the poisoned-update rule:
// the offset advances EVEN IF the handler fails, so one permanently failing
// update can never freeze the bot on redelivery.
func TestRunAdvancesOffsetDespiteHandlerErrors(t *testing.T) {
	script := &scriptedPoller{t: t, bodies: []string{updateJSON(5) + "," + updateJSON(6)}}
	srv := newScriptServer(t, script)
	client := NewClient("token", 5*time.Second, WithBaseURL(srv.URL+"/bot"))
	poller := NewPoller(client, silentTestLogger())

	var handled []int64
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handle := func(ctx context.Context, u Update) error {
		handled = append(handled, u.UpdateID)
		if u.UpdateID == 5 {
			return errors.New("poisoned update")
		}
		cancel() // end the run after the second update
		return nil
	}
	_ = poller.Run(ctx, handle)

	if len(handled) != 2 || handled[0] != 5 || handled[1] != 6 {
		t.Fatalf("both updates must be delivered in order, got %v", handled)
	}
	if poller.offset != 7 {
		t.Fatalf("offset = %d, want 7 (advanced past the failed update)", poller.offset)
	}
}

// TestRunProcessesBatchSequentially pins constraint #5 at runtime: updates
// from one batch are handled one at a time, in emission order -- a deletion
// can never overtake the save it refers to.
func TestRunProcessesBatchSequentially(t *testing.T) {
	script := &scriptedPoller{t: t, bodies: []string{
		updateJSON(1) + "," + updateJSON(2) + "," + updateJSON(3),
	}}
	srv := newScriptServer(t, script)
	client := NewClient("token", 5*time.Second, WithBaseURL(srv.URL+"/bot"))
	poller := NewPoller(client, silentTestLogger())

	var mu orderRecorder
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	count := 0
	_ = poller.Run(ctx, func(context.Context, Update) error {
		count++
		mu.add(count)
		if count == 3 {
			cancel()
		}
		return nil
	})
	if got := mu.snapshot(); len(got) != 3 || got[0] != 1 || got[2] != 3 {
		t.Fatalf("handling order = %v, want [1 2 3]", got)
	}
}

// TestRunReturnsOnImmediateCancellation pins the shutdown entry: a cancelled
// context returns the context error without a single getUpdates call.
func TestRunReturnsOnImmediateCancellation(t *testing.T) {
	script := &scriptedPoller{t: t}
	srv := newScriptServer(t, script)
	client := NewClient("token", 5*time.Second, WithBaseURL(srv.URL+"/bot"))
	poller := NewPoller(client, silentTestLogger())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := poller.Run(ctx, func(context.Context, Update) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run(cancelled) = %v, want context.Canceled", err)
	}
	if script.calls.Load() != 0 {
		t.Fatalf("cancelled run must issue no getUpdates, got %d", script.calls.Load())
	}
}

// TestRunRespectsRetryAfter pins the 429 discipline on the poll path: the
// wait equals retry_after (not the exponential backoff), then polling
// resumes and the offset flow continues.
func TestRunRespectsRetryAfter(t *testing.T) {
	var calls atomic.Int64
	srv := newRawServer(t, func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			fmt.Fprint(w, `{"ok":false,"error_code":429,"description":"slow down","parameters":{"retry_after":1}}`)
			return
		}
		fmt.Fprintf(w, `{"ok":true,"result":[%s]}`, updateJSON(40))
	})
	client := NewClient("token", 5*time.Second, WithBaseURL(srv.URL+"/bot"))
	poller := NewPoller(client, silentTestLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	start := time.Now()
	_ = poller.Run(ctx, func(context.Context, Update) error {
		cancel()
		return nil
	})
	if elapsed := time.Since(start); elapsed < time.Second {
		t.Fatalf("429 retry_after=1 must be honored, resumed after %v", elapsed)
	}
	if poller.offset != 41 {
		t.Fatalf("offset = %d, want 41", poller.offset)
	}
	if poller.LastSuccessfulPoll().IsZero() {
		t.Fatal("LastSuccessfulPoll must advance after the post-429 success")
	}
}
