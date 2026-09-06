package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"strconv"
	"strings"
	"unicode/utf16"
)

// captionLimit is the Bot API ceiling for a media caption, in UTF-16 units.
// Four times smaller than the 4096 of a text message: a caption is truncated
// (cf. TruncateCaption) and never split, because a media carries exactly one.
const captionLimit = 1024

// maxUploadBytes is the Bot API ceiling for a multipart upload (50 MB). A
// larger file is refused here rather than sent: Telegram would answer a 400
// that the outbox turns into a permanent failure anyway, minus the wasted
// transfer. The caller then falls back to the text alert.
const maxUploadBytes = 50 << 20

// maxGroupItems is the number of items sendMediaGroup accepts in one call.
const maxGroupItems = 10

// Sentinel errors of the media delivery. They all mean "this media will never
// go out, send the text instead": the caller (outbox.Worker) matches them with
// errors.Is to decide between the text fallback and a retry.
var (
	// ErrMediaUnavailable marks a file that is not readable where the alert
	// says it is: purged from disk by retention, storage not mounted, or a row
	// that was never downloaded.
	ErrMediaUnavailable = errors.New("telegram: media file unavailable")
	// ErrMediaTooLarge marks a file above the Bot API upload limit.
	ErrMediaTooLarge = errors.New("telegram: media file above the Bot API upload limit")
)

// MediaAlertItem is one file to deliver to the owner. Path is a filesystem
// path already resolved by the caller: this package never joins it to
// anything, and never derives it from Telegram data.
type MediaAlertItem struct {
	// Type is one of the MediaType* constants. An unknown value is delivered
	// as a document rather than dropped -- the same choice ExtractMedia makes.
	Type string
	Path string
	// FileName is what the recipient sees. It comes from the sender, so it is
	// sanitized before reaching a multipart header.
	FileName string
	// Caption is the caption of the original message. Truncated to the Bot API
	// limit at build time.
	Caption string
}

// MediaAlert is the media half of a deletion alert: the files of one deleted
// message (or of one album), addressed to the owner.
//
// Constraint #6: like every alert, it carries NO business_connection_id. The
// form built below only ever contains chat_id, the caption and the files;
// adding the connection would send the media as the owner, back into the
// monitored chat.
type MediaAlert struct {
	ChatID int64
	Items  []MediaAlertItem
}

// mediaSpec maps a media type onto the Bot API method that sends it.
type mediaSpec struct {
	// method is the Bot API method for a single send.
	method string
	// field is the multipart field carrying the file for that method.
	field string
	// caption reports whether the method accepts a caption. sendVideoNote and
	// sendSticker do not: their caption is dropped, the context having already
	// been delivered by the text chunks of the same alert.
	caption bool
	// groupType is the InputMedia type usable inside sendMediaGroup, empty
	// when the type cannot be grouped.
	groupType string
	// ext is the extension used when the original file name is unknown.
	ext string
}

// mediaSpecs is the exhaustive table of what this package can send. A type
// absent from it is sent as a document (cf. specFor): losing the file would be
// worse than delivering it under a generic type.
var mediaSpecs = map[string]mediaSpec{
	MediaTypePhoto:     {method: "sendPhoto", field: "photo", caption: true, groupType: "photo", ext: ".jpg"},
	MediaTypeVideo:     {method: "sendVideo", field: "video", caption: true, groupType: "video", ext: ".mp4"},
	MediaTypeAnimation: {method: "sendAnimation", field: "animation", caption: true, ext: ".mp4"},
	MediaTypeDocument:  {method: "sendDocument", field: "document", caption: true, groupType: "document", ext: ".bin"},
	MediaTypeAudio:     {method: "sendAudio", field: "audio", caption: true, groupType: "audio", ext: ".mp3"},
	MediaTypeVoice:     {method: "sendVoice", field: "voice", caption: true, ext: ".ogg"},
	MediaTypeVideoNote: {method: "sendVideoNote", field: "video_note", ext: ".mp4"},
	MediaTypeSticker:   {method: "sendSticker", field: "sticker", ext: ".webp"},
}

func specFor(mediaType string) mediaSpec {
	if spec, ok := mediaSpecs[mediaType]; ok {
		return spec
	}
	return mediaSpecs[MediaTypeDocument]
}

// TruncateCaption cuts a caption to the Bot API limit, counted in UTF-16 units
// like every other Telegram length. The ellipsis is included in the budget, so
// the result is always accepted by the API.
func TruncateCaption(caption string) string {
	if captionUnits(caption) <= captionLimit {
		return caption
	}
	const ellipsis = "…"
	budget := captionLimit - captionUnits(ellipsis)
	units := 0
	kept := make([]rune, 0, budget)
	for _, r := range caption {
		size := utf16.RuneLen(r)
		if size < 1 {
			size = 1
		}
		if units+size > budget {
			break
		}
		kept = append(kept, r)
		units += size
	}
	return string(kept) + ellipsis
}

func captionUnits(s string) int {
	units := 0
	for _, r := range s {
		size := utf16.RuneLen(r)
		if size < 1 {
			size = 1
		}
		units += size
	}
	return units
}

// SendMediaOnce delivers the files of one alert. Single attempt, like
// SendMessageOnce: rescheduling belongs to the outbox, whose backoff is
// persisted in PostgreSQL.
//
// An album is sent through sendMediaGroup when its items allow it, so Telegram
// renders it as one album, in the order given. A group that Telegram would
// refuse to mix is split into several calls, still in order.
//
// At-least-once, same contract as the text alerts: a failure in the middle of
// a multi-batch alert replays the whole job, so the first batches may reach the
// owner twice. A duplicated media is preferable to a missing one.
func (c *Client) SendMediaOnce(ctx context.Context, alert MediaAlert) error {
	if alert.ChatID == 0 {
		return errors.New("telegram: media alert without a target chat")
	}
	if len(alert.Items) == 0 {
		return fmt.Errorf("%w: alert without any item", ErrMediaUnavailable)
	}

	for _, batch := range planMediaBatches(alert.Items) {
		form, err := buildMediaForm(alert.ChatID, batch)
		if err != nil {
			return err
		}
		if err := c.sendForm(ctx, form); err != nil {
			return err
		}
	}
	return nil
}

// planMediaBatches splits the items into the calls to make, PRESERVING their
// order -- which is the album order (file_index), the only one the owner can
// check against what was deleted.
//
// Grouping rule, straight from the Bot API: sendMediaGroup accepts photos and
// videos mixed together, or documents alone, or audios alone, up to 10 items.
// Anything else (animation, voice, video note, sticker) is sent on its own.
// Only CONSECUTIVE compatible items are grouped, so no reordering ever happens
// to make a bigger group.
func planMediaBatches(items []MediaAlertItem) [][]MediaAlertItem {
	var batches [][]MediaAlertItem
	var current []MediaAlertItem
	currentClass := ""

	flush := func() {
		if len(current) > 0 {
			batches = append(batches, current)
			current, currentClass = nil, ""
		}
	}

	for _, item := range items {
		class := groupClass(item.Type)
		if class == "" {
			flush()
			batches = append(batches, []MediaAlertItem{item})
			continue
		}
		if class != currentClass || len(current) == maxGroupItems {
			flush()
			currentClass = class
		}
		current = append(current, item)
	}
	flush()
	return batches
}

// groupClass returns the compatibility class of a type inside a media group,
// or "" when the type can only be sent alone.
func groupClass(mediaType string) string {
	switch specFor(mediaType).groupType {
	case "photo", "video":
		// The only mix the Bot API allows.
		return "visual"
	case "document":
		return "document"
	case "audio":
		return "audio"
	default:
		return ""
	}
}

// formField is a plain multipart field. Fields are kept in an ordered slice
// rather than a map so the emitted body is deterministic, which is what makes
// it assertable in a test.
type formField struct{ name, value string }

// formFile is a file part, uploaded from disk.
type formFile struct{ name, filename, path string }

// mediaForm is a fully described multipart request, still unwritten.
type mediaForm struct {
	method string
	fields []formField
	files  []formFile
}

// buildMediaForm turns one batch into the multipart request to send.
//
// The files are checked BEFORE anything is written: a missing file or one
// above the upload limit is reported as a sentinel error, which the outbox
// converts into the text fallback instead of a retry that could never succeed.
func buildMediaForm(chatID int64, batch []MediaAlertItem) (mediaForm, error) {
	for _, item := range batch {
		if err := checkMediaFile(item); err != nil {
			return mediaForm{}, err
		}
	}

	if len(batch) == 1 {
		return buildSingleForm(chatID, batch[0]), nil
	}
	return buildGroupForm(chatID, batch)
}

func buildSingleForm(chatID int64, item MediaAlertItem) mediaForm {
	spec := specFor(item.Type)
	form := mediaForm{
		method: spec.method,
		fields: []formField{{name: "chat_id", value: strconv.FormatInt(chatID, 10)}},
	}
	if caption := TruncateCaption(item.Caption); caption != "" && spec.caption {
		form.fields = append(form.fields, formField{name: "caption", value: caption})
	}
	// Single-method uploads take the file directly under the method's own
	// field name; attach:// is only needed by sendMediaGroup, whose files are
	// referenced from a JSON array.
	form.files = append(form.files, formFile{
		name:     spec.field,
		filename: mediaFileName(item),
		path:     item.Path,
	})
	return form
}

// inputMedia is one entry of the sendMediaGroup `media` array. The file itself
// travels as a separate multipart part, referenced by attach://<part name>.
type inputMedia struct {
	Type    string `json:"type"`
	Media   string `json:"media"`
	Caption string `json:"caption,omitempty"`
}

func buildGroupForm(chatID int64, batch []MediaAlertItem) (mediaForm, error) {
	form := mediaForm{
		method: "sendMediaGroup",
		fields: []formField{{name: "chat_id", value: strconv.FormatInt(chatID, 10)}},
	}

	group := make([]inputMedia, 0, len(batch))
	for i, item := range batch {
		part := "file" + strconv.Itoa(i)
		group = append(group, inputMedia{
			Type:    specFor(item.Type).groupType,
			Media:   "attach://" + part,
			Caption: TruncateCaption(item.Caption),
		})
		form.files = append(form.files, formFile{
			name:     part,
			filename: mediaFileName(item),
			path:     item.Path,
		})
	}

	encoded, err := json.Marshal(group)
	if err != nil {
		return mediaForm{}, fmt.Errorf("telegram: serializing media group: %w", err)
	}
	form.fields = append(form.fields, formField{name: "media", value: string(encoded)})
	return form, nil
}

// checkMediaFile refuses, before any upload, what Telegram could not accept.
func checkMediaFile(item MediaAlertItem) error {
	if item.Path == "" {
		return fmt.Errorf("%w: no path for a %s", ErrMediaUnavailable, item.Type)
	}
	// Lstat, not Stat: a symlink at the media path would otherwise be followed
	// and its target uploaded to the owner's chat. Only a plain regular file
	// written by the media store is legitimate here.
	info, err := os.Lstat(item.Path)
	switch {
	case err != nil:
		return fmt.Errorf("%w: %s", ErrMediaUnavailable, item.Type)
	case !info.Mode().IsRegular():
		return fmt.Errorf("%w: %s is not a regular file", ErrMediaUnavailable, item.Type)
	case info.Size() == 0:
		// Telegram rejects an empty upload with a 400; the fallback is the
		// honest answer, and it costs no round trip.
		return fmt.Errorf("%w: %s is empty", ErrMediaUnavailable, item.Type)
	case info.Size() > maxUploadBytes:
		return fmt.Errorf("%w: %d bytes, limit %d", ErrMediaTooLarge, info.Size(), maxUploadBytes)
	}
	return nil
}

// mediaFileName is the name the recipient sees. The sender chooses it, so it
// is sanitized: a CR or LF would inject a header into the multipart part, and
// a path separator would suggest a directory that does not exist on the
// recipient's side. It never touches OUR filesystem -- the local path comes
// from the media store, which generates it server-side.
func mediaFileName(item MediaAlertItem) string {
	name := sanitizeFileName(item.FileName)
	if name != "" {
		return name
	}
	spec := specFor(item.Type)
	mediaType := item.Type
	if _, known := mediaSpecs[mediaType]; !known {
		mediaType = MediaTypeDocument
	}
	return mediaType + spec.ext
}

func sanitizeFileName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r < 0x20, r == 0x7f:
			// Control characters, CR and LF included: multipart headers.
		case r == '/', r == '\\', r == '"':
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}
	cleaned := strings.TrimSpace(b.String())
	if cleaned == "." || cleaned == ".." {
		return ""
	}
	// Telegram truncates long names anyway; bounding it here keeps the header
	// small and predictable.
	if len(cleaned) > 128 {
		cleaned = cleaned[:128]
	}
	return cleaned
}

// writeMediaForm writes the whole body into mw and closes it. Split out of
// sendForm so the emitted body can be asserted without any network.
func writeMediaForm(mw *multipart.Writer, form mediaForm) error {
	for _, field := range form.fields {
		if err := mw.WriteField(field.name, field.value); err != nil {
			return fmt.Errorf("telegram: writing field %s: %w", field.name, err)
		}
	}
	for _, file := range form.files {
		// Opened one at a time and streamed: an alert must never hold several
		// 50 MB files in memory at once.
		source, err := os.Open(file.path)
		if err != nil {
			return fmt.Errorf("%w: opening the file to upload", ErrMediaUnavailable)
		}
		part, err := mw.CreateFormFile(file.name, file.filename)
		if err != nil {
			source.Close()
			return fmt.Errorf("telegram: writing part %s: %w", file.name, err)
		}
		_, err = io.Copy(part, source)
		source.Close()
		if err != nil {
			return fmt.Errorf("telegram: uploading %s: %w", file.name, err)
		}
	}
	return mw.Close()
}

// sendForm streams the multipart body to the Bot API. io.Pipe rather than a
// buffer: the body is bounded by maxUploadBytes per file, and buffering it
// whole would make the bot's memory footprint proportional to the size of the
// media it restores.
func (c *Client) sendForm(ctx context.Context, form mediaForm) error {
	reader, writer := io.Pipe()
	mw := multipart.NewWriter(writer)
	go func() {
		// CloseWithError(nil) is a plain Close: the reader then sees a clean
		// EOF, and any write error is surfaced on the request side instead of
		// being silently truncated into a malformed body.
		writer.CloseWithError(writeMediaForm(mw, form))
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+c.token+"/"+form.method, reader)
	if err != nil {
		reader.CloseWithError(err)
		return fmt.Errorf("building request %s: %w", form.method, err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return c.do(req, form.method, nil)
}
