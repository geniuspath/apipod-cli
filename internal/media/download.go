package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DefaultMaxDownloadBytes bounds a single downloaded result.
const DefaultMaxDownloadBytes int64 = 2 << 30 // 2 GiB

const maxRedirects = 5

// Downloader saves generated results to disk.
//
// It owns its own HTTP client, separate from the API client, and never sets
// an Authorization header: result URLs point at object storage, which must
// not see the user's credential.
type Downloader struct {
	client   *http.Client
	MaxBytes int64
	Force    bool // overwrite existing files
}

// NewDownloader returns a Downloader with safe defaults.
func NewDownloader() *Downloader {
	return NewDownloaderWithClient(&http.Client{})
}

// NewDownloaderWithClient wraps base (tests pass a TLS test client). The
// redirect policy is always replaced with the https-only one.
func NewDownloaderWithClient(base *http.Client) *Downloader {
	c := *base
	c.Jar = nil
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirects {
			return errors.New("too many redirects")
		}
		if req.URL.Scheme != "https" {
			return fmt.Errorf("refusing redirect to non-https URL")
		}
		return nil
	}
	return &Downloader{client: &c, MaxBytes: DefaultMaxDownloadBytes}
}

// Save downloads rawURL into dir as "<jobID>-<index>.<ext>" and returns the
// path. The name is built by the CLI: nothing from the URL or the response
// headers is used, so the server cannot choose where the file lands. The
// extension comes from sniffing the content against the media allow-list.
func (d *Downloader) Save(ctx context.Context, rawURL, dir, jobID string, index int) (string, error) {
	resp, err := d.open(ctx, rawURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	// Write to a temp file in the destination directory, then rename: a
	// reader never sees a partial file and a failed download leaves nothing.
	tmp, err := os.CreateTemp(dir, ".apipod-*.part")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	done := false
	defer func() {
		tmp.Close()
		if !done {
			os.Remove(tmpName)
		}
	}()

	header := make([]byte, sniffBytes)
	n, err := io.ReadFull(resp.Body, header)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("download failed: %w", err)
	}
	header = header[:n]

	final := filepath.Join(dir, fmt.Sprintf("%s-%d.%s", safeName(jobID), index, resultExt(header)))
	release, err := d.reserve(final)
	if err != nil {
		return "", err
	}
	defer func() {
		if !done {
			release()
		}
	}()

	if err := d.writeBody(tmp, header, resp.Body); err != nil {
		return "", err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmpName, final); err != nil {
		return "", err
	}
	done = true
	return final, nil
}

// open validates the URL and starts the download. Only https is fetched, and
// the request carries no credential.
func (d *Downloader) open(ctx context.Context, rawURL string) (*http.Response, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("invalid result URL")
	}
	if u.Scheme != "https" {
		return nil, fmt.Errorf("refusing to download over %q: only https result URLs are fetched", u.Scheme)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download failed: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("download failed: HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > d.MaxBytes {
		resp.Body.Close()
		return nil, fmt.Errorf("result is larger than the %d MB download limit", d.MaxBytes/mb)
	}
	return resp, nil
}

// reserve claims the destination name before the body is fetched. Without
// Force it creates the file with O_EXCL, so an existing file — or one created
// by a concurrent run — is never replaced, and a large result is not
// downloaded only to be refused. The returned func removes the placeholder.
func (d *Downloader) reserve(final string) (release func(), err error) {
	if d.Force {
		return func() {}, nil
	}
	placeholder, err := os.OpenFile(final, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("%s already exists (use --force to overwrite)", final)
		}
		return nil, err
	}
	placeholder.Close()
	return func() { os.Remove(final) }, nil
}

// writeBody writes the sniffed header and the rest of the body to tmp,
// enforcing the size limit, and flushes it to disk.
func (d *Downloader) writeBody(tmp *os.File, header []byte, body io.Reader) error {
	if _, err := tmp.Write(header); err != nil {
		return err
	}
	// Read one byte past the limit to tell "exactly at the limit" from "over".
	written, err := io.Copy(tmp, io.LimitReader(body, d.MaxBytes-int64(len(header))+1))
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}
	if int64(len(header))+written > d.MaxBytes {
		return fmt.Errorf("result is larger than the %d MB download limit", d.MaxBytes/mb)
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	return tmp.Close()
}

// resultExt picks the extension from the content; unknown content gets .bin
// so it is still saved (the user paid for it) but never under a name the OS
// would treat as a document or program.
func resultExt(header []byte) string {
	if p, ok := lookupPolicy(header); ok {
		return p.ext
	}
	return "bin"
}

// safeName reduces a job id to [A-Za-z0-9_-], so it can never contain a path
// separator, a leading dot, or a Windows reserved character.
func safeName(id string) string {
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
		if b.Len() >= 80 {
			break
		}
	}
	name := strings.TrimLeft(b.String(), "-")
	if name == "" {
		return "job"
	}
	return name
}

// DownloadTimeout bounds one result download.
const DownloadTimeout = 15 * time.Minute
