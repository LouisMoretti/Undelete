package telegram

import (
	"strings"
	"testing"
	"unicode/utf16"
)

// TestParseCommandShapes covers what Telegram actually puts on the wire for a
// command, and — more importantly — what must NOT be read as one: a command
// is only a command when the slash sits at offset 0.
func TestParseCommandShapes(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
		ok   bool
	}{
		{name: "bare command", text: "/privacy", want: "/privacy", ok: true},
		{name: "with bot mention", text: "/privacy@undelete_bot", want: "/privacy", ok: true},
		{name: "with arguments", text: "/privacy please", want: "/privacy", ok: true},
		{name: "mention and arguments", text: "/privacy@undelete_bot now", want: "/privacy", ok: true},
		{name: "capitalised by the client", text: "/Privacy", want: "/privacy", ok: true},
		{name: "uppercase", text: "/PRIVACY", want: "/privacy", ok: true},
		{name: "newline separated arguments", text: "/privacy\nsecond line", want: "/privacy", ok: true},
		{name: "tab separated arguments", text: "/privacy\targ", want: "/privacy", ok: true},
		{name: "unknown command stays distinct", text: "/privacy_policy", want: "/privacy_policy", ok: true},
		{name: "leading space is not a command", text: " /privacy", ok: false},
		{name: "quoted inside a sentence", text: "tell me about /privacy", ok: false},
		{name: "empty text", text: "", ok: false},
		{name: "slash alone", text: "/", ok: false},
		{name: "slash with mention only", text: "/@undelete_bot", ok: false},
		{name: "plain message", text: "privacy", ok: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParseCommand(tt.text)
			if ok != tt.ok {
				t.Fatalf("ParseCommand(%q) ok = %v, want %v", tt.text, ok, tt.ok)
			}
			if got != tt.want {
				t.Fatalf("ParseCommand(%q) = %q, want %q", tt.text, got, tt.want)
			}
		})
	}
}

// TestCommandPrivacyIsTheRecognisedSpelling pins the constant the handler
// compares against to what a user types.
func TestCommandPrivacyIsTheRecognisedSpelling(t *testing.T) {
	command, ok := ParseCommand("/privacy")
	if !ok || command != CommandPrivacy {
		t.Fatalf("ParseCommand(%q) = (%q, %v), want (%q, true)", "/privacy", command, ok, CommandPrivacy)
	}
}

// TestBuildPrivacyMessageRequestsSplitsOnUTF16Units is the core guarantee of
// the command: the policy is longer than one Telegram message, so it always
// goes out split — and the limit is counted in UTF-16 units, the unit
// Telegram itself counts in.
func TestBuildPrivacyMessageRequestsSplitsOnUTF16Units(t *testing.T) {
	// 😀 and 𝄞 are outside the BMP: 2 UTF-16 units each, 4 bytes each, 1 rune
	// each. Counting bytes or runes here would produce a different split, and
	// a rejected message on the Telegram side.
	text := strings.Repeat("😀", 3000) + strings.Repeat("é", 1000) + strings.Repeat("𝄞", 500)

	requests := BuildPrivacyMessageRequests(700001, text)
	if len(requests) < 2 {
		t.Fatalf("number of requests = %d, want at least 2", len(requests))
	}

	var rebuilt strings.Builder
	for index, request := range requests {
		if request.ChatID != 700001 {
			t.Fatalf("request %d: chat_id = %d, want 700001", index, request.ChatID)
		}
		if units := utf16Len(request.Text); units > telegramTextLimit {
			t.Fatalf("request %d contains %d UTF-16 units, limit %d", index, units, telegramTextLimit)
		}
		rebuilt.WriteString(request.Text)
	}
	if rebuilt.String() != text {
		t.Fatal("splitting altered or truncated the policy")
	}
}

// TestBuildPrivacyMessageRequestsNeverSplitsACharacter guards the boundary
// case: a chunk must never end between the two UTF-16 units of a single
// non-BMP character, which would produce an unpaired surrogate.
func TestBuildPrivacyMessageRequestsNeverSplitsACharacter(t *testing.T) {
	// Exactly one unit short of the limit before the emoji: the emoji cannot
	// fit and must move to the next chunk whole.
	text := strings.Repeat("a", telegramTextLimit-1) + "😀" + "tail"

	requests := BuildPrivacyMessageRequests(700001, text)
	if len(requests) != 2 {
		t.Fatalf("number of requests = %d, want 2", len(requests))
	}
	if utf16Len(requests[0].Text) != telegramTextLimit-1 {
		t.Fatalf("first chunk = %d units, want %d", utf16Len(requests[0].Text), telegramTextLimit-1)
	}
	if requests[1].Text != "😀tail" {
		t.Fatalf("second chunk = %q, want %q", requests[1].Text, "😀tail")
	}
	for index, request := range requests {
		for _, r := range request.Text {
			if r == '�' {
				t.Fatalf("request %d contains a replacement character: a character was cut in half", index)
			}
		}
	}
}

// TestBuildPrivacyMessageRequestsShortTextStaysSingle keeps the common case
// honest: nothing is split that does not need to be.
func TestBuildPrivacyMessageRequestsShortTextStaysSingle(t *testing.T) {
	requests := BuildPrivacyMessageRequests(700001, "short policy")
	if len(requests) != 1 {
		t.Fatalf("number of requests = %d, want 1", len(requests))
	}
	if requests[0].Text != "short policy" {
		t.Fatalf("text = %q, want %q", requests[0].Text, "short policy")
	}
}

// TestBuildPrivacyMessageRequestsEmptyTextSendsNothing: Telegram rejects an
// empty sendMessage, so an empty policy must produce zero requests rather
// than one doomed call.
func TestBuildPrivacyMessageRequestsEmptyTextSendsNothing(t *testing.T) {
	if requests := BuildPrivacyMessageRequests(700001, ""); len(requests) != 0 {
		t.Fatalf("number of requests = %d, want 0", len(requests))
	}
}

func utf16Len(s string) int {
	units := 0
	for _, r := range s {
		units += utf16.RuneLen(r)
	}
	return units
}
