package api

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"time"
)

// Upload is the response of POST /v1/uploads.
type Upload struct {
	URL         string `json:"url"`
	Type        string `json:"type"`
	ContentType string `json:"content_type"`
	Bytes       int64  `json:"bytes"`
}

// UploadSource opens a fresh reader over the file for each attempt.
type UploadSource func() (io.ReadCloser, error)

var quoteEscaper = strings.NewReplacer("\\", "\\\\", `"`, "\\\"", "\r", "", "\n", "")

// UploadFile streams one file to POST /v1/uploads as the multipart field
// "file". filename must already be a bare name: the caller passes the
// basename so local directory structure never leaves the machine. size is the
// exact number of bytes open will yield.
//
// Only 429 is retried. An upload has no idempotency key, and a 429 is the one
// failure where the server states it did not store anything.
func (c *Client) UploadFile(ctx context.Context, filename, contentType string, size int64, open UploadSource) (Upload, error) {
	wait := c.retryWait
	for attempt := 1; ; attempt++ {
		out, err := c.uploadOnce(ctx, filename, contentType, size, open)
		if err == nil {
			return out, nil
		}
		var apiErr *Error
		if attempt >= maxAttempts || !asAPIError(err, &apiErr) || apiErr.HTTPStatus != http.StatusTooManyRequests {
			return Upload{}, err
		}
		delay := wait
		if ra := parseRetryAfter(apiErr.RetryAfter); ra > delay {
			delay = ra
		}
		c.Debug.Logf("upload attempt %d rate limited; retrying in %s", attempt, delay)
		select {
		case <-ctx.Done():
			return Upload{}, ctx.Err()
		case <-time.After(delay):
		}
		wait *= 2
	}
}

func (c *Client) uploadOnce(ctx context.Context, filename, contentType string, size int64, open UploadSource) (Upload, error) {
	src, err := open()
	if err != nil {
		return Upload{}, err
	}
	defer src.Close()

	// The body is head + file + tail, streamed: a 100 MB video is never held
	// in memory, and the exact Content-Length is known up front (some
	// proxies refuse chunked uploads).
	var head bytes.Buffer
	mw := multipart.NewWriter(&head)
	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, quoteEscaper.Replace(filename)))
	header.Set("Content-Type", contentType)
	if _, err := mw.CreatePart(header); err != nil {
		return Upload{}, err
	}
	tail := "\r\n--" + mw.Boundary() + "--\r\n"

	req, err := c.newRequest(ctx, http.MethodPost, "/v1/uploads", io.MultiReader(&head, src, strings.NewReader(tail)))
	if err != nil {
		return Upload{}, err
	}
	req.ContentLength = int64(head.Len()) + size + int64(len(tail))
	req.Header.Set("Content-Type", mw.FormDataContentType())
	c.Debug.logRequest(req, nil)

	var out Upload
	err = c.exec(c.uploadHTTP, req, &out)
	return out, err
}
