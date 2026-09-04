// Package store downloads Telegram media to the local filesystem and stores
// it so that a crash can never leave a half-written file behind.
//
// # Ordering: temp -> hash -> rename
//
// Every download follows the exact same three steps:
//
//  1. the bytes are streamed into a temporary file created in the SAME
//     directory as the final target (same filesystem, so the final rename is
//     a metadata-only operation);
//  2. the SHA-256 is computed on the fly, while writing, and verified once
//     the stream is complete (plus fsync of the temporary file);
//  3. the temporary file is renamed onto its final path with os.Rename,
//     which is atomic on POSIX, then the directory is fsynced.
//
// Crash-safety follows from that ordering: a process killed at any point
// leaves either nothing at all, or a leftover temporary file (prefixed with
// TempPrefix, never a valid media path), but NEVER a final path holding
// truncated or corrupted content. A reader that sees the final path can
// therefore assume the content is complete and matches its hash, without any
// lock or journal. Conversely, a hash mismatch or a size overrun aborts
// before step 3, so the final path is never created.
//
// # Server-generated paths
//
// The storage path is built exclusively from data the server controls:
// baseDir/<ownerUserID>/<yyyy-mm>/<dd>/<storage key>. Neither Telegram's
// file_path nor any client-provided filename ever reaches the filesystem —
// they are attacker-influenced values, and using them would open a path
// traversal. The storage key derives from file_unique_id, and falls back to
// its SHA-256 as soon as that identifier is not a plain safe token. The
// containment check against the root is kept anyway as defence in depth.
//
// # Secrets
//
// The bot token only ever exists in the download URL built in memory. It is
// never logged and never returned inside an error: transport errors from
// net/http are unwrapped (*url.Error carries the full URL, token included)
// and every message goes through redact before leaving the package. Nor does
// it leave over the network: redirects are refused, because net/http would
// forward the origin URL — token in its path — in the Referer header of the
// redirected request.
package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// defaultBaseURL is the Bot API file endpoint prefix, token excluded.
const defaultBaseURL = "https://api.telegram.org/file/bot"

// TempPrefix marks in-flight downloads. It starts with a dot and can never
// collide with a storage key, which is validated as a safe token.
const TempPrefix = ".dl-"

// Sentinel errors. Callers match them with errors.Is; the wrapping message
// never contains the bot token nor the download URL.
var (
	// ErrTooLarge means the response exceeded Config.MaxBytes. The download
	// is aborted mid-stream, nothing is kept.
	ErrTooLarge = errors.New("media: file too large")
	// ErrTimeout means an attempt exceeded Config.RequestTimeout.
	ErrTimeout = errors.New("media: download timed out")
	// ErrHashMismatch means the downloaded bytes do not match the expected
	// SHA-256. The final path is never created.
	ErrHashMismatch = errors.New("media: sha256 mismatch")
	// ErrPathTraversal means a path escaping the storage root, or a Telegram
	// file_path that is not a relative path, was rejected.
	ErrPathTraversal = errors.New("media: rejected path")
	// ErrHTTP means the Bot API answered with a non-2xx status that retries
	// could not clear.
	ErrHTTP = errors.New("media: unexpected http status")
)

// UniqueID is Telegram's file_unique_id: stable for a given file across
// bots, unlike file_id. It is the natural storage key, which makes the
// target path content-addressed and downloads idempotent.
type UniqueID string

// Config holds the download and storage settings.
type Config struct {
	// BaseDir is the storage root ("media" in production). Required.
	BaseDir string
	// HTTPClient is injectable so tests can point at an httptest server and
	// so production can use a transport dedicated to downloads (long
	// transfers must not share the poller's long-polling client).
	HTTPClient *http.Client
	// BaseURL replaces the Bot API file endpoint prefix (token excluded).
	// Only tests set it.
	BaseURL string
	// MaxBytes caps the downloaded size. Exceeding it aborts with
	// ErrTooLarge, which also bounds the damage a full disk can do.
	MaxBytes int64
	// RequestTimeout bounds a single attempt, body transfer included.
	RequestTimeout time.Duration
	// Retries is the number of RETRIES after the first attempt. Only network
	// errors and 5xx are retried; a 4xx is a definitive answer.
	Retries int
	// Backoff is the initial delay between attempts, doubled every time.
	Backoff time.Duration
	// Logger receives the download traces. Never the token nor the URL.
	Logger *slog.Logger
	// Now is injectable so tests get a deterministic dated path.
	Now func() time.Time
}

// noRedirect returns a copy of client that never follows a redirect. The
// token travels in the URL PATH, and net/http fills the Referer of the
// redirected request with the origin URL — a single 302 would hand the secret
// to a third-party host. The file endpoint serves the content directly, so a
// redirect is either a misconfiguration or an attack: the 3xx is surfaced as
// ErrHTTP instead, and no second request is ever issued.
//
// The copy is shallow (Transport and Jar are shared, which is what we want)
// and leaves the caller's client untouched.
func noRedirect(client *http.Client) *http.Client {
	copied := *client
	copied.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &copied
}

// Downloader fetches media from the Bot API and stores it atomically.
type Downloader struct {
	cfg Config
}

// Request describes one media to fetch.
//
// FilePath comes from getFile and is used ONLY to build the download URL;
// it never contributes to the storage path (cf. package comment).
type Request struct {
	// OwnerUserID scopes the storage tree per tenant.
	OwnerUserID int64
	// FileID is the identifier passed to getFile; kept for logs.
	FileID string
	// FilePath is the path returned by getFile, relative to the file
	// endpoint. Attacker-influenced: validated, never stored.
	FilePath string
	// UniqueID is the file_unique_id, used as the storage key.
	UniqueID UniqueID
	// ExpectedSHA256 is optional (lowercase hex). When set, the download is
	// rejected with ErrHashMismatch if the content differs, and an already
	// stored file is reused only if it matches.
	ExpectedSHA256 string
}

// StoredFile describes a media present on disk at the end of Download.
type StoredFile struct {
	// Path is the full path on disk, inside Config.BaseDir.
	Path string
	// RelPath is the path relative to BaseDir, the form to persist in
	// database (it survives moving the storage root).
	RelPath string
	// SHA256 is the lowercase hex digest of the stored content.
	SHA256 string
	// Bytes is the size on disk.
	Bytes int64
	// Reused reports that the file was already there and no network call was
	// made — the idempotent path after a restart.
	Reused bool
}

const (
	defaultMaxBytes       = 20 << 20 // Bot API caps getFile downloads at 20 MiB.
	defaultRequestTimeout = 60 * time.Second
	defaultRetries        = 2
	defaultBackoff        = 500 * time.Millisecond
)

// New validates the configuration and applies the defaults.
func New(cfg Config) (*Downloader, error) {
	if strings.TrimSpace(cfg.BaseDir) == "" {
		return nil, errors.New("media: BaseDir is required")
	}
	if cfg.HTTPClient == nil {
		// No Timeout here: each attempt carries its own context deadline,
		// which also covers the body transfer.
		cfg.HTTPClient = &http.Client{}
	}
	cfg.HTTPClient = noRedirect(cfg.HTTPClient)
	if cfg.BaseURL == "" {
		cfg.BaseURL = defaultBaseURL
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = defaultMaxBytes
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = defaultRequestTimeout
	}
	if cfg.Retries < 0 {
		cfg.Retries = defaultRetries
	}
	if cfg.Backoff <= 0 {
		cfg.Backoff = defaultBackoff
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(discardHandler{})
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Downloader{cfg: cfg}, nil
}

// Download fetches the media and stores it atomically, then returns its
// location and hash.
//
// It is idempotent: if the target path already exists (and matches
// ExpectedSHA256 when provided), no request is made and StoredFile.Reused is
// true. That is what makes a restart mid-batch safe to replay.
//
// Caveat on that replay: the target path embeds the CURRENT UTC date (cf.
// relPath), so a replay that crosses midnight recomputes a different path,
// finds nothing there and downloads a second copy, orphaning the first. A
// caller that persists StoredFile.RelPath should therefore resume through
// Verify on that stored path rather than by calling Download again.
func (d *Downloader) Download(ctx context.Context, token string, req Request) (StoredFile, error) {
	rel, err := d.relPath(req)
	if err != nil {
		return StoredFile{}, err
	}
	full, err := d.resolve(rel)
	if err != nil {
		return StoredFile{}, err
	}
	// Validated before any I/O: a file_path that is not a plain relative
	// path means either a compromised API or a malicious proxy.
	if err := validateFilePath(req.FilePath); err != nil {
		return StoredFile{}, err
	}

	if stored, ok, err := d.reuse(full, rel, req.ExpectedSHA256); err != nil {
		return StoredFile{}, err
	} else if ok {
		d.cfg.Logger.Debug("media already stored",
			"file_id", req.FileID, "unique_id", string(req.UniqueID))
		return stored, nil
	}

	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		return StoredFile{}, fmt.Errorf("media: creating directory: %w", redactErr(err, token))
	}

	sum, size, err := d.fetchInto(ctx, token, full, req)
	if err != nil {
		return StoredFile{}, err
	}
	return StoredFile{Path: full, RelPath: rel, SHA256: sum, Bytes: size}, nil
}

// reuse reports whether the final path already holds usable content.
func (d *Downloader) reuse(full, rel, wantSHA string) (StoredFile, bool, error) {
	// Lstat, not Stat: a symlink planted at the storage path would otherwise
	// be dereferenced, and the file it points at — anywhere on the filesystem
	// — hashed and returned as if it were the stored media. The containment
	// check in resolve works on strings and cannot see that. Anything that is
	// not a plain regular file is never legitimate here, so it is refused
	// loudly rather than silently overwritten.
	info, err := os.Lstat(full)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return StoredFile{}, false, nil
	case err != nil:
		return StoredFile{}, false, fmt.Errorf("media: inspecting target: %w", err)
	case !info.Mode().IsRegular():
		return StoredFile{}, false, fmt.Errorf(
			"%w: target is not a regular file (%s)", ErrPathTraversal, info.Mode().Type())
	}

	sum, err := sha256File(full)
	if err != nil {
		return StoredFile{}, false, fmt.Errorf("media: hashing stored file: %w", err)
	}
	// A leftover from an older version whose hash does not match is treated
	// as absent: the download below will atomically replace it.
	if wantSHA != "" && !strings.EqualFold(sum, wantSHA) {
		return StoredFile{}, false, nil
	}
	return StoredFile{
		Path: full, RelPath: rel, SHA256: sum, Bytes: info.Size(), Reused: true,
	}, true, nil
}

// fetchInto runs the attempts and, on success, leaves the content at full.
func (d *Downloader) fetchInto(ctx context.Context, token, full string, req Request) (string, int64, error) {
	backoff := d.cfg.Backoff
	var lastErr error
	for attempt := 0; attempt <= d.cfg.Retries; attempt++ {
		if attempt > 0 {
			timer := time.NewTimer(backoff)
			select {
			case <-ctx.Done():
				timer.Stop()
				return "", 0, ctx.Err()
			case <-timer.C:
			}
			backoff *= 2
		}

		sum, size, err := d.attempt(ctx, token, full, req)
		if err == nil {
			d.cfg.Logger.Info("media stored",
				"file_id", req.FileID, "unique_id", string(req.UniqueID),
				"bytes", size, "attempts", attempt+1)
			return sum, size, nil
		}
		lastErr = err
		if !retryable(err) {
			return "", 0, err
		}
		d.cfg.Logger.Warn("media download attempt failed",
			"file_id", req.FileID, "unique_id", string(req.UniqueID),
			"attempt", attempt+1, "error", err.Error())
	}
	return "", 0, lastErr
}

// attempt performs one request and the whole temp -> hash -> rename sequence.
func (d *Downloader) attempt(ctx context.Context, token, full string, req Request) (string, int64, error) {
	reqCtx, cancel := context.WithTimeout(ctx, d.cfg.RequestTimeout)
	defer cancel()

	// The token only exists here, in memory, and never leaves this function.
	endpoint := d.cfg.BaseURL + token + "/" + req.FilePath
	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", 0, fmt.Errorf("media: building request: %w", redactErr(err, token))
	}

	resp, err := d.cfg.HTTPClient.Do(httpReq)
	if err != nil {
		if netErr := d.transportError(ctx, reqCtx, err, token); netErr != nil {
			return "", 0, netErr
		}
		// Do() only fails on transport errors; the fallback keeps the
		// function total rather than returning a nil error with no result.
		return "", 0, fmt.Errorf("media: transport error: %w", redactErr(err, token))
	}
	defer func() {
		// Drain a bounded amount so the connection can be reused; the body is
		// closed either way.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return "", 0, &httpError{status: resp.StatusCode}
	}
	// Cheap pre-check: avoids opening a temporary file for a download we
	// already know we will refuse. The streaming counter below stays the
	// authority, Content-Length being optional and untrusted.
	if resp.ContentLength > 0 && resp.ContentLength > d.cfg.MaxBytes {
		return "", 0, fmt.Errorf("%w: %d bytes announced, limit %d",
			ErrTooLarge, resp.ContentLength, d.cfg.MaxBytes)
	}

	return d.writeAtomically(ctx, reqCtx, full, resp.Body, req.ExpectedSHA256, token)
}

// writeAtomically implements steps 1 to 3 of the package comment.
func (d *Downloader) writeAtomically(ctx, reqCtx context.Context, full string, body io.Reader, wantSHA, token string) (string, int64, error) {
	dir := filepath.Dir(full)
	// Same directory as the target, hence the same filesystem: os.Rename
	// stays atomic (a cross-device rename would fail with EXDEV).
	tmp, err := os.CreateTemp(dir, TempPrefix+"*")
	if err != nil {
		return "", 0, fmt.Errorf("media: creating temp file: %w", redactErr(err, token))
	}
	tmpName := tmp.Name()
	// Both are no-ops once the rename succeeded and tmp is closed.
	defer func() {
		tmp.Close()
		os.Remove(tmpName)
	}()

	hasher := sha256.New()
	// MaxBytes+1 so that hitting the limit exactly is legal while one extra
	// byte is detected; the stream is cut instead of filling the disk.
	limited := io.LimitReader(body, d.cfg.MaxBytes+1)
	size, err := io.Copy(io.MultiWriter(tmp, hasher), limited)
	if err != nil {
		if netErr := d.transportError(ctx, reqCtx, err, token); netErr != nil {
			return "", 0, netErr
		}
		return "", 0, fmt.Errorf("media: writing file: %w", redactErr(err, token))
	}
	if size > d.cfg.MaxBytes {
		return "", 0, fmt.Errorf("%w: over %d bytes", ErrTooLarge, d.cfg.MaxBytes)
	}

	sum := hex.EncodeToString(hasher.Sum(nil))
	if wantSHA != "" && !strings.EqualFold(sum, wantSHA) {
		// Return before the rename: the final path is never created, so no
		// caller can ever observe corrupted content.
		return "", 0, fmt.Errorf("%w: got %s", ErrHashMismatch, sum)
	}

	// Flush before the rename: without it a crash could publish the name
	// while the content is still only in the page cache.
	if err := tmp.Sync(); err != nil {
		return "", 0, fmt.Errorf("media: syncing temp file: %w", redactErr(err, token))
	}
	if err := tmp.Close(); err != nil {
		return "", 0, fmt.Errorf("media: closing temp file: %w", redactErr(err, token))
	}
	if err := os.Chmod(tmpName, 0o640); err != nil {
		return "", 0, fmt.Errorf("media: adjusting permissions: %w", redactErr(err, token))
	}
	if err := os.Rename(tmpName, full); err != nil {
		return "", 0, fmt.Errorf("media: publishing file: %w", redactErr(err, token))
	}
	syncDir(dir)
	return sum, size, nil
}

// transportError converts a net/http error, never exposing the URL (which
// carries the token). Returns nil when err is not a transport error.
func (d *Downloader) transportError(ctx, reqCtx context.Context, err error, token string) error {
	// Cancelling the parent takes precedence: it is the caller's decision,
	// not a timeout of ours.
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(reqCtx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w after %s", ErrTimeout, d.cfg.RequestTimeout)
	}
	// *url.Error stringifies the full URL, token included: only the cause is
	// kept (same reasoning as telegram.Client.call).
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return fmt.Errorf("media: transport error: %w", redactErr(urlErr.Err, token))
	}
	if isNetworkError(err) {
		return fmt.Errorf("media: transport error: %w", redactErr(err, token))
	}
	return nil
}

// Verify recomputes the SHA-256 of a stored file and compares it to wantSHA.
// Used at restart to confirm that a file referenced in database is intact
// before considering the download done.
func Verify(path string, wantSHA string) error {
	if wantSHA == "" {
		return errors.New("media: expected sha256 is empty")
	}
	sum, err := sha256File(path)
	if err != nil {
		return err
	}
	if !strings.EqualFold(sum, wantSHA) {
		return fmt.Errorf("%w: got %s, want %s", ErrHashMismatch, sum, strings.ToLower(wantSHA))
	}
	return nil
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// relPath builds the server-generated storage path. It never reads
// req.FilePath.
func (d *Downloader) relPath(req Request) (string, error) {
	if req.OwnerUserID == 0 {
		return "", errors.New("media: OwnerUserID is required")
	}
	if strings.TrimSpace(string(req.UniqueID)) == "" {
		return "", errors.New("media: UniqueID is required")
	}
	now := d.cfg.Now().UTC()
	return filepath.Join(
		strconv.FormatInt(req.OwnerUserID, 10),
		now.Format("2006-01"),
		now.Format("02"),
		storageKey(req.UniqueID),
	), nil
}

// storageKey turns a file_unique_id into a filename. Telegram documents an
// opaque token, but nothing guarantees it: anything outside [A-Za-z0-9_-] is
// replaced by its SHA-256, which is safe by construction.
func storageKey(id UniqueID) string {
	safe := len(id) > 0 && len(id) <= 128
	if safe {
		for i := 0; i < len(id); i++ {
			c := id[i]
			switch {
			case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
			default:
				safe = false
			}
		}
	}
	if safe {
		return string(id)
	}
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])
}

// resolve joins rel onto the root and checks containment. Generation is
// already safe; this check is the defence in depth that catches a future
// regression in relPath.
func (d *Downloader) resolve(rel string) (string, error) {
	if rel == "" || filepath.IsAbs(rel) || strings.Contains(rel, "..") {
		return "", fmt.Errorf("%w: %q is not a relative storage path", ErrPathTraversal, rel)
	}
	root, err := filepath.Abs(d.cfg.BaseDir)
	if err != nil {
		return "", fmt.Errorf("media: resolving root: %w", err)
	}
	full := filepath.Join(root, rel)
	if full != root && !strings.HasPrefix(full, root+string(os.PathSeparator)) {
		return "", fmt.Errorf("%w: target outside the storage root", ErrPathTraversal)
	}
	return full, nil
}

// validateFilePath refuses a Telegram file_path that is anything other than
// a plain relative path. It never reaches the filesystem, but a "../.." or
// an absolute URL there would let the API point the download somewhere else.
func validateFilePath(p string) error {
	if strings.TrimSpace(p) == "" {
		return fmt.Errorf("%w: empty file_path", ErrPathTraversal)
	}
	if strings.HasPrefix(p, "/") || filepath.IsAbs(p) {
		return fmt.Errorf("%w: absolute file_path", ErrPathTraversal)
	}
	if strings.Contains(p, "..") || strings.Contains(p, "\\") || strings.Contains(p, "://") {
		return fmt.Errorf("%w: suspicious file_path", ErrPathTraversal)
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: control character in file_path", ErrPathTraversal)
		}
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." {
			return fmt.Errorf("%w: empty segment in file_path", ErrPathTraversal)
		}
	}
	return nil
}

// httpError carries the status code alongside the ErrHTTP sentinel, so the
// retry decision reads a field instead of re-parsing the message it just
// formatted. errors.Is(err, ErrHTTP) keeps working for callers.
type httpError struct{ status int }

func (e *httpError) Error() string { return fmt.Sprintf("%s: %d", ErrHTTP.Error(), e.status) }

func (e *httpError) Unwrap() error { return ErrHTTP }

// retryable: only network glitches and the statuses that can clear on their
// own are worth another attempt. A 404 or a 401 (file expired, revoked token)
// would return the same thing forever; a 429 or a 408, on the contrary, is
// exactly what an exponential backoff is for — and giving up on it loses the
// media for good, since file_path expires one hour after getFile.
func retryable(err error) bool {
	switch {
	case errors.Is(err, ErrPathTraversal), errors.Is(err, ErrHashMismatch), errors.Is(err, ErrTooLarge):
		return false
	case errors.Is(err, context.Canceled):
		return false
	case errors.Is(err, ErrTimeout):
		return true
	}
	var httpErr *httpError
	if errors.As(err, &httpErr) {
		return httpErr.status >= 500 ||
			httpErr.status == http.StatusTooManyRequests ||
			httpErr.status == http.StatusRequestTimeout
	}
	return isNetworkError(err) || strings.Contains(err.Error(), "transport error")
}

func isNetworkError(err error) bool {
	var netErr interface{ Timeout() bool }
	if errors.As(err, &netErr) {
		return true
	}
	return errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF)
}

// redactErr rebuilds an error without the token, for the residual cases where
// a message could embed the download URL.
func redactErr(err error, token string) error {
	if err == nil {
		return nil
	}
	msg := redact(err.Error(), token)
	if msg == err.Error() {
		return err
	}
	return errors.New(msg)
}

// redact replaces the token with a fixed marker. Last safety net: nothing in
// this package is supposed to put it into a string in the first place.
func redact(s, token string) string {
	if token == "" {
		return s
	}
	return strings.ReplaceAll(s, token, "[REDACTED]")
}

// syncDir persists the rename itself. Best effort: a filesystem that refuses
// to open the directory must not fail an otherwise successful download.
func syncDir(dir string) {
	f, err := os.Open(dir)
	if err != nil {
		return
	}
	defer f.Close()
	_ = f.Sync()
}

// discardHandler is the default logger: silent, so that Config.Logger stays
// optional without a nil check on every call site.
type discardHandler struct{}

func (discardHandler) Enabled(context.Context, slog.Level) bool  { return false }
func (discardHandler) Handle(context.Context, slog.Record) error { return nil }
func (h discardHandler) WithAttrs([]slog.Attr) slog.Handler      { return h }
func (h discardHandler) WithGroup(string) slog.Handler           { return h }
