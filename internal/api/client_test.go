package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/geniuspath/apipod-cli/internal/exitcode"
)

type staticToken string

func (s staticToken) Token(context.Context) (string, error) { return string(s), nil }

// recorder is a fake API that records every request it receives.
type recorder struct {
	*httptest.Server
	mu       sync.Mutex
	requests []recorded
}

type recorded struct {
	Method string
	Path   string
	Header http.Header
	Body   []byte
}

func newRecorder(t *testing.T, handler func(n int, w http.ResponseWriter, r *http.Request)) *recorder {
	t.Helper()
	rec := &recorder{}
	rec.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec.mu.Lock()
		rec.requests = append(rec.requests, recorded{r.Method, r.URL.RequestURI(), r.Header.Clone(), body})
		n := len(rec.requests)
		rec.mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(body))
		handler(n, w, r)
	}))
	t.Cleanup(rec.Close)
	return rec
}

func (r *recorder) all() []recorded {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recorded(nil), r.requests...)
}

func (r *recorder) client(tokens TokenSource) *Client {
	c := New(r.URL, tokens, nil)
	c.SetRetryWait(time.Millisecond)
	return c
}

func writeData(w http.ResponseWriter, data string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"code":200,"message":"success","data":`+data+`}`)
}

func writeStatus(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// dropConnection closes the socket without answering: a connection error.
func dropConnection(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	conn, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
}

var uuidV4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestGenerateSendsIdempotencyKey(t *testing.T) {
	rec := newRecorder(t, func(_ int, w http.ResponseWriter, _ *http.Request) { writeData(w, `{"task_id":"t1"}`) })
	c := rec.client(staticToken("Bearer k"))

	for _, kind := range []string{KindImage, KindVideo} {
		if _, err := c.Generate(context.Background(), kind, map[string]any{"model": "m"}); err != nil {
			t.Fatal(err)
		}
	}
	reqs := rec.all()
	if reqs[0].Path != "/v1/images/generations" || reqs[1].Path != "/v1/videos/generations" {
		t.Fatalf("paths = %s, %s", reqs[0].Path, reqs[1].Path)
	}
	keys := map[string]bool{}
	for _, r := range reqs {
		key := r.Header.Get("Idempotency-Key")
		if !uuidV4.MatchString(key) {
			t.Errorf("%s: Idempotency-Key %q is not a UUIDv4", r.Path, key)
		}
		keys[key] = true
	}
	if len(keys) != 2 {
		t.Error("two separate generations shared an Idempotency-Key")
	}
}

// Every attempt of one generation carries the same key, so the server can
// replay the first result instead of creating — and billing — a second task.
func TestGenerateRetriesReuseTheSameIdempotencyKey(t *testing.T) {
	failures := map[string]func(t *testing.T, w http.ResponseWriter){
		"connection error": func(t *testing.T, w http.ResponseWriter) { dropConnection(t, w) },
		"429": func(_ *testing.T, w http.ResponseWriter) {
			writeStatus(w, 429, `{"code":429,"message":"Rate limit exceeded"}`)
		},
		"502": func(_ *testing.T, w http.ResponseWriter) { writeStatus(w, 502, `{"code":502,"message":"bad gateway"}`) },
		"503": func(_ *testing.T, w http.ResponseWriter) { writeStatus(w, 503, `{"code":503,"message":"unavailable"}`) },
	}
	for name, fail := range failures {
		t.Run(name, func(t *testing.T) {
			rec := newRecorder(t, func(n int, w http.ResponseWriter, _ *http.Request) {
				if n <= 2 {
					fail(t, w)
					return
				}
				writeData(w, `{"task_id":"t1","status":"pending"}`)
			})
			task, err := rec.client(staticToken("Bearer k")).Generate(context.Background(), KindVideo, map[string]any{"model": "m"})
			if err != nil {
				t.Fatalf("Generate: %v", err)
			}
			if task.TaskID != "t1" {
				t.Errorf("task = %+v", task)
			}
			reqs := rec.all()
			if len(reqs) != 3 {
				t.Fatalf("server saw %d requests, want 3 (two failures, one success)", len(reqs))
			}
			key := reqs[0].Header.Get("Idempotency-Key")
			if key == "" {
				t.Fatal("no Idempotency-Key on the first attempt")
			}
			for i, r := range reqs {
				if got := r.Header.Get("Idempotency-Key"); got != key {
					t.Errorf("attempt %d used key %q, want the original %q", i+1, got, key)
				}
				if !bytes.Equal(r.Body, reqs[0].Body) {
					t.Errorf("attempt %d sent a different body", i+1)
				}
			}
		})
	}
}

// Nothing but connection errors, 429, 502 and 503 is retried.
func TestGenerateDoesNotRetryOtherFailures(t *testing.T) {
	for _, status := range []int{400, 401, 402, 403, 404, 409, 413, 422, 500, 504} {
		rec := newRecorder(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
			writeStatus(w, status, `{"code":0,"message":"nope"}`)
		})
		_, err := rec.client(staticToken("Bearer k")).Generate(context.Background(), KindImage, map[string]any{"model": "m"})
		if err == nil {
			t.Errorf("status %d: no error", status)
		}
		if n := len(rec.all()); n != 1 {
			t.Errorf("status %d: %d requests sent, want exactly 1 (no retry)", status, n)
		}
	}
}

func TestRetriesAreBounded(t *testing.T) {
	rec := newRecorder(t, func(_ int, w http.ResponseWriter, _ *http.Request) { writeStatus(w, 503, `{"message":"down"}`) })
	_, err := rec.client(staticToken("Bearer k")).Generate(context.Background(), KindImage, map[string]any{"model": "m"})
	if err == nil {
		t.Fatal("no error after exhausting retries")
	}
	if n := len(rec.all()); n != maxAttempts {
		t.Errorf("%d attempts, want %d", n, maxAttempts)
	}
}

// A POST without an idempotency key may already have taken effect, so an
// ambiguous failure is reported, not repeated. 429 is unambiguous.
func TestNonIdempotentPostIsOnlyRetriedOn429(t *testing.T) {
	cases := []struct {
		name string
		fail func(t *testing.T, w http.ResponseWriter)
		want int
	}{
		{"connection error", func(t *testing.T, w http.ResponseWriter) { dropConnection(t, w) }, 1},
		{"502", func(_ *testing.T, w http.ResponseWriter) { writeStatus(w, 502, `{}`) }, 1},
		{"503", func(_ *testing.T, w http.ResponseWriter) { writeStatus(w, 503, `{}`) }, 1},
		{"429", func(_ *testing.T, w http.ResponseWriter) { writeStatus(w, 429, `{}`) }, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := newRecorder(t, func(n int, w http.ResponseWriter, _ *http.Request) {
				if n == 1 {
					tc.fail(t, w)
					return
				}
				writeData(w, `{"assetId":"a1","status":"PROCESSING"}`)
			})
			_, _ = rec.client(staticToken("Bearer k")).CreateAsset(context.Background(), CreateAssetRequest{AssetType: "Image", URL: "https://x/y.png"})
			reqs := rec.all()
			if len(reqs) != tc.want {
				t.Errorf("%d requests, want %d", len(reqs), tc.want)
			}
			if reqs[0].Header.Get("Idempotency-Key") != "" {
				t.Error("asset creation carries an Idempotency-Key the server does not honour")
			}
		})
	}
}

func TestReadsAreRetried(t *testing.T) {
	rec := newRecorder(t, func(n int, w http.ResponseWriter, _ *http.Request) {
		if n == 1 {
			dropConnection(t, w)
			return
		}
		writeData(w, `{"balance_usd":12.5,"level":"L1"}`)
	})
	bal, err := rec.client(staticToken("Bearer k")).GetBalance(context.Background())
	if err != nil || bal.BalanceUSD != 12.5 || bal.Level != "L1" {
		t.Errorf("GetBalance = %+v, %v", bal, err)
	}
}

func TestRetryStopsWhenContextEnds(t *testing.T) {
	rec := newRecorder(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "30")
		writeStatus(w, 429, `{}`)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := rec.client(staticToken("Bearer k")).GetBalance(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want the context deadline", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("the client slept through Retry-After after its context ended")
	}
}

func TestRequestCarriesCredentialAndUserAgent(t *testing.T) {
	Version = "1.2.3"
	t.Cleanup(func() { Version = "dev" })
	rec := newRecorder(t, func(_ int, w http.ResponseWriter, _ *http.Request) { writeData(w, `{}`) })
	if _, err := rec.client(staticToken("Bearer secret-token")).GetBalance(context.Background()); err != nil {
		t.Fatal(err)
	}
	r := rec.all()[0]
	if r.Header.Get("Authorization") != "Bearer secret-token" {
		t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
	}
	if r.Header.Get("User-Agent") != "apipod-cli/1.2.3" {
		t.Errorf("User-Agent = %q", r.Header.Get("User-Agent"))
	}
}

func TestPublicClientSendsNoCredential(t *testing.T) {
	rec := newRecorder(t, func(_ int, w http.ResponseWriter, _ *http.Request) { writeData(w, `[]`) })
	if _, err := rec.client(nil).ListModels(context.Background(), "text_to_image"); err != nil {
		t.Fatal(err)
	}
	r := rec.all()[0]
	if r.Path != "/public/models?classification=text_to_image" {
		t.Errorf("path = %s", r.Path)
	}
	if r.Header.Get("Authorization") != "" {
		t.Error("a public request carried an Authorization header")
	}
}

// A redirect must not carry the credential to another host: the client does
// not follow redirects at all.
func TestClientDoesNotFollowRedirects(t *testing.T) {
	other := newRecorder(t, func(_ int, w http.ResponseWriter, _ *http.Request) { writeData(w, `{}`) })
	rec := newRecorder(t, func(_ int, w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusTemporaryRedirect)
	})
	if _, err := rec.client(staticToken("Bearer secret-token")).GetBalance(context.Background()); err == nil {
		t.Error("a redirected request reported success")
	}
	if n := len(other.all()); n != 0 {
		t.Errorf("the redirect target received %d requests, want 0", n)
	}
}

func TestErrorEnvelopesAndExitCodes(t *testing.T) {
	cases := []struct {
		status   int
		body     string
		wantCode string
		wantMsg  string
		wantExit int
	}{
		{401, `{"error":{"message":"Authentication required.","type":"invalid_request_error","code":"invalid_api_key"}}`, "invalid_api_key", "Authentication required.", exitcode.Auth},
		{401, `{"code":401,"message":"user not authenticated","data":{"type":"invalid_request_error","error_code":"invalid_api_key"}}`, "invalid_api_key", "user not authenticated", exitcode.Auth},
		{403, `{"code":403,"message":"forbidden"}`, "", "forbidden", exitcode.Auth},
		{429, `{"code":429,"message":"Rate limit exceeded","data":{"type":"rate_limit_error","error_code":"rate_limit_exceeded"}}`, "rate_limit_exceeded", "Rate limit exceeded", exitcode.RateLimit},
		{404, `{"code":404,"message":"Task not found"}`, "", "Task not found", exitcode.NotFound},
		{400, `{"code":400,"message":"model gone","data":{"error_code":"model_not_available"}}`, "model_not_available", "model gone", exitcode.NotFound},
		{402, `{"code":402,"message":"Insufficient quota"}`, "", "Insufficient quota", exitcode.Billing},
		{403, `{"code":403,"message":"Insufficient quota","data":{"error_code":"insufficient_quota"}}`, "insufficient_quota", "Insufficient quota", exitcode.Billing},
		{400, `{"code":400,"message":"Invalid request: duration must be 5 or 10"}`, "", "Invalid request: duration must be 5 or 10", exitcode.Validation},
		{409, `{"code":409,"message":"in progress","data":{"error_code":"idempotency_in_progress"}}`, "idempotency_in_progress", "in progress", exitcode.Validation},
		{413, `{"code":413,"message":"file must be at most 100 MB"}`, "", "file must be at most 100 MB", exitcode.Validation},
		{504, `{"code":504,"message":"Upstream request timed out"}`, "", "Upstream request timed out", exitcode.Timeout},
		{500, `not json at all`, "", "", exitcode.General},
		{400, `{"error":"invalid_grant","error_description":"Refresh token is invalid"}`, "invalid_grant", "Refresh token is invalid", exitcode.Validation},
	}
	for _, tc := range cases {
		rec := newRecorder(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-Request-ID", "req_123")
			writeStatus(w, tc.status, tc.body)
		})
		// A read keeps this about parsing; retries are covered above.
		c := rec.client(staticToken("Bearer k"))
		c.SetRetryWait(time.Microsecond)
		_, err := c.CreateAsset(context.Background(), CreateAssetRequest{})
		var apiErr *Error
		if !errors.As(err, &apiErr) {
			t.Errorf("status %d: err = %v, want *api.Error", tc.status, err)
			continue
		}
		if apiErr.HTTPStatus != tc.status || apiErr.Code != tc.wantCode || apiErr.Message != tc.wantMsg {
			t.Errorf("status %d: got {%d %q %q}, want {%d %q %q}", tc.status, apiErr.HTTPStatus, apiErr.Code, apiErr.Message, tc.status, tc.wantCode, tc.wantMsg)
		}
		if apiErr.RequestID != "req_123" {
			t.Errorf("status %d: request id = %q", tc.status, apiErr.RequestID)
		}
		if got := exitcode.From(err); got != tc.wantExit {
			t.Errorf("status %d (%s): exit code = %d, want %d", tc.status, tc.wantCode, got, tc.wantExit)
		}
	}
}

func TestDebugLogRedactsCredentials(t *testing.T) {
	const secret = "sk-live-0123456789abcdefghijklmnopqrstuv"
	var log bytes.Buffer
	rec := newRecorder(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Set-Cookie", "session="+secret)
		writeData(w, `{"task_id":"t1"}`)
	})
	c := New(rec.URL, staticToken("Bearer "+secret), NewDebugLogger(&log))
	// The secret is also planted where a careless logger would print it.
	if _, err := c.Generate(context.Background(), KindImage, map[string]any{"model": "m", "prompt": "note: " + secret}); err != nil {
		t.Fatal(err)
	}
	out := log.String()
	if strings.Contains(out, secret) {
		t.Fatalf("the credential appears in --debug output:\n%s", out)
	}
	for _, want := range []string{"POST", "/v1/images/generations", "Authorization: Bearer [REDACTED]", "Idempotency-Key:", "200 OK"} {
		if !strings.Contains(out, want) {
			t.Errorf("debug output lacks %q:\n%s", want, out)
		}
	}
}

func TestDebugLoggerMasksSensitiveHeadersByName(t *testing.T) {
	var log bytes.Buffer
	d := NewDebugLogger(&log)
	// Never registered with AddSecret: the header name alone must protect it.
	d.logHeaders(">", http.Header{
		"Authorization":       {"Bearer unregistered-token-value"},
		"Proxy-Authorization": {"Basic dXNlcjpwYXNz"},
		"Cookie":              {"sid=unregistered-cookie"},
		"X-Api-Key":           {"unregistered-api-key"},
		"Accept":              {"application/json"},
	})
	out := log.String()
	for _, leaked := range []string{"unregistered-token-value", "dXNlcjpwYXNz", "unregistered-cookie", "unregistered-api-key"} {
		if strings.Contains(out, leaked) {
			t.Errorf("debug output leaks %q:\n%s", leaked, out)
		}
	}
	if !strings.Contains(out, "Accept: application/json") {
		t.Errorf("ordinary headers should stay readable:\n%s", out)
	}
}

func TestNilDebugLoggerIsNoOp(t *testing.T) {
	var d *DebugLogger
	d.AddSecret("anything-long-enough")
	d.Logf("nothing %s", "happens")
}

func TestGetJobFallsBackFromVideoToImage(t *testing.T) {
	rec := newRecorder(t, func(_ int, w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/videos/status/") {
			// The video endpoint reports an unknown id as a server error.
			writeStatus(w, 500, `{"code":500,"message":"task not found: record not found"}`)
			return
		}
		writeData(w, `{"task_id":"img_1","status":"completed","result":["https://cdn/a.png",{"url":"https://cdn/b.png","z_index":1}],"usage":{"cost":0.04},"completed_at":1700000000}`)
	})
	job, err := rec.client(staticToken("Bearer k")).GetJob(context.Background(), "img_1")
	if err != nil {
		t.Fatal(err)
	}
	if job.Kind != KindImage || job.Status != StatusCompleted || !job.Done() {
		t.Errorf("job = %+v", job)
	}
	if len(job.Results) != 2 || job.Results[0].URL != "https://cdn/a.png" || job.Results[1].URL != "https://cdn/b.png" {
		t.Errorf("results = %+v, want both the string and the object form decoded", job.Results)
	}
	if job.CostUSD == nil || *job.CostUSD != 0.04 {
		t.Errorf("cost = %v", job.CostUSD)
	}
	reqs := rec.all()
	if reqs[0].Path != "/v1/videos/status/img_1" || reqs[len(reqs)-1].Path != "/v1/images/status/img_1" {
		t.Errorf("lookup order = %s then %s, want video then image", reqs[0].Path, reqs[len(reqs)-1].Path)
	}
}

func TestGetJobDoesNotMaskAuthFailures(t *testing.T) {
	rec := newRecorder(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
		writeStatus(w, 401, `{"error":{"message":"bad key","code":"invalid_api_key"}}`)
	})
	_, err := rec.client(staticToken("Bearer k")).GetJob(context.Background(), "x")
	if exitcode.From(err) != exitcode.Auth {
		t.Errorf("err = %v, want the auth failure", err)
	}
	if n := len(rec.all()); n != 1 {
		t.Errorf("%d requests, want 1: an auth failure must not be retried on the image endpoint", n)
	}
}

func TestGetJobReportsFailureDetails(t *testing.T) {
	rec := newRecorder(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
		writeData(w, `{"task_id":"v1","status":"failed","error":"Upstream request failed."}`)
	})
	job, err := rec.client(staticToken("Bearer k")).GetJob(context.Background(), "v1")
	if err != nil {
		t.Fatal(err)
	}
	if job.Kind != KindVideo || job.Status != StatusFailed || job.ErrorMessage != "Upstream request failed." {
		t.Errorf("job = %+v", job)
	}
}

func TestUploadFileSendsMultipartFieldFile(t *testing.T) {
	payload := []byte("\x89PNG\r\n\x1a\n-pretend-image-bytes")
	var gotName, gotType string
	var gotBody []byte
	rec := newRecorder(t, func(_ int, w http.ResponseWriter, r *http.Request) {
		_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil {
			t.Errorf("content type: %v", err)
		}
		part, err := multipart.NewReader(r.Body, params["boundary"]).NextPart()
		if err != nil {
			t.Fatalf("read part: %v", err)
		}
		if part.FormName() != "file" {
			t.Errorf("form field = %q, want file", part.FormName())
		}
		gotName, gotType = part.FileName(), part.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(part)
		writeData(w, `{"url":"https://cdn/u.png","type":"image","content_type":"image/png","bytes":28}`)
	})
	open := func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(payload)), nil }
	up, err := rec.client(staticToken("Bearer k")).UploadFile(context.Background(), "photo.png", "image/png", int64(len(payload)), open)
	if err != nil {
		t.Fatal(err)
	}
	if up.URL != "https://cdn/u.png" || up.Type != "image" || up.ContentType != "image/png" || up.Bytes != 28 {
		t.Errorf("upload = %+v", up)
	}
	if gotName != "photo.png" || gotType != "image/png" || !bytes.Equal(gotBody, payload) {
		t.Errorf("server received name=%q type=%q %d bytes", gotName, gotType, len(gotBody))
	}
	r := rec.all()[0]
	if r.Method != http.MethodPost || r.Path != "/v1/uploads" || r.Header.Get("Authorization") != "Bearer k" {
		t.Errorf("request = %s %s auth=%q", r.Method, r.Path, r.Header.Get("Authorization"))
	}
}

func TestUploadIsOnlyRetriedOn429(t *testing.T) {
	open := func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("data")), nil }
	for status, want := range map[int]int{429: 2, 502: 1, 503: 1, 500: 1, 413: 1} {
		rec := newRecorder(t, func(n int, w http.ResponseWriter, _ *http.Request) {
			if n == 1 {
				writeStatus(w, status, `{"message":"x"}`)
				return
			}
			writeData(w, `{"url":"https://cdn/u.png"}`)
		})
		_, _ = rec.client(staticToken("Bearer k")).UploadFile(context.Background(), "a.png", "image/png", 4, open)
		if n := len(rec.all()); n != want {
			t.Errorf("status %d: %d upload requests, want %d", status, n, want)
		}
	}
}

func TestEstimateAndAssets(t *testing.T) {
	rec := newRecorder(t, func(_ int, w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/pricing/estimate":
			writeData(w, `{"price":0.35,"currency":"USD","is_pre_deduct":true}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/assets":
			writeData(w, `{"items":[{"assetId":"a1","assetType":"Image","url":"https://x/a.png","status":"ACTIVE","errorMessage":null,"name":"n"}],"total":1,"page":1,"page_size":20,"total_pages":1}`)
		case r.Method == http.MethodDelete:
			writeData(w, `{"deleted":true}`)
		default:
			writeData(w, `{"assetId":"a1","assetType":"Image","status":"PROCESSING"}`)
		}
	})
	c := rec.client(staticToken("Bearer k"))
	ctx := context.Background()

	est, err := c.EstimateCost(ctx, EstimateRequest{ModelID: "m", Dimensions: map[string]string{"duration": "5"}, HasVideoMaterial: true})
	if err != nil || est.Price != 0.35 || !est.IsPreDeduct {
		t.Errorf("estimate = %+v, %v", est, err)
	}
	var sent map[string]any
	_ = json.Unmarshal(rec.all()[0].Body, &sent)
	if sent["model_id"] != "m" || sent["has_video_material"] != true || sent["dimensions"].(map[string]any)["duration"] != "5" {
		t.Errorf("estimate body = %s", rec.all()[0].Body)
	}

	list, err := c.ListAssets(ctx, AssetListParams{Type: "Image", Status: "ACTIVE", Page: 2, PageSize: 5})
	if err != nil || len(list.Items) != 1 || list.Items[0].Ref != "assetId://a1" {
		t.Errorf("list = %+v, %v", list, err)
	}
	if got := rec.all()[1].Path; got != "/v1/assets?page=2&page_size=5&status=ACTIVE&type=Image" {
		t.Errorf("list path = %s", got)
	}

	// Both the bare id and the assetId:// reference address the same asset.
	for _, ref := range []string{"a1", "assetId://a1"} {
		if _, err := c.GetAsset(ctx, ref); err != nil {
			t.Fatal(err)
		}
		if got := rec.all()[len(rec.all())-1].Path; got != "/v1/assets/a1" {
			t.Errorf("GetAsset(%q) path = %s", ref, got)
		}
	}
	if err := c.DeleteAsset(ctx, "assetId://a1"); err != nil {
		t.Fatal(err)
	}
	last := rec.all()[len(rec.all())-1]
	if last.Method != http.MethodDelete || last.Path != "/v1/assets/a1" {
		t.Errorf("delete = %s %s", last.Method, last.Path)
	}
}

func TestPathSegmentsAreEscaped(t *testing.T) {
	rec := newRecorder(t, func(_ int, w http.ResponseWriter, _ *http.Request) { writeData(w, `{}`) })
	c := rec.client(staticToken("Bearer k"))
	_, _ = c.GetJobOfKind(context.Background(), KindImage, "../../admin/users?x=1")
	_, _ = c.GetModelSchema(context.Background(), "a/b#c")
	for _, r := range rec.all() {
		if strings.Contains(r.Path, "/admin/") || strings.Contains(r.Path, "?x=1") || strings.Contains(r.Path, "/b") {
			t.Errorf("an id escaped its path segment: %s", r.Path)
		}
	}
}

// A secret that straddles the truncation point must still be removed:
// redaction runs before the body is shortened.
func TestDebugLogRedactsSecretsAtTheTruncationBoundary(t *testing.T) {
	const secret = "sk-live-0123456789abcdefghijklmnopqrstuv"
	var log bytes.Buffer
	rec := newRecorder(t, func(_ int, w http.ResponseWriter, _ *http.Request) { writeData(w, `{"task_id":"t1"}`) })
	c := New(rec.URL, staticToken("Bearer "+secret), NewDebugLogger(&log))
	// Padding places the secret across the 2000-byte cut.
	prompt := strings.Repeat("a", 1950) + secret + strings.Repeat("b", 500)
	if _, err := c.Generate(context.Background(), KindImage, map[string]any{"prompt": prompt}); err != nil {
		t.Fatal(err)
	}
	if out := log.String(); strings.Contains(out, secret[:20]) {
		t.Errorf("a fragment of the credential survived truncation:\n%s", out[len(out)-300:])
	}
}

// --debug lines never carry terminal control bytes. Go's HTTP transport
// already rejects response headers that contain them, so this is defence in
// depth; the logger is exercised directly.
func TestDebugLogStripsControlSequences(t *testing.T) {
	var log bytes.Buffer
	d := NewDebugLogger(&log)
	d.logHeaders("<", http.Header{"X-Request-Id": {"req_1\x1b[2J\x1b]0;pwned\x07"}})
	d.Logf("attempt failed (%v)", errors.New("bad\x1b[31m thing\nFORGED LINE"))
	out := log.String()
	if strings.ContainsAny(out, "\x1b\x07") {
		t.Errorf("--debug output carries escape bytes: %q", out)
	}
	if strings.Count(out, "\n") != 2 {
		t.Errorf("a logged value forged an extra line: %q", out)
	}
}

// The upload declares its exact length instead of using chunked encoding.
func TestUploadDeclaresContentLength(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 70000)
	var declared int64
	var chunked bool
	var received int
	rec := newRecorder(t, func(_ int, w http.ResponseWriter, r *http.Request) {
		declared, chunked = r.ContentLength, len(r.TransferEncoding) > 0
		_, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		part, err := multipart.NewReader(r.Body, params["boundary"]).NextPart()
		if err != nil {
			t.Errorf("malformed multipart body: %v", err)
			return
		}
		data, _ := io.ReadAll(part)
		received = len(data)
		writeData(w, `{"url":"https://cdn/u.png"}`)
	})
	open := func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(payload)), nil }
	if _, err := rec.client(staticToken("Bearer k")).UploadFile(context.Background(), `we"ird\name.png`, "image/png", int64(len(payload)), open); err != nil {
		t.Fatal(err)
	}
	if chunked || declared <= int64(len(payload)) {
		t.Errorf("Content-Length = %d, chunked = %v; want an exact length above %d", declared, chunked, len(payload))
	}
	if received != len(payload) {
		t.Errorf("the server read %d file bytes, want %d", received, len(payload))
	}
}
