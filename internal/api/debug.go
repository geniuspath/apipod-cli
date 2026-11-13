package api

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/geniuspath/apipod-cli/internal/output"
)

// DebugLogger writes --debug request traces to stderr. Logf replaces every
// registered secret in every line; headers that carry credentials are also
// masked by name, so a secret that was never registered still does not leak
// through them.
type DebugLogger struct {
	w       io.Writer
	mu      sync.Mutex
	secrets []string
}

// NewDebugLogger returns a logger writing to w; a nil *DebugLogger is a no-op.
func NewDebugLogger(w io.Writer) *DebugLogger { return &DebugLogger{w: w} }

// AddSecret registers a value that must never appear in the output.
func (d *DebugLogger) AddSecret(s string) {
	if d == nil || len(s) < 6 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, have := range d.secrets {
		if have == s {
			return
		}
	}
	d.secrets = append(d.secrets, s)
}

// Logf writes one line. Registered secrets are replaced, and control
// characters are stripped: header values and bodies are server- or
// user-controlled and must not reach the terminal as escape sequences.
func (d *DebugLogger) Logf(format string, args ...any) {
	if d == nil {
		return
	}
	line := output.SanitizeLine(d.redact(fmt.Sprintf(format, args...)))
	d.mu.Lock()
	defer d.mu.Unlock()
	fmt.Fprintln(d.w, "debug: "+line)
}

func (d *DebugLogger) redact(s string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, secret := range d.secrets {
		s = strings.ReplaceAll(s, secret, "[REDACTED]")
	}
	return s
}

// sensitiveHeaders are masked regardless of their value.
var sensitiveHeaders = map[string]bool{
	"Authorization":       true,
	"Proxy-Authorization": true,
	"Cookie":              true,
	"Set-Cookie":          true,
	"X-Api-Key":           true,
	"X-Goog-Api-Key":      true,
}

func (d *DebugLogger) logRequest(req *http.Request, body []byte) {
	if d == nil {
		return
	}
	d.Logf("> %s %s", req.Method, req.URL.Redacted())
	d.logHeaders(">", req.Header)
	if len(body) > 0 && strings.HasPrefix(req.Header.Get("Content-Type"), "application/json") {
		// Redact first: truncating first could cut a secret in half and leave
		// a prefix that no longer matches.
		d.Logf("> body: %s", truncate(d.redact(string(body)), 2000))
	}
}

func (d *DebugLogger) logResponse(resp *http.Response) {
	if d == nil {
		return
	}
	d.Logf("< %s", resp.Status)
	d.logHeaders("<", resp.Header)
}

func (d *DebugLogger) logHeaders(prefix string, h http.Header) {
	names := make([]string, 0, len(h))
	for name := range h {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		value := strings.Join(h[name], ", ")
		if sensitiveHeaders[http.CanonicalHeaderKey(name)] {
			value = maskHeaderValue(value)
		}
		d.Logf("%s %s: %s", prefix, name, value)
	}
}

// maskHeaderValue keeps the auth scheme and hides the credential entirely.
func maskHeaderValue(v string) string {
	if scheme, _, ok := strings.Cut(v, " "); ok && len(scheme) <= 10 {
		return scheme + " [REDACTED]"
	}
	return "[REDACTED]"
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
