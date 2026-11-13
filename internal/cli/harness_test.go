package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/geniuspath/apipod-cli/internal/auth"
	"github.com/geniuspath/apipod-cli/internal/testutil"
)

// testKey is an API key the fake backend accepts.
const testKey = "sk-test-0123456789abcdefghijklmnopqrstuvwxyz"

// call is one request received by a fake server.
type call struct {
	Method string
	Path   string
	Query  url.Values
	Header http.Header
	Body   []byte
}

// upload is one file received by POST /v1/uploads.
type upload struct {
	Field       string
	Filename    string
	ContentType string
	Data        []byte
}

type authCode struct {
	challenge string
	method    string
	redirect  string
	clientID  string
}

// fakeAPI imitates the APIPod backend closely enough to drive every command:
// the standard envelopes, OAuth with real PKCE verification, async jobs.
type fakeAPI struct {
	t   *testing.T
	srv *httptest.Server

	mu        sync.Mutex
	calls     []call
	uploads   []upload
	authorize []url.Values
	tokens    map[string]bool // accepted bearer credentials
	refresh   map[string]bool // refresh tokens that are still valid
	codes     map[string]authCode
	revoked   []string
	failNext  map[string][]int // "METHOD /path" -> statuses to answer first
	seq       int
	polls     int

	// Behaviour knobs, set by tests before running a command.
	estimate       float64
	estimateStatus int
	balance        float64
	grantScope     string
	revokeStatus   int
	jobStatuses    []string // served in order by the status endpoints; the last repeats
	resultURLs     []string
	modelsJSON     string
	schemaJSON     string
	jobError       string
}

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	f := &fakeAPI{
		t:           t,
		tokens:      map[string]bool{testKey: true},
		refresh:     map[string]bool{},
		codes:       map[string]authCode{},
		failNext:    map[string][]int{},
		estimate:    0.10,
		balance:     25,
		grantScope:  "profile email apipod:api",
		jobStatuses: []string{"pending", "processing", "completed"},
		modelsJSON:  `[]`,
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.calls = append(f.calls, call{r.Method, r.URL.Path, r.URL.Query(), r.Header.Clone(), body})
	key := r.Method + " " + r.URL.Path
	var forced int
	if queue := f.failNext[key]; len(queue) > 0 {
		forced, f.failNext[key] = queue[0], queue[1:]
	}
	f.mu.Unlock()

	if forced == -1 { // drop the connection without answering
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			conn.Close()
		}
		return
	}
	if forced != 0 {
		f.fail(w, forced, "", fmt.Sprintf("forced failure %d", forced))
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))

	path := r.URL.Path
	switch {
	case strings.HasPrefix(path, "/oauth/"):
		f.serveOAuth(w, r)
	case path == "/public/models":
		f.ok(w, json.RawMessage(f.modelsJSON))
	case strings.HasPrefix(path, "/public/schemas/"):
		if f.schemaJSON == "" {
			f.fail(w, 404, "", "Schema not found")
			return
		}
		f.ok(w, json.RawMessage(f.schemaJSON))
	case path == "/v1/pricing/estimate":
		if f.estimateStatus != 0 {
			f.fail(w, f.estimateStatus, "", "Model not found or not enabled")
			return
		}
		f.ok(w, map[string]any{"price": f.estimate, "currency": "USD"})
	case strings.HasPrefix(path, "/v1/"):
		if !f.authorized(r) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"message":"Authentication required. Please provide a valid API key or sign in.","type":"invalid_request_error","code":"invalid_api_key"}}`)
			return
		}
		f.serveV1(w, r, body)
	default:
		f.fail(w, 404, "", "not found")
	}
}

func (f *fakeAPI) authorized(r *http.Request) bool {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	f.mu.Lock()
	defer f.mu.Unlock()
	return token != "" && f.tokens[token]
}

func (f *fakeAPI) ok(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "message": "success", "data": data})
}

func (f *fakeAPI) fail(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Request-ID", "req_test")
	w.WriteHeader(status)
	body := map[string]any{"code": status, "message": message}
	if code != "" {
		body["data"] = map[string]string{"error_code": code}
	}
	_ = json.NewEncoder(w).Encode(body)
}

func (f *fakeAPI) serveV1(w http.ResponseWriter, r *http.Request, body []byte) {
	path := r.URL.Path
	switch {
	case path == "/v1/account/balance":
		f.ok(w, map[string]any{"balance_usd": f.balance, "level": "L2"})
	case path == "/v1/account/status":
		f.ok(w, map[string]string{"object": "account_status", "status": "active"})
	case path == "/v1/images/generations":
		f.ok(w, map[string]string{"task_id": "img_1"})
	case path == "/v1/videos/generations":
		f.ok(w, map[string]string{"task_id": "vid_1", "status": "pending"})
	case strings.HasPrefix(path, "/v1/videos/status/"):
		id := strings.TrimPrefix(path, "/v1/videos/status/")
		if !strings.HasPrefix(id, "vid_") {
			// The real video endpoint reports an unknown id as a 5xx.
			f.fail(w, 500, "internal_error", "task not found: record not found")
			return
		}
		f.ok(w, f.jobStatus(id, true))
	case strings.HasPrefix(path, "/v1/images/status/"):
		id := strings.TrimPrefix(path, "/v1/images/status/")
		if !strings.HasPrefix(id, "img_") {
			f.fail(w, 404, "", "Task not found")
			return
		}
		f.ok(w, f.jobStatus(id, false))
	case path == "/v1/uploads":
		f.serveUpload(w, r)
	default:
		f.fail(w, 404, "", "not found")
	}
}

func (f *fakeAPI) jobStatus(id string, video bool) map[string]any {
	f.mu.Lock()
	n := f.polls
	f.polls++
	f.mu.Unlock()
	if n >= len(f.jobStatuses) {
		n = len(f.jobStatuses) - 1
	}
	status := f.jobStatuses[n]
	out := map[string]any{"task_id": id, "status": status}
	switch status {
	case "completed":
		out["result"] = f.resultURLs
		out["usage"] = map[string]float64{"cost": f.estimate}
		out["completed_at"] = 1700000000
	case "failed":
		if video {
			out["error"] = f.jobError
		} else {
			out["error_message"] = f.jobError
			out["error_code"] = "UPSTREAM_FAILED"
		}
	default:
		out["progress"] = 40
	}
	return out
}

func (f *fakeAPI) serveUpload(w http.ResponseWriter, r *http.Request) {
	_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		f.fail(w, 400, "", "file is required")
		return
	}
	part, err := multipart.NewReader(r.Body, params["boundary"]).NextPart()
	if err != nil {
		f.fail(w, 400, "", "file is required")
		return
	}
	data, _ := io.ReadAll(part)
	up := upload{part.FormName(), part.FileName(), part.Header.Get("Content-Type"), data}
	f.mu.Lock()
	f.uploads = append(f.uploads, up)
	n := len(f.uploads)
	f.mu.Unlock()
	kind, _, _ := strings.Cut(up.ContentType, "/")
	f.ok(w, map[string]any{
		"url":          fmt.Sprintf("https://cdn.example.test/uploads/%d", n),
		"type":         kind,
		"content_type": up.ContentType,
		"bytes":        len(data),
	})
}

func (f *fakeAPI) serveOAuth(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	oauthErr := func(status int, code string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "error_description": code})
	}
	issue := func() {
		f.mu.Lock()
		f.seq++
		access, refresh := fmt.Sprintf("oauth-access-token-%d-abcdefghijklmnop", f.seq), fmt.Sprintf("oauth-refresh-token-%d-abcdefghijklmnop", f.seq)
		f.tokens[access], f.refresh[refresh] = true, true
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": access, "refresh_token": refresh, "token_type": "Bearer", "expires_in": 3600, "scope": f.grantScope,
		})
	}
	switch r.URL.Path {
	case "/oauth/token":
		if r.PostForm.Get("client_id") != auth.ClientID || r.PostForm.Get("client_secret") != "" {
			oauthErr(401, "invalid_client")
			return
		}
		switch r.PostForm.Get("grant_type") {
		case "authorization_code":
			f.mu.Lock()
			code, found := f.codes[r.PostForm.Get("code")]
			delete(f.codes, r.PostForm.Get("code")) // single use
			f.mu.Unlock()
			sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
			if !found || code.method != "S256" || code.redirect != r.PostForm.Get("redirect_uri") ||
				base64.RawURLEncoding.EncodeToString(sum[:]) != code.challenge {
				oauthErr(400, "invalid_grant")
				return
			}
			issue()
		case "refresh_token":
			f.mu.Lock()
			valid := f.refresh[r.PostForm.Get("refresh_token")]
			delete(f.refresh, r.PostForm.Get("refresh_token")) // rotation
			f.mu.Unlock()
			if !valid {
				oauthErr(400, "invalid_grant")
				return
			}
			issue()
		default:
			oauthErr(400, "unsupported_grant_type")
		}
	case "/oauth/revoke":
		if f.revokeStatus != 0 {
			oauthErr(f.revokeStatus, "server_error")
			return
		}
		token := r.PostForm.Get("token")
		f.mu.Lock()
		f.revoked = append(f.revoked, token)
		delete(f.tokens, token)
		delete(f.refresh, token)
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	case "/oauth/userinfo":
		if !f.authorized(r) {
			oauthErr(401, "invalid_token")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"sub":"42","email":"dev@example.com","username":"dev"}`)
	default:
		oauthErr(404, "not_found")
	}
}

// approve plays the browser: it "visits" the authorize URL, grants access and
// follows the redirect back to the CLI's loopback listener.
func (f *fakeAPI) approve(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return err
	}
	q := u.Query()
	f.mu.Lock()
	f.authorize = append(f.authorize, q)
	code := fmt.Sprintf("auth-code-%d-abcdefghijklmnop", len(f.authorize))
	f.codes[code] = authCode{q.Get("code_challenge"), q.Get("code_challenge_method"), q.Get("redirect_uri"), q.Get("client_id")}
	f.mu.Unlock()
	resp, err := http.Get(q.Get("redirect_uri") + "?" + url.Values{"code": {code}, "state": {q.Get("state")}}.Encode())
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// failOnce queues statuses (-1 drops the connection) for a route.
func (f *fakeAPI) failOnce(route string, statuses ...int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failNext[route] = append(f.failNext[route], statuses...)
}

func (f *fakeAPI) all() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]call(nil), f.calls...)
}

// to returns the calls made to a path.
func (f *fakeAPI) to(path string) []call {
	var out []call
	for _, c := range f.all() {
		if c.Path == path {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeAPI) received() []upload {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]upload(nil), f.uploads...)
}

func (f *fakeAPI) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls, f.uploads, f.polls = nil, nil, 0
}

// harness runs the CLI in-process against the fake backend.
type harness struct {
	t   *testing.T
	api *fakeAPI
	cdn *httptest.Server // https object storage holding generated results

	cdnMu    sync.Mutex
	cdnCalls []call

	cfgDir      string
	interactive *bool
	browser     func(url string) error
}

type result struct {
	stdout string
	stderr string
	code   int
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, api: newFakeAPI(t), cfgDir: t.TempDir()}
	h.cdn = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.cdnMu.Lock()
		h.cdnCalls = append(h.cdnCalls, call{Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone()})
		h.cdnMu.Unlock()
		if strings.HasSuffix(r.URL.Path, ".mp4") {
			_, _ = w.Write(testutil.MP4)
			return
		}
		_, _ = w.Write(testutil.PNG)
	}))
	t.Cleanup(h.cdn.Close)
	h.api.resultURLs = []string{h.cdn.URL + "/results/one.png"}
	h.browser = h.api.approve

	// The environment is fully controlled: no real config, key or host.
	t.Setenv("APIPOD_CONFIG_DIR", h.cfgDir)
	t.Setenv(EnvBaseURL, h.api.srv.URL)
	t.Setenv(EnvConsoleURL, "")
	t.Setenv(auth.EnvAPIKey, "")
	return h
}

// useKey authenticates through APIPOD_API_KEY.
func (h *harness) useKey() { h.t.Setenv(auth.EnvAPIKey, testKey) }

func (h *harness) setInteractive(v bool) { h.interactive = &v }

func (h *harness) run(args ...string) result { return h.runIn("", args...) }

func (h *harness) runIn(stdin string, args ...string) result {
	h.t.Helper()
	var out, errb bytes.Buffer
	app := &App{
		In:          strings.NewReader(stdin),
		Out:         &out,
		Err:         &errb,
		Build:       BuildInfo{Version: "9.9.9-test", Commit: "abc1234", Date: "2026-11-13"},
		OpenBrowser: func(u string) error { return h.browser(u) },
		// Always the file backend: tests must never touch the real keychain.
		NewStore:       func(dir string) *auth.Store { return &auth.Store{Dir: dir} },
		DownloadClient: h.cdn.Client(),
		PollInterval:   2 * time.Millisecond,
		RetryWait:      time.Millisecond,
		Interactive:    h.interactive,
	}
	code := app.Run(context.Background(), args)
	return result{out.String(), errb.String(), code}
}

func (h *harness) store() *auth.Store { return &auth.Store{Dir: h.cfgDir} }

func (h *harness) cdnRequests() []call {
	h.cdnMu.Lock()
	defer h.cdnMu.Unlock()
	return append([]call(nil), h.cdnCalls...)
}

// file writes a fixture into a fresh directory and returns its path.
func (h *harness) file(name string, data []byte) string {
	h.t.Helper()
	path := filepath.Join(h.t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		h.t.Fatal(err)
	}
	return path
}

func (r result) wantCode(t *testing.T, want int) result {
	t.Helper()
	if r.code != want {
		t.Fatalf("exit code = %d, want %d\nstdout: %s\nstderr: %s", r.code, want, r.stdout, r.stderr)
	}
	return r
}

// decode parses stdout as exactly one JSON document.
func (r result) decode(t *testing.T, into any) {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(r.stdout))
	if err := dec.Decode(into); err != nil {
		t.Fatalf("stdout is not JSON: %v\nstdout: %s\nstderr: %s", err, r.stdout, r.stderr)
	}
	if dec.More() {
		t.Fatalf("stdout holds more than one JSON document:\n%s", r.stdout)
	}
}

// noSecret fails if any secret appears anywhere in the command's output.
func (r result) noSecret(t *testing.T, secrets ...string) {
	t.Helper()
	for _, s := range secrets {
		if s == "" {
			continue
		}
		if strings.Contains(r.stdout, s) {
			t.Errorf("secret %q appears on stdout:\n%s", s, r.stdout)
		}
		if strings.Contains(r.stderr, s) {
			t.Errorf("secret %q appears on stderr:\n%s", s, r.stderr)
		}
	}
}

func jsonBody(t *testing.T, c call) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(c.Body, &m); err != nil {
		t.Fatalf("request body of %s %s is not JSON: %v", c.Method, c.Path, err)
	}
	return m
}
