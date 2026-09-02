// This file is in package telegram_test (external test), not in package
// telegram: it imports telegramtest, which itself imports telegram. The
// external test is what makes this dependency legal, and it also guarantees
// that the contracts are verified via the package's public API, as app and
// business do.
package telegram_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/LouisMoretti/Undelete/bot/internal/telegram"
	"github.com/LouisMoretti/Undelete/bot/internal/telegram/telegramtest"
)

func TestGetUpdatesBotAPIContract(t *testing.T) {
	client := telegramtest.NewClient(t, telegramtest.Call{
		Method:          "getUpdates",
		RequestFixture:  "get-updates-request.json",
		ResponseFixture: "get-updates-response.json",
	})
	updates, err := client.GetUpdates(context.Background(), 9000, 50)
	if err != nil {
		t.Fatalf("GetUpdates(): %v", err)
	}
	if len(updates) != 4 {
		t.Fatalf("number of updates = %d, want 4", len(updates))
	}

	connection := updates[0].BusinessConnection
	if connection == nil || connection.UserChatID != 700002 || !connection.CanReply() {
		t.Fatalf("business_connection malformed: %#v", connection)
	}
	message := updates[1].BusinessMessage
	if message == nil || message.From == nil || message.Text != "Hello, coffee ☕ — seen before?" {
		t.Fatalf("business_message malformed: %#v", message)
	}
	edited := updates[2].EditedBusinessMessage
	if edited == nil || edited.From != nil || edited.Text != "Corrected text 🧪" {
		t.Fatalf("edited_business_message without from malformed: %#v", edited)
	}
	deleted := updates[3].DeletedBusinessMessages
	if deleted == nil || !reflect.DeepEqual(deleted.MessageIDs, []int64{501, 502, 503}) {
		t.Fatalf("bulk deletion malformed: %#v", deleted)
	}
}

func TestGetBusinessConnectionBotAPIContract(t *testing.T) {
	client := telegramtest.NewClient(t, telegramtest.Call{
		Method:          "getBusinessConnection",
		RequestFixture:  "get-business-connection-request.json",
		ResponseFixture: "get-business-connection-response.json",
	})
	connection, err := client.GetBusinessConnection(context.Background(), "bc_fixture_001")
	if err != nil {
		t.Fatalf("GetBusinessConnection(): %v", err)
	}
	if connection.ID != "bc_fixture_001" || connection.UserChatID != 700002 || !connection.CanReply() {
		t.Fatalf("connection malformed: %#v", connection)
	}
	if connection.User.LastName != "" || connection.User.Username != "" {
		t.Fatalf("unexpected optional user fields: %#v", connection.User)
	}
}

// TestGetBusinessConnectionLegacyCanReplyContract pins down the legacy
// BusinessConnection wire path: `can_reply` set directly on the connection,
// without a `rights` block. Until now this path was only exercised in-process
// (from a hand-built Go struct); the fixture proves that a Bot API response
// actually shaped this way is decoded as "can reply" and not silently as
// false.
func TestGetBusinessConnectionLegacyCanReplyContract(t *testing.T) {
	client := telegramtest.NewClient(t, telegramtest.Call{
		Method:          "getBusinessConnection",
		RequestFixture:  "get-business-connection-request.json",
		ResponseFixture: "get-business-connection-legacy-can-reply-response.json",
	})
	connection, err := client.GetBusinessConnection(context.Background(), "bc_fixture_001")
	if err != nil {
		t.Fatalf("GetBusinessConnection(): %v", err)
	}
	if connection.Rights != nil {
		t.Fatalf("the legacy fixture must not carry any rights block: %#v", connection.Rights)
	}
	if !connection.CanReplyLegacy || !connection.CanReply() {
		t.Fatalf("legacy can_reply malformed: %#v", connection)
	}
}

// TestGetUpdatesFirstPollOffsetZeroContract pins down the serialization of
// the VERY first poll: `offset` is `omitempty`, so offset 0 does not appear
// in the emitted body. This is the expected behavior — Telegram treats the
// absence of an offset as "from the oldest unacknowledged update" — but it
// is pinned nowhere else, even though it governs the first call of every bot
// startup.
func TestGetUpdatesFirstPollOffsetZeroContract(t *testing.T) {
	client := telegramtest.NewClient(t, telegramtest.Call{
		Method:          "getUpdates",
		RequestFixture:  "get-updates-first-poll-request.json",
		ResponseFixture: "get-updates-empty-response.json",
	})
	updates, err := client.GetUpdates(context.Background(), 0, 50)
	if err != nil {
		t.Fatalf("GetUpdates(): %v", err)
	}
	if len(updates) != 0 {
		t.Fatalf("number of updates = %d, want 0", len(updates))
	}

	// The body has already been compared byte by byte by the test server;
	// here we verify that it is indeed the ABSENCE of the key that is pinned,
	// and not an "offset":0 that would have been copied into the fixture.
	var body map[string]any
	if err := json.Unmarshal(telegramtest.Fixture(t, "get-updates-first-poll-request.json"), &body); err != nil {
		t.Fatalf("decoding first-poll fixture: %v", err)
	}
	if _, ok := body["offset"]; ok {
		t.Fatalf("the first-poll fixture serializes offset: %v", body["offset"])
	}
}

// TestSendMessageRetryAfterContract pins the RESENT request after a 429
// envelope: the client must respect retry_after then resend strictly
// identical bytes. The envelope itself was only covered in-process; the
// byte-by-byte mechanism additionally verifies that no parameter is added or
// lost on the second attempt.
//
// retry_after is 1 second in the fixture: enough to exercise SendMessage's
// real wait without lengthening the test suite.
func TestSendMessageRetryAfterContract(t *testing.T) {
	const fixture = "send-message-welcome-request.json"
	client := telegramtest.NewClient(t,
		telegramtest.Call{
			Method:          "sendMessage",
			RequestFixture:  fixture,
			ResponseFixture: "send-message-rate-limited-response.json",
		},
		telegramtest.Call{
			Method:          "sendMessage",
			RequestFixture:  fixture,
			ResponseFixture: telegramtest.OKEnvelopeFixture,
		},
	)

	start := time.Now()
	if err := client.SendMessage(context.Background(), telegram.BuildWelcomeMessageRequest(700002, 700001)); err != nil {
		t.Fatalf("SendMessage(): %v", err)
	}
	if elapsed := time.Since(start); elapsed < time.Second {
		t.Fatalf("retry_after not respected: resent after %s, want >= 1s", elapsed)
	}
}

// TestRateLimitedEnvelopeIsDecodedAsAPIError pins reading the 429 envelope
// itself: code and retry_after must surface in *telegram.APIError, the only
// way the poller and SendMessage know how long to wait.
func TestRateLimitedEnvelopeIsDecodedAsAPIError(t *testing.T) {
	client := telegramtest.NewClient(t, telegramtest.Call{
		Method:          "sendMessage",
		RequestFixture:  "send-message-welcome-request.json",
		ResponseFixture: "send-message-rate-limited-response.json",
	})

	err := client.SendMessageOnce(context.Background(), telegram.BuildWelcomeMessageRequest(700002, 700001))
	var apiErr *telegram.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("SendMessageOnce() = %v, want *telegram.APIError", err)
	}
	if apiErr.Code != http.StatusTooManyRequests || apiErr.RetryAfter != 1 || !apiErr.IsRateLimited() {
		t.Fatalf("429 envelope malformed: %#v", apiErr)
	}
}

// TestSendMessageRequestNeverSerializesBusinessConnectionID materializes
// constraint 7 without ever naming a Go field: the type is inspected through
// its JSON tags (recursively, embedded fields included) then through the
// payload it actually produces. The test thus stays valid if
// SendMessageRequest gains, loses, or renames fields.
func TestSendMessageRequestNeverSerializesBusinessConnectionID(t *testing.T) {
	assertNoBusinessConnectionIDTag(t, reflect.TypeOf(telegram.SendMessageRequest{}), "SendMessageRequest")

	payload, err := json.Marshal(telegram.SendMessageRequest{ChatID: 700001, Text: "control payload"})
	if err != nil {
		t.Fatalf("serializing SendMessageRequest: %v", err)
	}
	telegramtest.AssertNoBusinessConnectionID(t, payload)
}

// fixtureSenderID materializes a populated from_user_id (the column is
// NULLable, hence the pointer).
func fixtureSenderID() *int64 {
	id := int64(800001)
	return &id
}

func assertNoBusinessConnectionIDTag(t *testing.T, typ reflect.Type, path string) {
	t.Helper()
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ.Kind() != reflect.Struct {
		return
	}
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if !field.IsExported() {
			continue // not serialized by encoding/json
		}
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		if name == "-" {
			continue
		}
		if name == "" {
			name = field.Name
		}
		if name == "business_connection_id" {
			t.Fatalf("%s.%s serializes business_connection_id", path, field.Name)
		}
		// An embedded field hoists its own keys to the parent level: its tags
		// therefore count as much as those of the type itself.
		if field.Anonymous {
			assertNoBusinessConnectionIDTag(t, field.Type, path+"."+field.Name)
		}
	}
}

// TestSendMessageAlertContracts pins both alert payloads exactly as built by
// the production builders, byte by byte.
//
// The call paths that feed these builders in production are covered at their
// own level, otherwise this test would prove nothing about what the bot
// really sends: business.TestWelcomeAlertContract (welcome) and, for
// deletion, messages.Repository.MarkDeleted -- which writes these same
// chunks to the outbox -- verified by the PostgreSQL integration suite
// ("chat labels are tenant isolated and reach the alert").
func TestSendMessageAlertContracts(t *testing.T) {
	tests := []struct {
		name     string
		fixture  string
		requests []telegram.SendMessageRequest
	}{
		{
			name:     "welcome",
			fixture:  "send-message-welcome-request.json",
			requests: []telegram.SendMessageRequest{telegram.BuildWelcomeMessageRequest(700002, 700001)},
		},
		{
			name:    "deletion",
			fixture: "send-message-deletion-request.json",
			// Same scenario as the business_message update in
			// get-updates-response.json, seen from the deletion side: private
			// chat "Anaïs" (800001), namesake sender, message send date.
			requests: telegram.BuildDeletionMessageRequests(telegram.DeletionAlert{
				OwnerTelegramUserID: 700001,
				ChatID:              800001,
				ChatTitle:           "Anaïs",
				FromDisplay:         "Anaïs (@fixture_sender)",
				FromUserID:          fixtureSenderID(),
				MessageType:         "text",
				TelegramDate:        1788019201,
				Content:             "Hello, coffee ☕ — seen before?",
			}),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if len(tt.requests) != 1 {
				t.Fatalf("number of requests = %d, want 1 for this fixture", len(tt.requests))
			}
			client := telegramtest.NewClient(t, telegramtest.Call{
				Method:          "sendMessage",
				RequestFixture:  tt.fixture,
				ResponseFixture: telegramtest.OKEnvelopeFixture,
			})
			if err := client.SendMessage(context.Background(), tt.requests[0]); err != nil {
				t.Fatalf("SendMessage(): %v", err)
			}
			telegramtest.AssertNoBusinessConnectionID(t, telegramtest.Fixture(t, tt.fixture))
		})
	}
}

// TestWelcomeMessageTextIsTheProductionText locks the welcome text pinned in
// the fixture to the one produced by the production builder: the fixture
// cannot drift toward a "test" text.
func TestWelcomeMessageTextIsTheProductionText(t *testing.T) {
	var fixture struct {
		ChatID int64  `json:"chat_id"`
		Text   string `json:"text"`
	}
	if err := json.Unmarshal(telegramtest.Fixture(t, "send-message-welcome-request.json"), &fixture); err != nil {
		t.Fatalf("decoding welcome fixture: %v", err)
	}
	if got := telegram.BuildWelcomeMessageRequest(700002, 700001).Text; got != fixture.Text {
		t.Fatalf("production welcome text = %q, fixture = %q", got, fixture.Text)
	}
}
