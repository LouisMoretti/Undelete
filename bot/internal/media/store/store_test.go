package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testToken    = "123456:not-a-real-bot-token"
	testOwner    = int64(4242)
	testUniqueID = UniqueID("AQADuniqueid")
	testFilePath = "photos/file_0.jpg"
)

// fixedNow pins the dated portion of the storage path.
func fixedNow() time.Time {
	return time.Date(2026, 9, 4, 12, 30, 0, 0, time.UTC)
}

// newDownloader builds a Downloader pointed at srv, with a base configuration
// that keeps the tests fast (short backoff, short timeout).
func newDownloader(t *testing.T, srv *httptest.Server, tweak func(*Config)) (*Downloader, string) {
	t.Helper()
	root := t.TempDir()
	cfg := Config{
		BaseDir:        root,
		HTTPClient:     srv.Client(),
		BaseURL:        srv.URL + "/file/bot",
		MaxBytes:       1 << 20,
		RequestTimeout: 2 * time.Second,
		Retries:        0,
		Backoff:        time.Millisecond,
		Now:            fixedNow,
	}
	if tweak != nil {
		tweak(&cfg)
	}
	d, err := New(cfg)
	if err != nil {
		t.Fatalf("New() = %v", err)
	}
	return d, cfg.BaseDir
}

func defaultRequest() Request {
	return Request{
		OwnerUserID: testOwner,
		FileID:      "AgACAgQAAx",
		FilePath:    testFilePath,
		UniqueID:    testUniqueID,
	}
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// listFiles returns every regular file under root, relative to it. Used to
// assert that a failed download leaves NOTHING behind: no final file, and no
// leftover temporary file either.
func listFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		out = append(out, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	return out
}

func serveBytes(payload []byte) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	}))
}

func TestDownloadStoresContentAtServerGeneratedPath(t *testing.T) {
	payload := []byte("a media payload, intact from end to end")
	var gotURLPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURLPath = r.URL.Path
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	d, root := newDownloader(t, srv, nil)
	stored, err := d.Download(context.Background(), testToken, defaultRequest())
	if err != nil {
		t.Fatalf("Download() = %v", err)
	}

	// The token travels in the URL and nowhere else.
	if want := "/file/bot" + testToken + "/" + testFilePath; gotURLPath != want {
		t.Fatalf("requested path = %q, want %q", gotURLPath, want)
	}
	// Path built from ownerUserID + date + file_unique_id, never from
	// file_path ("photos/file_0.jpg" must leave no trace on disk).
	wantRel := filepath.Join("4242", "2026-09", "04", string(testUniqueID))
	if stored.RelPath != wantRel {
		t.Fatalf("RelPath = %q, want %q", stored.RelPath, wantRel)
	}
	if stored.Path != filepath.Join(root, wantRel) {
		t.Fatalf("Path = %q, want inside %q", stored.Path, root)
	}
	if stored.Reused {
		t.Fatal("first download should not be marked as reused")
	}

	onDisk, err := os.ReadFile(stored.Path)
	if err != nil {
		t.Fatalf("reading stored file: %v", err)
	}
	if !bytes.Equal(onDisk, payload) {
		t.Fatalf("stored content = %q, want %q", onDisk, payload)
	}
	if stored.SHA256 != sha256Hex(payload) {
		t.Fatalf("SHA256 = %q, want %q", stored.SHA256, sha256Hex(payload))
	}
	if stored.Bytes != int64(len(payload)) {
		t.Fatalf("Bytes = %d, want %d", stored.Bytes, len(payload))
	}
	if got := listFiles(t, root); len(got) != 1 || got[0] != wantRel {
		t.Fatalf("files on disk = %v, want only %q", got, wantRel)
	}
	if err := Verify(stored.Path, stored.SHA256); err != nil {
		t.Fatalf("Verify() = %v", err)
	}
}

// A malicious file_path must be rejected BEFORE any request and must never
// write outside the root.
func TestDownloadRejectsPathTraversalInFilePath(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte("pwned"))
	}))
	defer srv.Close()

	cases := map[string]string{
		"parent traversal":  "../../../etc/passwd",
		"absolute":          "/etc/passwd",
		"windows separator": `..\..\windows\system32`,
		"absolute url":      "https://evil.example/payload",
		"empty":             "",
		"empty segment":     "photos//file_0.jpg",
		"control character": "photos/\x00file_0.jpg",
	}
	for name, filePath := range cases {
		t.Run(name, func(t *testing.T) {
			d, root := newDownloader(t, srv, nil)
			req := defaultRequest()
			req.FilePath = filePath

			_, err := d.Download(context.Background(), testToken, req)
			if !errors.Is(err, ErrPathTraversal) {
				t.Fatalf("Download() = %v, want ErrPathTraversal", err)
			}
			if got := listFiles(t, root); len(got) != 0 {
				t.Fatalf("files written despite rejection: %v", got)
			}
			// Nothing outside the root either: the parent of the temp dir
			// must not have gained an "etc" or "passwd" entry.
			if _, err := os.Stat(filepath.Join(filepath.Dir(root), "etc")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("wrote outside the storage root: %v", err)
			}
		})
	}
	if hits.Load() != 0 {
		t.Fatalf("server was called %d times, want 0", hits.Load())
	}
}

// A hostile file_unique_id cannot escape the root either: it is replaced by
// its hash rather than used as a path.
func TestStorageKeyNeutralizesHostileUniqueID(t *testing.T) {
	payload := []byte("payload")
	srv := serveBytes(payload)
	defer srv.Close()

	d, root := newDownloader(t, srv, nil)
	req := defaultRequest()
	req.UniqueID = UniqueID("../../escape")

	stored, err := d.Download(context.Background(), testToken, req)
	if err != nil {
		t.Fatalf("Download() = %v", err)
	}
	if strings.Contains(stored.RelPath, "..") {
		t.Fatalf("RelPath = %q, still contains a traversal", stored.RelPath)
	}
	if !strings.HasPrefix(stored.Path, root+string(os.PathSeparator)) {
		t.Fatalf("Path = %q, outside root %q", stored.Path, root)
	}
	wantKey := sha256Hex([]byte("../../escape"))
	if filepath.Base(stored.Path) != wantKey {
		t.Fatalf("storage key = %q, want the hash %q", filepath.Base(stored.Path), wantKey)
	}
}

// Size guard: a response bigger than MaxBytes is aborted mid-stream instead
// of filling the disk.
func TestDownloadAbortsOverMaxBytes(t *testing.T) {
	t.Run("announced by Content-Length", func(t *testing.T) {
		srv := serveBytes(bytes.Repeat([]byte("x"), 4096))
		defer srv.Close()

		d, root := newDownloader(t, srv, func(c *Config) { c.MaxBytes = 1024 })
		_, err := d.Download(context.Background(), testToken, defaultRequest())
		if !errors.Is(err, ErrTooLarge) {
			t.Fatalf("Download() = %v, want ErrTooLarge", err)
		}
		if got := listFiles(t, root); len(got) != 0 {
			t.Fatalf("files left behind: %v", got)
		}
	})

	// Without Content-Length (chunked), only the streaming counter can stop
	// the write: this is the case that actually protects the disk.
	t.Run("chunked without Content-Length", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			flusher, ok := w.(http.Flusher)
			if !ok {
				t.Error("ResponseWriter is not a Flusher")
				return
			}
			chunk := bytes.Repeat([]byte("y"), 512)
			for i := 0; i < 8; i++ {
				if _, err := w.Write(chunk); err != nil {
					return
				}
				flusher.Flush()
			}
		}))
		defer srv.Close()

		d, root := newDownloader(t, srv, func(c *Config) { c.MaxBytes = 1024 })
		_, err := d.Download(context.Background(), testToken, defaultRequest())
		if !errors.Is(err, ErrTooLarge) {
			t.Fatalf("Download() = %v, want ErrTooLarge", err)
		}
		if got := listFiles(t, root); len(got) != 0 {
			t.Fatalf("files left behind: %v", got)
		}
	})
}

// A server returning different bytes than expected must produce no final
// file: the hash is checked BEFORE the rename.
func TestDownloadHashMismatchLeavesNothingBehind(t *testing.T) {
	srv := serveBytes([]byte("bytes substituted by an attacker"))
	defer srv.Close()

	d, root := newDownloader(t, srv, nil)
	req := defaultRequest()
	req.ExpectedSHA256 = sha256Hex([]byte("the legitimate content"))

	_, err := d.Download(context.Background(), testToken, req)
	if !errors.Is(err, ErrHashMismatch) {
		t.Fatalf("Download() = %v, want ErrHashMismatch", err)
	}
	if got := listFiles(t, root); len(got) != 0 {
		t.Fatalf("files left behind after a mismatch: %v", got)
	}
}

func TestDownloadTimesOut(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		_, _ = w.Write([]byte("too late"))
	}))
	defer srv.Close()
	defer close(release)

	d, root := newDownloader(t, srv, func(c *Config) {
		c.RequestTimeout = 50 * time.Millisecond
	})
	_, err := d.Download(context.Background(), testToken, defaultRequest())
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("Download() = %v, want ErrTimeout", err)
	}
	if got := listFiles(t, root); len(got) != 0 {
		t.Fatalf("files left behind after a timeout: %v", got)
	}
}

func TestDownloadRetriesServerErrorsThenSucceeds(t *testing.T) {
	payload := []byte("finally available")
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) <= 2 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	d, root := newDownloader(t, srv, func(c *Config) { c.Retries = 2 })
	stored, err := d.Download(context.Background(), testToken, defaultRequest())
	if err != nil {
		t.Fatalf("Download() = %v", err)
	}
	if hits.Load() != 3 {
		t.Fatalf("attempts = %d, want 3 (2 failures then a success)", hits.Load())
	}
	if stored.SHA256 != sha256Hex(payload) {
		t.Fatalf("SHA256 = %q, want %q", stored.SHA256, sha256Hex(payload))
	}
	if got := listFiles(t, root); len(got) != 1 {
		t.Fatalf("files on disk = %v, want exactly 1", got)
	}
}

// A 4xx is a definitive answer (expired file, revoked token): retrying it
// would only burn quota.
func TestDownloadDoesNotRetryClientErrors(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	d, _ := newDownloader(t, srv, func(c *Config) { c.Retries = 3 })
	_, err := d.Download(context.Background(), testToken, defaultRequest())
	if !errors.Is(err, ErrHTTP) {
		t.Fatalf("Download() = %v, want ErrHTTP", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("attempts = %d, want 1 (no retry on 4xx)", hits.Load())
	}
}

// Neither the returned error nor the logs may contain the token, even though
// net/http embeds the full URL into *url.Error.
func TestDownloadNeverLeaksTokenInErrorsOrLogs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	// Closed right away: every attempt hits a dead transport, which is the
	// path where *url.Error surfaces.
	srv.Close()

	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	d, _ := newDownloader(t, srv, func(c *Config) {
		c.Retries = 1
		c.Logger = logger
	})

	_, err := d.Download(context.Background(), testToken, defaultRequest())
	if err == nil {
		t.Fatal("Download() should have failed")
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("error exposes the token: %q", err)
	}
	if strings.Contains(err.Error(), srv.URL) {
		t.Fatalf("error exposes the download URL: %q", err)
	}
	if strings.Contains(logs.String(), testToken) {
		t.Fatalf("logs expose the token: %q", logs.String())
	}
	if strings.Contains(logs.String(), srv.URL) {
		t.Fatalf("logs expose the download URL: %q", logs.String())
	}
	// The retry was logged: the assertion above is not vacuously true.
	if !strings.Contains(logs.String(), "media download attempt failed") {
		t.Fatalf("no attempt logged: %q", logs.String())
	}
}

// Durable resume: after a restart, downloading the same media again is a
// no-op and issues no request.
func TestDownloadIsIdempotentAcrossRestarts(t *testing.T) {
	payload := []byte("already downloaded before the restart")
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	d, root := newDownloader(t, srv, nil)
	first, err := d.Download(context.Background(), testToken, defaultRequest())
	if err != nil {
		t.Fatalf("first Download() = %v", err)
	}

	req := defaultRequest()
	req.ExpectedSHA256 = first.SHA256
	second, err := d.Download(context.Background(), testToken, req)
	if err != nil {
		t.Fatalf("second Download() = %v", err)
	}
	if !second.Reused {
		t.Fatal("second Download() should have reused the stored file")
	}
	if hits.Load() != 1 {
		t.Fatalf("server called %d times, want 1", hits.Load())
	}
	if second.Path != first.Path || second.SHA256 != first.SHA256 || second.Bytes != first.Bytes {
		t.Fatalf("second = %+v, want identical to %+v", second, first)
	}
	if got := listFiles(t, root); len(got) != 1 {
		t.Fatalf("files on disk = %v, want exactly 1", got)
	}
}

// A file left over by an older version whose hash does not match is
// replaced, not reused.
func TestDownloadReplacesStoredFileWithWrongHash(t *testing.T) {
	payload := []byte("the correct content")
	srv := serveBytes(payload)
	defer srv.Close()

	d, root := newDownloader(t, srv, nil)
	target := filepath.Join(root, "4242", "2026-09", "04", string(testUniqueID))
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(target, []byte("truncated leftover"), 0o640); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	req := defaultRequest()
	req.ExpectedSHA256 = sha256Hex(payload)
	stored, err := d.Download(context.Background(), testToken, req)
	if err != nil {
		t.Fatalf("Download() = %v", err)
	}
	if stored.Reused {
		t.Fatal("a file with the wrong hash must not be reused")
	}
	onDisk, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("reading target: %v", err)
	}
	if !bytes.Equal(onDisk, payload) {
		t.Fatalf("content = %q, want %q", onDisk, payload)
	}
}

// The final path only appears once the content is complete: during the
// transfer, no partial file is visible under that name.
func TestFinalPathAppearsOnlyAfterCompleteTransfer(t *testing.T) {
	firstChunkSent := make(chan struct{})
	release := make(chan struct{})
	payload := bytes.Repeat([]byte("z"), 2048)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("ResponseWriter is not a Flusher")
			return
		}
		_, _ = w.Write(payload[:1024])
		flusher.Flush()
		close(firstChunkSent)
		<-release
		_, _ = w.Write(payload[1024:])
	}))
	defer srv.Close()

	d, root := newDownloader(t, srv, nil)
	target := filepath.Join(root, "4242", "2026-09", "04", string(testUniqueID))

	type result struct {
		stored StoredFile
		err    error
	}
	done := make(chan result, 1)
	go func() {
		stored, err := d.Download(context.Background(), testToken, defaultRequest())
		done <- result{stored, err}
	}()

	<-firstChunkSent
	// Give the copy a chance to actually write the first chunk to the
	// temporary file before observing the directory.
	time.Sleep(50 * time.Millisecond)
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the final path exists mid-transfer: %v", err)
	}
	// What is on disk at that point is a temporary file, recognisable as
	// such and never mistakable for a storage key.
	for _, name := range listFiles(t, root) {
		if !strings.HasPrefix(filepath.Base(name), tempPrefix) {
			t.Fatalf("non-temporary file %q visible mid-transfer", name)
		}
	}

	close(release)
	res := <-done
	if res.err != nil {
		t.Fatalf("Download() = %v", res.err)
	}
	onDisk, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("reading target: %v", err)
	}
	if !bytes.Equal(onDisk, payload) {
		t.Fatalf("content truncated: %d bytes, want %d", len(onDisk), len(payload))
	}
	if got := listFiles(t, root); len(got) != 1 {
		t.Fatalf("files on disk = %v, want exactly 1 (no temp leftover)", got)
	}
}

func TestVerify(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file")
	payload := []byte("content to verify")
	if err := os.WriteFile(path, payload, 0o640); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := Verify(path, sha256Hex(payload)); err != nil {
		t.Fatalf("Verify() = %v, want nil", err)
	}
	if err := Verify(path, strings.ToUpper(sha256Hex(payload))); err != nil {
		t.Fatalf("Verify() should accept an uppercase hex digest: %v", err)
	}
	if err := Verify(path, sha256Hex([]byte("other"))); !errors.Is(err, ErrHashMismatch) {
		t.Fatalf("Verify() = %v, want ErrHashMismatch", err)
	}
	if err := Verify(path, ""); err == nil {
		t.Fatal("Verify() with an empty digest should fail")
	}
	if err := Verify(filepath.Join(dir, "missing"), sha256Hex(payload)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Verify() on a missing file = %v, want ErrNotExist", err)
	}
}

func TestNewRequiresBaseDirAndAppliesDefaults(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("New() without BaseDir should fail")
	}
	d, err := New(Config{BaseDir: t.TempDir()})
	if err != nil {
		t.Fatalf("New() = %v", err)
	}
	if d.cfg.MaxBytes != defaultMaxBytes || d.cfg.RequestTimeout != defaultRequestTimeout {
		t.Fatalf("defaults not applied: %+v", d.cfg)
	}
	if d.cfg.BaseURL != defaultBaseURL || d.cfg.HTTPClient == nil || d.cfg.Now == nil || d.cfg.Logger == nil {
		t.Fatalf("defaults not applied: %+v", d.cfg)
	}
}

func TestDownloadRequiresOwnerAndUniqueID(t *testing.T) {
	srv := serveBytes([]byte("payload"))
	defer srv.Close()
	d, _ := newDownloader(t, srv, nil)

	req := defaultRequest()
	req.OwnerUserID = 0
	if _, err := d.Download(context.Background(), testToken, req); err == nil {
		t.Fatal("Download() without OwnerUserID should fail")
	}

	req = defaultRequest()
	req.UniqueID = "  "
	if _, err := d.Download(context.Background(), testToken, req); err == nil {
		t.Fatal("Download() without UniqueID should fail")
	}
}

func TestDownloadHonoursContextCancellation(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		_, _ = io.WriteString(w, "too late")
	}))
	defer srv.Close()
	defer close(release)

	d, root := newDownloader(t, srv, nil)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()

	_, err := d.Download(ctx, testToken, defaultRequest())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Download() = %v, want context.Canceled", err)
	}
	if got := listFiles(t, root); len(got) != 0 {
		t.Fatalf("files left behind after cancellation: %v", got)
	}
}
