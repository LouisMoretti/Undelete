package telegram

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// TestAllowedUpdatesIsTheExplicitBusinessSet pins constraint #1: without an
// explicit allowed_updates listing the four business_* types, Telegram sends
// none of them -- silently, with a 200 OK. Any change to this list (order,
// content) must be deliberate and breaks this test on purpose.
func TestAllowedUpdatesIsTheExplicitBusinessSet(t *testing.T) {
	want := []string{
		"business_connection",
		"business_message",
		"edited_business_message",
		"deleted_business_messages",
	}
	if !reflect.DeepEqual(AllowedUpdates, want) {
		t.Fatalf("AllowedUpdates = %q, want %q", AllowedUpdates, want)
	}
}

// TestSendMessageRequestHasNoBusinessConnectionID pins constraint #7 at the
// type level: the field must not exist (a send carrying it would go out AS
// the owner, inside the monitored chat), not merely be empty.
func TestSendMessageRequestHasNoBusinessConnectionID(t *testing.T) {
	typ := reflect.TypeOf(SendMessageRequest{})
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if strings.Contains(strings.ToLower(field.Name), "business") {
			t.Fatalf("SendMessageRequest must not expose a business field, found %s", field.Name)
		}
		if strings.Contains(strings.ToLower(field.Tag.Get("json")), "business") {
			t.Fatalf("SendMessageRequest must not serialize a business field, found tag %q", field.Tag.Get("json"))
		}
	}
	raw, err := json.Marshal(SendMessageRequest{ChatID: 42, Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "business_connection_id") {
		t.Fatalf("serialized SendMessageRequest leaks business_connection_id: %s", raw)
	}
}

// TestBuildWelcomeMessageRequestFallback pins the legacy fallback: when
// Telegram omits user_chat_id, the owner's user id is the recipient.
func TestBuildWelcomeMessageRequestFallback(t *testing.T) {
	got := BuildWelcomeMessageRequest(0, 700002)
	if got.ChatID != 700002 {
		t.Fatalf("fallback ChatID = %d, want 700002", got.ChatID)
	}
	got = BuildWelcomeMessageRequest(123456, 700002)
	if got.ChatID != 123456 {
		t.Fatalf("explicit user_chat_id must win, got ChatID = %d", got.ChatID)
	}
	if got.Text == "" {
		t.Fatal("welcome text must never be empty")
	}
}

// TestBuildMediaAlertTextCoversCardinality pins the one-line summary that
// travels with a media alert -- and its replacement when the files cannot go
// out (the worker appends MediaUnavailableNote to it).
func TestBuildMediaAlertTextCoversCardinality(t *testing.T) {
	tests := []struct {
		name  string
		types []string
		want  string
	}{
		{name: "empty means generic attachment", types: nil, want: "Attached media"},
		{name: "empty slice means generic attachment", types: []string{}, want: "Attached media"},
		{name: "singular", types: []string{"photo"}, want: "Attached media (1 file: photo)"},
		{name: "plural keeps declaration order", types: []string{"photo", "video", "document"}, want: "Attached media (3 files: photo, video, document)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := BuildMediaAlertText(tt.types); got != tt.want {
				t.Fatalf("BuildMediaAlertText(%q) = %q, want %q", tt.types, got, tt.want)
			}
		})
	}
}

// TestAPIErrorReporting pins the error rendering and the rate-limit
// predicate the poller and the outbox both rely on: a 429 WITHOUT retry_after
// is NOT rate-limited (plain backoff applies), so it must not take the
// retry_after path.
func TestAPIErrorReporting(t *testing.T) {
	err := &APIError{Method: "sendMessage", Code: 400, Description: "bad request"}
	if got := err.Error(); !strings.Contains(got, "sendMessage") || !strings.Contains(got, "400") {
		t.Fatalf("APIError.Error() = %q, must name method and code", got)
	}

	tests := []struct {
		name  string
		code  int
		retry int
		want  bool
	}{
		{name: "429 with retry_after", code: 429, retry: 17, want: true},
		{name: "429 without retry_after is not rate limited", code: 429, retry: 0, want: false},
		{name: "5xx is not rate limited", code: 503, retry: 5, want: false},
		{name: "400 is not rate limited", code: 400, retry: 5, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			apiErr := &APIError{Method: "getUpdates", Code: tt.code, RetryAfter: tt.retry}
			if got := apiErr.IsRateLimited(); got != tt.want {
				t.Fatalf("IsRateLimited(code=%d, retry_after=%d) = %t, want %t", tt.code, tt.retry, got, tt.want)
			}
		})
	}
}

// TestMessageUnmarshalJSONKeepsRawForUnknownMedia pins the forward-compat
// path: a message carrying a media type this version does not know must still
// decode (known fields intact) so ExtractMedia can surface it as unknown
// instead of dropping the update.
func TestMessageUnmarshalJSONKeepsRawForUnknownMedia(t *testing.T) {
	raw := `{"message_id":9,"chat":{"id":77,"type":"private"},"date":1700000000,"hologram":{"file_id":"abc"}}`
	var msg Message
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatalf("UnmarshalJSON must tolerate unknown media fields: %v", err)
	}
	if msg.MessageID != 9 || msg.Chat.ID != 77 {
		t.Fatalf("known fields lost in decode: %+v", msg)
	}
	attachments := ExtractMedia(&msg)
	found := false
	for _, a := range attachments {
		if a.Type == "unknown" {
			found = true
		}
	}
	if !found {
		t.Fatalf("unknown hologram media must surface as an attachment, got %+v", attachments)
	}
}

// TestFormatAlertDateEdges pins the date rendering of deletion alerts: 0 is
// "unknown", and the format is UTC regardless of the local timezone.
func TestFormatAlertDateEdges(t *testing.T) {
	if got := formatAlertDate(0); got != "unknown" {
		t.Fatalf("formatAlertDate(0) = %q, want unknown", got)
	}
	// 1700000000 = 2023-11-14 22:13 UTC.
	if got := formatAlertDate(1700000000); got != "2023-11-14 22:13 UTC" {
		t.Fatalf("formatAlertDate(1700000000) = %q, want 2023-11-14 22:13 UTC", got)
	}
}

// TestSplitTelegramTextLimitEdges pins the splitter at the exact Telegram
// limit: a text of exactly 4096 units stays one chunk, 4097 splits.
func TestSplitTelegramTextLimitEdges(t *testing.T) {
	exact := strings.Repeat("a", telegramTextLimit)
	if chunks := splitTelegramText(exact, telegramTextLimit); len(chunks) != 1 {
		t.Fatalf("exact-limit text must stay one chunk, got %d chunks", len(chunks))
	}
	over := strings.Repeat("b", telegramTextLimit+1)
	chunks := splitTelegramText(over, telegramTextLimit)
	if len(chunks) != 2 || len([]rune(chunks[0])) != telegramTextLimit || len([]rune(chunks[1])) != 1 {
		t.Fatalf("limit+1 text must split 4096/1, got %v", chunks)
	}
	if chunks := splitTelegramText("abc", 1); len(chunks) != 3 {
		t.Fatalf("limit=1 must split per rune, got %q", chunks)
	}
}

// TestBuildDeletionMessageRequestsWithZeroOwner pins the degenerate case: an
// alert without owner id still builds (delivery will fail downstream, but the
// builder itself must not panic or drop the content).
func TestBuildDeletionMessageRequestsWithZeroOwner(t *testing.T) {
	requests := BuildDeletionMessageRequests(DeletionAlert{ChatID: 77, Content: "hello"})
	if len(requests) != 1 || requests[0].ChatID != 0 {
		t.Fatalf("unexpected requests: %+v", requests)
	}
	if !strings.Contains(requests[0].Text, "hello") {
		t.Fatalf("content lost in degenerate alert: %q", requests[0].Text)
	}
}
