package messages

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/LouisMoretti/Undelete/bot/internal/media"
	"github.com/LouisMoretti/Undelete/bot/internal/outbox"
	"github.com/LouisMoretti/Undelete/bot/internal/telegram"
)

// mediaAlertScope carries the tenant identity of the deletion being notified.
// A struct rather than five positional arguments, so an owner id and a chat id
// -- both int64 -- can never be swapped at a call site.
type mediaAlertScope struct {
	ownerUserID          int64
	ownerTelegramUserID  int64
	businessConnectionID string
	chatID               int64
}

// enqueueMediaAlerts writes the media entries of a deletion into the outbox,
// inside the transaction that set deleted_at.
//
// One entry per album (media_group_id) or per lone message, never one per
// file: an album must reach the owner as an album, so its files travel
// together and in order (sendMediaGroup on the worker side).
//
// A message whose media is not on disk -- never downloaded, or already purged
// by retention -- simply produces no entry: its text alert already states the
// message type, and inventing an entry would only guarantee a fallback.
func enqueueMediaAlerts(ctx context.Context, tx pgx.Tx, scope mediaAlertScope, found []DeletedRecord, chunkCount map[int64]int) error {
	if len(found) == 0 {
		return nil
	}

	ids := make([]int64, 0, len(found))
	captions := make(map[int64]string, len(found))
	for _, d := range found {
		ids = append(ids, d.MessageID)
		// text_content holds the caption for a media message (cf. app.Handler):
		// it is what was written WITH the file, and the alert must give it back
		// attached to the file, not only in the text chunk.
		captions[d.MessageID] = d.TextContent
	}

	files, err := media.SelectStoredTx(ctx, tx, scope.businessConnectionID, scope.chatID, ids)
	if err != nil {
		return err
	}

	for _, group := range groupMedia(files) {
		payload := outbox.MediaPayload{MediaGroupID: group.mediaGroupID}
		for _, file := range group.files {
			payload.Items = append(payload.Items, outbox.MediaItem{
				MediaFileID:  file.ID,
				MessageID:    file.MessageID,
				FileIndex:    file.FileIndex,
				MediaType:    file.MediaType,
				RelativePath: file.RelativePath,
				FileName:     file.FileName,
				Caption:      telegram.TruncateCaption(captions[file.MessageID]),
			})
		}

		chunkIndex := chunkCount[group.anchorMessageID]
		if err := outbox.InsertMediaTx(ctx, tx, scope.ownerUserID, scope.ownerTelegramUserID,
			scope.businessConnectionID, scope.chatID, group.anchorMessageID,
			outbox.EventDeletedMessage, chunkIndex,
			telegram.BuildMediaAlertText(payload.MediaTypes()), payload); err != nil {
			return err
		}
		chunkCount[group.anchorMessageID] = chunkIndex + 1
	}
	return nil
}

// mediaGroup is the set of files delivered by a single outbox entry.
type mediaGroup struct {
	// anchorMessageID is the message the entry is keyed on. For an album it is
	// the SMALLEST message_id of the group: the outbox unique key is
	// (message, event, chunk_index), so the anchor has to be stable across a
	// redelivery of the same deletion, otherwise the same album would be
	// enqueued twice.
	anchorMessageID int64
	mediaGroupID    string
	files           []media.File
}

// groupMedia gathers the files of one album together, keeping the order given
// by SelectStoredTx (message_id, then file_index): that is the order the sender
// composed the album in, and the only one the owner can check against what
// disappeared.
func groupMedia(files []media.File) []mediaGroup {
	var groups []mediaGroup
	// Two key spaces that cannot collide: albums by media_group_id, lone media
	// by message. A media without media_group_id is only ever grouped with the
	// other files of its own message.
	byAlbum := make(map[string]int)
	byMessage := make(map[int64]int)

	for _, file := range files {
		index, found := -1, false
		switch {
		case file.MediaGroupID != "":
			index, found = byAlbum[file.MediaGroupID]
		default:
			index, found = byMessage[file.MessageID]
		}
		if found {
			groups[index].files = append(groups[index].files, file)
			continue
		}

		groups = append(groups, mediaGroup{
			anchorMessageID: file.MessageID,
			mediaGroupID:    file.MediaGroupID,
			files:           []media.File{file},
		})
		if file.MediaGroupID != "" {
			byAlbum[file.MediaGroupID] = len(groups) - 1
		} else {
			byMessage[file.MessageID] = len(groups) - 1
		}
	}
	return groups
}
