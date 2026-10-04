package tui

import (
	"io"
	"strings"
	"unicode"

	"github.com/charmbracelet/lipgloss"
)

// style is one SGR look. Colour is all-or-nothing per frame, decided by the caller (a terminal
// without NO_COLOR), so styles are plain escape codes rather than lipgloss styles that would
// detect the colour profile themselves.
type style int

const (
	stylePlain style = iota
	styleBold
	styleDim
	styleRed
	styleGreen
	styleYellow
	styleCyan
	styleDimRed
	styleDimGreen
	styleDimYellow
)

func (s style) code() string {
	switch s {
	case styleBold:
		return "\x1b[1m"
	case styleDim:
		return "\x1b[2m"
	case styleRed:
		return "\x1b[31m"
	case styleGreen:
		return "\x1b[32m"
	case styleYellow:
		return "\x1b[33m"
	case styleCyan:
		return "\x1b[36m"
	case styleDimRed:
		return "\x1b[2;31m"
	case styleDimGreen:
		return "\x1b[2;32m"
	case styleDimYellow:
		return "\x1b[2;33m"
	}
	return ""
}

func (s style) dimmed() style {
	switch s {
	case styleRed:
		return styleDimRed
	case styleGreen:
		return styleDimGreen
	case styleYellow:
		return styleDimYellow
	}
	return styleDim
}

const reset = "\x1b[0m"

// seg is a run of text in one style.
type seg struct {
	text  string
	style style
}

func plain(t string) seg  { return seg{t, stylePlain} }
func bold(t string) seg   { return seg{t, styleBold} }
func dim(t string) seg    { return seg{t, styleDim} }
func red(t string) seg    { return seg{t, styleRed} }
func green(t string) seg  { return seg{t, styleGreen} }
func yellow(t string) seg { return seg{t, styleYellow} }
func cyan(t string) seg   { return seg{t, styleCyan} }

// line is one row of the frame.
type line []seg

func (l line) width() int {
	w := 0
	for _, s := range l {
		w += cellWidth(s.text)
	}
	return w
}

// pad right-aligns right within width, keeping at least one space between the two.
func (l line) pad(width int, right ...seg) line {
	gap := max(1, width-l.width()-line(right).width())
	out := append(line{}, l...)
	out = append(out, plain(strings.Repeat(" ", gap)))
	return append(out, right...)
}

// fit cuts the line to width columns, ending in "…" when something was dropped. A frame line
// wider than the terminal would wrap and scroll the whole frame.
func (l line) fit(width int) line {
	if l.width() <= width {
		return l
	}
	var kept line
	room := width - 1
	for _, s := range l {
		if room <= 0 {
			break
		}
		if w := cellWidth(s.text); w <= room {
			kept = append(kept, s)
			room -= w
		} else {
			kept = append(kept, seg{truncCells(s.text, room), s.style})
			room = 0
		}
	}
	return append(kept, dim("…"))
}

func (l line) render(color bool) string {
	var b strings.Builder
	for _, s := range l {
		if !color || s.style == stylePlain {
			b.WriteString(s.text)
			continue
		}
		b.WriteString(s.style.code())
		b.WriteString(s.text)
		b.WriteString(reset)
	}
	return b.String()
}

// measure only truncates: a renderer bound to no terminal never adds styles of its own.
var measure = lipgloss.NewRenderer(io.Discard).NewStyle()

// cellWidth is the number of terminal columns s occupies (wide and combining characters
// included, escape sequences ignored).
func cellWidth(s string) int { return lipgloss.Width(s) }

// truncCells keeps the longest prefix of s that fits n columns.
func truncCells(s string, n int) string {
	if n <= 0 {
		return ""
	}
	return measure.MaxWidth(n).Render(s)
}

// truncate shortens s to at most n columns, ending in "…" when it was cut.
func truncate(s string, n int) string {
	if cellWidth(s) <= n {
		return s
	}
	return truncCells(s, max(1, n-1)) + "…"
}

// padRight cuts or pads s to exactly n columns.
func padRight(s string, n int) string {
	if cellWidth(s) > n {
		s = truncCells(s, n)
	}
	return s + strings.Repeat(" ", max(0, n-cellWidth(s)))
}

// sanitize replaces control and bidi-override characters in text that came from outside
// (agent tools and reasons, warnings, error messages) with spaces: a newline would break the
// frame and an escape sequence would reach the terminal.
func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || isBidiControl(r) || r == '\u2028' || r == '\u2029' {
			return ' '
		}
		return r
	}, s)
}

func isBidiControl(r rune) bool {
	return (r >= '\u202a' && r <= '\u202e') || (r >= '\u2066' && r <= '\u2069') ||
		r == '\u200e' || r == '\u200f' || r == '\u061c'
}
