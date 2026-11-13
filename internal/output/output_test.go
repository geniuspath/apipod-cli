package output

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func TestSanitizeStripsTerminalControlSequences(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"plain text", "Kling 3.0 Turbo", "Kling 3.0 Turbo"},
		{"unicode", "模型 «café» 🎬", "模型 «café» 🎬"},
		{"SGR color", "\x1b[31mred\x1b[0m", "red"},
		{"clear screen and home", "before\x1b[2J\x1b[Hafter", "beforeafter"},
		{"cursor up and erase line", "ok\x1b[1A\x1b[2Kforged", "okforged"},
		{"private mode", "\x1b[?25lhidden", "hidden"},
		{"OSC title, BEL", "\x1b]0;pwned\x07text", "text"},
		{"OSC title, ST", "\x1b]2;pwned\x1b\\text", "text"},
		{"OSC 8 hyperlink", "\x1b]8;;https://evil.example\x1b\\click\x1b]8;;\x1b\\", "click"},
		{"OSC 52 clipboard", "\x1b]52;c;cm0gLXJmIH4=\x07done", "done"},
		{"unterminated OSC", "text\x1b]0;never ends", "text"},
		{"DCS", "\x1bP1$r0m\x1b\\after", "after"},
		{"APC", "\x1b_payload\x1b\\after", "after"},
		{"8-bit CSI", "a\u009b31mb", "ab"},
		{"8-bit OSC", "a\u009d0;title\u009cb", "ab"},
		{"lone ESC", "a\x1bb", "a"},
		{"trailing ESC", "abc\x1b", "abc"},
		{"charset select", "a\x1b(0b", "ab"},
		{"backspace overwrite", "safe\b\b\b\bevil", "safeevil"},
		{"carriage return overwrite", "real\rfake", "realfake"},
		{"bell and NUL", "a\x07b\x00c", "abc"},
		{"DEL", "a\x7fb", "ab"},
		{"bidi override", "invoice‮gnp.exe", "invoicegnp.exe"},
		{"bidi isolate", "a⁦b⁩c", "abc"},
		{"newline and tab kept", "line1\n\tline2", "line1\n\tline2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Sanitize(tc.in)
			if got != tc.want {
				t.Errorf("Sanitize(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if strings.ContainsAny(got, "\x1b\x07\x08\r\x00\x7f\u009b\u009d") {
				t.Errorf("Sanitize(%q) = %q still contains control bytes", tc.in, got)
			}
		})
	}
}

func TestSanitizeLineFlattens(t *testing.T) {
	if got := SanitizeLine("row one\nFORGED ROW\tcell\r\n  tail "); got != "row one FORGED ROW cell tail" {
		t.Errorf("SanitizeLine = %q", got)
	}
}

const hostile = "\x1b]0;pwned\x07\x1b[2Jevil\x1b[31m-model\nFAKE\tROW"

func assertClean(t *testing.T, what, out string) {
	t.Helper()
	if strings.ContainsAny(out, "\x1b\x07") {
		t.Errorf("%s reached the terminal with escape bytes: %q", what, out)
	}
}

func TestTableAndKVSanitizeCells(t *testing.T) {
	p := New(io.Discard, io.Discard, nil, Options{})
	var buf bytes.Buffer
	p.Table(&buf, []string{"ID", "NAME"}, [][]string{{hostile, "ok"}})
	assertClean(t, "table cell", buf.String())
	if lines := strings.Count(strings.TrimSpace(buf.String()), "\n"); lines != 1 {
		t.Errorf("a cell forged extra rows (%d line breaks):\n%s", lines, buf.String())
	}
	if !strings.Contains(buf.String(), "evil-model FAKE ROW") {
		t.Errorf("printable text was lost:\n%s", buf.String())
	}

	buf.Reset()
	p.KV(&buf, "Name", hostile, "Status", "ok")
	assertClean(t, "key/value line", buf.String())
	if strings.Count(buf.String(), "\n") != 2 {
		t.Errorf("a value forged extra lines:\n%s", buf.String())
	}
}

func TestInfoAndWarnSanitize(t *testing.T) {
	var errBuf bytes.Buffer
	p := New(io.Discard, &errBuf, nil, Options{})
	p.Infof("model %s ready", hostile)
	p.Warnf("server said: %s", hostile)
	assertClean(t, "stderr message", errBuf.String())
	if !strings.Contains(errBuf.String(), "warning: server said:") {
		t.Errorf("stderr = %q", errBuf.String())
	}
}

func TestQuietSilencesInfoButNotWarnings(t *testing.T) {
	var errBuf bytes.Buffer
	p := New(io.Discard, &errBuf, nil, Options{Quiet: true})
	p.Infof("progress")
	p.Warnf("important")
	if strings.Contains(errBuf.String(), "progress") || !strings.Contains(errBuf.String(), "important") {
		t.Errorf("stderr = %q", errBuf.String())
	}
}

// In JSON mode stdout is exactly one JSON document; encoding/json escapes
// control characters, so hostile strings cannot reach the terminal raw.
func TestJSONModeKeepsStdoutClean(t *testing.T) {
	var out, errBuf bytes.Buffer
	p := New(&out, &errBuf, nil, Options{JSON: true})
	p.Infof("progress goes to stderr")
	humanCalled := false
	if err := p.Result(map[string]string{"name": hostile}, func(io.Writer) { humanCalled = true }); err != nil {
		t.Fatal(err)
	}
	if humanCalled {
		t.Error("the human renderer ran in JSON mode")
	}
	var decoded map[string]string
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("stdout is not a single JSON document: %v\n%s", err, out.String())
	}
	if decoded["name"] != hostile {
		t.Error("JSON output altered the data")
	}
	assertClean(t, "JSON output", out.String())
	if strings.Contains(out.String(), "progress") {
		t.Error("progress text leaked into stdout")
	}
}

func TestNonTerminalIsNotInteractiveAndHasNoColor(t *testing.T) {
	p := New(&bytes.Buffer{}, &bytes.Buffer{}, strings.NewReader(""), Options{})
	if p.Interactive() {
		t.Error("buffers were treated as an interactive terminal")
	}
	if got := p.Bold("x"); got != "x" {
		t.Errorf("Bold on a non-terminal = %q, want no escape codes", got)
	}
}

func TestTruncate(t *testing.T) {
	if got := Truncate("short", 10); got != "short" {
		t.Errorf("Truncate = %q", got)
	}
	if got := Truncate("日本語のテキストです", 5); got != "日本語の…" {
		t.Errorf("Truncate = %q", got)
	}
}
