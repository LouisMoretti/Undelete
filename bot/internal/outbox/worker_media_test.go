package outbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/LouisMoretti/Undelete/bot/internal/media"
	"github.com/LouisMoretti/Undelete/bot/internal/telegram"
)

// fakeMediaSender implements BOTH halves: a sender that can upload. The
// text-only fakeSender stays available to exercise the opposite case, a worker
// whose sender does not know how to send media at all.
type fakeMediaSender struct {
	fakeSender
	mediaErr    error
	mediaAlerts []telegram.MediaAlert
}

func (s *fakeMediaSender) SendMediaOnce(_ context.Context, alert telegram.MediaAlert) error {
	s.mediaAlerts = append(s.mediaAlerts, alert)
	return s.mediaErr
}

func newMediaWorker(store Store, sender Sender, dir string, logBuffer *bytes.Buffer) *Worker {
	logger := slog.New(slog.NewJSONHandler(logBuffer, nil))
	if dir == "" {
		return NewWorker(store, sender, logger)
	}
	return NewWorker(store, sender, logger, WithMediaDir(dir))
}

// mediaJob is the media counterpart of testJob: same alert, but the files
// travel with it and Text is only the documented fallback.
func mediaJob(relativePaths ...string) *Job {
	job := testJob()
	job.Text = "Attached media (1 file: photo)"
	job.PayloadKind = PayloadKindMedia
	job.Media = &MediaPayload{MediaGroupID: "album-1"}
	for i, path := range relativePaths {
		job.Media.Items = append(job.Media.Items, MediaItem{
			MediaFileID:  int64(i + 1),
			MessageID:    int64(100 + i),
			FileIndex:    i,
			MediaType:    telegram.MediaTypePhoto,
			RelativePath: path,
			FileName:     "holiday.jpg",
			Caption:      "at the beach",
		})
	}
	return job
}

// The nominal path: the files go out as media, the fallback text is NOT sent
// on top of them, and the job is acknowledged.
func TestWorkerDeliversMediaWithoutSendingTheFallbackText(t *testing.T) {
	store := &fakeStore{job: mediaJob("11/ab/photo.jpg")}
	sender := &fakeMediaSender{}
	var logs bytes.Buffer
	worker := newMediaWorker(store, sender, "/srv/media", &logs)

	processed, err := worker.ProcessOne(context.Background(), 11)
	if err != nil || !processed {
		t.Fatalf("ProcessOne() = %v, %v", processed, err)
	}
	if !store.sent {
		t.Fatal("media job not marked sent")
	}
	if len(sender.requests) != 0 {
		t.Fatalf("fallback text sent alongside the media: %#v", sender.requests)
	}
	if len(sender.mediaAlerts) != 1 {
		t.Fatalf("%d media alerts, want 1", len(sender.mediaAlerts))
	}
	alert := sender.mediaAlerts[0]
	if alert.ChatID != 42 {
		t.Fatalf("ChatID = %d, want the owner chat", alert.ChatID)
	}
	// The relative path of the payload is resolved against the worker's root,
	// never used as-is and never taken from Telegram data.
	if want := "/srv/media/11/ab/photo.jpg"; alert.Items[0].Path != want {
		t.Fatalf("Path = %q, want %q", alert.Items[0].Path, want)
	}
	if alert.Items[0].Caption != "at the beach" || alert.Items[0].FileName != "holiday.jpg" {
		t.Fatalf("caption/file name lost: %#v", alert.Items[0])
	}
}

// The payload order IS the album order (message_id, file_index): it must reach
// the sender untouched, otherwise the owner cannot match the restored album
// against what disappeared.
func TestWorkerPreservesTheAlbumOrder(t *testing.T) {
	store := &fakeStore{job: mediaJob("11/a/1.jpg", "11/a/2.jpg", "11/a/3.jpg")}
	sender := &fakeMediaSender{}
	var logs bytes.Buffer
	worker := newMediaWorker(store, sender, "/srv/media", &logs)

	if _, err := worker.ProcessOne(context.Background(), 11); err != nil {
		t.Fatalf("ProcessOne(): %v", err)
	}
	got := make([]string, 0, 3)
	for _, item := range sender.mediaAlerts[0].Items {
		got = append(got, item.Path)
	}
	want := []string{"/srv/media/11/a/1.jpg", "/srv/media/11/a/2.jpg", "/srv/media/11/a/3.jpg"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

// Every way a media can be definitively undeliverable must end on the SAME
// outcome: the text alert plus the unavailability note, acknowledged, no retry.
// This is the "an alert is never lost" invariant.
func TestWorkerFallsBackToTextOnHopelessMediaFailures(t *testing.T) {
	tests := []struct {
		name     string
		mediaDir string
		job      *Job
		sender   Sender
		wantLog  string
	}{
		{
			name:     "file missing from disk (purged between deletion and delivery)",
			mediaDir: "/srv/media",
			job:      mediaJob("11/a/1.jpg"),
			sender:   &fakeMediaSender{mediaErr: telegram.ErrMediaUnavailable},
			wantLog:  "media_missing",
		},
		{
			name:     "file above the Bot API upload limit",
			mediaDir: "/srv/media",
			job:      mediaJob("11/a/1.jpg"),
			sender:   &fakeMediaSender{mediaErr: telegram.ErrMediaTooLarge},
			wantLog:  "media_too_large",
		},
		{
			name:     "definitive Telegram refusal",
			mediaDir: "/srv/media",
			job:      mediaJob("11/a/1.jpg"),
			sender:   &fakeMediaSender{mediaErr: &telegram.APIError{Code: http.StatusBadRequest}},
			wantLog:  "telegram_400",
		},
		{
			name:     "unsafe relative path in the payload",
			mediaDir: "/srv/media",
			job:      mediaJob("../../etc/passwd"),
			sender:   &fakeMediaSender{},
			wantLog:  "media_unsafe_path",
		},
		{
			name:     "no media root configured",
			mediaDir: "",
			job:      mediaJob("11/a/1.jpg"),
			sender:   &fakeMediaSender{},
			wantLog:  "media_unsupported",
		},
		{
			name:     "sender without media support",
			mediaDir: "/srv/media",
			job:      mediaJob("11/a/1.jpg"),
			sender:   &fakeSender{},
			wantLog:  "media_unsupported",
		},
		{
			name:     "undecodable payload, read back as no media",
			mediaDir: "/srv/media",
			job: func() *Job {
				job := mediaJob("11/a/1.jpg")
				job.Media = nil
				return job
			}(),
			sender:  &fakeMediaSender{},
			wantLog: "media_unsupported",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &fakeStore{job: tt.job}
			var logs bytes.Buffer
			worker := newMediaWorker(store, tt.sender, tt.mediaDir, &logs)

			processed, err := worker.ProcessOne(context.Background(), 11)
			if err != nil || !processed {
				t.Fatalf("ProcessOne() = %v, %v", processed, err)
			}
			if !store.sent || store.failed {
				t.Fatalf("job not acknowledged: sent=%v failed=%v", store.sent, store.failed)
			}

			var requests []telegram.SendMessageRequest
			switch s := tt.sender.(type) {
			case *fakeMediaSender:
				requests = s.requests
			case *fakeSender:
				requests = s.requests
			}
			if len(requests) != 1 {
				t.Fatalf("%d text alerts, want exactly 1 (the fallback)", len(requests))
			}
			if !strings.HasSuffix(requests[0].Text, telegram.MediaUnavailableNote) {
				t.Fatalf("fallback without the unavailability note: %q", requests[0].Text)
			}
			if !strings.Contains(requests[0].Text, "Attached media") {
				t.Fatalf("fallback lost its text: %q", requests[0].Text)
			}
			// Constraint #6: the fallback is a plain bot message, like every
			// other alert. Asserted on the SERIALIZED request, the way the
			// existing text tests do -- that is what actually reaches Telegram.
			raw, err := json.Marshal(requests[0])
			if err != nil {
				t.Fatalf("serializing the fallback: %v", err)
			}
			if strings.Contains(string(raw), "business_connection_id") {
				t.Fatalf("business_connection_id sent with a media fallback: %s", raw)
			}
			if !strings.Contains(logs.String(), tt.wantLog) {
				t.Fatalf("error class %q absent from the logs: %s", tt.wantLog, logs.String())
			}
		})
	}
}

// A 429 is exactly what the outbox backoff exists for: it must NOT degrade the
// alert to text, and it must reschedule with the retry_after Telegram gave.
func TestWorkerRetriesMediaOnRateLimitInsteadOfFallingBack(t *testing.T) {
	store := &fakeStore{job: mediaJob("11/a/1.jpg")}
	sender := &fakeMediaSender{mediaErr: &telegram.APIError{Code: http.StatusTooManyRequests, RetryAfter: 12}}
	var logs bytes.Buffer
	worker := newMediaWorker(store, sender, "/srv/media", &logs)

	if _, err := worker.ProcessOne(context.Background(), 11); err != nil {
		t.Fatalf("ProcessOne(): %v", err)
	}
	if store.sent || store.failed {
		t.Fatalf("a rate-limited media must stay replayable: sent=%v failed=%v", store.sent, store.failed)
	}
	if store.retryIn != 12*1e9 {
		t.Fatalf("retry_in = %v, want the 12s retry_after", store.retryIn)
	}
	if len(sender.requests) != 0 {
		t.Fatalf("a 429 degraded the alert to text: %#v", sender.requests)
	}
}

// A 5xx or a transport error is transient too: the media can still go out
// later, so the job keeps its backoff rather than losing its files.
func TestWorkerRetriesMediaOnTransientFailures(t *testing.T) {
	for _, err := range []error{&telegram.APIError{Code: http.StatusInternalServerError}, errors.New("connection reset")} {
		store := &fakeStore{job: mediaJob("11/a/1.jpg")}
		sender := &fakeMediaSender{mediaErr: err}
		var logs bytes.Buffer
		worker := newMediaWorker(store, sender, "/srv/media", &logs)

		if _, procErr := worker.ProcessOne(context.Background(), 11); procErr != nil {
			t.Fatalf("ProcessOne(): %v", procErr)
		}
		if store.sent || len(sender.requests) != 0 {
			t.Fatalf("%v degraded the alert to text instead of retrying", err)
		}
	}
}

// A job read from a row written before migration 0005 carries an empty
// payload_kind: it is a text alert, and nothing about the media path may
// trigger on it.
func TestWorkerTreatsAnEmptyPayloadKindAsText(t *testing.T) {
	job := testJob()
	job.PayloadKind = ""
	store := &fakeStore{job: job}
	sender := &fakeMediaSender{}
	var logs bytes.Buffer
	worker := newMediaWorker(store, sender, "/srv/media", &logs)

	if _, err := worker.ProcessOne(context.Background(), 11); err != nil {
		t.Fatalf("ProcessOne(): %v", err)
	}
	if len(sender.mediaAlerts) != 0 {
		t.Fatal("a legacy text row went down the media path")
	}
	if len(sender.requests) != 1 || strings.Contains(sender.requests[0].Text, telegram.MediaUnavailableNote) {
		t.Fatalf("legacy text alert altered: %#v", sender.requests)
	}
}

func TestMediaPathRejectsEveryUnsafeRelativePath(t *testing.T) {
	worker := NewWorker(nil, nil, slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)), WithMediaDir("/srv/media"))
	for _, relative := range []string{"", "/etc/passwd", "../escape", "a/../../b", "a//b", `a\b`} {
		if _, err := worker.mediaPath(relative); !errors.Is(err, media.ErrUnsafeRelativePath) {
			t.Fatalf("mediaPath(%q) = %v, want ErrUnsafeRelativePath", relative, err)
		}
	}
}

// The payload is what the whole design rests on: written at deletion time, read
// back much later by another process. A field lost in the round trip would send
// the wrong file, in the wrong order, or none at all.
func TestMediaPayloadSurvivesTheRoundTrip(t *testing.T) {
	payload := MediaPayload{
		MediaGroupID: "album-1",
		Items: []MediaItem{
			{MediaFileID: 5, MessageID: 100, FileIndex: 0, MediaType: telegram.MediaTypePhoto, RelativePath: "11/a/1.jpg", FileName: "1.jpg", Caption: "first"},
			{MediaFileID: 6, MessageID: 101, FileIndex: 1, MediaType: telegram.MediaTypeVideo, RelativePath: "11/a/2.mp4"},
		},
	}
	raw, err := encodeMediaPayload(payload)
	if err != nil {
		t.Fatalf("encodeMediaPayload(): %v", err)
	}
	decoded, err := decodeMediaPayload(raw)
	if err != nil {
		t.Fatalf("decodeMediaPayload(): %v", err)
	}
	if decoded == nil {
		t.Fatal("decodeMediaPayload() = nil")
	}
	if !json.Valid(raw) {
		t.Fatal("payload is not valid JSON, JSONB would refuse it")
	}
	if decoded.MediaGroupID != payload.MediaGroupID || len(decoded.Items) != 2 {
		t.Fatalf("payload = %#v", decoded)
	}
	for i, item := range decoded.Items {
		if item != payload.Items[i] {
			t.Fatalf("item %d = %#v, want %#v", i, item, payload.Items[i])
		}
	}
	if got := strings.Join(decoded.MediaTypes(), ","); got != "photo,video" {
		t.Fatalf("MediaTypes() = %q", got)
	}
}

// An empty or unreadable payload must never strand the alert: the caller then
// simply has no media and delivers the fallback text.
func TestDecodeMediaPayloadDegradesInsteadOfStranding(t *testing.T) {
	for _, raw := range [][]byte{nil, {}, []byte(`{"items":[]}`)} {
		payload, err := decodeMediaPayload(raw)
		if err != nil || payload != nil {
			t.Fatalf("decodeMediaPayload(%q) = %v, %v, want nil, nil", raw, payload, err)
		}
	}
	if _, err := decodeMediaPayload([]byte(`{"items":`)); err == nil {
		t.Fatal("decodeMediaPayload(broken JSON) = nil error")
	}
}
