// Package tui renders the full-screen dashboard slimproxy shows on a terminal.
//
// It owns presentation only. Every number it displays and every action it can
// perform arrives through Deps, so the layout can be exercised without a proxy,
// a network, or a cloudflared binary.
package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
)

// The palette. Warm dark brown ground, amber for numbers that matter, clay for
// trouble, olive for healthy.
//
// Hex values rather than ANSI indices so the intended colour survives on a
// truecolor terminal; lipgloss degrades them to the nearest available shade on
// terminals that cannot show them, which is why nothing here encodes meaning in
// colour alone -- every state also carries a symbol.
const (
	colBG    = lipgloss.Color("#1a1410")
	colFG    = lipgloss.Color("#e8dcc8")
	colDim   = lipgloss.Color("#8a7d68")
	colFrame = lipgloss.Color("#5c5142")
	colAmber = lipgloss.Color("#ff9e3d")
	colClay  = lipgloss.Color("#d9603b")
	colOlive = lipgloss.Color("#7fa650")
)

// Every inline style sets the background explicitly.
//
// lipgloss does not inherit: each Render emits a self-contained escape
// sequence, so a span styled with only a foreground resets the background to
// the terminal default and punches a hole in the panel. These helpers exist so
// that cannot be forgotten one span at a time.
func fg(c lipgloss.Color) lipgloss.Style {
	return lipgloss.NewStyle().Foreground(c).Background(colBG)
}

var (
	sFrame = fg(colFrame)            // box-drawing characters
	sLabel = fg(colDim)              // 监听 / 隧道 / 凭据 ...
	sText  = fg(colFG)               // ordinary values
	sNum   = fg(colAmber).Bold(true) // the numbers being watched
	sOK    = fg(colOlive)            // ● healthy
	sBad   = fg(colClay)             // ▲ needs attention
	sPlain = lipgloss.NewStyle()     // outside the panel: terminal's own colours
	sDimO  = lipgloss.NewStyle().Foreground(colDim)
	sAmbO  = lipgloss.NewStyle().Foreground(colAmber)

	// Selected row in the completion list: amber ground, panel-dark text.
	sSel = lipgloss.NewStyle().Foreground(colBG).Background(colAmber).Bold(true)
)

// Status symbols. Paired with colour, never replaced by it: a monochrome
// terminal still distinguishes ● from ▲.
const (
	symOK  = "●"
	symBad = "▲"
	symNA  = "·"
)

// Box-drawing pieces, named so the renderers read as structure rather than
// punctuation.
const (
	boxTL, boxTR, boxBL, boxBR = "┌", "┐", "└", "┘"
	boxH, boxV                 = "─", "│"
	boxTeeL, boxTeeR           = "├", "┤"
)

// sanitize strips the control characters that would break a rendered frame.
//
// Everything the panel displays that is not its own text -- model names, route
// names, hostnames, account addresses, credential filenames, upstream error
// bodies -- arrives from a config file, a provider's response, or a filename on
// disk. None of those is guaranteed to be one clean line:
//
//   - \n splits one row into two, and every row below it shifts
//   - \t expands to a variable number of cells the width arithmetic cannot see
//   - \x1b begins an escape sequence, so a model name containing one is
//     executed by the terminal rather than shown; \x1b[2J clears the screen
//
// Replacing rather than dropping, so the text stays the same width as what was
// measured and the substitution is visible instead of silently changing the
// content.
func sanitize(s string) string {
	if !strings.ContainsFunc(s, isControl) {
		return s
	}
	return strings.Map(func(r rune) rune {
		if isControl(r) {
			return '·'
		}
		return r
	}, s)
}

// isControl reports whether r would disturb the frame: C0 controls (including
// ESC, tab and newline), DEL, and the C1 range.
func isControl(r rune) bool {
	return r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f)
}

// cells is the width table every measurement in this package goes through.
//
// Pinned, not runewidth's DefaultCondition. That default is chosen at init
// from the host: with RUNEWIDTH_EASTASIAN unset, a CJK locale -- on Windows a
// console code page of 936 outside Windows Terminal -- turns on East Asian
// width, and the Unicode "ambiguous" runes count as two cells. That class is
// every box-drawing piece plus the ●, ▲, ·, …, → and — this panel draws.
// lipgloss and bubbletea, which measure these rows and cut them at the
// terminal edge, never look at the locale: they count those runes as one
// cell unless RUNEWIDTH_EASTASIAN is set. So on a Chinese console every row
// came out one cell short per ambiguous rune and the right border walked.
//
// One cell is also the only width the frame can be drawn at: the borders are
// strings.Repeat(boxH, n) with n in cells.
var cells = &runewidth.Condition{EastAsianWidth: false, StrictEmojiNeutral: true}

// width is the display width of s in terminal cells.
//
// Not len(s) and not utf8.RuneCountInString: the labels are Chinese, and a CJK
// rune occupies two cells. Getting this wrong does not misalign by a rune, it
// misaligns by however many CJK characters precede the error -- and the right
// border of every row drifts.
func width(s string) int { return cells.StringWidth(s) }

// pad right-pads s with spaces to exactly w display cells.
//
// Over-wide input is truncated rather than allowed to overflow: a row that
// exceeds the inner width pushes the right border off the panel, and one long
// model name would break every frame below it.
func pad(s string, w int) string {
	n := width(s)
	if n > w {
		return truncate(s, w)
	}
	return s + strings.Repeat(" ", w-n)
}

// padLeft is pad for right-aligned columns (counts, durations, percentages).
func padLeft(s string, w int) string {
	n := width(s)
	if n > w {
		return truncate(s, w)
	}
	return strings.Repeat(" ", w-n) + s
}

// truncate cuts s to at most w display cells, appending an ellipsis when
// anything was removed.
//
// Cuts on rune boundaries and accounts for double-width runes, so truncating
// mid-CJK cannot leave a half cell that shifts the border by one.
func truncate(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if width(s) <= w {
		return s
	}
	if w == 1 {
		return "…"
	}
	// Reserve one cell for the ellipsis.
	limit := w - 1
	var b strings.Builder
	used := 0
	for _, r := range s {
		rw := cells.RuneWidth(r)
		if used+rw > limit {
			break
		}
		b.WriteRune(r)
		used += rw
	}
	// A double-width rune may have left one cell short of the limit; pad it so
	// the result is exactly w cells wide.
	out := b.String() + "…"
	if d := w - width(out); d > 0 {
		out += strings.Repeat(" ", d)
	}
	return out
}
