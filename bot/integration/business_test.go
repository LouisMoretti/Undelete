//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/LouisMoretti/Undelete/bot/internal/business"
	"github.com/LouisMoretti/Undelete/bot/internal/storage"
	"github.com/LouisMoretti/Undelete/bot/internal/telegram"
	"github.com/LouisMoretti/Undelete/bot/internal/users"
)

// scriptedBotAPI is a minimal Bot API double: getBusinessConnection answers
// the scripted connection, every other method answers the success envelope
// and records its body.
type scriptedBotAPI struct {
	t      *testing.T
	conn   map[string]any
	bodies [][]byte
	mu     sync.Mutex
}

func (s *scriptedBotAPI) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.bodies = append(s.bodies, body)
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if strings.HasSuffix(r.URL.Path, "/getBusinessConnection") {
		var req struct {
			BusinessConnectionID string `json:"business_connection_id"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t := s.t
			t.Errorf("unreadable getBusinessConnection request: %v", err)
		}
		conn, ok := s.conn[requestID(body)]
		if !ok {
			fmt.Fprint(w, `{"ok":false,"error_code":400,"description":"connection not found"}`)
			return
		}
		raw, _ := json.Marshal(conn)
		fmt.Fprintf(w, `{"ok":true,"result":%s}`, raw)
		return
	}
	fmt.Fprint(w, `{"ok":true,"result":true}`)
}

func requestID(body []byte) string {
	var req struct {
		BusinessConnectionID string `json:"business_connection_id"`
	}
	_ = json.Unmarshal(body, &req)
	return req.BusinessConnectionID
}

func businessConn(id string, userID int64, enabled bool) map[string]any {
	return map[string]any{
		"id":           id,
		"user":         map[string]any{"id": userID, "first_name": "Louis"},
		"user_chat_id": userID,
		"date":         1700000000,
		"is_enabled":   enabled,
		"rights":       map[string]any{"can_reply": true},
	}
}

// TestPostgreSQL16BusinessResolution proves on a real PostgreSQL 16 the
// three-level chain of business.Service (cache -> database -> Telegram API)
// and the mono-tenant guard at every level:
//
//   - HandleBusinessConnection persists the connection and welcomes the owner;
//   - a refused connection (guard) persists nothing and sends nothing;
//   - Resolve serves from cache (no second HTTP call), then from the database
//     after a restart (new Service, silent API), then from the API for a
//     connection never seen (upserted to the database and cached);
//   - a historical row for a now-guarded owner is refused from the database;
//   - a disabled connection is stored but never welcomed.
func TestPostgreSQL16BusinessResolution(t *testing.T) {
	adminDSN := requireEnv(t, "POSTGRES_INTEGRATION_ADMIN_DSN")
	runtimeDSN := requireEnv(t, "POSTGRES_INTEGRATION_RUNTIME_DSN")
	if err := validateExplicitDestructiveOptIn(os.Getenv("POSTGRES_INTEGRATION_ALLOW_DESTRUCTIVE")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), setupTimeout)
	defer cancel()

	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	defer admin.Close(ctx)
	var databaseName string
	if err := admin.QueryRow(ctx, `SELECT current_database()`).Scan(&databaseName); err != nil {
		t.Fatalf("read current database for destructive interlock: %v", err)
	}
	if err := validateDestructiveInterlock(os.Getenv("POSTGRES_INTEGRATION_ALLOW_DESTRUCTIVE"), databaseName); err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := storage.RunMigrations(ctx, adminDSN, logger); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	db, err := storage.NewPool(ctx, runtimeDSN)
	if err != nil {
		t.Fatalf("open runtime pool: %v", err)
	}
	defer db.Close()

	api := &scriptedBotAPI{t: t, conn: map[string]any{"bc-int-1": businessConn("bc-int-1", 93101, true)}}
	server := httptest.NewServer(http.HandlerFunc(api.handler))
	defer server.Close()
	newClient := func() *telegram.Client {
		return telegram.NewClient("test-token", 5*time.Second, telegram.WithBaseURL(server.URL+"/bot"))
	}
	userRepo := users.NewRepository(db.Pool)
	newService := func(filter int64) *business.Service {
		return business.NewService(db.Pool, newClient(), userRepo, filter, logger)
	}
	sends := func() int {
		api.mu.Lock()
		defer api.mu.Unlock()
		n := 0
		for _, body := range api.bodies {
			var decoded map[string]any
			_ = json.Unmarshal(body, &decoded)
			if _, ok := decoded["text"]; ok {
				n++
			}
		}
		return n
	}
	rowExists := func(id string) bool {
		var found bool
		if err := admin.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM business_connections WHERE id = $1)`, id).Scan(&found); err != nil {
			t.Fatalf("connection existence: %v", err)
		}
		return found
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), `DELETE FROM business_connections WHERE id LIKE 'bc-int-%'`)
		_, _ = admin.Exec(context.Background(), `DELETE FROM users WHERE telegram_user_id IN (93101, 93102)`)
	})

	t.Run("welcome on a fresh connection", func(t *testing.T) {
		ctx := phaseContext(t)
		svc := newService(0)
		before := sends()
		if err := svc.HandleBusinessConnection(ctx, telegram.BusinessConnection{
			ID: "bc-int-1", User: telegram.User{ID: 93101, FirstName: "Louis"},
			UserChatID: 93101, IsEnabled: true, Rights: &telegram.BusinessBotRights{CanReply: true},
		}); err != nil {
			t.Fatalf("handle: %v", err)
		}
		if !rowExists("bc-int-1") {
			t.Fatal("connection not persisted")
		}
		if sends() != before+1 {
			t.Fatal("fresh enabled connection must trigger exactly one welcome")
		}
		// No welcome carries a business_connection_id: the welcome is a plain
		// sendMessage to the owner's chat.
		api.mu.Lock()
		last := api.bodies[len(api.bodies)-1]
		api.mu.Unlock()
		var decoded map[string]any
		if err := json.Unmarshal(last, &decoded); err != nil {
			t.Fatalf("welcome payload unreadable: %v", err)
		}
		if _, ok := decoded["business_connection_id"]; ok {
			t.Fatalf("welcome exposes business_connection_id: %s", last)
		}
	})

	t.Run("guard refusal persists and sends nothing", func(t *testing.T) {
		ctx := phaseContext(t)
		svc := newService(93101) // only Louis's account is allowed
		before := sends()
		if err := svc.HandleBusinessConnection(ctx, telegram.BusinessConnection{
			ID: "bc-int-foreign", User: telegram.User{ID: 93102, FirstName: "Mallory"},
			UserChatID: 93102, IsEnabled: true,
		}); err != nil {
			t.Fatalf("refusal must return nil, got %v", err)
		}
		if rowExists("bc-int-foreign") {
			t.Fatal("refused connection must not be persisted")
		}
		if sends() != before {
			t.Fatal("refused connection must not send a welcome")
		}
	})

	t.Run("resolve serves cache then database then API", func(t *testing.T) {
		ctx := phaseContext(t)
		svc := newService(0)
		first, err := svc.Resolve(ctx, "bc-int-1")
		if err != nil {
			t.Fatalf("resolve cached: %v", err)
		}
		if first.OwnerTelegramUserID != 93101 || !first.IsEnabled {
			t.Fatalf("unexpected connection: %+v", first)
		}

		// Restart: a new Service with a silent API. The row must come from
		// the database, with no HTTP call at all.
		silent := &scriptedBotAPI{t: t, conn: map[string]any{}}
		silentServer := httptest.NewServer(http.HandlerFunc(silent.handler))
		defer silentServer.Close()
		restarted := business.NewService(db.Pool,
			telegram.NewClient("test-token", 5*time.Second, telegram.WithBaseURL(silentServer.URL+"/bot")),
			userRepo, 0, logger)
		resolved, err := restarted.Resolve(ctx, "bc-int-1")
		if err != nil {
			t.Fatalf("resolve from database: %v", err)
		}
		if resolved.OwnerTelegramUserID != 93101 {
			t.Fatalf("database hit returned %+v", resolved)
		}
		silent.mu.Lock()
		calls := len(silent.bodies)
		silent.mu.Unlock()
		if calls != 0 {
			t.Fatalf("database hit issued %d HTTP calls, want 0", calls)
		}

		// Unknown connection: the API is the last resort, and the result is
		// upserted (a second Resolve is a cache hit, no new call).
		api.conn["bc-int-2"] = businessConn("bc-int-2", 93101, true)
		withAPI := newService(0)
		viaAPI, err := withAPI.Resolve(ctx, "bc-int-2")
		if err != nil {
			t.Fatalf("resolve via API: %v", err)
		}
		if viaAPI.OwnerTelegramUserID != 93101 {
			t.Fatalf("API hit returned %+v", viaAPI)
		}
		if !rowExists("bc-int-2") {
			t.Fatal("API-resolved connection must be upserted to the database")
		}
	})

	t.Run("historical row for a guarded owner stays refused", func(t *testing.T) {
		ctx := phaseContext(t)
		// bc-int-1 belongs to 93101 in the database; enabling the guard for
		// NOBODY restarts the refusal on historical data too.
		guarded := business.NewService(db.Pool, newClient(), userRepo, 999999, logger)
		if _, err := guarded.Resolve(ctx, "bc-int-1"); err == nil {
			t.Fatal("historical connection for a guarded owner must be refused")
		} else if !strings.Contains(err.Error(), "does not match") {
			t.Fatalf("refusal must name the guard, got %v", err)
		}
	})

	t.Run("disabled connection is stored but never welcomed", func(t *testing.T) {
		ctx := phaseContext(t)
		svc := newService(0)
		before := sends()
		if err := svc.HandleBusinessConnection(ctx, telegram.BusinessConnection{
			ID: "bc-int-off", User: telegram.User{ID: 93101, FirstName: "Louis"},
			UserChatID: 93101, IsEnabled: false,
		}); err != nil {
			t.Fatalf("handle disabled: %v", err)
		}
		if !rowExists("bc-int-off") {
			t.Fatal("disabled connection must still be persisted")
		}
		if sends() != before {
			t.Fatal("disabled connection must not trigger a welcome")
		}
	})
}
