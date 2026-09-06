package outbox

import (
	"encoding/json"
	"fmt"
)

// Payload kinds, mirroring the payload_kind CHECK of migration 0005.
const (
	PayloadKindText  = "text"
	PayloadKindMedia = "media"
)

// MediaItem is one attachment frozen into a media outbox entry.
//
// RelativePath is relative to the media root: an absolute path would break the
// day the storage root moves, and the row would then point at nothing. The
// worker joins it to its own root and validates the result before opening
// anything.
type MediaItem struct {
	// MediaFileID is the media_files row this item was built from. Kept for
	// traceability (logs, later reconciliation), never used as a path.
	MediaFileID int64 `json:"media_file_id"`
	// MessageID and FileIndex give the ORDER of an album: the items of one
	// media_group_id are sorted by (message_id, file_index), which is the
	// order the sender saw.
	MessageID    int64  `json:"message_id"`
	FileIndex    int    `json:"file_index"`
	MediaType    string `json:"media_type"`
	RelativePath string `json:"relative_path"`
	FileName     string `json:"file_name,omitempty"`
	Caption      string `json:"caption,omitempty"`
}

// MediaPayload is the JSON stored in notification_outbox.media_payload.
type MediaPayload struct {
	// MediaGroupID is set when the entry restores a whole album (several
	// deleted messages sharing one media_group_id), empty for a lone media.
	MediaGroupID string      `json:"media_group_id,omitempty"`
	Items        []MediaItem `json:"items"`
}

// MediaTypes lists the types of the payload, in order. Used to build the text
// that accompanies the media -- and replaces it when the files cannot go out.
func (p MediaPayload) MediaTypes() []string {
	types := make([]string, 0, len(p.Items))
	for _, item := range p.Items {
		types = append(types, item.MediaType)
	}
	return types
}

func encodeMediaPayload(payload MediaPayload) ([]byte, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("outbox media payload: %w", err)
	}
	return raw, nil
}

// decodeMediaPayload reads back a payload written by encodeMediaPayload. A row
// whose JSON cannot be decoded is not a reason to lose the alert: the caller
// treats it as "no media" and sends the text, which is exactly the documented
// fallback.
func decodeMediaPayload(raw []byte) (*MediaPayload, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var payload MediaPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("outbox media payload: %w", err)
	}
	if len(payload.Items) == 0 {
		return nil, nil
	}
	return &payload, nil
}
