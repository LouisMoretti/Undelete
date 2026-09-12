package telegram

import (
	"fmt"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/LouisMoretti/Undelete/bot/internal/privacy"
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
		// A client on Windows ends a line with CRLF: stopping on "\n" alone
		// would leave the carriage return glued to the command, which then
		// matches nothing.
		{name: "crlf separated arguments", text: "/privacy\r\nsecond line", want: "/privacy", ok: true},
		{name: "trailing carriage return", text: "/privacy\r", want: "/privacy", ok: true},
		{name: "non-breaking space separated arguments", text: "/privacy\u00a0arg", want: "/privacy", ok: true},
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

// policyChunkBody strips the label a chunk carries and, doing so, checks that
// the label announces the answer that was actually built: rank and total. Every
// assertion about the document itself runs on the body -- the label is the
// envelope, not the policy.
func policyChunkBody(t *testing.T, requests []SendMessageRequest, index int) string {
	t.Helper()
	prefix := fmt.Sprintf("Privacy policy (%d/%d)", index+1, len(requests)) + "\n\n"
	if !strings.HasPrefix(requests[index].Text, prefix) {
		t.Fatalf("request %d does not start with %q, got %q...", index, prefix, head(requests[index].Text, 40))
	}
	return strings.TrimPrefix(requests[index].Text, prefix)
}

// policyBodies is the answer as the owner reads it: the chunks without their
// labels, in order. Joining them must return the document byte for byte.
func policyBodies(t *testing.T, requests []SendMessageRequest) []string {
	t.Helper()
	bodies := make([]string, 0, len(requests))
	for index := range requests {
		bodies = append(bodies, policyChunkBody(t, requests, index))
	}
	return bodies
}

// TestPrivacyAnswerIsLabelledAndWholeOnTheRealDocument runs on the document
// actually served, not on a fixture: it is the only text whose length, and
// therefore whose split, ships to users.
func TestPrivacyAnswerIsLabelledAndWholeOnTheRealDocument(t *testing.T) {
	policyText := privacy.Text()
	requests := BuildPrivacyMessageRequests(700001, policyText)
	if len(requests) < 2 {
		t.Fatalf("requests = %d: the policy is longer than one Telegram message, the split must happen", len(requests))
	}

	var rebuilt strings.Builder
	for index, body := range policyBodies(t, requests) {
		if requests[index].ChatID != 700001 {
			t.Fatalf("request %d: chat_id = %d, want the owner (700001)", index, requests[index].ChatID)
		}
		// The label counts against the limit like any other character: the
		// assertion is on the FINAL text, not on the body.
		if units := utf16Units(requests[index].Text); units > telegramTextLimit {
			t.Fatalf("request %d contains %d UTF-16 units, limit %d", index, units, telegramTextLimit)
		}
		rebuilt.WriteString(body)
	}
	if rebuilt.String() != policyText {
		t.Fatal("the labelled chunks do not rebuild the policy document, whole and in order")
	}
}

// TestPrivacyAnswerCutsOnParagraphBoundaries is the regression this fix is
// about. The blind splitter cut the real document inside "purged
// automatically", in the very section stating that media archives are NOT
// purged: the reader of chunk 1 was left with a sentence saying the opposite of
// what the policy says.
func TestPrivacyAnswerCutsOnParagraphBoundaries(t *testing.T) {
	policyText := privacy.Text()
	bodies := policyBodies(t, BuildPrivacyMessageRequests(700001, policyText))

	offset := 0
	for index, body := range bodies[:len(bodies)-1] {
		offset += len(body)
		before, _ := utf8.DecodeLastRuneInString(policyText[:offset])
		// A chunk ends with the blank line that closed its last paragraph, so
		// the character before a boundary is always whitespace: no boundary
		// lands inside a word. This also fails the day the document grows a
		// single paragraph longer than a whole message -- which is worth
		// knowing, since that paragraph would go out cut mid-word.
		if !unicode.IsSpace(before) {
			after, _ := utf8.DecodeRuneInString(policyText[offset:])
			t.Fatalf("the boundary after chunk %d falls inside %q%q, not on a blank line",
				index+1, before, after)
		}
	}

	// The exact phrase the old split cut in half, asserted whole inside one
	// chunk rather than merely "somewhere in the answer".
	const sentence = "Media archives are NOT\npurged automatically"
	if !strings.Contains(policyText, sentence) {
		t.Fatalf("the document no longer contains %q: update this test to the sentence it now carries", sentence)
	}
	found := false
	for _, body := range bodies {
		if strings.Contains(body, sentence) {
			found = true
		}
	}
	if !found {
		t.Fatalf("%q is split across two messages", sentence)
	}
}

// TestPrivacyAnswerSplitsOnUTF16Units keeps the unit of the limit honest: a
// text of 3000 emoji is 3000 runes but 6000 UTF-16 units, and Telegram counts
// the latter.
func TestPrivacyAnswerSplitsOnUTF16Units(t *testing.T) {
	// 😀 and 𝄞 are outside the BMP: 2 UTF-16 units each, 4 bytes each, 1 rune
	// each. Counting bytes or runes here would produce a different split, and a
	// rejected message on the Telegram side. No blank line anywhere: this text
	// is one paragraph, so it exercises the rune-level fallback.
	text := strings.Repeat("😀", 3000) + strings.Repeat("é", 1000) + strings.Repeat("𝄞", 500)

	requests := BuildPrivacyMessageRequests(700001, text)
	if len(requests) < 2 {
		t.Fatalf("number of requests = %d, want at least 2", len(requests))
	}

	var rebuilt strings.Builder
	for index, body := range policyBodies(t, requests) {
		if requests[index].ChatID != 700001 {
			t.Fatalf("request %d: chat_id = %d, want 700001", index, requests[index].ChatID)
		}
		if units := utf16Units(requests[index].Text); units > telegramTextLimit {
			t.Fatalf("request %d contains %d UTF-16 units, limit %d", index, units, telegramTextLimit)
		}
		if strings.ContainsRune(body, '�') {
			t.Fatalf("request %d contains a replacement character: a character was cut in half", index)
		}
		rebuilt.WriteString(body)
	}
	if rebuilt.String() != text {
		t.Fatal("splitting altered or truncated the text")
	}
}

// TestPrivacyAnswerKeepsTheLabelWithinTheLimitWhenChunksAreMany covers the
// knot between the label and the count: the label announces the total, and the
// width of the total is what the split has to reserve. A two-digit total must
// not push the last chunk over the limit.
func TestPrivacyAnswerKeepsTheLabelWithinTheLimitWhenChunksAreMany(t *testing.T) {
	// Paragraphs of ~1000 units: enough of them to need a two-digit total.
	paragraph := strings.Repeat("word ", 200)
	text := strings.TrimSuffix(strings.Repeat(paragraph+"\n\n", 60), "\n\n")

	requests := BuildPrivacyMessageRequests(700001, text)
	if len(requests) < 10 {
		t.Fatalf("requests = %d, want at least 10 to exercise a two-digit total", len(requests))
	}

	var rebuilt strings.Builder
	for index, body := range policyBodies(t, requests) {
		if units := utf16Units(requests[index].Text); units > telegramTextLimit {
			t.Fatalf("request %d contains %d UTF-16 units, limit %d", index, units, telegramTextLimit)
		}
		rebuilt.WriteString(body)
	}
	if rebuilt.String() != text {
		t.Fatal("splitting altered or truncated the text")
	}
}

// TestPrivacyAnswerShortTextStaysSingle keeps the common case honest: nothing
// is split that does not need to be, and a single chunk is still labelled --
// "(1/1)" is what tells the reader nothing is missing.
func TestPrivacyAnswerShortTextStaysSingle(t *testing.T) {
	requests := BuildPrivacyMessageRequests(700001, "short policy")
	if len(requests) != 1 {
		t.Fatalf("number of requests = %d, want 1", len(requests))
	}
	if requests[0].Text != "Privacy policy (1/1)\n\nshort policy" {
		t.Fatalf("text = %q", requests[0].Text)
	}
}

// TestPrivacyAnswerEmptyTextSendsNothing: Telegram rejects an empty
// sendMessage, so an empty policy must produce zero requests rather than one
// doomed call carrying nothing but a label.
func TestPrivacyAnswerEmptyTextSendsNothing(t *testing.T) {
	if requests := BuildPrivacyMessageRequests(700001, ""); len(requests) != 0 {
		t.Fatalf("number of requests = %d, want 0", len(requests))
	}
}

// TestSplitPolicyTextFallsBackInsideAnOversizedParagraph: a paragraph longer
// than one message has no boundary to align on. The fallback is word-blind by
// necessity, but it must stay lossless and must never cut a surrogate pair.
func TestSplitPolicyTextFallsBackInsideAnOversizedParagraph(t *testing.T) {
	const limit = 100
	oversized := strings.Repeat("a", 99) + "😀" + strings.Repeat("b", 150)
	text := "short first paragraph\n\n" + oversized + "\n\nshort last paragraph"

	chunks := splitPolicyText(text, limit)
	if strings.Join(chunks, "") != text {
		t.Fatalf("the split is not lossless: %q", chunks)
	}
	for index, chunk := range chunks {
		if units := utf16Units(chunk); units > limit {
			t.Fatalf("chunk %d = %d units, limit %d", index, units, limit)
		}
		if strings.ContainsRune(chunk, '�') {
			t.Fatalf("chunk %d cut a character in half: %q", index, chunk)
		}
	}
	// The 99 "a" plus the 2-unit emoji do not fit in 100 units: the emoji moves
	// to the next chunk whole rather than leaving a lone surrogate behind.
	if !strings.Contains(chunks[1], strings.Repeat("a", 99)) {
		t.Fatalf("chunk 1 = %q, want the 99 a's of the oversized paragraph", head(chunks[1], 40))
	}
	if !strings.HasPrefix(chunks[2], "😀") {
		t.Fatalf("chunk 2 = %q, want the emoji whole at its head", head(chunks[2], 40))
	}
}

// TestSplitPolicyTextEdges covers the degenerate inputs the fixpoint in
// BuildPrivacyMessageRequests relies on, and the blank-line run a hand-edited
// document eventually grows.
func TestSplitPolicyTextEdges(t *testing.T) {
	if chunks := splitPolicyText("", telegramTextLimit); chunks != nil {
		t.Fatalf("empty text = %q, want nil", chunks)
	}
	if chunks := splitPolicyText("text", 0); chunks != nil {
		t.Fatalf("limit 0 = %q, want nil", chunks)
	}

	// Three blank lines are ONE boundary: absorbing them keeps the empty lines
	// attached to the paragraph they follow instead of producing a chunk made
	// of newlines.
	text := "aaaa\n\n\n\nbbbb\n\ncccc"
	chunks := splitPolicyText(text, 10)
	if strings.Join(chunks, "") != text {
		t.Fatalf("the split is not lossless: %q", chunks)
	}
	if len(chunks) != 2 || chunks[0] != "aaaa\n\n\n\n" {
		t.Fatalf("chunks = %q, want the blank-line run kept with the paragraph it follows", chunks)
	}
	for index, chunk := range chunks {
		if strings.TrimSpace(chunk) == "" {
			t.Fatalf("chunk %d is blank: %q", index, chunk)
		}
	}

	// A paragraph that fits exactly is not split.
	if chunks := splitPolicyText(strings.Repeat("a", 10), 10); len(chunks) != 1 {
		t.Fatalf("exact-limit paragraph = %d chunks, want 1", len(chunks))
	}
}

// head is the readable prefix of a string in a failure message, cut on a rune
// boundary.
func head(text string, runes int) string {
	r := []rune(text)
	if len(r) <= runes {
		return text
	}
	return string(r[:runes])
}
