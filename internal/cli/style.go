package cli

import (
	"os"
	"strings"
	"sync"

	"golang.org/x/term"
)

// Terminal styling, written as SGR escapes. This used to be lipgloss, which
// brought termenv, uniseg, runewidth and half a dozen more modules along for
// four colors and one table.
//
// The colors were adaptive (a light and a dark variant each); every pair was
// the same hue two shades apart, so each collapses to one mid value that reads
// on either background.
const (
	ansiReset  = "\x1b[0m"
	ansiOK     = "\x1b[1;38;5;36m"  // green
	ansiDim    = "\x1b[38;5;244m"   // grey
	ansiAccent = "\x1b[1;38;5;208m" // amber
	ansiWarn   = "\x1b[1;38;5;167m" // red
	ansiHead   = "\x1b[1;38;5;244m" // grey, bold — table headers
)

// colorOK reports whether stdout should carry escape codes at all: a terminal,
// and not muzzled by NO_COLOR (https://no-color.org). Resolved once — stdout
// does not change under us mid-command.
var colorOK = sync.OnceValue(func() bool {
	return isTTY() && os.Getenv("NO_COLOR") == ""
})

func paint(code, s string) string {
	if s == "" || !colorOK() {
		return s
	}
	return code + s + ansiReset
}

func styleOK(s string) string     { return paint(ansiOK, s) }
func styleDim(s string) string    { return paint(ansiDim, s) }
func styleAccent(s string) string { return paint(ansiAccent, s) }
func styleWarn(s string) string   { return paint(ansiWarn, s) }

// renderTable lays rows out under a header, each column padded to its widest
// cell. Not text/tabwriter: cells arrive already colored (a token's status,
// say) and tabwriter measures bytes, so the escape sequences would shove every
// column after them out of line.
func renderTable(headers []string, rows [][]string) string {
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = visibleWidth(h)
	}
	for _, r := range rows {
		for i, c := range r {
			if i < len(widths) {
				widths[i] = max(widths[i], visibleWidth(c))
			}
		}
	}
	var b strings.Builder
	row := func(cells []string, style string) {
		for i, c := range cells {
			if style != "" {
				c = paint(style, c)
			}
			b.WriteString(c)
			if i < len(cells)-1 && i < len(widths) {
				b.WriteString(strings.Repeat(" ", widths[i]-visibleWidth(c)+2))
			}
		}
		b.WriteByte('\n')
	}
	row(headers, ansiHead)
	for _, r := range rows {
		row(r, "")
	}
	return strings.TrimRight(b.String(), "\n")
}

// visibleWidth counts the runes a terminal will actually draw, skipping SGR
// escape sequences.
// ponytail: assumes single-width runes — these columns hold ids, names, store
// hashes and dates. Reach for a width table if that ever stops being true.
func visibleWidth(s string) int {
	n, esc := 0, false
	for _, r := range s {
		switch {
		case esc:
			esc = r != 'm'
		case r == 0x1b:
			esc = true
		default:
			n++
		}
	}
	return n
}

// isTTY reports whether stdout is a terminal (progress bars, spinners).
func isTTY() bool { return term.IsTerminal(int(os.Stdout.Fd())) }
