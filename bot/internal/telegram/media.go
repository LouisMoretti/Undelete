package telegram

import (
	"encoding/json"
	"slices"
)

// Media types carried by MediaAttachment.Type. They are the vocabulary of the
// downstream layers (message_type, restitution of a deleted message), and are
// deliberately the Bot API field names so a payload can be read back without
// any translation table.
const (
	MediaTypePhoto     = "photo"
	MediaTypeVideo     = "video"
	MediaTypeAnimation = "animation"
	MediaTypeDocument  = "document"
	MediaTypeAudio     = "audio"
	MediaTypeVoice     = "voice"
	MediaTypeVideoNote = "video_note"
	MediaTypeSticker   = "sticker"
	// MediaTypeUnknown marks a media whose Bot API field this package does
	// not know. Cf. extractUnknownMedia: the attachment is kept with its
	// file_id rather than dropped.
	MediaTypeUnknown = "unknown"
)

// MediaAttachment is the consolidated form of a media, independent of the Bot
// API type it comes from. It is what the persistence and restitution layers
// consume; the fields absent from the source type stay at their zero value
// (no duration on a photo, no dimensions on a voice message...).
//
// ByteSize, Width, Height and DurationSec are OPTIONAL on the Telegram side:
// a zero value means "not sent", never "empty file" or "zero-length video".
type MediaAttachment struct {
	Type         string
	FileID       string
	FileUniqueID string
	MediaGroupID string
	Caption      string
	FileName     string
	MimeType     string
	ByteSize     int64
	Width        int
	Height       int
	DurationSec  int
}

// ExtractMedia consolidates the media of a message into an ordered list. Pure
// function: no I/O, no network call, no mutation of msg.
//
// Ordering — a Telegram message carries at most one media, an album being N
// messages sharing the same media_group_id. The order of the returned list is
// therefore the declaration order of the types below, and the album order is
// simply the order in which the messages are received: concatenating the
// results of ExtractMedia over the messages of a media_group yields the
// album's file order, which is enough for an implicit file_index. A slice is
// returned anyway, because nothing in the API forbids a future message from
// carrying several files.
//
// A message without media (plain text, or a service message) returns an empty
// list, never an error: the absence of media is not an anomaly.
func ExtractMedia(msg *Message) []MediaAttachment {
	if msg == nil {
		return nil
	}

	var media []MediaAttachment
	add := func(attachment MediaAttachment) {
		attachment.MediaGroupID = msg.MediaGroupID
		attachment.Caption = msg.Caption
		media = append(media, attachment)
	}

	if size, ok := selectLargestPhoto(msg.Photo); ok {
		add(MediaAttachment{
			Type:         MediaTypePhoto,
			FileID:       size.FileID,
			FileUniqueID: size.FileUniqueID,
			ByteSize:     size.FileSize,
			Width:        size.Width,
			Height:       size.Height,
		})
	}
	if video := msg.Video; video != nil {
		add(MediaAttachment{
			Type:         MediaTypeVideo,
			FileID:       video.FileID,
			FileUniqueID: video.FileUniqueID,
			FileName:     video.FileName,
			MimeType:     video.MimeType,
			ByteSize:     video.FileSize,
			Width:        video.Width,
			Height:       video.Height,
			DurationSec:  video.Duration,
		})
	}
	if animation := msg.Animation; animation != nil {
		add(MediaAttachment{
			Type:         MediaTypeAnimation,
			FileID:       animation.FileID,
			FileUniqueID: animation.FileUniqueID,
			FileName:     animation.FileName,
			MimeType:     animation.MimeType,
			ByteSize:     animation.FileSize,
			Width:        animation.Width,
			Height:       animation.Height,
			DurationSec:  animation.Duration,
		})
	}
	// The Bot API states, on Message.animation: "For backward compatibility,
	// when this field is set, the document field will also be set." The two
	// fields then describe the SAME file; consolidating both would persist the
	// media twice, download it twice, and restore it twice. The animation is
	// the richer of the two (dimensions, duration), so it wins.
	if document := msg.Document; document != nil && msg.Animation == nil {
		add(MediaAttachment{
			Type:         MediaTypeDocument,
			FileID:       document.FileID,
			FileUniqueID: document.FileUniqueID,
			FileName:     document.FileName,
			MimeType:     document.MimeType,
			ByteSize:     document.FileSize,
		})
	}
	if audio := msg.Audio; audio != nil {
		add(MediaAttachment{
			Type:         MediaTypeAudio,
			FileID:       audio.FileID,
			FileUniqueID: audio.FileUniqueID,
			FileName:     audio.FileName,
			MimeType:     audio.MimeType,
			ByteSize:     audio.FileSize,
			DurationSec:  audio.Duration,
		})
	}
	if voice := msg.Voice; voice != nil {
		add(MediaAttachment{
			Type:         MediaTypeVoice,
			FileID:       voice.FileID,
			FileUniqueID: voice.FileUniqueID,
			MimeType:     voice.MimeType,
			ByteSize:     voice.FileSize,
			DurationSec:  voice.Duration,
		})
	}
	if note := msg.VideoNote; note != nil {
		// A video note is square: Length is its single side, reported on both
		// dimensions so the consumers do not have to special-case the type.
		add(MediaAttachment{
			Type:         MediaTypeVideoNote,
			FileID:       note.FileID,
			FileUniqueID: note.FileUniqueID,
			ByteSize:     note.FileSize,
			Width:        note.Length,
			Height:       note.Length,
			DurationSec:  note.Duration,
		})
	}
	if sticker := msg.Sticker; sticker != nil {
		add(MediaAttachment{
			Type:         MediaTypeSticker,
			FileID:       sticker.FileID,
			FileUniqueID: sticker.FileUniqueID,
			ByteSize:     sticker.FileSize,
			Width:        sticker.Width,
			Height:       sticker.Height,
		})
	}

	for _, unknown := range extractUnknownMedia(msg.raw) {
		add(unknown)
	}
	return media
}

// selectLargestPhoto picks the size to keep from the sizes offered by
// Telegram for one photo.
//
// DOCUMENTED CHOICE: the LARGEST available size. The product restores a
// deleted message; degrading its quality would lose information the user
// cannot get back, whereas the cost of the large size is only storage. The
// Bot API documents the array as sorted from the smallest to the largest, but
// that order is not a contract: the selection is made on the pixel area, with
// file_size then position as tie-breakers (file_size being optional, it
// cannot be the primary criterion).
func selectLargestPhoto(sizes []PhotoSize) (PhotoSize, bool) {
	var largest PhotoSize
	found := false
	for _, size := range sizes {
		if !found {
			largest, found = size, true
			continue
		}
		area, largestArea := size.Width*size.Height, largest.Width*largest.Height
		if area > largestArea || (area == largestArea && size.FileSize > largest.FileSize) {
			largest = size
		}
	}
	return largest, found
}

// knownMediaFields are the message fields already consolidated above. Any
// OTHER field carrying a file_id is an unknown media.
var knownMediaFields = map[string]bool{
	MediaTypePhoto:     true,
	MediaTypeVideo:     true,
	MediaTypeAnimation: true,
	MediaTypeDocument:  true,
	MediaTypeAudio:     true,
	MediaTypeVoice:     true,
	MediaTypeVideoNote: true,
	MediaTypeSticker:   true,
}

// rawFile is the common denominator of every Bot API media object: a file_id,
// a file_unique_id, and optional descriptive fields. It is enough to keep an
// unrecognized media addressable (file_id is what getFile needs).
type rawFile struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	FileName     string `json:"file_name"`
	MimeType     string `json:"mime_type"`
	FileSize     int64  `json:"file_size"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	Duration     int    `json:"duration"`
}

// extractUnknownMedia scans the raw message for a media type this package
// does not model yet — the Bot API adds some at every release, and a media
// silently dropped is a message the product cannot restore.
//
// Recognition criterion: a top-level field whose value is an object holding a
// non-empty file_id, or an array of such objects. Only the top level is
// walked, so the thumbnails nested inside a known media are not reported a
// second time. Fields are visited in alphabetical order, which makes the
// output deterministic across two decodings of the same payload.
func extractUnknownMedia(raw json.RawMessage) []MediaAttachment {
	if len(raw) == 0 {
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil
	}

	names := make([]string, 0, len(fields))
	for name := range fields {
		if !knownMediaFields[name] {
			names = append(names, name)
		}
	}
	slices.Sort(names)

	var media []MediaAttachment
	for _, name := range names {
		for _, file := range decodeRawFiles(fields[name]) {
			media = append(media, MediaAttachment{
				Type:         MediaTypeUnknown,
				FileID:       file.FileID,
				FileUniqueID: file.FileUniqueID,
				FileName:     file.FileName,
				MimeType:     file.MimeType,
				ByteSize:     file.FileSize,
				Width:        file.Width,
				Height:       file.Height,
				DurationSec:  file.Duration,
			})
		}
	}
	return media
}

// decodeRawFiles returns the file-bearing objects of a raw value: the value
// itself if it is one, its elements if it is an array of them, nothing
// otherwise. Every element of an array is kept — the semantics of an unknown
// array are unknown too, so keeping them all is the only option that drops
// nothing.
func decodeRawFiles(value json.RawMessage) []rawFile {
	var single rawFile
	if err := json.Unmarshal(value, &single); err == nil {
		if single.FileID == "" {
			return nil
		}
		return []rawFile{single}
	}
	var list []rawFile
	if err := json.Unmarshal(value, &list); err != nil {
		return nil
	}
	files := make([]rawFile, 0, len(list))
	for _, file := range list {
		if file.FileID != "" {
			files = append(files, file)
		}
	}
	return files
}
