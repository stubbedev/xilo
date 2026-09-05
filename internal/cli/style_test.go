package cli

import (
	"strings"
	"testing"
)

// A colored cell must not shift the columns after it: the escape bytes are
// invisible on screen, so only the drawn runes may count toward the width.
func TestRenderTableAlignsAroundColorCodes(t *testing.T) {
	rows := [][]string{
		{"1", ansiOK + "active" + ansiReset, "keep"},
		{"2", "revoked", "keep"},
	}
	out := renderTable([]string{"ID", "STATUS", "NOTE"}, rows)
	lines := strings.Split(out, "\n")
	if len(lines) != 3 {
		t.Fatalf("want header + 2 rows, got %d lines: %q", len(lines), out)
	}
	col := func(line string) int { return strings.Index(stripANSI(line), "keep") }
	if a, b := col(lines[1]), col(lines[2]); a != b {
		t.Fatalf("last column misaligned: %d vs %d\n%s", a, b, out)
	}
	if got := strings.Index(stripANSI(lines[0]), "NOTE"); got != col(lines[1]) {
		t.Fatalf("header misaligned with rows: %d vs %d\n%s", got, col(lines[1]), out)
	}
}

func TestVisibleWidthSkipsEscapes(t *testing.T) {
	if got := visibleWidth(ansiWarn + "revoked" + ansiReset); got != len("revoked") {
		t.Fatalf("visibleWidth = %d, want %d", got, len("revoked"))
	}
	if got := visibleWidth(""); got != 0 {
		t.Fatalf("visibleWidth(\"\") = %d", got)
	}
}

// stripANSI is the test's own reader of what a terminal would draw.
func stripANSI(s string) string {
	var b strings.Builder
	esc := false
	for _, r := range s {
		switch {
		case esc:
			esc = r != 'm'
		case r == 0x1b:
			esc = true
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
