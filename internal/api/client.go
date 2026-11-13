// Package api is the HTTP client for the APIPod API.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Version is the CLI version, set from main at startup.
var Version = "dev"

// UserAgent identifies the CLI to the server (used for client attribution).
func UserAgent() string { return "apipod-cli/" + Version }

// TokenSource supplies the Authorization header value for the client's base
// URL. auth.Session implements it.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

const (
	maxAttempts   = 4
	maxRetryAfter = 30 * time.Second
	maxErrorBody  = 64 << 10
	maxJSONBody   = 32 << 20
)

// Client talks to exactly one base URL. The credential in Auth was resolved
// for that base URL and is attached only to requests built here; redirects
// are not followed, so it cannot be carried to another host.
type Client struct {
	BaseURL string
	Auth    TokenSource // nil for public endpoints
	Debug   *DebugLogger

	http       *http.Client
	uploadHTTP *http.Client
	retryWait  time.Duration
}

// New builds a client for a normalized base URL.
func New(baseURL string, auth TokenSource, debug *DebugLogger) *Client {
	noRedirect := func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{
		BaseURL: baseURL,
		Auth:    auth,
		Debug:   debug,
		http:    &http.Client{Timeout: 60 * time.Second, CheckRedirect: noRedirect},
		// Uploads can be 100 MB; they are bounded by the command context
		// instead of a fixed client timeout.
		uploadHTTP: &http.Client{CheckRedirect: noRedirect},
		retryWait:  time.Second,
	}
}

// SetRetryWait overrides the base backoff (tests).
func (c *Client) SetRetryWait(d time.Duration) { c.retryWait = d }

// call describes one logical request.
type call struct {
	method string
	path   string // path plus optional query string
	body   any    // JSON-encoded when non-nil
	// idempotent adds an Idempotency-Key that stays the same across retries.
	idempotent bool
	// readOnly marks a POST that changes nothing server-side.
	readOnly bool
}

func (c call) replaySafe() bool {
	return c.method == http.MethodGet || c.method == http.MethodDelete || c.idempotent || c.readOnly
}

// do runs a call with retries and decodes the `data` field of the standard
// envelope into out (out may be nil).
func (c *Client) do(ctx context.Context, cl call, out any) error {
	var payload []byte
	if cl.body != nil {
		var err error
		if payload, err = json.Marshal(cl.body); err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
	}
	// One key per logical request, reused by every attempt: after an
	// ambiguous failure the server may already have created the task, and
	// only the same key lets it replay that result instead of billing twice.
	idempotencyKey := ""
	if cl.idempotent {
		idempotencyKey = uuid.NewString()
	}

	wait := c.retryWait
	for attempt := 1; ; attempt++ {
		err := c.attempt(ctx, cl, payload, idempotencyKey, out)
		if err == nil {
			return nil
		}
		if attempt >= maxAttempts || !retryable(err, cl.replaySafe()) {
			return err
		}
		delay := wait
		var apiErr *Error
		if errors.As(err, &apiErr) {
			if ra := parseRetryAfter(apiErr.RetryAfter); ra > delay {
				delay = ra
			}
		}
		c.Debug.Logf("attempt %d failed (%v); retrying in %s", attempt, err, delay)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		wait *= 2
	}
}

func (c *Client) attempt(ctx context.Context, cl call, payload []byte, idempotencyKey string, out any) error {
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := c.newRequest(ctx, cl.method, cl.path, body)
	if err != nil {
		return err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	c.Debug.logRequest(req, payload)
	return c.exec(c.http, req, out)
}

// newRequest builds a request against the client's own base URL and attaches
// the credential. It is the only place Authorization is ever set.
func (c *Client) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", UserAgent())
	if c.Auth != nil {
		header, err := c.Auth.Token(ctx)
		if err != nil {
			return nil, err
		}
		c.Debug.AddSecret(strings.TrimPrefix(header, "Bearer "))
		req.Header.Set("Authorization", header)
	}
	return req, nil
}

// exec sends req and decodes the response.
func (c *Client) exec(hc *http.Client, req *http.Request, out any) error {
	path := req.URL.Path
	resp, err := hc.Do(req)
	if err != nil {
		if ctxErr := req.Context().Err(); ctxErr != nil {
			return ctxErr
		}
		return &TransportError{Method: req.Method, Path: path, Err: err}
	}
	defer resp.Body.Close()
	c.Debug.logResponse(resp)

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return parseError(req.Method, path, resp)
	}
	if out == nil {
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxJSONBody))
	if err != nil {
		return &TransportError{Method: req.Method, Path: path, Err: err}
	}
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return fmt.Errorf("decode response from %s: %w", path, err)
	}
	if len(env.Data) == 0 || string(env.Data) == "null" {
		return nil
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return fmt.Errorf("decode response from %s: %w", path, err)
	}
	return nil
}

// parseError understands the three documented error envelopes: the standard
// {code,message,data:{error_code}}, the OpenAI-style {error:{message,code}},
// and the OAuth {error,error_description}.
func parseError(method, path string, resp *http.Response) *Error {
	e := &Error{
		Method:     method,
		Path:       path,
		HTTPStatus: resp.StatusCode,
		RequestID:  resp.Header.Get("X-Request-ID"),
		RetryAfter: resp.Header.Get("Retry-After"),
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	var env struct {
		Message string          `json:"message"`
		Error   json.RawMessage `json:"error"`
		Desc    string          `json:"error_description"`
		Data    json.RawMessage `json:"data"`
	}
	if json.Unmarshal(body, &env) != nil {
		return e
	}
	e.Message = env.Message
	var meta struct {
		ErrorCode string `json:"error_code"`
	}
	if json.Unmarshal(env.Data, &meta) == nil {
		e.Code = meta.ErrorCode
	}
	var openAI struct {
		Message string `json:"message"`
		Code    string `json:"code"`
	}
	var oauthCode string
	switch {
	case json.Unmarshal(env.Error, &openAI) == nil && openAI.Message != "":
		e.Message, e.Code = openAI.Message, openAI.Code
	case json.Unmarshal(env.Error, &oauthCode) == nil && oauthCode != "":
		e.Code, e.Message = oauthCode, env.Desc
	}
	return e
}

func parseRetryAfter(v string) time.Duration {
	secs, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || secs <= 0 {
		return 0
	}
	d := time.Duration(secs) * time.Second
	if d > maxRetryAfter {
		return maxRetryAfter
	}
	return d
}
