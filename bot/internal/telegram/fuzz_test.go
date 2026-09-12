package telegram

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode"
)

// FuzzParseCommand fuzzes the command gate: it must never panic, a non-slash
// input is never a command, and an accepted command is normalised (leading
// slash, no whitespace, no "@suffix", lowercased, never bare "/").
// Run with: go test -fuzz=FuzzParseCommand -fuzztime=30s ./internal/telegram/
func FuzzParseCommand(f *testing.F) {
	seeds := []string{
		"/privacy",
		"/privacy@undelete_bot",
		"/privacy please",
		"/Privacy",
		"/PRIVACY",
		"/privacy\r\nsecond line",
		"/privacy arg",
		"/privacy_policy",
		" /privacy",
		"tell me about /privacy",
		"",
		"/",
		"/@undelete_bot",
		"privacy",
		"/privacy\x00arg",
		"//privacy",
		"/é",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, text string) {
		cmd, ok := ParseCommand(text)
		if !strings.HasPrefix(text, "/") {
			if ok {
				t.Fatalf("ParseCommand(%q) ok = true, want false (must start with /)", text)
			}
			return
		}
		if !ok {
			// Only "/"-shaped non-commands may be refused: bare "/" and
			// "/@mention" (suffix strips down to "/").
			return
		}
		if cmd == "" || cmd == "/" {
			t.Fatalf("ParseCommand(%q) = %q, want a real command", text, cmd)
		}
		if !strings.HasPrefix(cmd, "/") {
			t.Fatalf("ParseCommand(%q) = %q, want leading slash", text, cmd)
		}
		if strings.IndexFunc(cmd, unicode.IsSpace) >= 0 {
			t.Fatalf("ParseCommand(%q) = %q, want no whitespace", text, cmd)
		}
		if strings.Contains(cmd, "@") {
			t.Fatalf("ParseCommand(%q) = %q, want no @suffix", text, cmd)
		}
		if cmd != strings.ToLower(cmd) {
			t.Fatalf("ParseCommand(%q) = %q, want lowercase", text, cmd)
		}
	})
}

// FuzzSplitTelegramText fuzzes the rune-level splitter: lossless partition
// (concat == input), no empty chunks, and every chunk within the limit as soon
// as the limit can hold the widest UTF-16 rune (2 units).
// Run with: go test -fuzz=FuzzSplitTelegramText -fuzztime=30s ./internal/telegram/
func FuzzSplitTelegramText(f *testing.F) {
	seeds := []string{
		"hello",
		strings.Repeat("a", 4096),
		strings.Repeat("b", 4097),
		strings.Repeat("😀", 2048),
		strings.Repeat("😀", 2049),
		"a😀b",
		"",
		"a\nb\nc",
	}
	for _, s := range seeds {
		f.Add(s, 4096)
	}
	f.Fuzz(func(t *testing.T, text string, limit int) {
		// Keep the fuzzer on meaningful inputs: degenerate limits return nil
		// by contract, and gigantic limits only waste time.
		if limit < 2 || limit > 8192 {
			t.Skip()
		}
		chunks := splitTelegramText(text, limit)
		if text == "" {
			if chunks != nil {
				t.Fatalf("splitTelegramText(\"\", %d) = %q, want nil", limit, chunks)
			}
			return
		}
		if len(chunks) == 0 {
			t.Fatalf("splitTelegramText(%q, %d) = empty, want ≥1 chunk", text, limit)
		}
		for i, c := range chunks {
			if c == "" {
				t.Fatalf("splitTelegramText(%q, %d) chunk %d is empty", text, limit, i)
			}
			if u := utf16Units(c); u > limit {
				t.Fatalf("splitTelegramText(%q, %d) chunk %d is %d units, limit %d", text, limit, i, u, limit)
			}
		}
		if got := strings.Join(chunks, ""); got != text {
			t.Fatalf("splitTelegramText(%q, %d) is lossy: rejoin = %q", text, limit, got)
		}
	})
}

// FuzzSplitPolicyText fuzzes the paragraph-aware splitter with the same
// contract: lossless, no empty chunks, every chunk within the limit
// (limit >= 2, same UTF-16 reason as above).
// Run with: go test -fuzz=FuzzSplitPolicyText -fuzztime=30s ./internal/telegram/
func FuzzSplitPolicyText(f *testing.F) {
	seeds := []string{
		"first paragraph\n\nsecond paragraph",
		"short\n\n" + strings.Repeat("a", 5000) + "\n\ntail",
		"a\n\n\n\nb",
		"",
		"single line",
		"para one\n\npara two\n\npara three",
		strings.Repeat("word ", 200),
		"emoji 😀\n\n" + strings.Repeat("😀", 3000),
	}
	for _, s := range seeds {
		f.Add(s, 4096)
	}
	f.Fuzz(func(t *testing.T, text string, limit int) {
		if limit < 2 || limit > 8192 {
			t.Skip()
		}
		chunks := splitPolicyText(text, limit)
		if text == "" {
			if chunks != nil {
				t.Fatalf("splitPolicyText(\"\", %d) = %q, want nil", limit, chunks)
			}
			return
		}
		if len(chunks) == 0 {
			t.Fatalf("splitPolicyText(%q, %d) = empty, want ≥1 chunk", text, limit)
		}
		for i, c := range chunks {
			if c == "" {
				t.Fatalf("splitPolicyText(%q, %d) chunk %d is empty", text, limit, i)
			}
			if u := utf16Units(c); u > limit {
				t.Fatalf("splitPolicyText(%q, %d) chunk %d is %d units, limit %d", text, limit, i, u, limit)
			}
		}
		if got := strings.Join(chunks, ""); got != text {
			t.Fatalf("splitPolicyText(%q, %d) is lossy: rejoin = %q", text, limit, got)
		}
	})
}

// FuzzMessageUnmarshalJSON fuzzes the business_message decoder: arbitrary bytes
// must never panic, and decoding must be deterministic (same input twice =>
// same result, raw copy included).
// Run with: go test -fuzz=FuzzMessageUnmarshalJSON -fuzztime=30s ./internal/telegram/
func FuzzMessageUnmarshalJSON(f *testing.F) {
	seeds := []string{
		`{"message_id":1,"date":1700000000,"chat":{"id":7,"type":"private"},"text":"hi"}`,
		`{"message_id":2,"date":1700000000,"chat":{"id":7,"type":"private"},"photo":[{"file_id":"a","file_unique_id":"b","width":1,"height":1}]}`,
		`{}`,
		`null`,
		`[]`,
		`{"message_id":"not-a-number"}`,
		`{"message_id":3,"chat":{"id":7,"type":"private"},"unknown_media":{"file_id":"x"}}`,
		`not json at all`,
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		var first Message
		firstErr := json.Unmarshal(data, &first)
		var second Message
		secondErr := json.Unmarshal(data, &second)
		if (firstErr == nil) != (secondErr == nil) {
			t.Fatalf("non-deterministic errors for %q: %v vs %v", data, firstErr, secondErr)
		}
		if firstErr != nil {
			return
		}
		firstRaw, _ := json.Marshal(first.raw)
		secondRaw, _ := json.Marshal(second.raw)
		if string(firstRaw) != string(secondRaw) {
			t.Fatalf("non-deterministic raw for %q", data)
		}
		if first.MessageID != second.MessageID || first.Text != second.Text || first.Caption != second.Caption {
			t.Fatalf("non-deterministic decode for %q", data)
		}
	})
}
