package telegram

import "context"

// File is the getFile result. FilePath is valid for at most one hour and can
// be empty (files over 20 MB are not downloadable through the Bot API):
// media must therefore be fetched as soon as the message arrives, never
// lazily at restore time.
//
// FileUniqueID, unlike FileID, is stable across bots and over time: it is the
// identifier to use as a storage key and as a deduplication key.
type File struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	FileSize     int64  `json:"file_size,omitempty"`
	FilePath     string `json:"file_path,omitempty"`
}

// GetFile resolves a file_id into a download path. The returned FilePath is
// consumed by the media store, which builds the download URL in memory —
// this path never serves as a storage path.
func (c *Client) GetFile(ctx context.Context, fileID string) (*File, error) {
	var file File
	if err := c.call(ctx, "getFile", map[string]string{"file_id": fileID}, &file); err != nil {
		return nil, err
	}
	return &file, nil
}
