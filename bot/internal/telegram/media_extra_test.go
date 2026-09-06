package telegram

import (
	"encoding/json"
	"testing"
)

// TestExtractMediaAnimationShadowsDocument pins the documented Bot API
// duplication: Telegram mirrors every animation into Document for backward
// compatibility. ExtractMedia collapses the pair back into ONE attachment
// (the animation wins), even when the two file_ids differ.
func TestExtractMediaAnimationShadowsDocument(t *testing.T) {
	raw := `{"message_id":1,"chat":{"id":77,"type":"private"},"date":1700000000,
		"animation":{"file_id":"anim-id","file_unique_id":"anim-unique","width":320,"height":240,"duration":5},
		"document":{"file_id":"doc-id","file_unique_id":"doc-unique","mime_type":"video/mp4"}}`
	var msg Message
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	attachments := ExtractMedia(&msg)
	if len(attachments) != 1 {
		t.Fatalf("animation+document must collapse to 1 attachment, got %+v", attachments)
	}
	if attachments[0].Type != MediaTypeAnimation || attachments[0].FileID != "anim-id" {
		t.Fatalf("the animation must win, got %+v", attachments[0])
	}
}

// TestExtractMediaMultiAttachmentKeepsDeclarationOrder pins the ordering
// rule consumed by file_index: when a message carries several media (never
// in practice, always possible in the API), the declaration order is kept so
// a redelivery hits the same upsert slots.
func TestExtractMediaMultiAttachmentKeepsDeclarationOrder(t *testing.T) {
	raw := `{"message_id":1,"chat":{"id":77,"type":"private"},"date":1700000000,
		"video":{"file_id":"vid","file_unique_id":"vid-u","width":640,"height":480,"duration":10},
		"photo":[{"file_id":"ph","file_unique_id":"ph-u","width":800,"height":600}]}`
	var msg Message
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	attachments := ExtractMedia(&msg)
	if len(attachments) < 2 {
		t.Fatalf("expected at least 2 attachments, got %+v", attachments)
	}
	// Photo is extracted before video in ExtractMedia's declaration order.
	if attachments[0].Type != MediaTypePhoto || attachments[1].Type != MediaTypeVideo {
		t.Fatalf("declaration order not kept: %+v", attachments)
	}
}

// TestExtractMediaEmptyPhotoArray pins the degenerate array: "photo": []
// is not a media, and must not produce an attachment with empty ids.
func TestExtractMediaEmptyPhotoArray(t *testing.T) {
	raw := `{"message_id":1,"chat":{"id":77,"type":"private"},"date":1700000000,"photo":[]}`
	var msg Message
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if attachments := ExtractMedia(&msg); len(attachments) != 0 {
		t.Fatalf("empty photo array must yield no attachment, got %+v", attachments)
	}
}

// TestExtractMediaPhotoWithoutFileID pins the guard: a photo size without
// file_id is not downloadable and must be skipped, not stored with an empty
// handle the fetch loop could never resolve.
func TestExtractMediaPhotoWithoutFileID(t *testing.T) {
	raw := `{"message_id":1,"chat":{"id":77,"type":"private"},"date":1700000000,
		"photo":[{"file_unique_id":"ph-u","width":800,"height":600}]}`
	var msg Message
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if attachments := ExtractMedia(&msg); len(attachments) != 0 {
		t.Fatalf("photo without file_id must yield no attachment, got %+v", attachments)
	}
}

// TestExtractMediaCorruptRawFallsBackToNothing pins the robustness bound: a
// message whose raw bytes are gone (zero struct, no UnmarshalJSON) still
// extracts its KNOWN media; the unknown-media fallback simply finds nothing.
func TestExtractMediaCorruptRawFallsBackToNothing(t *testing.T) {
	msg := &Message{
		MessageID: 1,
		Chat:      Chat{ID: 77, Type: "private"},
		Text:      "plain",
	}
	if attachments := ExtractMedia(msg); len(attachments) != 0 {
		t.Fatalf("text-only zero struct must yield no attachment, got %+v", attachments)
	}
}
