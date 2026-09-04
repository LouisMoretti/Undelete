// Package media manages the media_files table, protected by FORCE ROW LEVEL
// SECURITY like messages. Every method here goes through storage.DB.InTenant:
// there is deliberately no path that would query the table via the bare pool
// (constraint #4).
//
// The table catalogues attachments; it never holds their bytes. The blob lives
// on disk under ./media, designated by a relative path generated on our side
// and validated by ValidateRelativePath before it ever reaches the database
// (see migration 0004 for the rationale and the mirrored CHECK constraints).
//
// Downloading the files (writing them under ./media), backing them up (#13)
// and purging them from disk (#12) live outside this package: here we only
// carry the metadata and the status those workflows drive.
package media

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/LouisMoretti/Undelete/bot/internal/storage"
)

// Media types accepted by the media_type CHECK of migration 0004. TypeUnknown
// is the deliberate fallback: an attachment kind we do not recognise must still
// be catalogued (and therefore restorable) rather than dropped.
const (
	TypePhoto     = "photo"
	TypeVideo     = "video"
	TypeAnimation = "animation"
	TypeDocument  = "document"
	TypeAudio     = "audio"
	TypeVoice     = "voice"
	TypeVideoNote = "video_note"
	TypeSticker   = "sticker"
	TypeUnknown   = "unknown"
)

// Lifecycle statuses. A row starts pending (nothing on disk yet), becomes
// stored once the file is written, then purged once retention removed it from
// disk -- the row survives so a deletion alert can still say a media existed.
const (
	StatusPending = "pending"
	StatusStored  = "stored"
	StatusPurged  = "purged"
)

// ErrNotFound is returned when an update targets a row that the tenant cannot
// see: either it does not exist, or it belongs to another owner and RLS hides
// it. The two are indistinguishable by design, and both are equally a
// programming error on the caller's side.
var ErrNotFound = errors.New("media file not found for this tenant")

// Record is the metadata known when the message is received, before anything
// has been downloaded. The path and the hash are absent on purpose: they only
// exist once the file is on disk (MarkStored).
type Record struct {
	BusinessConnectionID string
	ChatID               int64
	MessageID            int64
	// FileIndex orders the attachments within a single message (0..N). An
	// album spreads its items across several messages instead, and is tracked
	// by MediaGroupID.
	FileIndex int
	// TelegramFileID is the download handle, valid only for the bot that
	// received it and free to change between updates. TelegramFileUniqueID is
	// stable and identifies the same content across messages, but cannot be
	// used to download.
	TelegramFileID       string
	TelegramFileUniqueID string
	MediaType            string
	// Optional metadata: empty strings and nil pointers are stored as NULL
	// rather than as a zero value, so "unknown size" stays distinguishable
	// from "empty file".
	MimeType     string
	ByteSize     *int64
	Width        *int
	Height       *int
	DurationSec  *int
	MediaGroupID string
}

// File is a catalogued attachment, as read back from the table.
type File struct {
	ID                   int64
	BusinessConnectionID string
	ChatID               int64
	MessageID            int64
	FileIndex            int
	TelegramFileID       string
	TelegramFileUniqueID string
	MediaType            string
	MimeType             string
	ByteSize             *int64
	Width                *int
	Height               *int
	DurationSec          *int
	// RelativePath and ThumbnailRelativePath are relative to the media
	// directory, empty while the file is still pending.
	RelativePath          string
	ThumbnailRelativePath string
	SHA256                string
	Status                string
	MediaGroupID          string
}

// StoredFile describes a file that has just been written under ./media.
type StoredFile struct {
	RelativePath string
	// ThumbnailRelativePath is optional: empty means "no thumbnail", stored as
	// NULL, and never overwrites nothing with an invalid empty path.
	ThumbnailRelativePath string
	// SHA256 must be lowercase hex, 64 characters (CHECK in migration 0004).
	SHA256   string
	ByteSize int64
}

// Repository provides access to the media_files table, exclusively via
// InTenant.
type Repository struct {
	db *storage.DB
}

func NewRepository(db *storage.DB) *Repository {
	return &Repository{db: db}
}

// selectColumns is shared by every read so the scan order stays in one place.
// COALESCE on the nullable TEXT columns keeps the Go struct free of pointers
// for fields where "" and NULL mean the same thing (unknown).
const selectColumns = `
	id, business_connection_id, chat_id, message_id, file_index,
	telegram_file_id, telegram_file_unique_id, media_type,
	COALESCE(mime_type, ''), byte_size, width, height, duration_sec,
	COALESCE(relative_path, ''), COALESCE(thumbnail_relative_path, ''),
	COALESCE(sha256, ''), status, COALESCE(media_group_id, '')
`

// Save inserts or refreshes the metadata of one attachment and returns its id.
//
// ON CONFLICT DO UPDATE on the anti-collision key (owner_user_id,
// business_connection_id, chat_id, message_id, file_index): Telegram may
// redeliver an already processed update (poller restart before offset
// confirmation, network retry), and an edited_business_message re-describes the
// same attachment. The upsert therefore refreshes telegram_file_id, which
// Telegram is free to change between updates.
//
// What the upsert deliberately does NOT touch: status, relative_path, sha256,
// thumbnail_relative_path, and byte_size once the file is stored. Those
// describe the file ON DISK, a fact this method knows nothing about. Resetting
// them to pending/NULL on a redelivery would orphan an already downloaded blob
// -- nothing would ever come back to delete it, since the row would no longer
// point at it.
func (r *Repository) Save(ctx context.Context, ownerUserID int64, m Record) (int64, error) {
	var id int64
	err := r.db.InTenant(ctx, ownerUserID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO media_files (
				owner_user_id, business_connection_id, chat_id, message_id, file_index,
				telegram_file_id, telegram_file_unique_id, media_type,
				mime_type, byte_size, width, height, duration_sec, media_group_id
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NULLIF($9, ''), $10, $11, $12, $13, NULLIF($14, ''))
			ON CONFLICT (owner_user_id, business_connection_id, chat_id, message_id, file_index)
			DO UPDATE SET
				telegram_file_id        = EXCLUDED.telegram_file_id,
				telegram_file_unique_id = EXCLUDED.telegram_file_unique_id,
				media_type              = EXCLUDED.media_type,
				mime_type               = EXCLUDED.mime_type,
				-- byte_size carries two different facts: the size DECLARED by
				-- Telegram while the row is pending, then the size actually
				-- MEASURED on disk once MarkStored ran. A redelivery must not
				-- push the declared value back over the measured one -- the
				-- disk purge and any integrity check would then compare a real
				-- file against a size that was never its own.
				byte_size               = CASE WHEN media_files.status = 'stored'
				                              THEN media_files.byte_size
				                              ELSE EXCLUDED.byte_size END,
				width                   = EXCLUDED.width,
				height                  = EXCLUDED.height,
				duration_sec            = EXCLUDED.duration_sec,
				media_group_id          = EXCLUDED.media_group_id,
				updated_at              = now()
			RETURNING id
		`,
			ownerUserID, m.BusinessConnectionID, m.ChatID, m.MessageID, m.FileIndex,
			m.TelegramFileID, m.TelegramFileUniqueID, m.MediaType,
			m.MimeType, m.ByteSize, m.Width, m.Height, m.DurationSec, m.MediaGroupID,
		).Scan(&id)
	})
	if err != nil {
		return 0, fmt.Errorf("media upsert: %w", err)
	}
	return id, nil
}

// GetByMessage returns the attachments of one message, ordered by file_index so
// an album or a multi-file message is rebuilt in the order it was sent.
func (r *Repository) GetByMessage(ctx context.Context, ownerUserID int64, businessConnectionID string, chatID, messageID int64) ([]File, error) {
	var files []File
	err := r.db.InTenant(ctx, ownerUserID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT`+selectColumns+`
			FROM media_files
			WHERE business_connection_id = $1 AND chat_id = $2 AND message_id = $3
			ORDER BY file_index
		`, businessConnectionID, chatID, messageID)
		if err != nil {
			return err
		}
		defer rows.Close()
		files, err = scanFiles(rows)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("reading media of message %d: %w", messageID, err)
	}
	return files, nil
}

// ListPending returns at most limit attachments still awaiting download, oldest
// first so a burst of new media never starves an older one. The caller loops
// tenant by tenant: like every other query here, this one only sees the rows of
// the owner whose context InTenant set.
func (r *Repository) ListPending(ctx context.Context, ownerUserID int64, limit int) ([]File, error) {
	var files []File
	err := r.db.InTenant(ctx, ownerUserID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT`+selectColumns+`
			FROM media_files
			WHERE status = $1
			ORDER BY id
			LIMIT $2
		`, StatusPending, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		files, err = scanFiles(rows)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("listing pending media: %w", err)
	}
	return files, nil
}

// MarkStored records that the file is on disk: path, hash, real size, optional
// thumbnail, status stored.
//
// Both paths and the hash are validated BEFORE the query. The CHECK constraints
// of migration 0004 would catch them too, but only after the file has been
// written under that same path: refusing here is what keeps the write inside
// ./media, and what stops a malformed hash from leaving the row pending while
// the blob sits on disk with nothing pointing at it.
func (r *Repository) MarkStored(ctx context.Context, ownerUserID, id int64, s StoredFile) error {
	if err := ValidateRelativePath(s.RelativePath); err != nil {
		return fmt.Errorf("media %d: %w", id, err)
	}
	if s.ThumbnailRelativePath != "" {
		if err := ValidateRelativePath(s.ThumbnailRelativePath); err != nil {
			return fmt.Errorf("media %d thumbnail: %w", id, err)
		}
	}
	if err := ValidateSHA256(s.SHA256); err != nil {
		return fmt.Errorf("media %d: %w", id, err)
	}
	return r.update(ctx, ownerUserID, id, `
		UPDATE media_files
		SET status                  = 'stored',
		    relative_path           = $2,
		    thumbnail_relative_path = NULLIF($3, ''),
		    sha256                  = $4,
		    byte_size               = $5,
		    updated_at              = now()
		WHERE id = $1
	`, s.RelativePath, s.ThumbnailRelativePath, s.SHA256, s.ByteSize)
}

// MarkPurged records that retention deleted the blob from disk. The row is
// KEPT: a deletion alert must still be able to say that a media existed, and
// the metadata (type, size, sender) is not what took up the space. relative_path
// is cleared so nothing keeps pointing at a file that is gone.
func (r *Repository) MarkPurged(ctx context.Context, ownerUserID, id int64) error {
	return r.update(ctx, ownerUserID, id, `
		UPDATE media_files
		SET status                  = 'purged',
		    relative_path           = NULL,
		    thumbnail_relative_path = NULL,
		    updated_at              = now()
		WHERE id = $1
	`)
}

// update runs a single-row UPDATE inside the tenant context and turns "no row
// affected" into ErrNotFound. Without this check the call would silently
// succeed against another tenant's row hidden by RLS -- the same fail-closed
// trap documented on messages.PurgeExpired.
func (r *Repository) update(ctx context.Context, ownerUserID, id int64, sql string, args ...any) error {
	return r.db.InTenant(ctx, ownerUserID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, sql, append([]any{id}, args...)...)
		if err != nil {
			return fmt.Errorf("updating media %d: %w", id, err)
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("%w: id=%d owner=%d", ErrNotFound, id, ownerUserID)
		}
		return nil
	})
}

func scanFiles(rows pgx.Rows) ([]File, error) {
	var files []File
	for rows.Next() {
		var f File
		if err := rows.Scan(
			&f.ID, &f.BusinessConnectionID, &f.ChatID, &f.MessageID, &f.FileIndex,
			&f.TelegramFileID, &f.TelegramFileUniqueID, &f.MediaType,
			&f.MimeType, &f.ByteSize, &f.Width, &f.Height, &f.DurationSec,
			&f.RelativePath, &f.ThumbnailRelativePath,
			&f.SHA256, &f.Status, &f.MediaGroupID,
		); err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	return files, rows.Err()
}
