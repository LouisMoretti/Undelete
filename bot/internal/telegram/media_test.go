package telegram

import (
	"encoding/json"
	"reflect"
	"testing"
)

// decodeMessage decodes an inline fixture. The fixtures are deliberately
// written as raw JSON rather than as Go structs: what has to be pinned is the
// wire format actually sent by Telegram, including the fields this package
// does not model.
func decodeMessage(t *testing.T, payload string) *Message {
	t.Helper()
	var msg Message
	if err := json.Unmarshal([]byte(payload), &msg); err != nil {
		t.Fatalf("json.Unmarshal() error: %v", err)
	}
	return &msg
}

// The fixtures below are fully synthetic: no real account, chat, file_id or
// personal content. They follow the Bot API 10.3 shapes, like those in
// testdata/bot-api-10.3.
func TestExtractMedia(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		want    []MediaAttachment
	}{
		{
			name: "photo alone keeps the largest size",
			// The three sizes come in the ascending order documented by the
			// Bot API; the middle one carries no file_size, which is legal.
			payload: `{
				"message_id": 601,
				"chat": {"id": 800001, "type": "private", "first_name": "Anaïs"},
				"date": 1788019300,
				"photo": [
					{"file_id": "photo-s", "file_unique_id": "u-photo-s", "width": 90, "height": 60, "file_size": 1204},
					{"file_id": "photo-m", "file_unique_id": "u-photo-m", "width": 320, "height": 213},
					{"file_id": "photo-l", "file_unique_id": "u-photo-l", "width": 1280, "height": 853, "file_size": 184320}
				],
				"business_connection_id": "bc_fixture_001"
			}`,
			want: []MediaAttachment{{
				Type:         MediaTypePhoto,
				FileID:       "photo-l",
				FileUniqueID: "u-photo-l",
				ByteSize:     184320,
				Width:        1280,
				Height:       853,
			}},
		},
		{
			name: "photo with caption",
			payload: `{
				"message_id": 602,
				"chat": {"id": 800001, "type": "private", "first_name": "Anaïs"},
				"date": 1788019301,
				"photo": [
					{"file_id": "photo-s", "file_unique_id": "u-photo-s", "width": 90, "height": 60, "file_size": 1204},
					{"file_id": "photo-l", "file_unique_id": "u-photo-l", "width": 1280, "height": 853, "file_size": 184320}
				],
				"caption": "Coffee ☕ — before the meeting",
				"caption_entities": [{"type": "bold", "offset": 0, "length": 6}]
			}`,
			want: []MediaAttachment{{
				Type:         MediaTypePhoto,
				FileID:       "photo-l",
				FileUniqueID: "u-photo-l",
				Caption:      "Coffee ☕ — before the meeting",
				ByteSize:     184320,
				Width:        1280,
				Height:       853,
			}},
		},
		{
			name: "video",
			payload: `{
				"message_id": 603,
				"chat": {"id": 800001, "type": "private", "first_name": "Anaïs"},
				"date": 1788019302,
				"video": {
					"file_id": "video-1",
					"file_unique_id": "u-video-1",
					"width": 1920,
					"height": 1080,
					"duration": 42,
					"file_name": "demo.mp4",
					"mime_type": "video/mp4",
					"file_size": 8388608,
					"supports_streaming": true,
					"thumbnail": {"file_id": "video-thumb", "file_unique_id": "u-video-thumb", "width": 320, "height": 180, "file_size": 9012}
				}
			}`,
			want: []MediaAttachment{{
				Type:         MediaTypeVideo,
				FileID:       "video-1",
				FileUniqueID: "u-video-1",
				FileName:     "demo.mp4",
				MimeType:     "video/mp4",
				ByteSize:     8388608,
				Width:        1920,
				Height:       1080,
				DurationSec:  42,
			}},
		},
		{
			name: "animation is not doubled by its backward-compatibility document",
			// Telegram sets `document` alongside `animation`, on the same
			// file, for backward compatibility. Only the animation must come
			// out: a doubled attachment means the GIF stored and restored
			// twice.
			payload: `{
				"message_id": 604,
				"chat": {"id": 800001, "type": "private", "first_name": "Anaïs"},
				"date": 1788019303,
				"animation": {
					"file_id": "anim-1",
					"file_unique_id": "u-anim-1",
					"width": 480,
					"height": 480,
					"duration": 3,
					"file_name": "reaction.mp4",
					"mime_type": "video/mp4",
					"file_size": 245760,
					"thumbnail": {"file_id": "anim-thumb", "file_unique_id": "u-anim-thumb", "width": 90, "height": 90}
				},
				"document": {
					"file_id": "anim-1",
					"file_unique_id": "u-anim-1",
					"file_name": "reaction.mp4",
					"mime_type": "video/mp4",
					"file_size": 245760,
					"thumbnail": {"file_id": "anim-thumb", "file_unique_id": "u-anim-thumb", "width": 90, "height": 90}
				}
			}`,
			want: []MediaAttachment{{
				Type:         MediaTypeAnimation,
				FileID:       "anim-1",
				FileUniqueID: "u-anim-1",
				FileName:     "reaction.mp4",
				MimeType:     "video/mp4",
				ByteSize:     245760,
				Width:        480,
				Height:       480,
				DurationSec:  3,
			}},
		},
		{
			name: "document with a non-ASCII file name",
			payload: `{
				"message_id": 605,
				"chat": {"id": 800001, "type": "private", "first_name": "Anaïs"},
				"date": 1788019304,
				"document": {
					"file_id": "doc-1",
					"file_unique_id": "u-doc-1",
					"file_name": "Devis — été 2026 (réf. n°7).pdf",
					"mime_type": "application/pdf",
					"file_size": 102400,
					"thumbnail": {"file_id": "doc-thumb", "file_unique_id": "u-doc-thumb", "width": 60, "height": 85}
				}
			}`,
			want: []MediaAttachment{{
				Type:         MediaTypeDocument,
				FileID:       "doc-1",
				FileUniqueID: "u-doc-1",
				FileName:     "Devis — été 2026 (réf. n°7).pdf",
				MimeType:     "application/pdf",
				ByteSize:     102400,
			}},
		},
		{
			name: "audio",
			payload: `{
				"message_id": 606,
				"chat": {"id": 800001, "type": "private", "first_name": "Anaïs"},
				"date": 1788019305,
				"audio": {
					"file_id": "audio-1",
					"file_unique_id": "u-audio-1",
					"duration": 213,
					"performer": "Zoë Test",
					"title": "Fixture in C",
					"file_name": "fixture-in-c.mp3",
					"mime_type": "audio/mpeg",
					"file_size": 3407872,
					"thumbnail": {"file_id": "audio-cover", "file_unique_id": "u-audio-cover", "width": 320, "height": 320}
				}
			}`,
			want: []MediaAttachment{{
				Type:         MediaTypeAudio,
				FileID:       "audio-1",
				FileUniqueID: "u-audio-1",
				FileName:     "fixture-in-c.mp3",
				MimeType:     "audio/mpeg",
				ByteSize:     3407872,
				DurationSec:  213,
			}},
		},
		{
			name: "voice",
			payload: `{
				"message_id": 607,
				"chat": {"id": 800001, "type": "private", "first_name": "Anaïs"},
				"date": 1788019306,
				"voice": {
					"file_id": "voice-1",
					"file_unique_id": "u-voice-1",
					"duration": 7,
					"mime_type": "audio/ogg",
					"file_size": 15360
				}
			}`,
			want: []MediaAttachment{{
				Type:         MediaTypeVoice,
				FileID:       "voice-1",
				FileUniqueID: "u-voice-1",
				MimeType:     "audio/ogg",
				ByteSize:     15360,
				DurationSec:  7,
			}},
		},
		{
			name: "video note reports its side on both dimensions",
			payload: `{
				"message_id": 608,
				"chat": {"id": 800001, "type": "private", "first_name": "Anaïs"},
				"date": 1788019307,
				"video_note": {
					"file_id": "note-1",
					"file_unique_id": "u-note-1",
					"length": 384,
					"duration": 11,
					"file_size": 524288,
					"thumbnail": {"file_id": "note-thumb", "file_unique_id": "u-note-thumb", "width": 384, "height": 384}
				}
			}`,
			want: []MediaAttachment{{
				Type:         MediaTypeVideoNote,
				FileID:       "note-1",
				FileUniqueID: "u-note-1",
				ByteSize:     524288,
				Width:        384,
				Height:       384,
				DurationSec:  11,
			}},
		},
		{
			name: "sticker",
			payload: `{
				"message_id": 609,
				"chat": {"id": 800001, "type": "private", "first_name": "Anaïs"},
				"date": 1788019308,
				"sticker": {
					"file_id": "sticker-1",
					"file_unique_id": "u-sticker-1",
					"type": "regular",
					"width": 512,
					"height": 512,
					"is_animated": false,
					"is_video": true,
					"emoji": "🎉",
					"set_name": "FixturePack",
					"file_size": 40960,
					"thumbnail": {"file_id": "sticker-thumb", "file_unique_id": "u-sticker-thumb", "width": 128, "height": 128}
				}
			}`,
			want: []MediaAttachment{{
				Type:         MediaTypeSticker,
				FileID:       "sticker-1",
				FileUniqueID: "u-sticker-1",
				ByteSize:     40960,
				Width:        512,
				Height:       512,
			}},
		},
		{
			name: "text message carries no attachment",
			payload: `{
				"message_id": 610,
				"from": {"id": 800001, "is_bot": false, "first_name": "Anaïs"},
				"chat": {"id": 800001, "type": "private", "first_name": "Anaïs"},
				"date": 1788019309,
				"text": "Hello, coffee ☕ — seen before?",
				"business_connection_id": "bc_fixture_001"
			}`,
			want: nil,
		},
		{
			name: "unknown media type is kept with its file_id",
			// Hypothetical future field: the point is precisely that this
			// package does not model it. It must not be dropped.
			payload: `{
				"message_id": 611,
				"chat": {"id": 800001, "type": "private", "first_name": "Anaïs"},
				"date": 1788019310,
				"caption": "New Telegram type",
				"hologram": {
					"file_id": "holo-1",
					"file_unique_id": "u-holo-1",
					"mime_type": "model/gltf-binary",
					"file_size": 65536,
					"duration": 5
				}
			}`,
			want: []MediaAttachment{{
				Type:         MediaTypeUnknown,
				FileID:       "holo-1",
				FileUniqueID: "u-holo-1",
				Caption:      "New Telegram type",
				MimeType:     "model/gltf-binary",
				ByteSize:     65536,
				DurationSec:  5,
			}},
		},
		{
			name: "media objects without file_id are dropped",
			// Malformed or truncated payload: file_id is required in every
			// Bot API media object. Such an attachment is neither
			// downloadable nor restorable, so it must not reach the
			// persistence layer — and it must not panic either.
			payload: `{
				"message_id": 612,
				"chat": {"id": 800001, "type": "private", "first_name": "Anaïs"},
				"date": 1788019311,
				"photo": [{}],
				"video": {},
				"voice": {"duration": 3}
			}`,
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractMedia(decodeMessage(t, tt.payload))
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("ExtractMedia() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

// TestExtractMediaAlbumPreservesOrder pins the album contract: three photos
// sent together arrive as three messages sharing the same media_group_id, and
// only the concatenation of the extractions in reception order gives the file
// order (the implicit file_index). The caption is on a single message of the
// group — Telegram's behavior — and must not be spread over the others.
func TestExtractMediaAlbumPreservesOrder(t *testing.T) {
	payloads := []string{
		`{
			"message_id": 701,
			"chat": {"id": 800001, "type": "private", "first_name": "Anaïs"},
			"date": 1788019400,
			"media_group_id": "13560000000000001",
			"caption": "Three views ☕",
			"photo": [
				{"file_id": "album-1-s", "file_unique_id": "u-album-1-s", "width": 90, "height": 60, "file_size": 1100},
				{"file_id": "album-1-l", "file_unique_id": "u-album-1-l", "width": 1280, "height": 853, "file_size": 180000}
			]
		}`,
		`{
			"message_id": 702,
			"chat": {"id": 800001, "type": "private", "first_name": "Anaïs"},
			"date": 1788019400,
			"media_group_id": "13560000000000001",
			"photo": [
				{"file_id": "album-2-s", "file_unique_id": "u-album-2-s", "width": 90, "height": 60, "file_size": 1150},
				{"file_id": "album-2-l", "file_unique_id": "u-album-2-l", "width": 1280, "height": 853, "file_size": 190000}
			]
		}`,
		`{
			"message_id": 703,
			"chat": {"id": 800001, "type": "private", "first_name": "Anaïs"},
			"date": 1788019401,
			"media_group_id": "13560000000000001",
			"photo": [
				{"file_id": "album-3-s", "file_unique_id": "u-album-3-s", "width": 90, "height": 60, "file_size": 1180},
				{"file_id": "album-3-l", "file_unique_id": "u-album-3-l", "width": 1280, "height": 853, "file_size": 200000}
			]
		}`,
	}

	var got []MediaAttachment
	for _, payload := range payloads {
		got = append(got, ExtractMedia(decodeMessage(t, payload))...)
	}

	want := []MediaAttachment{
		{Type: MediaTypePhoto, FileID: "album-1-l", FileUniqueID: "u-album-1-l", MediaGroupID: "13560000000000001", Caption: "Three views ☕", ByteSize: 180000, Width: 1280, Height: 853},
		{Type: MediaTypePhoto, FileID: "album-2-l", FileUniqueID: "u-album-2-l", MediaGroupID: "13560000000000001", ByteSize: 190000, Width: 1280, Height: 853},
		{Type: MediaTypePhoto, FileID: "album-3-l", FileUniqueID: "u-album-3-l", MediaGroupID: "13560000000000001", ByteSize: 200000, Width: 1280, Height: 853},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("album extraction = %#v, want %#v", got, want)
	}
}

// TestExtractMediaCaptionEditIsIdempotent covers the edited_business_message
// case: editing a caption re-sends the WHOLE message, media included. Two
// decodings of the same payload must give the same result (no accumulation,
// no state kept between calls), and only the caption must differ from the
// original message.
func TestExtractMediaCaptionEditIsIdempotent(t *testing.T) {
	const original = `{
		"message_id": 801,
		"chat": {"id": 800001, "type": "private", "first_name": "Anaïs"},
		"date": 1788019500,
		"photo": [
			{"file_id": "edit-s", "file_unique_id": "u-edit-s", "width": 90, "height": 60, "file_size": 1204},
			{"file_id": "edit-l", "file_unique_id": "u-edit-l", "width": 1280, "height": 853, "file_size": 184320}
		],
		"caption": "First version"
	}`
	const edited = `{
		"message_id": 801,
		"chat": {"id": 800001, "type": "private", "first_name": "Anaïs"},
		"date": 1788019500,
		"edit_date": 1788019560,
		"photo": [
			{"file_id": "edit-s", "file_unique_id": "u-edit-s", "width": 90, "height": 60, "file_size": 1204},
			{"file_id": "edit-l", "file_unique_id": "u-edit-l", "width": 1280, "height": 853, "file_size": 184320}
		],
		"caption": "Corrected version 🧪"
	}`

	msg := decodeMessage(t, edited)
	first := ExtractMedia(msg)
	second := ExtractMedia(msg)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("re-decoding is not idempotent: %#v then %#v", first, second)
	}
	// Decoding the payload again, from the bytes, must give the same thing.
	if again := ExtractMedia(decodeMessage(t, edited)); !reflect.DeepEqual(first, again) {
		t.Fatalf("re-decoding from the payload = %#v, want %#v", again, first)
	}

	want := []MediaAttachment{{
		Type:         MediaTypePhoto,
		FileID:       "edit-l",
		FileUniqueID: "u-edit-l",
		Caption:      "Corrected version 🧪",
		ByteSize:     184320,
		Width:        1280,
		Height:       853,
	}}
	if !reflect.DeepEqual(first, want) {
		t.Fatalf("edited extraction = %#v, want %#v", first, want)
	}

	// Same file, same dimensions: only the caption moved.
	before := ExtractMedia(decodeMessage(t, original))
	if len(before) != 1 || before[0].FileID != first[0].FileID || before[0].Caption != "First version" {
		t.Fatalf("original extraction = %#v", before)
	}
}

// TestSelectLargestPhotoIgnoresTheDeclaredOrder guards the documented choice:
// the retained size is the largest by pixel area, even if Telegram sends the
// array in another order than the documented ascending one.
func TestSelectLargestPhotoIgnoresTheDeclaredOrder(t *testing.T) {
	msg := decodeMessage(t, `{
		"message_id": 901,
		"chat": {"id": 800001, "type": "private", "first_name": "Anaïs"},
		"date": 1788019600,
		"photo": [
			{"file_id": "unsorted-l", "file_unique_id": "u-unsorted-l", "width": 1280, "height": 853, "file_size": 184320},
			{"file_id": "unsorted-s", "file_unique_id": "u-unsorted-s", "width": 90, "height": 60, "file_size": 1204}
		]
	}`)
	media := ExtractMedia(msg)
	if len(media) != 1 || media[0].FileID != "unsorted-l" {
		t.Fatalf("ExtractMedia() = %#v, want the 1280x853 size", media)
	}
}

// TestExtractMediaNilMessage covers the defensive path: the extraction is
// pure and must not panic on a nil message.
func TestExtractMediaNilMessage(t *testing.T) {
	if media := ExtractMedia(nil); media != nil {
		t.Fatalf("ExtractMedia(nil) = %#v, want nil", media)
	}
}

// TestExtractMediaKnownMediaIsNotReportedTwice pins the boundary of the
// unknown fallback: the fields modelled by the package, their nested
// thumbnails, and the non-media fields of the message (from, chat, entities)
// must not produce any "unknown" attachment.
func TestExtractMediaKnownMediaIsNotReportedTwice(t *testing.T) {
	msg := decodeMessage(t, `{
		"message_id": 902,
		"from": {"id": 800001, "is_bot": false, "first_name": "Anaïs"},
		"chat": {"id": 800001, "type": "private", "first_name": "Anaïs"},
		"date": 1788019601,
		"caption": "Report",
		"caption_entities": [{"type": "bold", "offset": 0, "length": 6}],
		"document": {
			"file_id": "doc-2",
			"file_unique_id": "u-doc-2",
			"file_name": "report.pdf",
			"mime_type": "application/pdf",
			"file_size": 2048,
			"thumbnail": {"file_id": "doc-2-thumb", "file_unique_id": "u-doc-2-thumb", "width": 60, "height": 85}
		}
	}`)
	media := ExtractMedia(msg)
	if len(media) != 1 || media[0].Type != MediaTypeDocument || media[0].FileID != "doc-2" {
		t.Fatalf("ExtractMedia() = %#v, want the single document", media)
	}
}
