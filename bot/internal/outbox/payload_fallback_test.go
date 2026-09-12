package outbox

import (
	"testing"
)

func TestDecodeCorruptPayloadFallsBackToText(t *testing.T) {
	payload, err := decodeMediaPayload([]byte(`{"items":[{invalid`))
	if err == nil {
		t.Fatalf("expected decode error for corrupt JSON")
	}
	if payload != nil {
		t.Fatalf("corrupt payload must decode to nil (text fallback), got %+v", payload)
	}
	empty, err := decodeMediaPayload(nil)
	if err != nil || empty != nil {
		t.Fatalf("empty payload must be (nil,nil), got (%v,%v)", empty, err)
	}
}
