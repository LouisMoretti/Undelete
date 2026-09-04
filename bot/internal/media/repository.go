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
// Downloading the files (writing them under ./media, see media/store and
// media/fetch), backing them up (#13) and purging them from disk (media/purge)
// live outside this package: here we only carry the metadata and the status
// those workflows drive. The queries the purge needs are still here, for the
// same reason as every other one: InTenant is the only way to the table.
package media

import (
	"context"
	"errors"
	"fmt"
	"time"

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
	MimeType string
	// FileName is the name chosen by the sender, when Telegram carries one. It
	// is display metadata only: it never contributes to a storage path.
	FileName     string
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
	FileName             string
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
	// CreatedAt is the capture time of the attachment, the instant retention
	// counts from -- the media equivalent of messages.saved_at. It is read
	// back because the disk purge decides on it: a 'stored' row whose file
	// vanished is re-downloaded while it is still within retention, and
	// written off as purged once it is not.
	CreatedAt time.Time
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
	COALESCE(mime_type, ''), COALESCE(file_name, ''),
	byte_size, width, height, duration_sec,
	COALESCE(relative_path, ''), COALESCE(thumbnail_relative_path, ''),
	COALESCE(sha256, ''), status, COALESCE(media_group_id, ''), created_at
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
//
// The optional metadata (mime_type, dimensions, duration, album) is refreshed
// through COALESCE rather than overwritten, for the same reason at a smaller
// scale: a redelivery that says nothing must not erase what a previous one
// knew.
func (r *Repository) Save(ctx context.Context, ownerUserID int64, m Record) (int64, error) {
	var id int64
	err := r.db.InTenant(ctx, ownerUserID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO media_files (
				owner_user_id, business_connection_id, chat_id, message_id, file_index,
				telegram_file_id, telegram_file_unique_id, media_type,
				mime_type, file_name, byte_size, width, height, duration_sec, media_group_id
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NULLIF($9, ''), NULLIF($10, ''), $11, $12, $13, $14, NULLIF($15, ''))
			ON CONFLICT (owner_user_id, business_connection_id, chat_id, message_id, file_index)
			DO UPDATE SET
				telegram_file_id        = EXCLUDED.telegram_file_id,
				telegram_file_unique_id = EXCLUDED.telegram_file_unique_id,
				media_type              = EXCLUDED.media_type,
				-- COALESCE, not a plain overwrite: NULL here means "this
				-- delivery did not say", never "the value was cleared". An
				-- edited_business_message re-describes the attachment and may
				-- carry less than the original update did (a photo edit that
				-- reports no MIME, dimensions dropped by a client): keeping the
				-- known value is always closer to the truth than replacing it
				-- with NULL. A richer redelivery still wins, since a non-NULL
				-- EXCLUDED value takes precedence.
				mime_type               = COALESCE(EXCLUDED.mime_type, media_files.mime_type),
				file_name               = COALESCE(EXCLUDED.file_name, media_files.file_name),
				-- byte_size carries two different facts: the size DECLARED by
				-- Telegram while the row is pending, then the size actually
				-- MEASURED on disk once MarkStored ran. A redelivery must not
				-- push the declared value back over the measured one -- the
				-- disk purge and any integrity check would then compare a real
				-- file against a size that was never its own.
				byte_size               = CASE WHEN media_files.status = 'stored'
				                              THEN media_files.byte_size
				                              ELSE COALESCE(EXCLUDED.byte_size, media_files.byte_size) END,
				width                   = COALESCE(EXCLUDED.width, media_files.width),
				height                  = COALESCE(EXCLUDED.height, media_files.height),
				duration_sec            = COALESCE(EXCLUDED.duration_sec, media_files.duration_sec),
				media_group_id          = COALESCE(EXCLUDED.media_group_id, media_files.media_group_id),
				updated_at              = now()
			RETURNING id
		`,
			ownerUserID, m.BusinessConnectionID, m.ChatID, m.MessageID, m.FileIndex,
			m.TelegramFileID, m.TelegramFileUniqueID, m.MediaType,
			m.MimeType, m.FileName, m.ByteSize, m.Width, m.Height, m.DurationSec, m.MediaGroupID,
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

// SelectStoredTx returns the attachments of the given messages that are
// actually ON DISK, ordered by (message_id, file_index) -- the order the sender
// saw, and the one an album must be restored in.
//
// It takes the caller's transaction instead of opening its own: the deletion
// alert is written by messages.MarkDeleted inside a single InTenant
// transaction, and the media entry must be enqueued atomically with the
// deleted_at it belongs to. The tenant context is therefore already set by the
// caller, and RLS applies exactly as it does everywhere else in this package.
//
// Only 'stored' rows are returned: a pending row has no path yet, and a purged
// one no longer has a file. Both leave the alert to its text, which already
// states the message type.
func SelectStoredTx(ctx context.Context, tx pgx.Tx, businessConnectionID string, chatID int64, messageIDs []int64) ([]File, error) {
	rows, err := tx.Query(ctx, `
		SELECT`+selectColumns+`
		FROM media_files
		WHERE business_connection_id = $1
		  AND chat_id = $2
		  AND message_id = ANY($3)
		  AND status = 'stored'
		  AND relative_path IS NOT NULL
		ORDER BY message_id, file_index
	`, businessConnectionID, chatID, messageIDs)
	if err != nil {
		return nil, fmt.Errorf("reading stored media: %w", err)
	}
	defer rows.Close()
	files, err := scanFiles(rows)
	if err != nil {
		return nil, fmt.Errorf("reading stored media: %w", err)
	}
	return files, nil
}

// SelectAlbumAnchorsTx returns, for each album touched by the given messages,
// the SMALLEST message_id it is made of -- whatever the status of its files.
//
// Status-independent on purpose, and that is the whole point of this query: the
// set of 'stored' rows moves under our feet (the fetch loop turns pending into
// stored, and a failed download into purged), so an anchor derived from the
// stored subset would shift between two deliveries of the SAME deletion. The
// outbox anti-duplicate key contains the message_id, so a shifting anchor lets
// the same album through twice. Catalogued membership, itself written once at
// capture time, does not move.
//
// Same transaction contract as SelectStoredTx: the tenant context is set by the
// caller, RLS applies unchanged.
func SelectAlbumAnchorsTx(ctx context.Context, tx pgx.Tx, businessConnectionID string, chatID int64, messageIDs []int64) (map[string]int64, error) {
	rows, err := tx.Query(ctx, `
		SELECT media_group_id, MIN(message_id)
		FROM media_files
		WHERE business_connection_id = $1
		  AND chat_id = $2
		  AND message_id = ANY($3)
		  AND media_group_id IS NOT NULL
		GROUP BY media_group_id
	`, businessConnectionID, chatID, messageIDs)
	if err != nil {
		return nil, fmt.Errorf("reading album anchors: %w", err)
	}
	defer rows.Close()

	anchors := make(map[string]int64)
	for rows.Next() {
		var groupID string
		var anchor int64
		if err := rows.Scan(&groupID, &anchor); err != nil {
			return nil, fmt.Errorf("reading album anchors: %w", err)
		}
		anchors[groupID] = anchor
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading album anchors: %w", err)
	}
	return anchors, nil
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

// MarkPendingRetry sends a row back to the download queue: the file it claimed
// to have on disk is not there any more.
//
// The reverse of MarkStored, and the reconciliation half of the disk purge. It
// only makes sense while the attachment is still within retention -- past that
// point there is nothing left to download for, and MarkPurged is the right
// answer. The path and the hash are cleared, so nothing keeps pointing at a
// file that does not exist and the fetch loop recomputes both.
func (r *Repository) MarkPendingRetry(ctx context.Context, ownerUserID, id int64) error {
	return r.update(ctx, ownerUserID, id, `
		UPDATE media_files
		SET status                  = 'pending',
		    relative_path           = NULL,
		    thumbnail_relative_path = NULL,
		    sha256                  = NULL,
		    updated_at              = now()
		WHERE id = $1
	`)
}

// ListExpiredStored returns at most limit attachments whose file is on disk and
// whose retention has elapsed, oldest first. The caller deletes the blob then
// calls MarkPurged; the batch is bounded so one pass can never turn into an
// unbounded scan-and-delete.
func (r *Repository) ListExpiredStored(ctx context.Context, ownerUserID int64, retentionDays, limit int) ([]File, error) {
	var files []File
	err := r.db.InTenant(ctx, ownerUserID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT`+selectColumns+`
			FROM media_files
			WHERE status = 'stored'
			  AND created_at < now() - make_interval(days => $1)
			ORDER BY id
			LIMIT $2
		`, retentionDays, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		files, err = scanFiles(rows)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("listing expired media: %w", err)
	}
	return files, nil
}

// ListStoredPage returns at most limit stored attachments with an id strictly
// greater than afterID, ordered by id.
//
// Keyset pagination rather than OFFSET: the reconciliation sweeps the whole
// catalogue a page at a time across successive runs, and rows disappear under
// it (that is precisely what it is there for). A cursor on the primary key
// keeps the sweep total, where an offset would skip rows every time an earlier
// one is deleted.
func (r *Repository) ListStoredPage(ctx context.Context, ownerUserID, afterID int64, limit int) ([]File, error) {
	var files []File
	err := r.db.InTenant(ctx, ownerUserID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT`+selectColumns+`
			FROM media_files
			WHERE status = 'stored' AND id > $1
			ORDER BY id
			LIMIT $2
		`, afterID, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		files, err = scanFiles(rows)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("listing stored media: %w", err)
	}
	return files, nil
}

// KnownPaths returns the subset of relPaths that a row of THIS tenant still
// references, either as a file or as a thumbnail.
//
// The question the disk side of the reconciliation asks: "does anything still
// point at what I just found on disk?". Answering it with the paths in hand,
// rather than by loading the catalogue, is what bounds the query by the size of
// the batch the caller scanned.
func (r *Repository) KnownPaths(ctx context.Context, ownerUserID int64, relPaths []string) (map[string]struct{}, error) {
	known := make(map[string]struct{}, len(relPaths))
	if len(relPaths) == 0 {
		return known, nil
	}
	err := r.db.InTenant(ctx, ownerUserID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT relative_path FROM media_files WHERE relative_path = ANY($1)
			UNION
			SELECT thumbnail_relative_path FROM media_files WHERE thumbnail_relative_path = ANY($1)
		`, relPaths)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var path string
			if err := rows.Scan(&path); err != nil {
				return err
			}
			known[path] = struct{}{}
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("resolving media paths: %w", err)
	}
	return known, nil
}

// DeleteStalePending removes the rows that have been waiting for a download
// that is never coming, and returns how many went.
//
// The row is deleted rather than marked purged: 'purged' means "we had the
// file and retention took it", and a pending row never had one -- keeping it
// would only make the fetch loop ask Telegram for it forever.
//
// Two independent deadlines, whichever comes first: maxAge, past which no
// retry can succeed any more (a Telegram file_id does not stay downloadable
// indefinitely), and the tenant's own retention, which no metadata may
// outlive.
//
// DELETE ... WHERE id IN (SELECT ... LIMIT): PostgreSQL has no LIMIT on
// DELETE, and the bound is the point.
func (r *Repository) DeleteStalePending(ctx context.Context, ownerUserID int64, maxAge time.Duration, retentionDays, limit int) (int64, error) {
	var deleted int64
	err := r.db.InTenant(ctx, ownerUserID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			DELETE FROM media_files
			WHERE id IN (
				SELECT id FROM media_files
				WHERE status = 'pending'
				  AND created_at < now() - LEAST(
				        make_interval(secs => $1),
				        make_interval(days => $2))
				ORDER BY id
				LIMIT $3
			)
		`, maxAge.Seconds(), retentionDays, limit)
		if err != nil {
			return err
		}
		deleted = tag.RowsAffected()
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("deleting stale pending media: %w", err)
	}
	return deleted, nil
}

// DeletePurged removes the rows whose file is long gone, and returns how many
// went.
//
// Two conditions, and the first one is the one that is easy to forget: a
// 'purged' row is not necessarily a purged FILE. The fetch loop also marks
// purged what Telegram will never hand over (over the 20 MB ceiling, expired
// handle), and that row is the only remaining trace that an attachment
// existed -- a deletion alert within retention still needs it. So retention
// gates the deletion, and grace (counted from updated_at, the instant the row
// became purged) only adds a margin on top of it.
func (r *Repository) DeletePurged(ctx context.Context, ownerUserID int64, grace time.Duration, retentionDays, limit int) (int64, error) {
	var deleted int64
	err := r.db.InTenant(ctx, ownerUserID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			DELETE FROM media_files
			WHERE id IN (
				SELECT id FROM media_files
				WHERE status = 'purged'
				  AND created_at < now() - make_interval(days => $1)
				  AND updated_at < now() - make_interval(secs => $2)
				ORDER BY id
				LIMIT $3
			)
		`, retentionDays, grace.Seconds(), limit)
		if err != nil {
			return err
		}
		deleted = tag.RowsAffected()
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("deleting purged media rows: %w", err)
	}
	return deleted, nil
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
			&f.MimeType, &f.FileName, &f.ByteSize, &f.Width, &f.Height, &f.DurationSec,
			&f.RelativePath, &f.ThumbnailRelativePath,
			&f.SHA256, &f.Status, &f.MediaGroupID, &f.CreatedAt,
		); err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	return files, rows.Err()
}
