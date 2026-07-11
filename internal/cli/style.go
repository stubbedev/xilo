package cli

import (
	"os"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/lipgloss/table"
	"golang.org/x/term"
)

// Terminal styling (issue: charm feel). lipgloss only — no TUI framework;
// colors degrade to plain text on non-TTY output automatically.
var (
	sOK     = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "35", Dark: "42"}).Bold(true)
	sDim    = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "245", Dark: "243"})
	sAccent = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "172", Dark: "214"}).Bold(true)
	sHead   = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "240", Dark: "248"}).Bold(true)
	sWarn   = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "160", Dark: "203"}).Bold(true)
)

func styleOK(s string) string     { return sOK.Render(s) }
func styleDim(s string) string    { return sDim.Render(s) }
func styleAccent(s string) string { return sAccent.Render(s) }
func styleWarn(s string) string   { return sWarn.Render(s) }

// renderTable prints rows under a styled header with subtle row separation.
func renderTable(headers []string, rows [][]string) string {
	t := table.New().
		Border(lipgloss.HiddenBorder()).
		StyleFunc(func(row, _ int) lipgloss.Style {
			if row == table.HeaderRow {
				return sHead.PaddingRight(2)
			}
			return lipgloss.NewStyle().PaddingRight(2)
		}).
		Headers(headers...).
		Rows(rows...)
	return t.Render()
}

// isTTY reports whether stdout is a terminal (progress bars, spinners).
func isTTY() bool { return term.IsTerminal(int(os.Stdout.Fd())) }
