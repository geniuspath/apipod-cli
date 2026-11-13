package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/geniuspath/apipod-cli/internal/exitcode"
)

const (
	imageGen = "/v1/images/generations"
	videoGen = "/v1/videos/generations"
	estimate = "/v1/pricing/estimate"
	uploads  = "/v1/uploads"
)

func TestImageGeneratesWaitsAndDownloads(t *testing.T) {
	h := newHarness(t)
	h.useKey()
	out := t.TempDir()
	res := h.run("image", "a red fox", "-m", "gpt-image-2", "--aspect", "16:9", "-n", "2", "-x", "seed=42", "-x", "quality=high", "-o", out, "--json").wantCode(t, exitcode.OK)

	body := jsonBody(t, h.api.to(imageGen)[0])
	want := map[string]any{"model": "gpt-image-2", "prompt": "a red fox", "aspect_ratio": "16:9", "n": float64(2), "seed": float64(42), "quality": "high"}
	for k, v := range want {
		if body[k] != v {
			t.Errorf("request %s = %#v, want %#v", k, body[k], v)
		}
	}

	var job jobView
	res.decode(t, &job)
	if job.JobID != "img_1" || job.Status != "completed" || job.Kind != "image" {
		t.Errorf("job = %+v", job.Job)
	}
	if job.CostUSD == nil || *job.CostUSD != 0.10 || job.EstimatedUSD == nil || *job.EstimatedUSD != 0.10 {
		t.Errorf("cost = %v, estimate = %v", job.CostUSD, job.EstimatedUSD)
	}
	wantPath := filepath.Join(out, "img_1-1.png")
	if len(job.Results) != 1 || job.Results[0].Path != wantPath || job.Results[0].URL == "" {
		t.Fatalf("results = %+v, want one result saved at %s", job.Results, wantPath)
	}
	if data, err := os.ReadFile(wantPath); err != nil || string(data) != string(pngFixture) {
		t.Errorf("downloaded file mismatch (err=%v)", err)
	}

	// Result downloads go to object storage without the user's credential.
	downloads := h.cdnRequests()
	if len(downloads) != 1 {
		t.Fatalf("%d download requests, want 1", len(downloads))
	}
	for name, values := range downloads[0].Header {
		if name == "Authorization" || strings.Contains(strings.Join(values, " "), testKey) {
			t.Errorf("the download request carried the credential in %s", name)
		}
	}
	// Progress went to stderr; stdout is the JSON document alone.
	if !strings.Contains(res.stderr, "Estimated cost: $0.10") || !strings.Contains(res.stderr, "Waiting for job img_1") {
		t.Errorf("stderr = %s", res.stderr)
	}
}

func TestVideoRequestFields(t *testing.T) {
	h := newHarness(t)
	h.useKey()
	h.api.resultURLs = []string{h.cdn.URL + "/results/clip.mp4"}
	out := t.TempDir()
	h.run("video", "a drone shot", "-m", "kling", "--duration", "5", "--resolution", "1080p", "--aspect", "9:16",
		"--image", "https://example.com/first.png", "-x", "generate_audio=true", "-x", `camera={"zoom":1.5}`, "-x", "callback_url=https://attacker.example/hook",
		"-o", out).wantCode(t, exitcode.OK)

	body := jsonBody(t, h.api.to(videoGen)[0])
	if body["model"] != "kling" || body["prompt"] != "a drone shot" || body["duration"] != float64(5) || body["resolution"] != "1080p" || body["aspect_ratio"] != "9:16" {
		t.Errorf("body = %v", body)
	}
	if body["generate_audio"] != true {
		t.Errorf("-x generate_audio=true was sent as %#v, want the JSON boolean", body["generate_audio"])
	}
	if cam, ok := body["camera"].(map[string]any); !ok || cam["zoom"] != 1.5 {
		t.Errorf("-x camera={...} was sent as %#v, want a JSON object", body["camera"])
	}
	if urls, ok := body["image_urls"].([]any); !ok || len(urls) != 1 || urls[0] != "https://example.com/first.png" {
		t.Errorf("image_urls = %#v", body["image_urls"])
	}
	// The CLI polls; it never lets the server push task data elsewhere.
	if _, present := body["callback_url"]; present {
		t.Error("callback_url was forwarded to the server")
	}
	if _, err := os.Stat(filepath.Join(out, "vid_1-1.mp4")); err != nil {
		t.Errorf("video result not saved: %v", err)
	}
	if len(h.api.received()) != 0 {
		t.Error("a remote URL reference triggered an upload")
	}
}

func TestDedicatedFlagsWinOverExtras(t *testing.T) {
	h := newHarness(t)
	h.useKey()
	h.run("image", "real prompt", "-m", "real-model", "--aspect", "1:1", "-x", "model=other", "-x", "prompt=other", "-x", "aspect_ratio=4:3", "--no-wait").wantCode(t, exitcode.OK)
	body := jsonBody(t, h.api.to(imageGen)[0])
	if body["model"] != "real-model" || body["prompt"] != "real prompt" || body["aspect_ratio"] != "1:1" {
		t.Errorf("body = %v", body)
	}
}

func TestNoWaitReturnsJobID(t *testing.T) {
	h := newHarness(t)
	h.useKey()
	res := h.run("video", "x", "-m", "m", "--no-wait", "--json").wantCode(t, exitcode.OK)
	var job jobView
	res.decode(t, &job)
	if job.JobID != "vid_1" || job.Status != "pending" || len(job.Results) != 0 {
		t.Errorf("job = %+v", job.Job)
	}
	if n := len(h.api.to("/v1/videos/status/vid_1")); n != 0 {
		t.Errorf("--no-wait polled the job %d times", n)
	}
	if len(h.cdnRequests()) != 0 {
		t.Error("--no-wait downloaded a result")
	}
}

var uuidV4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// Every generation carries an Idempotency-Key, and every retry of it reuses
// the same key, so a retry can never be billed as a second task.
func TestGenerationIdempotencyKeyAcrossRetries(t *testing.T) {
	h := newHarness(t)
	h.useKey()
	h.api.failOnce("POST "+imageGen, -1, 429, 503) // connection reset, then two retryable statuses
	h.run("image", "a fox", "-m", "m", "--no-wait").wantCode(t, exitcode.OK)

	calls := h.api.to(imageGen)
	if len(calls) != 4 {
		t.Fatalf("%d generation requests, want 4 (three failures, one success)", len(calls))
	}
	key := calls[0].Header.Get("Idempotency-Key")
	if !uuidV4.MatchString(key) {
		t.Fatalf("Idempotency-Key %q is not a UUIDv4", key)
	}
	for i, c := range calls {
		if c.Header.Get("Idempotency-Key") != key {
			t.Errorf("attempt %d used a different Idempotency-Key", i+1)
		}
		if string(c.Body) != string(calls[0].Body) {
			t.Errorf("attempt %d sent a different body", i+1)
		}
	}

	h.api.reset()
	h.run("image", "a fox", "-m", "m", "--no-wait").wantCode(t, exitcode.OK)
	if next := h.api.to(imageGen)[0].Header.Get("Idempotency-Key"); next == key || !uuidV4.MatchString(next) {
		t.Errorf("a new command reused the previous Idempotency-Key (%q)", next)
	}
}

func TestGenerationIsNotRetriedOnClientErrors(t *testing.T) {
	cases := map[int]int{400: exitcode.Validation, 402: exitcode.Billing, 403: exitcode.Auth, 404: exitcode.NotFound, 409: exitcode.Validation, 422: exitcode.Validation, 500: exitcode.General}
	for status, wantExit := range cases {
		h := newHarness(t)
		h.useKey()
		h.api.failOnce("POST "+videoGen, status)
		h.run("video", "x", "-m", "m", "--yes").wantCode(t, wantExit)
		if n := len(h.api.to(videoGen)); n != 1 {
			t.Errorf("status %d: %d generation requests, want exactly 1", status, n)
		}
	}
}

func TestRateLimitedGenerationExitsWith4(t *testing.T) {
	h := newHarness(t)
	h.useKey()
	h.api.failOnce("POST "+imageGen, 429, 429, 429, 429)
	h.run("image", "x", "-m", "m").wantCode(t, exitcode.RateLimit)
	if n := len(h.api.to(imageGen)); n != 4 {
		t.Errorf("%d attempts, want 4", n)
	}
}

// ---- spending guard ----

// Without a terminal, an estimate above the threshold needs --yes or
// --max-cost. Otherwise the command refuses with exit code 6 — before
// uploading anything or creating a task.
func TestSpendGuardRefusesNonInteractiveOverThreshold(t *testing.T) {
	h := newHarness(t)
	h.useKey()
	h.setInteractive(false)
	h.api.estimate = 3.20
	img := h.file("ref.png", pngFixture)

	for _, args := range [][]string{
		{"image", "a fox", "-m", "m", "--image", img},
		{"video", "a fox", "-m", "m", "--image", img},
		{"video", "a fox", "-m", "m", "--no-wait", "--json"},
	} {
		h.api.reset()
		res := h.run(args...).wantCode(t, exitcode.Billing)
		if !strings.Contains(res.stderr, "$3.20") || !strings.Contains(res.stderr, "--yes") || !strings.Contains(res.stderr, "--max-cost") {
			t.Errorf("%v: stderr does not explain the refusal:\n%s", args, res.stderr)
		}
		for _, c := range h.api.all() {
			if c.Path != estimate {
				t.Errorf("%v: %s %s was sent although spending was refused", args, c.Method, c.Path)
			}
		}
		if res.stdout != "" {
			t.Errorf("%v: stdout = %q, want nothing", args, res.stdout)
		}
	}
}

func TestSpendGuardAllowsBelowThreshold(t *testing.T) {
	h := newHarness(t)
	h.useKey()
	h.setInteractive(false)
	h.api.estimate = 1.00 // exactly at the default threshold: not above it
	h.run("image", "a fox", "-m", "m", "--no-wait").wantCode(t, exitcode.OK)
	if len(h.api.to(imageGen)) != 1 {
		t.Error("a generation at the threshold was not submitted")
	}
}

func TestSpendGuardYesApproves(t *testing.T) {
	h := newHarness(t)
	h.useKey()
	h.setInteractive(false)
	h.api.estimate = 3.20
	h.run("video", "a fox", "-m", "m", "--yes", "--no-wait").wantCode(t, exitcode.OK)
	if len(h.api.to(videoGen)) != 1 {
		t.Error("--yes did not allow the generation")
	}
}

func TestSpendGuardMaxCost(t *testing.T) {
	h := newHarness(t)
	h.useKey()
	h.setInteractive(false)
	h.api.estimate = 3.20

	// Under the cap: proceeds without --yes.
	h.run("video", "a fox", "-m", "m", "--max-cost", "5", "--no-wait").wantCode(t, exitcode.OK)
	if len(h.api.to(videoGen)) != 1 {
		t.Fatal("--max-cost above the estimate did not allow the generation")
	}

	// Over the cap: refused, and --yes does not override a cap.
	for _, extra := range [][]string{nil, {"--yes"}} {
		h.api.reset()
		args := append([]string{"video", "a fox", "-m", "m", "--max-cost", "3", "--no-wait"}, extra...)
		res := h.run(args...).wantCode(t, exitcode.Billing)
		if !strings.Contains(res.stderr, "exceeds --max-cost") {
			t.Errorf("stderr = %s", res.stderr)
		}
		if len(h.api.to(videoGen)) != 0 {
			t.Errorf("%v: a generation above --max-cost was submitted", args)
		}
	}

	// A cap applies below the confirmation threshold too.
	h.api.reset()
	h.api.estimate = 0.50
	h.run("image", "a fox", "-m", "m", "--max-cost", "0.25", "--no-wait").wantCode(t, exitcode.Billing)
	if len(h.api.to(imageGen)) != 0 {
		t.Error("a cheap generation above --max-cost was submitted")
	}
	h.run("image", "a fox", "-m", "m", "--max-cost", "-1").wantCode(t, exitcode.Usage)
}

// When the price is unknown the guard fails closed: no estimate, no spend.
func TestSpendGuardFailsClosedWithoutEstimate(t *testing.T) {
	h := newHarness(t)
	h.useKey()
	h.setInteractive(false)
	h.api.estimateStatus = 400

	h.run("image", "a fox", "-m", "m", "--no-wait").wantCode(t, exitcode.Billing)
	h.run("image", "a fox", "-m", "m", "--no-wait", "--max-cost", "100").wantCode(t, exitcode.Billing)
	if len(h.api.to(imageGen)) != 0 {
		t.Error("a generation without a cost estimate was submitted")
	}
	// --yes is the explicit acceptance of an unknown cost.
	res := h.run("image", "a fox", "-m", "m", "--no-wait", "--yes").wantCode(t, exitcode.OK)
	if !strings.Contains(res.stderr, "could not be estimated") {
		t.Errorf("stderr = %s", res.stderr)
	}
}

func TestSpendGuardConfirmsInteractively(t *testing.T) {
	h := newHarness(t)
	h.useKey()
	h.setInteractive(true)
	h.api.estimate = 3.20

	for _, answer := range []string{"n\n", "\n", "maybe\n", ""} {
		h.api.reset()
		res := h.runIn(answer, "video", "a fox", "-m", "m", "--no-wait").wantCode(t, exitcode.Billing)
		if !strings.Contains(res.stderr, "Continue? [y/N]") {
			t.Errorf("answer %q: no prompt shown:\n%s", answer, res.stderr)
		}
		if len(h.api.to(videoGen)) != 0 {
			t.Errorf("answer %q: the generation was submitted", answer)
		}
	}
	for _, answer := range []string{"y\n", "YES\n"} {
		h.api.reset()
		h.runIn(answer, "video", "a fox", "-m", "m", "--no-wait").wantCode(t, exitcode.OK)
		if len(h.api.to(videoGen)) != 1 {
			t.Errorf("answer %q: the generation was not submitted", answer)
		}
	}
}

func TestSpendThresholdComesFromConfig(t *testing.T) {
	h := newHarness(t)
	h.useKey()
	h.setInteractive(false)
	h.api.estimate = 0.50
	h.run("config", "set", "spend.confirm_above", "0.25").wantCode(t, exitcode.OK)
	h.run("image", "a fox", "-m", "m", "--no-wait").wantCode(t, exitcode.Billing)
	h.run("config", "set", "spend.confirm_above", "10").wantCode(t, exitcode.OK)
	h.run("image", "a fox", "-m", "m", "--no-wait").wantCode(t, exitcode.OK)
}

func TestEstimateUsesRequestParameters(t *testing.T) {
	h := newHarness(t)
	h.useKey()
	clip := h.file("ref.mp4", mp4Fixture)
	h.run("video", "a fox", "-m", "kling", "--duration", "10", "--resolution", "1080p", "-x", "generate_audio=true", "--video", clip, "--no-wait", "--yes").wantCode(t, exitcode.OK)

	body := jsonBody(t, h.api.to(estimate)[0])
	if body["model_id"] != "kling" || body["has_video_material"] != true {
		t.Errorf("estimate body = %v", body)
	}
	dims, _ := body["dimensions"].(map[string]any)
	if dims["duration"] != "10" || dims["resolution"] != "1080p" || dims["generate_audio"] != "true" {
		t.Errorf("dimensions = %v, want the request's scalar parameters as strings", dims)
	}
	for _, leaked := range []string{"prompt", "model", "video_urls"} {
		if _, ok := dims[leaked]; ok {
			t.Errorf("dimension %q should not be sent to the pricing API", leaked)
		}
	}
}

func TestUnknownModelExitsWith5(t *testing.T) {
	h := newHarness(t)
	h.useKey()
	h.api.estimateStatus = 404
	res := h.run("image", "a fox", "-m", "nope", "--yes").wantCode(t, exitcode.NotFound)
	if !strings.Contains(res.stderr, "apipod models list") {
		t.Errorf("stderr = %s", res.stderr)
	}
	if len(h.api.to(imageGen)) != 0 {
		t.Error("a generation for an unknown model was submitted")
	}
}

func TestGenerateUsageErrors(t *testing.T) {
	h := newHarness(t)
	h.useKey()
	for _, args := range [][]string{
		{"image"},                      // no prompt
		{"image", "a fox"},             // no model
		{"image", "   ", "-m", "m"},    // blank prompt
		{"image", "a", "b", "-m", "m"}, // two prompts
		{"image", "a", "-m", "m", "-x", "noequals"},
		{"image", "a", "-m", "m", "-x", "=value"},
		{"image", "a", "-m", "m", "-x", "ref=@"},
		{"video", "a", "b", "-m", "m"},
		{"image", "a", "-m", "m", "--image", "x.png", "-x", "image_urls=@y.png"},
	} {
		h.run(args...).wantCode(t, exitcode.Usage)
	}
	if n := len(h.api.all()); n != 0 {
		t.Errorf("%d requests were sent by commands with usage errors", n)
	}
}

func TestDefaultModelFromConfig(t *testing.T) {
	h := newHarness(t)
	h.useKey()
	h.run("config", "set", "defaults.image_model", "my-default").wantCode(t, exitcode.OK)
	h.run("image", "a fox", "--no-wait").wantCode(t, exitcode.OK)
	if got := jsonBody(t, h.api.to(imageGen)[0])["model"]; got != "my-default" {
		t.Errorf("model = %v", got)
	}
	// A video has its own default.
	h.run("video", "a fox", "--no-wait").wantCode(t, exitcode.Usage)
}

func TestGenerateRequiresCredential(t *testing.T) {
	h := newHarness(t)
	res := h.run("image", "a fox", "-m", "m").wantCode(t, exitcode.Auth)
	if !strings.Contains(res.stderr, "apipod auth login") {
		t.Errorf("stderr = %s", res.stderr)
	}
	if n := len(h.api.all()); n != 0 {
		t.Errorf("%d requests sent without a credential", n)
	}
}

// ---- local reference files ----

func TestLocalReferencesAreUploadedThenReferencedByURL(t *testing.T) {
	h := newHarness(t)
	h.useKey()
	first := h.file("first frame.png", pngFixture)
	clip := h.file("motion.mp4", mp4Fixture)
	voice := h.file("voice.mp3", mp3Fixture)
	mask := h.file("mask.png", pngFixture)

	h.run("video", "she turns", "-m", "m", "--image", first, "--image", "https://example.com/second.png",
		"--video", clip, "--audio", voice, "-x", "mask_url=@"+mask, "--no-wait", "--yes").wantCode(t, exitcode.OK)

	ups := h.api.received()
	if len(ups) != 4 {
		t.Fatalf("%d files uploaded, want 4", len(ups))
	}
	for _, up := range ups {
		if up.Field != "file" {
			t.Errorf("upload used form field %q, want file", up.Field)
		}
		if strings.ContainsAny(up.Filename, `/\`) {
			t.Errorf("upload filename %q reveals a local path", up.Filename)
		}
	}
	if ups[0].Filename != "first frame.png" || ups[0].ContentType != "image/png" || string(ups[0].Data) != string(pngFixture) {
		t.Errorf("first upload = %q %q (%d bytes)", ups[0].Filename, ups[0].ContentType, len(ups[0].Data))
	}

	body := jsonBody(t, h.api.to(videoGen)[0])
	images, _ := body["image_urls"].([]any)
	if len(images) != 2 || images[0] != "https://cdn.example.test/uploads/1" || images[1] != "https://example.com/second.png" {
		t.Errorf("image_urls = %v, want the uploaded URL then the remote one, in order", images)
	}
	if v, _ := body["video_urls"].([]any); len(v) != 1 || v[0] != "https://cdn.example.test/uploads/2" {
		t.Errorf("video_urls = %v", body["video_urls"])
	}
	if a, _ := body["audio_urls"].([]any); len(a) != 1 || a[0] != "https://cdn.example.test/uploads/3" {
		t.Errorf("audio_urls = %v", body["audio_urls"])
	}
	// A single -x key=@file on a non-list key is sent as a plain string.
	if body["mask_url"] != "https://cdn.example.test/uploads/4" {
		t.Errorf("mask_url = %#v", body["mask_url"])
	}
	// No file content or local path travels in the JSON request.
	raw := string(h.api.to(videoGen)[0].Body)
	if strings.Contains(raw, "base64") || strings.Contains(raw, filepath.Dir(first)) {
		t.Errorf("the generation request embeds local data: %s", raw)
	}
}

func TestExtraFileOnListKeyIsSentAsList(t *testing.T) {
	h := newHarness(t)
	h.useKey()
	a, b := h.file("a.png", pngFixture), h.file("b.png", pngFixture)
	h.run("image", "merge", "-m", "m", "-x", "reference_urls=@"+a, "-x", "style=@"+a, "-x", "style=@"+b, "--no-wait").wantCode(t, exitcode.OK)
	body := jsonBody(t, h.api.to(imageGen)[0])
	if refs, ok := body["reference_urls"].([]any); !ok || len(refs) != 1 {
		t.Errorf("reference_urls = %#v, want a one-element list", body["reference_urls"])
	}
	if style, ok := body["style"].([]any); !ok || len(style) != 2 {
		t.Errorf("style = %#v, want both files as a list", body["style"])
	}
}

// A bad reference stops the command before any request: nothing is uploaded,
// estimated or generated.
func TestBadLocalReferenceSendsNothing(t *testing.T) {
	h := newHarness(t)
	h.useKey()
	good := h.file("ok.png", pngFixture)
	secret := h.file("id_rsa.png", []byte("-----BEGIN OPENSSH PRIVATE KEY-----\nAAAA\n-----END OPENSSH PRIVATE KEY-----\n"))
	clip := h.file("clip.mp4", mp4Fixture)

	for _, args := range [][]string{
		{"image", "x", "-m", "m", "--image", good, "--image", secret, "--yes"},
		{"image", "x", "-m", "m", "--image", filepath.Dir(good), "--yes"},
		{"image", "x", "-m", "m", "--image", clip, "--yes"}, // a video passed as an image
		{"video", "x", "-m", "m", "--audio", good, "--yes"},
		{"video", "x", "-m", "m", "-x", "ref=@" + secret, "--yes"},
		{"image", "x", "-m", "m", "--image", filepath.Join(t.TempDir(), "missing.png"), "--yes"},
		{"image", "x", "-m", "m", "--image", "data:image/png;base64,AAAA", "--yes"},
	} {
		h.run(args...).wantCode(t, exitcode.Validation)
	}
	if n := len(h.api.all()); n != 0 {
		t.Errorf("%d requests were sent although a reference was rejected", n)
	}
}

func TestTooManyLocalReferences(t *testing.T) {
	h := newHarness(t)
	h.useKey()
	args := []string{"image", "x", "-m", "m", "--yes"}
	for i := 0; i < 21; i++ {
		args = append(args, "--image", h.file("f.png", pngFixture))
	}
	h.run(args...).wantCode(t, exitcode.Validation)
	if n := len(h.api.all()); n != 0 {
		t.Errorf("%d requests were sent", n)
	}
}

// A model whose input is a single URL (video_url, not video_urls) is served
// through -x key=@file: the file is uploaded, validated like any other, and
// its URL is sent as a plain string under exactly that key.
func TestSingleURLInputThroughExtraFile(t *testing.T) {
	h := newHarness(t)
	h.useKey()
	clip := h.file("clip.mp4", mp4Fixture)
	h.run("video", "extend the shot", "-m", "m", "-x", "video_url=@"+clip, "--no-wait", "--yes").wantCode(t, exitcode.OK)

	body := jsonBody(t, h.api.to(videoGen)[0])
	if body["video_url"] != "https://cdn.example.test/uploads/1" {
		t.Errorf("video_url = %#v, want the uploaded URL as a string", body["video_url"])
	}
	if _, present := body["video_urls"]; present {
		t.Error("video_urls was sent although only video_url was given")
	}
	// A reference video is billed extra by some models, whichever key carries it.
	if est := jsonBody(t, h.api.to(estimate)[0]); est["has_video_material"] != true {
		t.Errorf("estimate body = %v, want has_video_material", est)
	}
}
