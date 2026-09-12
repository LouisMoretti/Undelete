// Package fetch turns the pending rows of media_files into files on disk.
//
// It is the join between the three pieces that already exist and knew nothing
// of each other: the catalogue (media.Repository), the download handle
// (telegram getFile) and the atomic storage (media/store). Without it a
// deletion alert could never carry anything, since only a 'stored' row is ever
// delivered.
//
// Runs in its own loop, off the poller path: a slow download must never delay
// the processing of Telegram updates (long-polling responsiveness), and a
// media that fails to arrive must never block the alert that says a message
// was deleted.
package fetch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/LouisMoretti/Undelete/bot/internal/media"
	"github.com/LouisMoretti/Undelete/bot/internal/media/store"
	"github.com/LouisMoretti/Undelete/bot/internal/telegram"
	"github.com/LouisMoretti/Undelete/bot/internal/tenantexcl"
)

// defaultBatch bounds one pass per tenant. Small on purpose: the loop comes
// back regularly, and a huge batch would hold a burst of downloads in front of
// the alerts of a quieter tenant.
const defaultBatch = 20

// Resolver is the getFile half of the Bot API client.
type Resolver interface {
	GetFile(ctx context.Context, fileID string) (*telegram.File, error)
}

// Downloader is the storage half (media/store.Downloader).
type Downloader interface {
	Download(ctx context.Context, token string, req store.Request) (store.StoredFile, error)
}

// catalogue is the subset of media.Repository used by Fetcher: listing the
// pending rows, then flipping each of them to stored or purged. An interface
// (rather than the concrete type) so unit tests can substitute a fake without
// a database.
type catalogue interface {
	ListPending(ctx context.Context, ownerUserID int64, limit int) ([]media.File, error)
	MarkStored(ctx context.Context, ownerUserID, id int64, s media.StoredFile) error
	MarkPurged(ctx context.Context, ownerUserID, id int64) error
}

// Fetcher downloads the attachments catalogued as pending.
type Fetcher struct {
	repo       catalogue
	resolver   Resolver
	downloader Downloader
	// token never leaves this field except as an argument to Download, which
	// builds the URL in memory and never logs it.
	token string
	// guard is the per-tenant exclusion shared with the erasure (same
	// instance, wired in cmd/bot). One batch holds the shared side from the
	// listing to the last MarkStored: an erasure waits for the batch to
	// finish, then deletes rows and sweeps the disk, so no download can land
	// a blob after the sweep. A batch that starts during an erasure waits,
	// then lists an emptied catalogue. Nil disables the coordination (unit
	// tests); production always passes the shared guard.
	guard  *tenantexcl.Guard
	logger *slog.Logger
	batch  int
}

func New(repo catalogue, resolver Resolver, downloader Downloader, token string, logger *slog.Logger, guard *tenantexcl.Guard) *Fetcher {
	return &Fetcher{
		repo:       repo,
		resolver:   resolver,
		downloader: downloader,
		token:      token,
		guard:      guard,
		logger:     logger,
		batch:      defaultBatch,
	}
}

// ProcessTenant downloads at most one batch for a tenant and returns how many
// files were stored.
//
// A transient failure (5xx, rate limit, network) leaves the row pending: the
// next pass retries it, and nothing is lost. A DEFINITIVE failure -- a file
// Telegram will never hand over (over the 20 MB getFile ceiling, expired
// file_id, refused path) -- marks the row purged: the attachment stays
// catalogued, so the deletion alert can still say a media existed, and the
// loop stops asking for it forever.
func (f *Fetcher) ProcessTenant(ctx context.Context, ownerUserID int64) (int, error) {
	if f.guard != nil {
		release, err := f.guard.Shared(ctx, ownerUserID)
		if err != nil {
			return 0, err
		}
		defer release()
	}
	pending, err := f.repo.ListPending(ctx, ownerUserID, f.batch)
	if err != nil {
		return 0, err
	}

	stored := 0
	for _, file := range pending {
		if ctx.Err() != nil {
			return stored, nil
		}
		switch err := f.fetchOne(ctx, ownerUserID, file); {
		case err == nil:
			stored++
		case isDefinitive(err):
			if markErr := f.repo.MarkPurged(ctx, ownerUserID, file.ID); markErr != nil {
				return stored, markErr
			}
			// The class of the failure, never the path, the file_id or the URL.
			f.logger.Warn("media not retrievable, catalogued without a file",
				slog.Int64("media_file_id", file.ID),
				slog.String("media_type", file.MediaType))
		case ctx.Err() != nil:
			return stored, nil
		default:
			f.logger.Warn("media download postponed",
				slog.Int64("media_file_id", file.ID),
				slog.String("media_type", file.MediaType))
		}
	}
	return stored, nil
}

func (f *Fetcher) fetchOne(ctx context.Context, ownerUserID int64, file media.File) error {
	resolved, err := f.resolver.GetFile(ctx, file.TelegramFileID)
	if err != nil {
		return fmt.Errorf("resolving media %d: %w", file.ID, err)
	}
	if resolved.FilePath == "" {
		// Documented by the Bot API: no file_path means the file is not
		// downloadable by a bot (over 20 MB). Definitive, hence the sentinel.
		return fmt.Errorf("media %d: %w", file.ID, store.ErrTooLarge)
	}

	saved, err := f.downloader.Download(ctx, f.token, store.Request{
		OwnerUserID: ownerUserID,
		FileID:      file.TelegramFileID,
		FilePath:    resolved.FilePath,
		UniqueID:    store.UniqueID(file.TelegramFileUniqueID),
	})
	if err != nil {
		return fmt.Errorf("downloading media %d: %w", file.ID, err)
	}

	return f.repo.MarkStored(ctx, ownerUserID, file.ID, media.StoredFile{
		RelativePath: saved.RelPath,
		SHA256:       saved.SHA256,
		ByteSize:     saved.Bytes,
	})
}

// isDefinitive reports a failure that retrying could never clear. Everything
// else stays pending, because a media lost by giving up too early is a media
// the owner will never get back.
func isDefinitive(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, store.ErrTooLarge) || errors.Is(err, store.ErrPathTraversal) {
		return true
	}
	var apiErr *telegram.APIError
	if errors.As(err, &apiErr) {
		return apiErr.Code >= http.StatusBadRequest &&
			apiErr.Code < http.StatusInternalServerError &&
			apiErr.Code != http.StatusTooManyRequests
	}
	return false
}
