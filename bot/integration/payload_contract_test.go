//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/LouisMoretti/Undelete/bot/internal/messages"
	"github.com/LouisMoretti/Undelete/bot/internal/outbox"
	"github.com/LouisMoretti/Undelete/bot/internal/storage"
	"github.com/LouisMoretti/Undelete/bot/internal/users"
)

// TestPostgreSQL16OutboxPayloadContract proves on a real PostgreSQL 16 the
// wire contract of the rows MarkDeleted pins to notification_outbox:
//
//   - no chunk -- text or media -- carries a business_connection_id KEY (a
//     send carrying it would go out AS the owner, inside the monitored chat);
//   - the text chunks carry the chat identity header (label + id) and the
//     restored content;
//   - chunks of one deletion arrive in insertion (chunk_index) order;
//   - a media entry is written with its payload kind and survives the
//     encode/decode round trip through the database.
//
// The key search is structural (JSON keys, not a substring): a deleted
// message whose text mentions "business_connection_id" stays legitimate.
func TestPostgreSQL16OutboxPayloadContract(t *testing.T) {
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

	userRepo := users.NewRepository(db.Pool)
	owner, err := userRepo.UpsertByTelegramID(ctx, 93001)
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), `DELETE FROM users WHERE telegram_user_id IN (93001)`)
	})

	msgRepo := messages.NewRepository(db)
	longText := strings.Repeat("alerte ", 1000) // forces several 4096-unit chunks
	if err := msgRepo.Save(ctx, owner.ID, messages.Record{
		BusinessConnectionID: "payload-conn", ChatID: 7701, MessageID: 1,
		FromDisplay: "Louis", MessageType: "text", TextContent: longText,
		TelegramDate: 1788019201, ChatTitle: "Famille", ChatUsername: "famille",
		ChatType: "private",
	}, false); err != nil {
		t.Fatalf("save: %v", err)
	}
	// A message whose content mentions the forbidden key: the structural
	// check must not flag its TEXT.
	if err := msgRepo.Save(ctx, owner.ID, messages.Record{
		BusinessConnectionID: "payload-conn", ChatID: 7701, MessageID: 2,
		MessageType: "text", TextContent: "the words business_connection_id in plain text",
		TelegramDate: 1788019201, ChatTitle: "Famille", ChatType: "private",
	}, false); err != nil {
		t.Fatalf("save trap message: %v", err)
	}

	found, err := msgRepo.MarkDeleted(ctx, owner.ID, owner.TelegramUserID, "payload-conn", 7701, []int64{1, 2})
	if err != nil {
		t.Fatalf("mark deleted: %v", err)
	}
	if len(found) != 2 {
		t.Fatalf("found = %d, want 2", len(found))
	}

	// A media entry pinned directly: exercises InsertMediaTx (which no other
	// integration test calls nominally) and the media payload round trip.
	mediaPayload := outbox.MediaPayload{Items: []outbox.MediaItem{{
		MediaFileID:  1,
		MessageID:    1,
		MediaType:    "photo",
		RelativePath: "93001/2026/01/01/unique-1/photo",
		FileName:     "holiday.jpg",
		Caption:      "at the beach",
	}}}
	if err := db.InTenant(ctx, owner.ID, func(tx pgx.Tx) error {
		return outbox.InsertMediaTx(ctx, tx, owner.ID, owner.TelegramUserID,
			"payload-conn", 7701, 1, outbox.EventDeletedMessage, 99, "Attached media (1 file: photo)", mediaPayload)
	}); err != nil {
		t.Fatalf("insert media payload: %v", err)
	}

	type chunk struct {
		messageID int64
		index     int
		kind      string
		text      string
		payload   []byte
	}
	var chunks []chunk
	if err := db.InTenant(ctx, owner.ID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT message_id, chunk_index, payload_kind, payload_text, media_payload
			FROM notification_outbox WHERE chat_id = 7701 ORDER BY message_id, chunk_index
		`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c chunk
			if err := rows.Scan(&c.messageID, &c.index, &c.kind, &c.text, &c.payload); err != nil {
				return err
			}
			chunks = append(chunks, c)
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("read chunks: %v", err)
	}
	if len(chunks) < 3 {
		t.Fatalf("got %d chunks, want the text chunks plus the media entry", len(chunks))
	}
	// chunk_index restarts at 0 for each message (it is part of the
	// per-message anti-duplicate key): assert 0..N ordering WITHIN each
	// message rather than across the batch.
	byMessage := map[int64][]chunk{}
	for _, c := range chunks {
		byMessage[c.messageID] = append(byMessage[c.messageID], c)
	}
	for messageID, list := range byMessage {
		for i, c := range list {
			if c.kind == outbox.PayloadKindMedia {
				continue // the media entry takes the next free index
			}
			if c.index != i {
				t.Fatalf("message %d: chunk order broken at position %d: %+v", messageID, i, c)
			}
		}
	}
	for _, c := range chunks {
		assertNoJSONKey(t, c.text, "business_connection_id")
		if c.payload != nil {
			assertNoJSONKey(t, string(c.payload), "business_connection_id")
		}
	}
	if !strings.Contains(chunks[0].text, "Chat: Famille (@famille) (7701)") {
		t.Fatalf("first chunk lacks the chat identity header: %q", chunks[0].text)
	}
	trap := false
	for _, c := range byMessage[2] {
		if strings.Contains(c.text, "business_connection_id in plain text") {
			trap = true
		}
	}
	if !trap {
		t.Fatal("message content mentioning the key must survive verbatim")
	}

	var mediaChunk *chunk
	for i := range chunks {
		if chunks[i].kind == outbox.PayloadKindMedia {
			mediaChunk = &chunks[i]
		}
	}
	if mediaChunk == nil {
		t.Fatal("no media-kind chunk found")
	}
	var decoded outbox.MediaPayload
	if err := json.Unmarshal(mediaChunk.payload, &decoded); err != nil {
		t.Fatalf("media payload round trip through PostgreSQL: %v", err)
	}
	if len(decoded.Items) != 1 || decoded.Items[0].MediaType != "photo" || decoded.Items[0].Caption != "at the beach" {
		t.Fatalf("media payload corrupted: %+v", decoded)
	}
	if got := decoded.MediaTypes(); len(got) != 1 || got[0] != "photo" {
		t.Fatalf("MediaTypes() = %q", got)
	}
}

// assertNoJSONKey fails if the JSON document exposes key at any depth. A
// plain-text payload (the alert chunks) trivially passes: it is not JSON.
func assertNoJSONKey(t *testing.T, document, key string) {
	t.Helper()
	var decoded any
	if err := json.Unmarshal([]byte(document), &decoded); err != nil {
		return // not JSON: no key to leak
	}
	if path := findJSONKey(decoded, key, ""); path != "" {
		t.Fatalf("payload exposes %s at %s: %.200s", key, path, document)
	}
}

func findJSONKey(value any, key, path string) string {
	switch v := value.(type) {
	case map[string]any:
		for name, child := range v {
			childPath := path + "." + name
			if name == key {
				return childPath
			}
			if found := findJSONKey(child, key, childPath); found != "" {
				return found
			}
		}
	case []any:
		for i, child := range v {
			if found := findJSONKey(child, key, path+"[]"); found != "" {
				_ = i
				return found
			}
		}
	}
	return ""
}
