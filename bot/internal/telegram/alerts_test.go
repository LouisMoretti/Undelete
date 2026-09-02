package telegram

import (
	"strings"
	"testing"
	"unicode/utf16"
)

// baseAlert is a complete alert; each test degrades it on the single point
// it checks.
func baseAlert() DeletionAlert {
	senderID := int64(900123)
	return DeletionAlert{
		OwnerTelegramUserID: 700001,
		ChatID:              800001,
		ChatTitle:           "Anaïs",
		FromDisplay:         "Anaïs (@fixture_sender)",
		FromUserID:          &senderID,
		MessageType:         "text",
		TelegramDate:        1788019201,
		Content:             "Hello, coffee ☕ — seen before?",
	}
}

func singleText(t *testing.T, alert DeletionAlert) string {
	t.Helper()
	requests := BuildDeletionMessageRequests(alert)
	if len(requests) != 1 {
		t.Fatalf("number of requests = %d, want 1", len(requests))
	}
	if requests[0].ChatID != alert.OwnerTelegramUserID {
		t.Fatalf("alert addressed to chat %d, want the owner %d", requests[0].ChatID, alert.OwnerTelegramUserID)
	}
	return requests[0].Text
}

func TestBuildDeletionMessageRequestsFullIdentity(t *testing.T) {
	want := "Recovered deleted message\n" +
		"Chat: Anaïs (800001)\n" +
		"From: Anaïs (@fixture_sender) (900123)\n" +
		"Type: text\n" +
		"Date: 2026-08-29 16:00 UTC\n" +
		"\n" +
		"Hello, coffee ☕ — seen before?"
	if got := singleText(t, baseAlert()); got != want {
		t.Fatalf("alert =\n%s\nwant\n%s", got, want)
	}
}

func TestBuildDeletionMessageRequestsFallbackForUnknownIdentity(t *testing.T) {
	tests := []struct {
		name    string
		degrade func(*DeletionAlert)
		want    string
	}{
		{
			name:    "chat label missing",
			degrade: func(a *DeletionAlert) { a.ChatTitle = "" },
			want:    "Chat: chat 800001",
		},
		{
			name:    "chat known by its @username alone",
			degrade: func(a *DeletionAlert) { a.ChatTitle = ""; a.ChatUsername = "salon_test" },
			want:    "Chat: @salon_test (800001)",
		},
		{
			name:    "title and @username",
			degrade: func(a *DeletionAlert) { a.ChatUsername = "salon_test" },
			want:    "Chat: Anaïs (@salon_test) (800001)",
		},
		{
			name:    "from_display empty",
			degrade: func(a *DeletionAlert) { a.FromDisplay = "" },
			want:    "From: unknown (900123)",
		},
		{
			name:    "from_user_id NULL",
			degrade: func(a *DeletionAlert) { a.FromUserID = nil },
			want:    "From: Anaïs (@fixture_sender)",
		},
		{
			name:    "sender entirely unknown",
			degrade: func(a *DeletionAlert) { a.FromDisplay = ""; a.FromUserID = nil },
			want:    "From: unknown",
		},
		{
			name:    "date missing",
			degrade: func(a *DeletionAlert) { a.TelegramDate = 0 },
			want:    "Date: unknown",
		},
		{
			name:    "type missing",
			degrade: func(a *DeletionAlert) { a.MessageType = "" },
			want:    "Type: unknown",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			alert := baseAlert()
			tt.degrade(&alert)
			text := singleText(t, alert)
			if !strings.Contains(text, tt.want+"\n") {
				t.Fatalf("alert =\n%s\nwant a line %q", text, tt.want)
			}
			// The numeric chat_id stays visible no matter what: it is the
			// only identifier Telegram guarantees.
			if !strings.Contains(text, "800001") {
				t.Fatalf("chat_id missing from the alert:\n%s", text)
			}
		})
	}
}

// TestBuildDeletionMessageRequestsWithoutContent covers future attachments
// (Phase 2): a non-text message_type has no text to restore, so the header
// must stand on its own without a dangling empty line or broken format.
func TestBuildDeletionMessageRequestsWithoutContent(t *testing.T) {
	alert := baseAlert()
	alert.MessageType = "photo"
	alert.Content = ""

	text := singleText(t, alert)
	if !strings.HasSuffix(text, "Date: 2026-08-29 16:00 UTC") {
		t.Fatalf("alert without content badly terminated:\n%q", text)
	}
	if !strings.Contains(text, "Type: photo\n") {
		t.Fatalf("type missing from the alert without content:\n%s", text)
	}
}

// TestBuildDeletionMessageRequestsPreservesContentAndUTF16Limit verifies that
// the limit applies to the FINAL text (identity header included): the
// enrichment decides the number of chunks, not the content alone.
func TestBuildDeletionMessageRequestsPreservesContentAndUTF16Limit(t *testing.T) {
	content := strings.Repeat("a", 4095) + "😀" + strings.Repeat("é", 10)
	alert := baseAlert()
	alert.Content = content
	requests := BuildDeletionMessageRequests(alert)

	if len(requests) != 2 {
		t.Fatalf("number of requests = %d, want 2", len(requests))
	}
	header := "Recovered deleted message\n" +
		"Chat: Anaïs (800001)\n" +
		"From: Anaïs (@fixture_sender) (900123)\n" +
		"Type: text\n" +
		"Date: 2026-08-29 16:00 UTC\n\n"
	texts := make([]string, 0, len(requests))
	for i, request := range requests {
		if request.ChatID != 700001 {
			t.Fatalf("request %d: chat_id = %d, want 700001", i, request.ChatID)
		}
		texts = append(texts, request.Text)
		units := 0
		for _, r := range request.Text {
			units += utf16.RuneLen(r)
		}
		if units > telegramTextLimit {
			t.Fatalf("request %d contains %d UTF-16 units", i, units)
		}
	}
	if strings.Join(texts, "") != header+content {
		t.Fatal("splitting modified or truncated the notification")
	}
	// The header consumes units: the very first chunk therefore ends BEFORE
	// the end of content that alone fit within 4096 units.
	if strings.HasSuffix(texts[0], "😀") {
		t.Fatal("splitting seems to ignore the identity header")
	}
}

// TestBuildDeletionMessageRequestsUnicodeInHeader verifies that splitting
// holds even when it is the identity labels, not the content, that carry
// non-BMP characters.
func TestBuildDeletionMessageRequestsUnicodeInHeader(t *testing.T) {
	alert := baseAlert()
	alert.ChatTitle = strings.Repeat("🐈", 2000)
	alert.FromDisplay = strings.Repeat("é", 500)
	alert.Content = strings.Repeat("😀", 200)

	requests := BuildDeletionMessageRequests(alert)
	if len(requests) < 2 {
		t.Fatalf("number of requests = %d, want at least 2", len(requests))
	}
	var rebuilt strings.Builder
	for i, request := range requests {
		units := 0
		for _, r := range request.Text {
			units += utf16.RuneLen(r)
		}
		if units > telegramTextLimit {
			t.Fatalf("request %d contains %d UTF-16 units", i, units)
		}
		rebuilt.WriteString(request.Text)
	}
	if !strings.HasSuffix(rebuilt.String(), alert.Content) {
		t.Fatal("the Unicode content was truncated by splitting")
	}
}

func TestSplitTelegramTextRejectsInvalidInput(t *testing.T) {
	if chunks := splitTelegramText("", telegramTextLimit); chunks != nil {
		t.Fatalf("empty text: chunks = %#v, want nil", chunks)
	}
	if chunks := splitTelegramText("test", 0); chunks != nil {
		t.Fatalf("invalid limit: chunks = %#v, want nil", chunks)
	}
}
