package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

// writeMedia creates a readable file with the given content and returns its
// path. Nothing here ever touches the network: the tests exercise the bodies
// the client would send, against an httptest server at most.
func writeMedia(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing the test media: %v", err)
	}
	return path
}

// parseForm reads back a body written by writeMediaForm, the way the Bot API
// would. Returns the plain fields and the file parts (name -> content).
func parseForm(t *testing.T, body io.Reader, contentType string) (map[string]string, map[string]string, map[string]string) {
	t.Helper()
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		t.Fatalf("unreadable Content-Type %q: %v", contentType, err)
	}
	reader := multipart.NewReader(body, params["boundary"])

	fields := map[string]string{}
	files := map[string]string{}
	names := map[string]string{}
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("reading a part: %v", err)
		}
		content, err := io.ReadAll(part)
		if err != nil {
			t.Fatalf("reading the content of part %s: %v", part.FormName(), err)
		}
		if part.FileName() != "" {
			files[part.FormName()] = string(content)
			names[part.FormName()] = part.FileName()
			continue
		}
		fields[part.FormName()] = string(content)
	}
	return fields, files, names
}

func buildAndWrite(t *testing.T, chatID int64, items []MediaAlertItem) (mediaForm, map[string]string, map[string]string, map[string]string) {
	t.Helper()
	form, err := buildMediaForm(chatID, items)
	if err != nil {
		t.Fatalf("buildMediaForm(): %v", err)
	}
	var body strings.Builder
	writer := multipart.NewWriter(&body)
	if err := writeMediaForm(writer, form); err != nil {
		t.Fatalf("writeMediaForm(): %v", err)
	}
	fields, files, names := parseForm(t, strings.NewReader(body.String()), writer.FormDataContentType())
	return form, fields, files, names
}

// TestSingleMediaFormUploadsUnderTheMethodField pins down the shape of a lone
// media upload: the file travels under the method's own field (photo for
// sendPhoto), not behind an attach:// reference, which only sendMediaGroup
// uses.
func TestSingleMediaFormUploadsUnderTheMethodField(t *testing.T) {
	path := writeMedia(t, "holiday.jpg", "photo-bytes")
	form, fields, files, names := buildAndWrite(t, 42, []MediaAlertItem{
		{Type: MediaTypePhoto, Path: path, FileName: "holiday.jpg", Caption: "at the beach"},
	})

	if form.method != "sendPhoto" {
		t.Fatalf("method = %s, want sendPhoto", form.method)
	}
	if fields["chat_id"] != "42" || fields["caption"] != "at the beach" {
		t.Fatalf("unexpected fields: %#v", fields)
	}
	if files["photo"] != "photo-bytes" {
		t.Fatalf("photo part = %q, want the file content", files["photo"])
	}
	if names["photo"] != "holiday.jpg" {
		t.Fatalf("file name = %q, want holiday.jpg", names["photo"])
	}
}

// A video note and a sticker take no caption in the Bot API: sending one would
// be a 400, and the context has already been delivered by the text chunks of
// the same alert.
func TestCaptionOmittedForMethodsThatRefuseIt(t *testing.T) {
	for _, mediaType := range []string{MediaTypeVideoNote, MediaTypeSticker} {
		t.Run(mediaType, func(t *testing.T) {
			path := writeMedia(t, "file.bin", "bytes")
			_, fields, _, _ := buildAndWrite(t, 42, []MediaAlertItem{
				{Type: mediaType, Path: path, Caption: "dropped"},
			})
			if _, present := fields["caption"]; present {
				t.Fatalf("caption sent to a method that refuses it: %#v", fields)
			}
		})
	}
}

// TestMediaGroupFormReferencesEveryFileWithAttach checks the other upload
// shape: a JSON array whose entries point at the multipart parts, in the order
// of the items -- which is the album order.
func TestMediaGroupFormReferencesEveryFileWithAttach(t *testing.T) {
	first := writeMedia(t, "one.jpg", "first-bytes")
	second := writeMedia(t, "two.mp4", "second-bytes")

	form, fields, files, _ := buildAndWrite(t, 42, []MediaAlertItem{
		{Type: MediaTypePhoto, Path: first, Caption: "album"},
		{Type: MediaTypeVideo, Path: second},
	})
	if form.method != "sendMediaGroup" {
		t.Fatalf("method = %s, want sendMediaGroup", form.method)
	}

	var group []inputMedia
	if err := json.Unmarshal([]byte(fields["media"]), &group); err != nil {
		t.Fatalf("unreadable media field %q: %v", fields["media"], err)
	}
	if len(group) != 2 {
		t.Fatalf("%d entries in media, want 2", len(group))
	}
	if group[0].Type != "photo" || group[0].Media != "attach://file0" || group[0].Caption != "album" {
		t.Fatalf("first entry = %#v", group[0])
	}
	if group[1].Type != "video" || group[1].Media != "attach://file1" {
		t.Fatalf("second entry = %#v", group[1])
	}
	if files["file0"] != "first-bytes" || files["file1"] != "second-bytes" {
		t.Fatalf("parts out of order or missing: %#v", files)
	}
}

// TestPlanMediaBatchesFollowsBotAPIGroupingRules: photos and videos mix,
// documents and audios only group with their own kind, everything else is sent
// alone -- and a group never exceeds 10 items. The ORDER is never rearranged
// to build a bigger group.
func TestPlanMediaBatchesFollowsBotAPIGroupingRules(t *testing.T) {
	item := func(mediaType string) MediaAlertItem { return MediaAlertItem{Type: mediaType} }
	repeat := func(mediaType string, n int) []MediaAlertItem {
		items := make([]MediaAlertItem, 0, n)
		for i := 0; i < n; i++ {
			items = append(items, item(mediaType))
		}
		return items
	}

	tests := []struct {
		name  string
		items []MediaAlertItem
		want  []int // size of each batch, in order
	}{
		{"photo and video mixed", []MediaAlertItem{item(MediaTypePhoto), item(MediaTypeVideo)}, []int{2}},
		{"documents together", repeat(MediaTypeDocument, 3), []int{3}},
		{"audios together", repeat(MediaTypeAudio, 2), []int{2}},
		{
			"document and photo never mix",
			[]MediaAlertItem{item(MediaTypeDocument), item(MediaTypePhoto)},
			[]int{1, 1},
		},
		{
			"ungroupable types stay alone",
			[]MediaAlertItem{item(MediaTypePhoto), item(MediaTypeVoice), item(MediaTypePhoto)},
			[]int{1, 1, 1},
		},
		{"album capped at ten", repeat(MediaTypePhoto, 12), []int{10, 2}},
		{"sticker alone", []MediaAlertItem{item(MediaTypeSticker)}, []int{1}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			batches := planMediaBatches(tt.items)
			if len(batches) != len(tt.want) {
				t.Fatalf("%d batches, want %d", len(batches), len(tt.want))
			}
			for i, batch := range batches {
				if len(batch) != tt.want[i] {
					t.Fatalf("batch %d holds %d items, want %d", i, len(batch), tt.want[i])
				}
			}
		})
	}
}

func TestTruncateCaptionRespectsTheUTF16Limit(t *testing.T) {
	short := "unchanged caption"
	if got := TruncateCaption(short); got != short {
		t.Fatalf("TruncateCaption(short) = %q, want it untouched", got)
	}

	// Emoji: 2 UTF-16 units each, so 700 of them exceed the 1024 limit while
	// being only 700 runes -- the exact case a rune-based cut would miss.
	long := strings.Repeat("🧪", 700)
	got := TruncateCaption(long)
	if units := len(utf16.Encode([]rune(got))); units > captionLimit {
		t.Fatalf("truncated caption = %d UTF-16 units, limit %d", units, captionLimit)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("a truncated caption must show it: %q", got[len(got)-8:])
	}
}

// A caption longer than the limit must not silently become a 400 at send time:
// the form carries the truncated value.
func TestFormCaptionIsTruncated(t *testing.T) {
	path := writeMedia(t, "photo.jpg", "bytes")
	_, fields, _, _ := buildAndWrite(t, 42, []MediaAlertItem{
		{Type: MediaTypePhoto, Path: path, Caption: strings.Repeat("a", captionLimit+50)},
	})
	if units := len(utf16.Encode([]rune(fields["caption"]))); units > captionLimit {
		t.Fatalf("caption of %d UTF-16 units sent, limit %d", units, captionLimit)
	}
}

// The three refusals that must happen BEFORE any upload, and that the outbox
// turns into the text fallback rather than into a doomed retry.
func TestBuildMediaFormRefusesUnusableFiles(t *testing.T) {
	dir := t.TempDir()

	missing := filepath.Join(dir, "gone.jpg")
	empty := writeMedia(t, "empty.jpg", "")
	oversize := filepath.Join(dir, "huge.mp4")
	if err := os.WriteFile(oversize, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Sparse file: costs no disk, and only its size matters here.
	if err := os.Truncate(oversize, maxUploadBytes+1); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		item MediaAlertItem
		want error
	}{
		{"no path", MediaAlertItem{Type: MediaTypePhoto}, ErrMediaUnavailable},
		{"purged file", MediaAlertItem{Type: MediaTypePhoto, Path: missing}, ErrMediaUnavailable},
		{"empty file", MediaAlertItem{Type: MediaTypePhoto, Path: empty}, ErrMediaUnavailable},
		{"directory", MediaAlertItem{Type: MediaTypePhoto, Path: dir}, ErrMediaUnavailable},
		{"above the upload limit", MediaAlertItem{Type: MediaTypeVideo, Path: oversize}, ErrMediaTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := buildMediaForm(42, []MediaAlertItem{tt.item}); !errors.Is(err, tt.want) {
				t.Fatalf("buildMediaForm() = %v, want %v", err, tt.want)
			}
		})
	}
}

// A symlink at the media path would upload whatever it points at into the
// owner's chat. Only a regular file written by the media store is legitimate.
func TestBuildMediaFormRefusesSymlink(t *testing.T) {
	target := writeMedia(t, "real.jpg", "bytes")
	link := filepath.Join(t.TempDir(), "link.jpg")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := buildMediaForm(42, []MediaAlertItem{{Type: MediaTypePhoto, Path: link}}); !errors.Is(err, ErrMediaUnavailable) {
		t.Fatalf("buildMediaForm(symlink) = %v, want ErrMediaUnavailable", err)
	}
}

// The file name comes from the SENDER. A CR/LF in it would inject a header
// into the multipart part; a separator would suggest a path.
func TestFileNameIsSanitizedAndDefaulted(t *testing.T) {
	tests := []struct {
		name string
		item MediaAlertItem
		want string
	}{
		{"kept as is", MediaAlertItem{Type: MediaTypeDocument, FileName: "report 2026.pdf"}, "report 2026.pdf"},
		{"header injection", MediaAlertItem{Type: MediaTypeDocument, FileName: "a\r\nContent-Type: x"}, "aContent-Type: x"},
		{"separators neutralised", MediaAlertItem{Type: MediaTypeDocument, FileName: `../etc/passwd`}, ".._etc_passwd"},
		{"empty name defaults per type", MediaAlertItem{Type: MediaTypePhoto}, "photo.jpg"},
		{"dot only", MediaAlertItem{Type: MediaTypeVoice, FileName: ".."}, "voice.ogg"},
		{"unknown type falls back to a document", MediaAlertItem{Type: "hologram"}, "document.bin"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mediaFileName(tt.item); got != tt.want {
				t.Fatalf("mediaFileName() = %q, want %q", got, tt.want)
			}
		})
	}
}

// recordingServer answers every call with the success envelope and records the
// path plus the parsed body.
type recordedCall struct {
	path   string
	fields map[string]string
	files  map[string]string
}

func newMediaServer(t *testing.T, status int, response string) (*Client, *[]recordedCall) {
	t.Helper()
	var calls []recordedCall
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fields, files, _ := parseForm(t, r.Body, r.Header.Get("Content-Type"))
		calls = append(calls, recordedCall{path: r.URL.Path, fields: fields, files: files})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write([]byte(response))
	}))
	t.Cleanup(server.Close)
	return NewClient("test-token", 5*time.Second, WithBaseURL(server.URL+"/bot")), &calls
}

// TestSendMediaOnceCallsTheMethodOfEachType is the delivery contract: one Bot
// API method per media type, and never a business_connection_id (constraint
// #6 -- that field would send the media as the owner, back into the monitored
// chat).
func TestSendMediaOnceCallsTheMethodOfEachType(t *testing.T) {
	tests := []struct {
		mediaType string
		method    string
		field     string
	}{
		{MediaTypePhoto, "sendPhoto", "photo"},
		{MediaTypeVideo, "sendVideo", "video"},
		{MediaTypeAnimation, "sendAnimation", "animation"},
		{MediaTypeDocument, "sendDocument", "document"},
		{MediaTypeAudio, "sendAudio", "audio"},
		{MediaTypeVoice, "sendVoice", "voice"},
		{MediaTypeVideoNote, "sendVideoNote", "video_note"},
		{MediaTypeSticker, "sendSticker", "sticker"},
		{MediaTypeUnknown, "sendDocument", "document"},
	}

	for _, tt := range tests {
		t.Run(tt.mediaType, func(t *testing.T) {
			client, calls := newMediaServer(t, http.StatusOK, `{"ok":true,"result":{}}`)
			path := writeMedia(t, "media.bin", "restored-bytes")

			err := client.SendMediaOnce(context.Background(), MediaAlert{
				ChatID: 700001,
				Items:  []MediaAlertItem{{Type: tt.mediaType, Path: path, Caption: "caption"}},
			})
			if err != nil {
				t.Fatalf("SendMediaOnce(): %v", err)
			}
			if len(*calls) != 1 {
				t.Fatalf("%d calls, want 1", len(*calls))
			}
			call := (*calls)[0]
			if want := "/bottest-token/" + tt.method; call.path != want {
				t.Fatalf("path = %s, want %s", call.path, want)
			}
			if call.fields["chat_id"] != "700001" {
				t.Fatalf("chat_id = %q, want the owner chat", call.fields["chat_id"])
			}
			if _, leaked := call.fields["business_connection_id"]; leaked {
				t.Fatal("business_connection_id sent with a media alert")
			}
			if call.files[tt.field] != "restored-bytes" {
				t.Fatalf("file absent from field %s: %#v", tt.field, call.files)
			}
		})
	}
}

// An album goes out as ONE sendMediaGroup, so Telegram renders it as an album
// rather than as a burst of separate messages.
func TestSendMediaOnceSendsAlbumAsOneGroup(t *testing.T) {
	client, calls := newMediaServer(t, http.StatusOK, `{"ok":true,"result":[]}`)
	first := writeMedia(t, "1.jpg", "one")
	second := writeMedia(t, "2.jpg", "two")

	err := client.SendMediaOnce(context.Background(), MediaAlert{
		ChatID: 700001,
		Items: []MediaAlertItem{
			{Type: MediaTypePhoto, Path: first},
			{Type: MediaTypePhoto, Path: second},
		},
	})
	if err != nil {
		t.Fatalf("SendMediaOnce(): %v", err)
	}
	if len(*calls) != 1 || !strings.HasSuffix((*calls)[0].path, "/sendMediaGroup") {
		t.Fatalf("calls = %#v, want a single sendMediaGroup", *calls)
	}
}

// A Telegram refusal must surface as an APIError with its code: that is what
// lets the outbox tell a permanent 4xx (text fallback) from a 429 (backoff).
func TestSendMediaOnceSurfacesAPIErrors(t *testing.T) {
	tests := []struct {
		name       string
		response   string
		code       int
		retryAfter int
	}{
		{"permanent refusal", `{"ok":false,"error_code":400,"description":"IMAGE_PROCESS_FAILED"}`, 400, 0},
		{"rate limit", `{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":12}}`, 429, 12},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, _ := newMediaServer(t, http.StatusOK, tt.response)
			path := writeMedia(t, "photo.jpg", "bytes")

			err := client.SendMediaOnce(context.Background(), MediaAlert{
				ChatID: 700001,
				Items:  []MediaAlertItem{{Type: MediaTypePhoto, Path: path}},
			})
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("SendMediaOnce() = %v, want an *APIError", err)
			}
			if apiErr.Code != tt.code || apiErr.RetryAfter != tt.retryAfter {
				t.Fatalf("error = %#v, want code %d retry_after %d", apiErr, tt.code, tt.retryAfter)
			}
		})
	}
}

func TestSendMediaOnceRefusesEmptyAlert(t *testing.T) {
	client, calls := newMediaServer(t, http.StatusOK, `{"ok":true,"result":{}}`)
	if err := client.SendMediaOnce(context.Background(), MediaAlert{ChatID: 700001}); !errors.Is(err, ErrMediaUnavailable) {
		t.Fatalf("SendMediaOnce(no item) = %v, want ErrMediaUnavailable", err)
	}
	if len(*calls) != 0 {
		t.Fatalf("%d calls for an empty alert, want none", len(*calls))
	}
}
