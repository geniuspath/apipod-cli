package cli

import (
	"strings"
	"testing"

	"github.com/geniuspath/apipod-cli/internal/exitcode"
)

// --max-cost must be a real, finite, non-negative number. NaN compares false
// against everything, so it would pass both "negative?" and "over the cap?".
func TestMaxCostRejectsNonFiniteValues(t *testing.T) {
	h := newHarness(t)
	h.useKey()
	h.setInteractive(false)
	h.api.estimate = 50
	for _, v := range []string{"NaN", "nan", "Inf", "+Inf", "-Inf", "-1"} {
		h.api.reset()
		h.run("image", "a fox", "-m", "m", "--max-cost", v, "--no-wait").wantCode(t, exitcode.Usage)
		if n := len(h.api.all()); n != 0 {
			t.Errorf("--max-cost %s: %d requests were sent", v, n)
		}
	}
}

// The server matches request fields case-insensitively but prices dimensions
// by exact key. A key in another case would be billed and not estimated, so
// -x accepts lowercase snake_case names only.
func TestExtraKeysMustBeLowercase(t *testing.T) {
	h := newHarness(t)
	h.useKey()
	h.setInteractive(false)
	for _, pair := range []string{
		"N=10", "Duration=15", "RESOLUTION=4k", "Callback_URL=https://attacker.example/h",
		"callbackUrl=https://attacker.example/h", "reſolution=4k", "Key=1", "a-b=1", "a b=1", "1n=2", "n.x=1",
	} {
		h.api.reset()
		res := h.run("image", "a fox", "-m", "m", "--max-cost", "0.20", "-x", pair, "--no-wait").wantCode(t, exitcode.Usage)
		if n := len(h.api.all()); n != 0 {
			t.Errorf("-x %q: %d requests were sent", pair, n)
		}
		if !strings.Contains(res.stderr, "lowercase") {
			t.Errorf("-x %q: stderr does not explain the rule: %s", pair, res.stderr)
		}
	}
	// The same names in their canonical form are accepted and priced.
	h.api.reset()
	h.run("image", "a fox", "-m", "m", "--max-cost", "0.20", "-x", "n=10", "-x", "seed_2=7", "--no-wait").wantCode(t, exitcode.OK)
	dims, _ := jsonBody(t, h.api.to(estimate)[0])["dimensions"].(map[string]any)
	if dims["n"] != "10" {
		t.Errorf("dimensions = %v, want n priced", dims)
	}
}

// A reference video is billed extra by some models, whichever parameter
// carries it. The estimate must know about it.
func TestVideoInputThroughExtrasIsEstimated(t *testing.T) {
	cases := [][]string{
		{"-x", "video_url=https://example.com/clip.mp4"},
		{"-x", `video_urls=["https://example.com/clip.mp4"]`},
		{"-x", `reference_videos=["https://example.com/clip.mp4"]`},
		{"-x", "source_video_url=https://example.com/clip.mp4"},
		{"-x", `reference_video_urls=["https://example.com/clip.mp4"]`},
	}
	for _, extra := range cases {
		h := newHarness(t)
		h.useKey()
		args := append([]string{"video", "extend", "-m", "m", "--no-wait", "--yes"}, extra...)
		h.run(args...).wantCode(t, exitcode.OK)
		if est := jsonBody(t, h.api.to(estimate)[0]); est["has_video_material"] != true {
			t.Errorf("%v: estimate body = %v, want has_video_material", extra, est)
		}
	}
	// Parameters that merely mention video, or are empty, do not count.
	for _, extra := range [][]string{{"-x", "video_quality=high"}, {"-x", "video_urls=[]"}, {"-x", "video_url="}, {"-x", "generate_video=true"}} {
		h := newHarness(t)
		h.useKey()
		args := append([]string{"video", "x", "-m", "m", "--no-wait", "--yes"}, extra...)
		h.run(args...).wantCode(t, exitcode.OK)
		if est := jsonBody(t, h.api.to(estimate)[0]); est["has_video_material"] == true {
			t.Errorf("%v: reported as video input", extra)
		}
	}
}

// A secret pasted where a flag does not take one must not be repeated in the
// error, which ends up in CI and agent logs.
func TestFlagErrorsDoNotEchoValues(t *testing.T) {
	h := newHarness(t)
	for _, args := range [][]string{
		{"auth", "login", "--with-api-key=" + testKey},
		{"balance", "--quiet=" + testKey},
		{"balance", "-k" + testKey},
		{"balance", "--max-cost", testKey},
		{"job", "wait", "x", "--timeout", testKey},
		{"balance", "--api-key=" + testKey},
		{"balance", "---" + testKey},
	} {
		res := h.run(args...).wantCode(t, exitcode.Usage)
		res.noSecret(t, testKey)
		if strings.TrimSpace(res.stderr) == "error:" {
			t.Errorf("%v: the error message is empty", args[:2])
		}
	}
	if n := len(h.api.all()); n != 0 {
		t.Errorf("%d requests were sent", n)
	}
}

// Text typed in the wrong place is never repeated by a usage error. The
// sentinel stands for a secret pasted as a command, a key, a flag value or a
// help topic; whatever the mistake, it must not reach stdout or stderr.
func TestUsageErrorsNeverEchoArguments(t *testing.T) {
	const s = "sk-SENTINEL0123456789abcdefghijklmnop"
	h := newHarness(t)
	for _, args := range [][]string{
		{s},
		{"auth", s},
		{"auth", "login", s},
		{"auth", "login", "--with-api-key", s},
		{"auth", "login", "--with-api-key=" + s},
		{"models", s},
		{"config", s},
		{"config", "get", s},
		{"config", "set", s, "x"},
		{"config", "set", "defaults.output", s},
		{"config", "set", "spend.confirm_above", s},
		{"job", s},
		{"completion", s},
		{"help", s},
		{"help", "auth", s},
		{"balance", s},
		{"balance", "--quiet=" + s},
		{"balance", "-k" + s},
		{"balance", "--max-cost", s},
		{"balance", "--" + s},
		{"version", s},
		{"job", "wait", "x", "--timeout", s},
		{"models", "list", "--category", s},
		{"image", "x", "-m", "m", "-x", s},
		{"image", "x", "-m", "m", "-x", s + "=1"},
		{"image", "x", "-m", "m", "-x", "=" + s},
		{"cost", "m", "-d", s},
		{"cost", "m", "-d", s + "=1"},
	} {
		res := h.run(args...)
		if res.code != exitcode.Usage {
			t.Errorf("%v: exit code = %d, want %d", redact(args, s), res.code, exitcode.Usage)
		}
		if strings.Contains(res.stdout, s) || strings.Contains(res.stderr, s) {
			t.Errorf("%v: the argument was echoed:\n%s%s", redact(args, s), res.stdout, strings.ReplaceAll(res.stderr, s, "<SENTINEL>"))
		}
		if len(strings.TrimSpace(res.stderr)) < len("error: x") {
			t.Errorf("%v: no usable error message: %q", redact(args, s), res.stderr)
		}
	}
	if n := len(h.api.all()); n != 0 {
		t.Errorf("%d requests were sent by malformed commands", n)
	}
}

func redact(args []string, secret string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = strings.ReplaceAll(a, secret, "<SENTINEL>")
	}
	return out
}

// Errors stay useful: they name the flag (which the CLI defines) and say
// what is valid, without quoting the input.
func TestUsageErrorsStayHelpful(t *testing.T) {
	h := newHarness(t)
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"job", "wait", "x", "--timeout", "soon"}, "invalid value for --timeout"},
		{[]string{"balance", "--max-cost", "lots"}, "invalid value for --max-cost"},
		{[]string{"image", "x", "-m"}, "--model needs a value"},
		{[]string{"image", "x", "-o"}, "--output needs a value"},
		{[]string{"balance", "--nope"}, "apipod balance --help"},
		{[]string{"balance", "--api-key", "x"}, "APIPOD_API_KEY"},
		{[]string{"balance", "--token=x"}, "apipod auth login"},
		{[]string{"nosuchcommand"}, "apipod --help"},
		{[]string{"models", "nosuchsub"}, "available: list, params"},
		{[]string{"config", "get", "nope"}, "valid: defaults.image_model"},
		{[]string{"image", "x", "-m", "m", "-x", "noequals"}, "-x takes key=value"},
		{[]string{"cost", "m", "-d", "noequals"}, "-d takes key=value"},
		{[]string{"image", "x", "-m", "m", "-x", "ref=@"}, "-x ref=@: missing file path"},
		{[]string{"help", "nosuchtopic"}, "unknown help topic"},
	}
	for _, tc := range cases {
		res := h.run(tc.args...).wantCode(t, exitcode.Usage)
		if !strings.Contains(res.stderr, tc.want) {
			t.Errorf("%v: stderr lacks %q:\n%s", tc.args, tc.want, res.stderr)
		}
	}
}

func TestHelpCommandStillWorks(t *testing.T) {
	h := newHarness(t)
	for _, args := range [][]string{{"help"}, {"help", "image"}, {"help", "auth", "login"}, {"--help"}, {"image", "--help"}, {"auth", "login", "-h"}} {
		res := h.run(args...).wantCode(t, exitcode.OK)
		if !strings.Contains(res.stdout, "Usage:") {
			t.Errorf("%v: no usage text:\n%s", args, res.stdout)
		}
	}
	if res := h.run("help", "auth", "login"); !strings.Contains(res.stdout, "--with-api-key") {
		t.Errorf("help auth login does not describe the command:\n%s", res.stdout)
	}
}

// -d dimension names follow the same rule as -x parameter names: the pricing
// API matches them exactly, so another case would silently price the default.
func TestCostDimensionKeysMustBeLowercase(t *testing.T) {
	h := newHarness(t)
	h.useKey()
	h.run("cost", "m", "-d", "Duration=15").wantCode(t, exitcode.Usage)
	h.run("cost", "m", "-d", "N=10").wantCode(t, exitcode.Usage)
	if n := len(h.api.all()); n != 0 {
		t.Errorf("%d requests were sent", n)
	}
	h.run("cost", "m", "-d", "duration=15").wantCode(t, exitcode.OK)
}

// URLs are media references, not pricing parameters: they are left out of
// the estimate's dimensions (a reference video is reported separately).
func TestURLsAreNotSentAsPricingDimensions(t *testing.T) {
	h := newHarness(t)
	h.useKey()
	h.run("video", "x", "-m", "m", "--no-wait", "--yes", "--duration", "5",
		"-x", "video_url=https://example.com/clip.mp4", "-x", "mask=assetId://asset_1", "-x", "style=cinematic").wantCode(t, exitcode.OK)
	body := jsonBody(t, h.api.to(estimate)[0])
	dims, _ := body["dimensions"].(map[string]any)
	if _, ok := dims["video_url"]; ok {
		t.Errorf("a URL was sent as a pricing dimension: %v", dims)
	}
	if _, ok := dims["mask"]; ok {
		t.Errorf("a server-side reference was sent as a pricing dimension: %v", dims)
	}
	if dims["duration"] != "5" || dims["style"] != "cinematic" {
		t.Errorf("dimensions = %v, want the scalar parameters kept", dims)
	}
	if body["has_video_material"] != true {
		t.Errorf("estimate body = %v, want has_video_material", body)
	}
}
