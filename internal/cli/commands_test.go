package cli

import (
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"

	"github.com/geniuspath/apipod-cli/internal/exitcode"
	"github.com/geniuspath/apipod-cli/internal/testutil"
)

// ---- upload ----

func TestUploadPrintsURLs(t *testing.T) {
	h := newHarness(t)
	h.useKey()
	img, clip := h.file("photo.png", pngFixture), h.file("clip.mp4", mp4Fixture)
	res := h.run("upload", img, clip, "--json").wantCode(t, exitcode.OK)

	var files []uploadedFile
	res.decode(t, &files)
	if len(files) != 2 {
		t.Fatalf("files = %+v", files)
	}
	if files[0].File != img || files[0].URL != "https://cdn.example.test/uploads/1" || files[0].Type != "image" || files[0].ContentType != "image/png" || files[0].Bytes != int64(len(pngFixture)) {
		t.Errorf("first = %+v", files[0])
	}
	if files[1].Type != "video" || files[1].ContentType != "video/mp4" {
		t.Errorf("second = %+v", files[1])
	}
	ups := h.api.received()
	if len(ups) != 2 || ups[0].Filename != "photo.png" || ups[1].Filename != "clip.mp4" {
		t.Errorf("server received %+v", ups)
	}
	// The public-URL notice is shown, on stderr, without breaking the JSON.
	if !strings.Contains(res.stderr, "public URLs") {
		t.Errorf("stderr lacks the public URL notice:\n%s", res.stderr)
	}
}

// Whatever is wrong with a path, the command stops before sending anything —
// including the good files named next to it.
func TestUploadRejectsLocallyWithZeroRequests(t *testing.T) {
	h := newHarness(t)
	h.useKey()
	good := h.file("ok.png", pngFixture)
	dir := filepath.Dir(good)

	cases := map[string]string{
		"private key":            h.file("id_rsa", testutil.PrivateKey),
		"private key named .png": h.file("id_rsa.png", testutil.PrivateKey),
		".env":                   h.file(".env", testutil.DotEnv),
		"pem":                    h.file("server.pem", []byte("-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n")),
		"html named .jpg":        h.file("page.jpg", testutil.HTML),
		"svg":                    h.file("logo.svg", testutil.SVG),
		"executable named .mp4":  h.file("tool.mp4", testutil.ELF),
		"pdf":                    h.file("doc.pdf", testutil.PDF),
		"directory":              dir,
		"missing file":           filepath.Join(dir, "missing.png"),
		"empty file":             h.file("empty.png", nil),
		"a URL":                  "https://example.com/a.png",
	}
	if goruntime.GOOS != "windows" {
		link := filepath.Join(t.TempDir(), "link.png")
		if err := os.Symlink(good, link); err != nil {
			t.Fatal(err)
		}
		cases["symlink to a valid image"] = link
		cases["device file"] = "/dev/null"

		big := h.file("huge.png", pngFixture)
		if err := os.Truncate(big, 20*1024*1024+1); err != nil {
			t.Fatal(err)
		}
		cases["image over 20 MB"] = big
	}

	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			h.api.reset()
			// The bad path comes last: the good file before it must not be sent either.
			res := h.run("upload", good, path).wantCode(t, exitcode.Validation)
			if n := len(h.api.all()); n != 0 {
				t.Errorf("%d requests were sent although %q was rejected", n, path)
			}
			if res.stdout != "" {
				t.Errorf("stdout = %q", res.stdout)
			}
		})
	}
}

func TestUploadRejectsGlobsAndTooManyFiles(t *testing.T) {
	h := newHarness(t)
	h.useKey()
	dir := t.TempDir()
	for _, name := range []string{"a.png", "b.png"} {
		if err := os.WriteFile(filepath.Join(dir, name), pngFixture, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The CLI never expands patterns itself: an unexpanded glob is just a
	// file name that does not exist.
	h.run("upload", filepath.Join(dir, "*.png")).wantCode(t, exitcode.Validation)
	h.run("upload", filepath.Join(dir, "**")).wantCode(t, exitcode.Validation)

	args := []string{"upload"}
	for i := 0; i < 21; i++ {
		args = append(args, filepath.Join(dir, "a.png"))
	}
	res := h.run(args...).wantCode(t, exitcode.Validation)
	if !strings.Contains(res.stderr, "at most 20") {
		t.Errorf("stderr = %s", res.stderr)
	}
	h.run("upload").wantCode(t, exitcode.Usage)
	if n := len(h.api.all()); n != 0 {
		t.Errorf("%d requests were sent", n)
	}
}

func TestUploadRequiresCredentialBeforeSending(t *testing.T) {
	h := newHarness(t)
	h.run("upload", h.file("a.png", pngFixture)).wantCode(t, exitcode.Auth)
	if n := len(h.api.all()); n != 0 {
		t.Errorf("%d requests sent without a credential", n)
	}
}

func TestUploadServerRejectionIsReported(t *testing.T) {
	h := newHarness(t)
	h.useKey()
	img := h.file("a.png", pngFixture)
	h.api.failOnce("POST "+uploads, 413)
	h.run("upload", img).wantCode(t, exitcode.Validation)
	if n := len(h.api.to(uploads)); n != 1 {
		t.Errorf("%d upload attempts after a 413, want 1", n)
	}
	h.api.reset()
	h.api.failOnce("POST "+uploads, 429)
	h.run("upload", img).wantCode(t, exitcode.OK)
	if n := len(h.api.to(uploads)); n != 2 {
		t.Errorf("%d upload attempts after one 429, want 2", n)
	}
}

func TestPublicUploadNoticeIsShownOnceToPeople(t *testing.T) {
	h := newHarness(t)
	h.useKey()
	img := h.file("a.png", pngFixture)
	const notice = "public URLs"

	// Scripts see it every time and never use it up.
	h.setInteractive(false)
	for i := 0; i < 2; i++ {
		if res := h.run("upload", img).wantCode(t, exitcode.OK); !strings.Contains(res.stderr, notice) {
			t.Errorf("non-interactive run %d: notice missing", i+1)
		}
	}
	// A person sees it once.
	h.setInteractive(true)
	if res := h.run("upload", img).wantCode(t, exitcode.OK); !strings.Contains(res.stderr, notice) {
		t.Error("first interactive run: notice missing")
	}
	if res := h.run("upload", img).wantCode(t, exitcode.OK); strings.Contains(res.stderr, notice) {
		t.Error("second interactive run: notice repeated")
	}
}

// ---- job ----

func TestJobGet(t *testing.T) {
	h := newHarness(t)
	h.useKey()
	h.api.jobStatuses = []string{"processing"}
	res := h.run("job", "get", "vid_1", "--json").wantCode(t, exitcode.OK)
	var job jobView
	res.decode(t, &job)
	if job.JobID != "vid_1" || job.Kind != "video" || job.Status != "processing" || job.Progress == nil || *job.Progress != 40 {
		t.Errorf("job = %+v", job.Job)
	}
	if len(h.cdnRequests()) != 0 {
		t.Error("job get downloaded results")
	}

	// An image id is found through the video-then-image lookup.
	h.api.jobStatuses = []string{"completed"}
	res = h.run("job", "get", "img_1", "--json").wantCode(t, exitcode.OK)
	res.decode(t, &job)
	if job.Kind != "image" || job.Status != "completed" || len(job.Results) != 1 || job.Results[0].Path != "" {
		t.Errorf("job = %+v", job.Job)
	}
	h.run("job", "get", "unknown_id").wantCode(t, exitcode.NotFound)
	h.run("job", "get").wantCode(t, exitcode.Usage)
}

func TestJobWaitDownloadsResults(t *testing.T) {
	h := newHarness(t)
	h.useKey()
	h.api.resultURLs = []string{h.cdn.URL + "/r/a.png", h.cdn.URL + "/r/b.png"}
	out := t.TempDir()
	res := h.run("job", "wait", "img_1", "-o", out, "--json").wantCode(t, exitcode.OK)
	var job jobView
	res.decode(t, &job)
	if len(job.Results) != 2 || job.Results[0].Path != filepath.Join(out, "img_1-1.png") || job.Results[1].Path != filepath.Join(out, "img_1-2.png") {
		t.Errorf("results = %+v", job.Results)
	}
	// After the first lookup settles the kind, only that endpoint is polled.
	if n := len(h.api.to("/v1/videos/status/img_1")); n != 1 {
		t.Errorf("the video endpoint was polled %d times for an image job, want 1", n)
	}
}

func TestJobWaitTimeoutExitsWith7(t *testing.T) {
	h := newHarness(t)
	h.useKey()
	h.api.jobStatuses = []string{"processing"}
	res := h.run("job", "wait", "vid_1", "--timeout", "40ms").wantCode(t, exitcode.Timeout)
	if !strings.Contains(res.stderr, "apipod job wait vid_1") {
		t.Errorf("stderr does not say how to resume:\n%s", res.stderr)
	}
}

func TestFailedJobExitsNonZeroAndReportsReason(t *testing.T) {
	h := newHarness(t)
	h.useKey()
	h.api.jobStatuses = []string{"processing", "failed"}
	h.api.jobError = "Upstream request failed. \x1b[2J\x1b]0;pwned\x07Please retry later."
	res := h.run("job", "wait", "vid_1", "--json").wantCode(t, exitcode.General)
	var job jobView
	res.decode(t, &job)
	if job.Status != "failed" || !strings.Contains(job.ErrorMessage, "Upstream request failed.") {
		t.Errorf("job = %+v", job.Job)
	}
	if !strings.Contains(res.stderr, "refunded") {
		t.Errorf("stderr = %s", res.stderr)
	}
	if len(h.cdnRequests()) != 0 {
		t.Error("a failed job triggered a download")
	}
}

func TestDownloadsDoNotOverwriteWithoutForce(t *testing.T) {
	h := newHarness(t)
	h.useKey()
	h.api.jobStatuses = []string{"completed"}
	out := t.TempDir()
	existing := filepath.Join(out, "img_1-1.png")
	if err := os.WriteFile(existing, []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := h.run("job", "wait", "img_1", "-o", out).wantCode(t, exitcode.General)
	if got, _ := os.ReadFile(existing); string(got) != "precious" {
		t.Errorf("the existing file was overwritten: %q", got)
	}
	if !strings.Contains(res.stderr, "already exists") || !strings.Contains(res.stderr, "--force") {
		t.Errorf("stderr = %s", res.stderr)
	}
	// The URL is still printed, so the paid-for result is not lost.
	if !strings.Contains(res.stdout, h.cdn.URL) {
		t.Errorf("stdout lacks the result URL:\n%s", res.stdout)
	}

	h.run("job", "wait", "img_1", "-o", out, "--force").wantCode(t, exitcode.OK)
	if got, _ := os.ReadFile(existing); string(got) != string(pngFixture) {
		t.Error("--force did not replace the file")
	}
}

func TestResultOverPlainHTTPIsNotDownloaded(t *testing.T) {
	h := newHarness(t)
	h.useKey()
	h.api.jobStatuses = []string{"completed"}
	h.api.resultURLs = []string{"http://cdn.example.test/r/a.png"}
	out := t.TempDir()
	res := h.run("job", "wait", "img_1", "-o", out).wantCode(t, exitcode.General)
	if !strings.Contains(res.stderr, "only https") {
		t.Errorf("stderr = %s", res.stderr)
	}
	if entries, _ := os.ReadDir(out); len(entries) != 0 {
		t.Errorf("files were written for a refused download: %v", entries)
	}
}

// ---- terminal safety ----

// Text controlled by the server never reaches the terminal with escape
// sequences in it, on stdout or stderr.
func TestServerStringsAreSanitizedBeforeTheTerminal(t *testing.T) {
	const evil = "\x1b]0;pwned\x07\x1b[2J\x1b[1;1Hevil\x1b[31m"
	h := newHarness(t)
	h.useKey()
	h.api.modelsJSON = `[{"model_id":"m1` + `\u001b[2J","display_name":"` + `\u001b]0;pwned\u0007Nice\u001b[31m Model","classifications":["text_to_image"],"input_types":["text"],"lifecycle":"ga","provider_id":"prov\u001b[0m"}]`
	h.api.schemaJSON = `{"components":{"schemas":{"Input":{"properties":{"style":{"type":"string","description":"pick\u001b[2J one","enum":["a\u001b[31m","b"]}}}}}}`
	h.api.jobStatuses = []string{"failed"}
	h.api.jobError = evil

	for _, args := range [][]string{
		{"models", "list"},
		{"models", "params", "m1"},
		{"job", "get", "vid_1"},
		{"job", "wait", "vid_1"},
		{"assets", "get", "nope\x1b[2J"},
	} {
		res := h.run(args...)
		for name, stream := range map[string]string{"stdout": res.stdout, "stderr": res.stderr} {
			if strings.ContainsAny(stream, "\x1b\x07") {
				t.Errorf("%v: %s carries terminal escape bytes: %q", args, name, stream)
			}
		}
	}
	res := h.run("models", "list").wantCode(t, exitcode.OK)
	if !strings.Contains(res.stdout, "Nice Model") {
		t.Errorf("the printable part of the name was lost:\n%s", res.stdout)
	}

	// API error messages are sanitized too.
	h.api.failOnce("GET /v1/account/balance", 400)
	h.api.estimateStatus = 0
	res = h.run("balance")
	if strings.ContainsAny(res.stderr, "\x1b\x07") {
		t.Errorf("error output carries escape bytes: %q", res.stderr)
	}
}

// ---- models, cost, balance ----

func TestModelsList(t *testing.T) {
	h := newHarness(t)
	h.api.modelsJSON = `[
		{"model_id":"vid-b","display_name":"Video B","classifications":["text_to_video"],"input_types":["text"],"lifecycle":"ga","provider_id":"p","series_id":"s1"},
		{"model_id":"img-a","display_name":null,"classifications":["text_to_image","image_to_image"],"input_types":["text","image"],"lifecycle":"beta","provider_id":"p"},
		{"model_id":"llm-x","classifications":["llm"],"lifecycle":"ga","provider_id":"p"},
		{"model_id":"old","classifications":["text_to_image"],"lifecycle":"offline","provider_id":"p"}
	]`
	// The catalog is public: no credential needed, none sent.
	res := h.run("models", "list", "--json").wantCode(t, exitcode.OK)
	var out struct {
		Models []modelSummary `json:"models"`
	}
	res.decode(t, &out)
	if len(out.Models) != 2 || out.Models[0].ID != "img-a" || out.Models[1].ID != "vid-b" {
		t.Fatalf("models = %+v, want img-a then vid-b (LLM and offline models excluded)", out.Models)
	}
	if out.Models[0].Category != "image" || out.Models[0].Name != "img-a" || out.Models[1].Category != "video" || out.Models[1].Series != "s1" {
		t.Errorf("models = %+v", out.Models)
	}
	for _, c := range h.api.all() {
		if c.Header.Get("Authorization") != "" {
			t.Error("the public catalog request carried a credential")
		}
	}

	h.api.reset()
	h.run("models", "list", "--category", "video").wantCode(t, exitcode.OK)
	var classes []string
	for _, c := range h.api.to("/public/models") {
		classes = append(classes, c.Query.Get("classification"))
	}
	if strings.Join(classes, ",") != "text_to_video,image_to_video,video_to_video" {
		t.Errorf("classification filters = %v", classes)
	}
	h.run("models", "list", "--category", "llm").wantCode(t, exitcode.Usage)
}

func TestModelsParams(t *testing.T) {
	h := newHarness(t)
	h.api.schemaJSON = `{"components":{"schemas":{"Input":{"type":"object","required":["prompt"],"x-apipod-order-properties":["prompt","duration"],
		"properties":{
			"model":{"type":"string"},
			"aspect_ratio":{"type":"string","enum":["16:9","9:16"],"default":"16:9","description":"Output aspect ratio."},
			"duration":{"type":"integer","minimum":3,"maximum":15,"default":5},
			"prompt":{"type":"string","description":"What to generate."}
		}}}}}`
	res := h.run("models", "params", "kling").wantCode(t, exitcode.OK)
	lines := strings.Split(strings.TrimSpace(res.stdout), "\n")
	if len(lines) != 4 {
		t.Fatalf("table has %d lines, want a header and 3 parameters (model hidden):\n%s", len(lines), res.stdout)
	}
	// Declared order first, the rest alphabetically.
	for i, want := range []string{"prompt", "duration", "aspect_ratio"} {
		if !strings.HasPrefix(lines[i+1], want) {
			t.Errorf("row %d = %q, want it to start with %s", i+1, lines[i+1], want)
		}
	}
	if !strings.Contains(lines[1], "yes") || !strings.Contains(lines[2], "3 to 15") || !strings.Contains(lines[3], "16:9 | 9:16") {
		t.Errorf("table:\n%s", res.stdout)
	}

	var out map[string]any
	h.run("models", "params", "kling", "--json").wantCode(t, exitcode.OK).decode(t, &out)
	if out["model"] != "kling" || out["input_schema"] == nil {
		t.Errorf("json = %v", out)
	}
	h.api.schemaJSON = ""
	h.run("models", "params", "nope").wantCode(t, exitcode.NotFound)
}

func TestCost(t *testing.T) {
	h := newHarness(t)
	h.useKey()
	h.api.estimate, h.api.balance = 0.35, 0.20
	res := h.run("cost", "kling", "-d", "duration=10", "-d", "resolution=1080p", "--has-video", "--json").wantCode(t, exitcode.OK)
	var out costResult
	res.decode(t, &out)
	if out.Model != "kling" || out.EstimatedUSD != 0.35 || out.BalanceUSD == nil || *out.BalanceUSD != 0.20 || out.Sufficient == nil || *out.Sufficient {
		t.Errorf("cost = %+v", out)
	}
	body := jsonBody(t, h.api.to(estimate)[0])
	dims, _ := body["dimensions"].(map[string]any)
	if body["model_id"] != "kling" || body["has_video_material"] != true || dims["duration"] != "10" || dims["resolution"] != "1080p" {
		t.Errorf("estimate body = %v", body)
	}
	h.run("cost", "kling", "-d", "bad").wantCode(t, exitcode.Usage)
	h.run("cost").wantCode(t, exitcode.Usage)
}

// Without a credential the public price is still quoted; only the balance
// is missing.
func TestCostWorksSignedOut(t *testing.T) {
	h := newHarness(t)
	res := h.run("cost", "kling", "--json").wantCode(t, exitcode.OK)
	var out costResult
	res.decode(t, &out)
	if out.EstimatedUSD != 0.10 || out.BalanceUSD != nil || out.Sufficient != nil {
		t.Errorf("cost = %+v", out)
	}
	if len(h.api.to("/v1/account/balance")) != 0 {
		t.Error("the balance was requested without a credential")
	}
}

func TestBalance(t *testing.T) {
	h := newHarness(t)
	h.useKey()
	t.Setenv(EnvConsoleURL, "http://localhost:3000")
	opened := ""
	h.browser = func(u string) error { opened = u; return nil }

	res := h.run("balance", "--json").wantCode(t, exitcode.OK)
	var out balanceResult
	res.decode(t, &out)
	if out.BalanceUSD != 25 || out.Level != "L2" || out.TopUpURL != "http://localhost:3000/console/billing" {
		t.Errorf("balance = %+v", out)
	}
	if opened != "" {
		t.Error("the browser was opened without --open")
	}
	h.run("balance", "--open").wantCode(t, exitcode.OK)
	if opened != "http://localhost:3000/console/billing" {
		t.Errorf("--open opened %q", opened)
	}
}

// ---- assets ----

func TestAssetsCreateFromLocalFileAndWait(t *testing.T) {
	h := newHarness(t)
	h.useKey()
	face := h.file("face.png", pngFixture)
	res := h.run("assets", "create", face, "--name", "lead actor", "--wait", "--json").wantCode(t, exitcode.OK)

	var asset map[string]any
	res.decode(t, &asset)
	if asset["assetId"] != "asset_1" || asset["status"] != "ACTIVE" || asset["ref"] != "assetId://asset_1" {
		t.Errorf("asset = %v", asset)
	}
	if ups := h.api.received(); len(ups) != 1 || ups[0].Filename != "face.png" {
		t.Errorf("uploads = %+v", ups)
	}
	var created map[string]any
	for _, c := range h.api.to("/v1/assets") {
		if c.Method == "POST" {
			created = jsonBody(t, c)
		}
	}
	// The type is detected from the file; the URL is the uploaded one.
	if created["assetType"] != "Image" || created["url"] != "https://cdn.example.test/uploads/1" || created["name"] != "lead actor" {
		t.Errorf("create body = %v", created)
	}
}

func TestAssetsCreateRejectedReviewExitsWith8(t *testing.T) {
	h := newHarness(t)
	h.useKey()
	h.api.assetStatuses = []string{"PROCESSING", "FAILED"}
	res := h.run("assets", "create", "https://example.com/face.jpg", "--type", "image", "--wait").wantCode(t, exitcode.Validation)
	if !strings.Contains(res.stderr, "face not detected") {
		t.Errorf("stderr = %s", res.stderr)
	}
	if len(h.api.received()) != 0 {
		t.Error("a URL source was uploaded")
	}
}

func TestAssetsCreateValidation(t *testing.T) {
	h := newHarness(t)
	h.useKey()
	clip := h.file("clip.mp4", mp4Fixture)
	h.run("assets", "create", "https://example.com/a.jpg").wantCode(t, exitcode.Usage) // URL needs --type
	h.run("assets", "create", "https://example.com/a.jpg", "--type", "pdf").wantCode(t, exitcode.Usage)
	h.run("assets", "create", "assetId://asset_1", "--type", "image").wantCode(t, exitcode.Usage)
	h.run("assets", "create", clip, "--type", "image").wantCode(t, exitcode.Validation)              // content disagrees with --type
	h.run("assets", "create", h.file("k.png", testutil.PrivateKey)).wantCode(t, exitcode.Validation) // not media
	if n := len(h.api.all()); n != 0 {
		t.Errorf("%d requests were sent by rejected commands", n)
	}
}

func TestAssetsListGetDelete(t *testing.T) {
	h := newHarness(t)
	h.useKey()
	res := h.run("assets", "list", "--type", "image", "--status", "ACTIVE").wantCode(t, exitcode.OK)
	if !strings.Contains(res.stdout, "assetId://asset_1") {
		t.Errorf("list output lacks the reference:\n%s", res.stdout)
	}
	q := h.api.to("/v1/assets")[0].Query
	if q.Get("type") != "Image" || q.Get("status") != "ACTIVE" {
		t.Errorf("list query = %v", q)
	}
	h.run("assets", "list", "--type", "pdf").wantCode(t, exitcode.Usage)

	// The reference form printed by the CLI is accepted back.
	h.run("assets", "get", "assetId://asset_1").wantCode(t, exitcode.OK)
	h.run("assets", "get", "missing").wantCode(t, exitcode.NotFound)

	var del map[string]any
	h.run("assets", "delete", "assetId://asset_1", "--json").wantCode(t, exitcode.OK).decode(t, &del)
	if del["deleted"] != true || del["assetId"] != "asset_1" {
		t.Errorf("delete = %v", del)
	}
	h.run("assets", "delete", "missing").wantCode(t, exitcode.NotFound)
}

// ---- config, version, completion, errors ----

func TestConfigCommands(t *testing.T) {
	h := newHarness(t)
	h.run("config", "set", "defaults.video_model", "kling").wantCode(t, exitcode.OK)
	if res := h.run("config", "get", "defaults.video_model").wantCode(t, exitcode.OK); strings.TrimSpace(res.stdout) != "kling" {
		t.Errorf("get = %q", res.stdout)
	}
	if res := h.run("config", "get", "spend.confirm_above").wantCode(t, exitcode.OK); strings.TrimSpace(res.stdout) != "1" {
		t.Errorf("default threshold = %q", res.stdout)
	}
	var list struct {
		Path   string            `json:"path"`
		Values map[string]string `json:"values"`
	}
	h.run("config", "list", "--json").wantCode(t, exitcode.OK).decode(t, &list)
	if list.Values["defaults.video_model"] != "kling" || list.Path != filepath.Join(h.cfgDir, "config.toml") {
		t.Errorf("list = %+v", list)
	}
	h.run("config", "set", "nope", "x").wantCode(t, exitcode.Usage)
	h.run("config", "set", "spend.confirm_above", "-3").wantCode(t, exitcode.Usage)
	h.run("config", "get").wantCode(t, exitcode.Usage)

	// A secret has no home in config.toml.
	h.run("config", "set", "api_key", testKey).wantCode(t, exitcode.Usage)
	if data, _ := os.ReadFile(filepath.Join(h.cfgDir, "config.toml")); strings.Contains(string(data), testKey) {
		t.Error("a key was written to config.toml")
	}
}

func TestDefaultOutputJSONFromConfig(t *testing.T) {
	h := newHarness(t)
	h.run("config", "set", "defaults.output", "json").wantCode(t, exitcode.OK)
	var v map[string]string
	h.run("version").wantCode(t, exitcode.OK).decode(t, &v)
	if v["version"] != "9.9.9-test" {
		t.Errorf("version = %v", v)
	}
}

func TestVersion(t *testing.T) {
	h := newHarness(t)
	res := h.run("version").wantCode(t, exitcode.OK)
	if !strings.Contains(res.stdout, "apipod 9.9.9-test") || !strings.Contains(res.stdout, "abc1234") {
		t.Errorf("version = %q", res.stdout)
	}
	if n := len(h.api.all()); n != 0 {
		t.Errorf("version made %d network requests", n)
	}
}

func TestCompletionScripts(t *testing.T) {
	h := newHarness(t)
	for _, shell := range []string{"bash", "zsh", "fish", "powershell"} {
		res := h.run("completion", shell).wantCode(t, exitcode.OK)
		if !strings.Contains(res.stdout, "apipod") || len(res.stdout) < 500 {
			t.Errorf("completion %s produced %d bytes", shell, len(res.stdout))
		}
	}
}

func TestUsageErrorsExitWith2(t *testing.T) {
	h := newHarness(t)
	for _, args := range [][]string{
		{"nosuchcommand"},
		{"models", "nosuchsub"},
		{"balance", "--nosuchflag"},
		{"job", "wait", "x", "--timeout", "soon"},
		{"balance", "extra"},
		{"image", "x", "-m"},
	} {
		h.run(args...).wantCode(t, exitcode.Usage)
	}
}

// In --json mode a failure is one JSON object on stderr, and stdout stays
// empty so a pipeline never parses half an answer.
func TestErrorsAreJSONInJSONMode(t *testing.T) {
	h := newHarness(t)
	h.useKey()
	h.api.failOnce("GET /v1/account/balance", 429, 429, 429, 429)
	res := h.run("balance", "--json").wantCode(t, exitcode.RateLimit)
	if res.stdout != "" {
		t.Errorf("stdout = %q, want nothing on failure", res.stdout)
	}
	var out struct {
		Error struct {
			Message    string `json:"message"`
			ExitCode   int    `json:"exit_code"`
			HTTPStatus int    `json:"http_status"`
		} `json:"error"`
	}
	dec := strings.TrimSpace(res.stderr)
	if err := jsonUnmarshal(dec, &out); err != nil {
		t.Fatalf("stderr is not a JSON object: %v\n%s", err, res.stderr)
	}
	if out.Error.ExitCode != exitcode.RateLimit || out.Error.HTTPStatus != 429 || out.Error.Message == "" {
		t.Errorf("error = %+v", out.Error)
	}
}

func TestQuietSuppressesProgress(t *testing.T) {
	h := newHarness(t)
	h.useKey()
	res := h.run("image", "a fox", "-m", "m", "-o", t.TempDir(), "--quiet").wantCode(t, exitcode.OK)
	if res.stderr != "" {
		t.Errorf("--quiet still wrote to stderr: %q", res.stderr)
	}
	if !strings.Contains(res.stdout, "img_1") {
		t.Errorf("stdout = %s", res.stdout)
	}
}

// A job-related failure repeats the job id in the JSON error, so an agent
// can resume instead of generating (and paying) again.
func TestJobErrorsCarryTheJobID(t *testing.T) {
	h := newHarness(t)
	h.useKey()
	h.api.jobStatuses = []string{"processing"}
	res := h.run("video", "a fox", "-m", "m", "--timeout", "40ms", "--json").wantCode(t, exitcode.Timeout)
	var out struct {
		Error struct {
			JobID    string `json:"job_id"`
			ExitCode int    `json:"exit_code"`
		} `json:"error"`
	}
	if err := jsonUnmarshal(strings.TrimSpace(res.stderr[strings.LastIndex(res.stderr, `{"error"`):]), &out); err != nil {
		t.Fatalf("no JSON error on stderr: %v\n%s", err, res.stderr)
	}
	if out.Error.JobID != "vid_1" || out.Error.ExitCode != exitcode.Timeout {
		t.Errorf("error = %+v", out.Error)
	}
}

func TestAPIKeyFlagHintPointsToTheEnvironment(t *testing.T) {
	h := newHarness(t)
	res := h.run("balance", "--api-key", testKey).wantCode(t, exitcode.Usage)
	res.noSecret(t, testKey)
	if !strings.Contains(res.stderr, "APIPOD_API_KEY") || !strings.Contains(res.stderr, "auth login") {
		t.Errorf("stderr = %s", res.stderr)
	}
}
