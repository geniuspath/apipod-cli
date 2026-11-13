package output

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"golang.org/x/term"
)

// Printer writes command output. In JSON mode stdout carries exactly one JSON
// document and nothing else; progress, notices and errors go to stderr, so
// `apipod … --json | jq` always parses.
type Printer struct {
	Out   io.Writer
	Err   io.Writer
	JSON  bool
	Quiet bool

	color       bool
	interactive bool
}

// Options configures New.
type Options struct {
	JSON    bool
	Quiet   bool
	NoColor bool
}

// New builds a Printer. Colors and prompts are enabled only when the relevant
// streams are real terminals.
func New(out, errw io.Writer, in io.Reader, opts Options) *Printer {
	return &Printer{
		Out:         out,
		Err:         errw,
		JSON:        opts.JSON,
		Quiet:       opts.Quiet,
		color:       !opts.NoColor && !opts.JSON && os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb" && IsTerminal(out),
		interactive: IsTerminal(in) && IsTerminal(errw),
	}
}

// IsTerminal reports whether v is an *os.File attached to a terminal.
func IsTerminal(v any) bool {
	f, ok := v.(*os.File)
	return ok && f != nil && term.IsTerminal(int(f.Fd()))
}

// Interactive reports whether the user can be prompted: stdin and stderr are
// both terminals. Agents, pipes and CI are never interactive.
func (p *Printer) Interactive() bool { return p.interactive }

// SetInteractive overrides terminal detection (tests).
func (p *Printer) SetInteractive(v bool) { p.interactive = v }

// EmitJSON writes v to stdout as indented JSON. encoding/json escapes control
// characters, so server strings cannot inject terminal sequences here.
func (p *Printer) EmitJSON(v any) error {
	enc := json.NewEncoder(p.Out)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// Result prints v as JSON in JSON mode, otherwise calls human.
func (p *Printer) Result(v any, human func(w io.Writer)) error {
	if p.JSON {
		return p.EmitJSON(v)
	}
	human(p.Out)
	return nil
}

// Infof writes a progress or notice line to stderr unless --quiet. The
// formatted line is sanitized as a whole, so arguments may be server strings.
func (p *Printer) Infof(format string, args ...any) {
	if p.Quiet {
		return
	}
	fmt.Fprintln(p.Err, Sanitize(fmt.Sprintf(format, args...)))
}

// Warnf writes a warning to stderr; --quiet does not silence warnings.
func (p *Printer) Warnf(format string, args ...any) {
	fmt.Fprintln(p.Err, "warning: "+Sanitize(fmt.Sprintf(format, args...)))
}

// Bold and Dim style text when color is enabled. Apply them to trusted
// strings only, or sanitize first.
func (p *Printer) Bold(s string) string { return p.paint("1", s) }
func (p *Printer) Dim(s string) string  { return p.paint("2", s) }

func (p *Printer) paint(code, s string) string {
	if !p.color {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

// Table writes aligned columns. Every cell is sanitized, so callers can pass
// server strings directly.
func (p *Printer) Table(w io.Writer, header []string, rows [][]string) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, strings.Join(header, "\t"))
	for _, row := range rows {
		cells := make([]string, len(row))
		for i, c := range row {
			cells[i] = SanitizeLine(c)
		}
		fmt.Fprintln(tw, strings.Join(cells, "\t"))
	}
	tw.Flush()
}

// KV writes "label  value" lines with sanitized values.
func (p *Printer) KV(w io.Writer, pairs ...string) {
	width := 0
	for i := 0; i+1 < len(pairs); i += 2 {
		if len(pairs[i]) > width {
			width = len(pairs[i])
		}
	}
	for i := 0; i+1 < len(pairs); i += 2 {
		fmt.Fprintf(w, "%-*s  %s\n", width, pairs[i], SanitizeLine(pairs[i+1]))
	}
}

// Truncate shortens s to at most n runes, ending with an ellipsis.
func Truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n-1])) + "…"
}
