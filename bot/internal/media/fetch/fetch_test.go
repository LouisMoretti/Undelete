package fetch

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/LouisMoretti/Undelete/bot/internal/media"
	"github.com/LouisMoretti/Undelete/bot/internal/media/store"
	"github.com/LouisMoretti/Undelete/bot/internal/telegram"
)

// fakeCatalogue implements catalogue in memory: pending rows are flipped to
// stored or purged exactly like the SQL transitions, without PostgreSQL.
type fakeCatalogue struct {
	pending   []media.File
	stored    map[int64]media.StoredFile
	purged    []int64
	listErr   error
	storedErr error
	purgedErr error
}

func (f *fakeCatalogue) ListPending(context.Context, int64, int) ([]media.File, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.pending, nil
}

func (f *fakeCatalogue) MarkStored(_ context.Context, _, id int64, s media.StoredFile) error {
	if f.storedErr != nil {
		return f.storedErr
	}
	if f.stored == nil {
		f.stored = map[int64]media.StoredFile{}
	}
	f.stored[id] = s
	return nil
}

func (f *fakeCatalogue) MarkPurged(_ context.Context, _, id int64) error {
	if f.purgedErr != nil {
		return f.purgedErr
	}
	f.purged = append(f.purged, id)
	return nil
}

type fakeResolver struct {
	files map[string]*telegram.File
	err   error
}

func (f *fakeResolver) GetFile(context.Context, string) (*telegram.File, error) {
	if f.err != nil {
		return nil, f.err
	}
	for _, file := range f.files {
		return file, nil
	}
	return &telegram.File{FilePath: "photos/file.jpg"}, nil
}

type fakeDownloader struct {
	saved store.StoredFile
	err   error
	calls int
}

func (f *fakeDownloader) Download(context.Context, string, store.Request) (store.StoredFile, error) {
	f.calls++
	if f.err != nil {
		return store.StoredFile{}, f.err
	}
	return f.saved, nil
}

func testFetcher(cat *fakeCatalogue, res *fakeResolver, dl *fakeDownloader) *Fetcher {
	if res == nil {
		res = &fakeResolver{}
	}
	if dl == nil {
		dl = &fakeDownloader{saved: store.StoredFile{
			RelPath: "2026/01/01/u1/photo",
			SHA256:  "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			Bytes:   1024,
		}}
	}
	return New(cat, res, dl, "token", slog.New(slog.NewJSONHandler(io.Discard, nil)))
}

func pendingFile(id int64) media.File {
	return media.File{ID: id, TelegramFileID: "file-id", TelegramFileUniqueID: "unique", MediaType: "photo"}
}

// TestIsDefinitiveCoversEveryFailureClass pins the retry policy: only
// failures Telegram could never clear on retry (oversize, refused path,
// definitive 4xx) abandon the row. Everything else -- cancellations, rate
// limits, 5xx, transport -- stays pending, because a media lost by giving up
// too early is a media the owner never gets back.
func TestIsDefinitiveCoversEveryFailureClass(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "cancellation stays pending", err: context.Canceled, want: false},
		{name: "deadline stays pending", err: context.DeadlineExceeded, want: false},
		{name: "oversize is definitive", err: store.ErrTooLarge, want: true},
		{name: "path traversal is definitive", err: store.ErrPathTraversal, want: true},
		{name: "400 is definitive", err: &telegram.APIError{Method: "getFile", Code: 400}, want: true},
		{name: "404 is definitive", err: &telegram.APIError{Method: "getFile", Code: 404}, want: true},
		{name: "429 stays pending", err: &telegram.APIError{Method: "getFile", Code: 429, RetryAfter: 5}, want: false},
		{name: "bare 429 stays pending", err: &telegram.APIError{Method: "getFile", Code: 429}, want: false},
		{name: "500 stays pending", err: &telegram.APIError{Method: "getFile", Code: 500}, want: false},
		{name: "wrapped 400 stays definitive", err: errors.Join(errors.New("resolving"), &telegram.APIError{Method: "getFile", Code: 400}), want: true},
		{name: "transport stays pending", err: errors.New("connection reset"), want: false},
		{name: "http status stays pending", err: store.ErrHTTP, want: false},
	}
	_ = http.StatusBadRequest // documents the 4xx boundary used above
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isDefinitive(tt.err); got != tt.want {
				t.Fatalf("isDefinitive(%v) = %t, want %t", tt.err, got, tt.want)
			}
		})
	}
}

// TestFetchOneEmptyPathIsDefinitive pins the Bot API contract: no file_path
// means the file is not downloadable by a bot (over the getFile ceiling) --
// definitive, so the loop stops asking forever.
func TestFetchOneEmptyPathIsDefinitive(t *testing.T) {
	cat := &fakeCatalogue{}
	f := testFetcher(cat, &fakeResolver{files: map[string]*telegram.File{"x": {FilePath: ""}}}, nil)
	err := f.fetchOne(context.Background(), 11, pendingFile(1))
	if !errors.Is(err, store.ErrTooLarge) {
		t.Fatalf("fetchOne(empty path) = %v, want ErrTooLarge", err)
	}
}

// TestProcessTenantStoresPendingFiles pins the happy path: one batch lists,
// resolves, downloads and marks stored, returning the stored count.
func TestProcessTenantStoresPendingFiles(t *testing.T) {
	cat := &fakeCatalogue{pending: []media.File{pendingFile(1), pendingFile(2)}}
	dl := &fakeDownloader{saved: store.StoredFile{
		RelPath: "2026/01/01/u1/photo",
		SHA256:  "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Bytes:   512,
	}}
	f := testFetcher(cat, nil, dl)
	stored, err := f.ProcessTenant(context.Background(), 11)
	if err != nil || stored != 2 {
		t.Fatalf("ProcessTenant = (%d, %v), want (2, nil)", stored, err)
	}
	if len(cat.stored) != 2 || dl.calls != 2 {
		t.Fatalf("stored=%d downloads=%d, want 2 and 2", len(cat.stored), dl.calls)
	}
}

// TestProcessTenantPurgesDefinitiveFailures pins the abandon path: a row
// Telegram will never hand over is marked purged (still catalogued, so the
// deletion alert can say a media existed) and does not count as stored.
func TestProcessTenantPurgesDefinitiveFailures(t *testing.T) {
	cat := &fakeCatalogue{pending: []media.File{pendingFile(1)}}
	f := testFetcher(cat, &fakeResolver{files: map[string]*telegram.File{"x": {FilePath: ""}}}, nil)
	stored, err := f.ProcessTenant(context.Background(), 11)
	if err != nil || stored != 0 {
		t.Fatalf("ProcessTenant = (%d, %v), want (0, nil)", stored, err)
	}
	if len(cat.purged) != 1 || cat.purged[0] != 1 {
		t.Fatalf("definitive failure must mark purged, got %v", cat.purged)
	}
}

// TestProcessTenantKeepsTransientFailuresPending pins the retry path: a 503
// leaves the row pending (neither stored nor purged) for the next pass.
func TestProcessTenantKeepsTransientFailuresPending(t *testing.T) {
	cat := &fakeCatalogue{pending: []media.File{pendingFile(1)}}
	f := testFetcher(cat,
		&fakeResolver{err: &telegram.APIError{Method: "getFile", Code: 503}},
		&fakeDownloader{})
	stored, err := f.ProcessTenant(context.Background(), 11)
	if err != nil || stored != 0 {
		t.Fatalf("ProcessTenant = (%d, %v), want (0, nil)", stored, err)
	}
	if len(cat.stored) != 0 || len(cat.purged) != 0 {
		t.Fatal("transient failure must leave the row pending")
	}
}

// TestProcessTenantPropagatesCatalogueErrors pins the failure surfaces: a
// ListPending error aborts the pass, and a MarkPurged error surfaces instead
// of silently skipping the row.
func TestProcessTenantPropagatesCatalogueErrors(t *testing.T) {
	t.Run("list error", func(t *testing.T) {
		f := testFetcher(&fakeCatalogue{listErr: errors.New("db down")}, nil, nil)
		if _, err := f.ProcessTenant(context.Background(), 11); err == nil {
			t.Fatal("ListPending error must propagate")
		}
	})
	t.Run("mark purged error", func(t *testing.T) {
		cat := &fakeCatalogue{pending: []media.File{pendingFile(1)}, purgedErr: errors.New("db down")}
		f := testFetcher(cat, &fakeResolver{files: map[string]*telegram.File{"x": {FilePath: ""}}}, nil)
		if _, err := f.ProcessTenant(context.Background(), 11); err == nil {
			t.Fatal("MarkPurged error must propagate")
		}
	})
	t.Run("mark stored error stays pending for the next pass", func(t *testing.T) {
		// A MarkStored failure is transient (dead connection, serialization):
		// the row is neither stored nor purged, the pass succeeds, and the
		// next pass retries it -- exactly like a download failure.
		cat := &fakeCatalogue{pending: []media.File{pendingFile(1)}, storedErr: errors.New("db down")}
		f := testFetcher(cat, nil, nil)
		stored, err := f.ProcessTenant(context.Background(), 11)
		if err != nil || stored != 0 {
			t.Fatalf("ProcessTenant = (%d, %v), want (0, nil)", stored, err)
		}
		if len(cat.stored) != 0 || len(cat.purged) != 0 {
			t.Fatal("MarkStored failure must leave the row pending")
		}
	})
}

// TestProcessTenantCancelledContextStopsQuietly pins the shutdown path: a
// dead context ends the pass with what was stored so far and no error.
func TestProcessTenantCancelledContextStopsQuietly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cat := &fakeCatalogue{pending: []media.File{pendingFile(1)}}
	f := testFetcher(cat, nil, nil)
	stored, err := f.ProcessTenant(ctx, 11)
	if err != nil || stored != 0 {
		t.Fatalf("ProcessTenant(cancelled) = (%d, %v), want (0, nil)", stored, err)
	}
}

// TestNewFetcherDefaultsBatch pins construction: the per-tenant batch is the
// small default, so one tenant's burst never starves a quieter tenant.
func TestNewFetcherDefaultsBatch(t *testing.T) {
	f := testFetcher(&fakeCatalogue{}, nil, nil)
	if f.batch != defaultBatch || f.batch <= 0 {
		t.Fatalf("batch = %d, want default %d", f.batch, defaultBatch)
	}
}
