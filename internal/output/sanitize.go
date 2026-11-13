// Package output renders results for people (tables, colors) and for
// programs (--json), and sanitizes server text before it reaches a terminal.
package output

import (
	"regexp"
	"strings"
	"unicode"
)

// escapeSeq matches the escape-sequence families a hostile string could use
// to rewrite a terminal: CSI (colors, cursor movement, screen clears), OSC
// (window title, OSC 8 hyperlinks, OSC 52 clipboard writes), DCS/SOS/PM/APC
// strings, and other ESC-introduced sequences. Both 7-bit (ESC [) and 8-bit
// C1 introducers are covered.
var escapeSeq = regexp.MustCompile(
	`(?:\x1b\[|\x{9b})[0-?]*[ -/]*[@-~]` + // CSI
		`|(?:\x1b\]|\x{9d})[^\x07\x1b\x{9c}]*(?:\x07|\x1b\\|\x{9c})?` + // OSC
		`|(?:\x1b[PX^_]|[\x{90}\x{98}\x{9e}\x{9f}])[^\x1b\x{9c}]*(?:\x1b\\|\x{9c})?` + // DCS, SOS, PM, APC
		`|\x1b[ -/]*[0-~]?`, // any other ESC sequence, or a lone ESC
)

// Sanitize removes terminal control sequences and control characters from
// text that came from the server (model names, error messages, job fields)
// before it is written to a terminal. Newlines and tabs survive. Unicode
// bidirectional overrides are removed as well, so text cannot visually
// reorder itself — for example to disguise a file extension.
func Sanitize(s string) string {
	s = escapeSeq.ReplaceAllString(s, "")
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case unicode.IsControl(r): // C0, DEL and C1
			return -1
		case isBidiControl(r):
			return -1
		}
		return r
	}, s)
}

// SanitizeLine is Sanitize for single-line contexts such as table cells: line
// breaks and tabs become spaces, so a value cannot forge extra rows.
func SanitizeLine(s string) string {
	return strings.Join(strings.Fields(Sanitize(s)), " ")
}

func isBidiControl(r rune) bool {
	return (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) ||
		r == 0x200e || r == 0x200f || r == 0x061c
}
