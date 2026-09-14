package telegramtest

import (
	"bytes"
	"context"
	"testing"

	"github.com/LouisMoretti/Undelete/bot/internal/telegram"
)

// TestFindKeyCoversShapes pins the key search backing
// AssertNoBusinessConnectionID: top-level hit, nested maps, slices, scalars
// and absence. A pure function: every branch is reachable here.
func TestFindKeyCoversShapes(t *testing.T) {
	payload := map[string]any{
		"chat_id": float64(7),
		"nested": map[string]any{
			"list": []any{
				map[string]any{"business_connection_id": "bc-1"},
			},
		},
	}
	if got := findKey(payload, "business_connection_id", ""); got != ".nested.list[0].business_connection_id" {
		t.Fatalf("findKey(nested) = %q, want the full path", got)
	}
	if got := findKey(payload, "chat_id", ""); got != ".chat_id" {
		t.Fatalf("findKey(top) = %q, want .chat_id", got)
	}
	if got := findKey(payload, "absent", ""); got != "" {
		t.Fatalf("findKey(absent) = %q, want \"\"", got)
	}
	if got := findKey("scalar", "business_connection_id", ""); got != "" {
		t.Fatalf("findKey(scalar) = %q, want \"\"", got)
	}
	if got := findKey(nil, "business_connection_id", ""); got != "" {
		t.Fatalf("findKey(nil) = %q, want \"\"", got)
	}
}

// TestAssertNoBusinessConnectionIDPassesCleanPayload pins the guard on a
// payload that only MENTIONS the words in its text: the check targets keys,
// so a deleted message quoting "business_connection_id" stays a legitimate
// notification.
func TestAssertNoBusinessConnectionIDPassesCleanPayload(t *testing.T) {
	payload := []byte(`{"chat_id":7,"text":"someone wrote business_connection_id in the chat"}`)
	AssertNoBusinessConnectionID(t, payload)
}

// TestFixtureStripsExactlyOneNewline pins the comparison convention: the raw
// bytes of the body are compared to the fixture minus its single storage LF.
func TestFixtureStripsExactlyOneNewline(t *testing.T) {
	raw := Fixture(t, OKEnvelopeFixture)
	if len(raw) == 0 || bytes.HasSuffix(raw, []byte("\n")) {
		t.Fatalf("fixture %s: want non-empty bytes without trailing newline", OKEnvelopeFixture)
	}
}

// TestRecordingClientCapturesBodies pins the recorder: one send produces one
// recorded body carrying the request, in order.
func TestRecordingClientCapturesBodies(t *testing.T) {
	client, bodies := NewRecordingClient(t)
	if err := client.SendMessageOnce(context.Background(), telegram.SendMessageRequest{ChatID: 7, Text: "hi"}); err != nil {
		t.Fatalf("SendMessageOnce through the recorder: %v", err)
	}
	recorded := bodies()
	if len(recorded) != 1 {
		t.Fatalf("recorded %d bodies, want 1", len(recorded))
	}
	if !bytes.Contains(recorded[0], []byte(`"chat_id":7`)) {
		t.Fatalf("recorded body %s does not carry chat_id 7", recorded[0])
	}
}
