package fetch

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/LouisMoretti/Undelete/bot/internal/media"
	"github.com/LouisMoretti/Undelete/bot/internal/media/store"
	"github.com/LouisMoretti/Undelete/bot/internal/tenantexcl"
)

// gatingDownloader suspends one download until release is closed: a batch
// with a download in flight, and a batch processed only halfway when the
// erasure arrives.
type gatingDownloader struct {
	mu      sync.Mutex
	calls   int
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	saved   store.StoredFile
}

func newGatingDownloader() *gatingDownloader {
	return &gatingDownloader{
		entered: make(chan struct{}),
		release: make(chan struct{}),
		saved: store.StoredFile{
			RelPath: "2026/01/01/u1/photo",
			SHA256:  "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			Bytes:   1024,
		},
	}
}

func (d *gatingDownloader) Download(ctx context.Context, _ string, _ store.Request) (store.StoredFile, error) {
	d.mu.Lock()
	d.calls++
	first := d.calls == 1
	d.mu.Unlock()
	if first {
		d.once.Do(func() { close(d.entered) })
		select {
		case <-d.release:
		case <-ctx.Done():
			return store.StoredFile{}, ctx.Err()
		}
	}
	return d.saved, nil
}

func (d *gatingDownloader) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

// TestErasureWaitsForTheInFlightBatch: with the first of two downloads
// suspended, the batch is loaded and partially processed when the erasure
// arrives. The erasure must wait for the whole batch -- not proceed between
// two items, and not while a download is suspended -- so that the row
// deletions and the disk sweep that follow see the batch's final state, and
// no download lands a blob after the sweep.
func TestErasureWaitsForTheInFlightBatch(t *testing.T) {
	guard := tenantexcl.New()
	cat := &fakeCatalogue{pending: []media.File{pendingFile(1), pendingFile(2)}}
	dl := newGatingDownloader()
	fetcher := New(cat, &fakeResolver{}, dl, "token",
		slog.New(slog.NewJSONHandler(io.Discard, nil)), guard)

	type result struct {
		stored int
		err    error
	}
	done := make(chan result, 1)
	go func() {
		stored, err := fetcher.ProcessTenant(context.Background(), 11)
		done <- result{stored: stored, err: err}
	}()

	select {
	case <-dl.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the fetcher never started its first download")
	}

	drained := make(chan struct{})
	go func() {
		release, err := guard.Exclusive(context.Background(), 11)
		if err != nil {
			t.Errorf("Exclusive: %v", err)
			return
		}
		defer release()
		close(drained)
	}()

	// The batch is loaded and its first download suspended: the erasure must
	// not slip in now, neither before the batch finishes nor between its items.
	select {
	case <-drained:
		t.Fatal("the erasure proceeded while a download was suspended and a batch half-processed")
	case <-time.After(100 * time.Millisecond):
	}

	close(dl.release)

	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("ProcessTenant: %v", res.err)
		}
		if res.stored != 2 {
			t.Fatalf("stored = %d, want the whole batch", res.stored)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the batch never finished after the download was released")
	}

	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("the erasure never proceeded after the batch finished")
	}
}

// TestBatchStartingDuringAnErasureSeesTheEmptiedCatalogue: a batch that
// starts while the tenant is being erased waits on the shared side instead
// of listing rows about to be deleted; it then runs against whatever is
// left.
func TestBatchStartingDuringAnErasureSeesTheEmptiedCatalogue(t *testing.T) {
	guard := tenantexcl.New()
	release, err := guard.Exclusive(context.Background(), 11)
	if err != nil {
		t.Fatalf("Exclusive: %v", err)
	}

	listed := make(chan struct{})
	var once sync.Once
	cat := &signallingCatalogue{catalogue: &fakeCatalogue{}, listed: listed, once: &once}
	fetcher := New(cat, &fakeResolver{}, &fakeDownloader{saved: store.StoredFile{
		RelPath: "2026/01/01/u1/photo",
		SHA256:  "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Bytes:   1024,
	}}, "token", slog.New(slog.NewJSONHandler(io.Discard, nil)), guard)

	done := make(chan error, 1)
	go func() {
		_, err := fetcher.ProcessTenant(context.Background(), 11)
		done <- err
	}()

	select {
	case <-listed:
		t.Fatal("the fetcher listed the catalogue while the erasure held the tenant")
	case <-time.After(100 * time.Millisecond):
	}

	release()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ProcessTenant: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the batch never proceeded after the erasure finished")
	}
}

// signallingCatalogue reports the first ListPending so the test can tell a
// batch that read the catalogue from one still waiting on the exclusion.
type signallingCatalogue struct {
	catalogue *fakeCatalogue
	listed    chan struct{}
	once      *sync.Once
}

func (c *signallingCatalogue) ListPending(ctx context.Context, owner int64, limit int) ([]media.File, error) {
	c.once.Do(func() { close(c.listed) })
	return c.catalogue.ListPending(ctx, owner, limit)
}

func (c *signallingCatalogue) MarkStored(ctx context.Context, owner, id int64, s media.StoredFile) error {
	return c.catalogue.MarkStored(ctx, owner, id, s)
}

func (c *signallingCatalogue) MarkPurged(ctx context.Context, owner, id int64) error {
	return c.catalogue.MarkPurged(ctx, owner, id)
}
