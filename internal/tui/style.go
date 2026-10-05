package tui

import (
	"fmt"
	"image/color"
	"regexp"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// darkBackground follows the terminal, which the TUI asks at start.
var darkBackground = true

// adaptive is a colour with a shade for each background, chosen at render
// time. The palette is the one of office's and mnemo's TUIs.
type adaptive struct{ Light, Dark string }

func (c adaptive) RGBA() (r, g, b, a uint32) {
	if darkBackground {
		return lipgloss.Color(c.Dark).RGBA()
	}
	return lipgloss.Color(c.Light).RGBA()
}

var _ color.Color = adaptive{}

var (
	cAccent  = adaptive{Light: "#5A4FCF", Dark: "#A99CFF"}
	cMuted   = adaptive{Light: "#8A8A8A", Dark: "#6E6E6E"}
	cText    = adaptive{Light: "#1A1A1A", Dark: "#E6E6E6"}
	cSel     = adaptive{Light: "#D4CCFF", Dark: "#4B3F99"}
	cSelBg   = adaptive{Light: "#ECE9FF", Dark: "#2D2A4A"}
	cWorking = adaptive{Light: "#B7791F", Dark: "#F2C14E"}
	cReady   = adaptive{Light: "#2F855A", Dark: "#68D391"}
	cStopped = adaptive{Light: "#C53030", Dark: "#FC8181"}
	cPerso   = adaptive{Light: "#2B6CB0", Dark: "#63B3ED"}
	cPro     = adaptive{Light: "#C05621", Dark: "#F6AD55"}
	cOther   = adaptive{Light: "#2C7A7B", Dark: "#4FD1C5"}

	sTitle   = lipgloss.NewStyle().Bold(true).Foreground(cAccent)
	sMuted   = lipgloss.NewStyle().Foreground(cMuted)
	sText    = lipgloss.NewStyle().Foreground(cText)
	sBold    = lipgloss.NewStyle().Bold(true).Foreground(cText)
	sErr     = lipgloss.NewStyle().Bold(true).Foreground(cStopped)
	sOK      = lipgloss.NewStyle().Foreground(cReady)
	sWarn    = lipgloss.NewStyle().Bold(true).Foreground(cWorking)
	sKey     = lipgloss.NewStyle().Bold(true).Foreground(cAccent)
	sSection = lipgloss.NewStyle().Bold(true).Foreground(cAccent).Underline(true)
)

// sgrReset matches the resets lipgloss puts after each styled span.
var sgrReset = regexp.MustCompile(`\x1b\[0?m`)

// selectLine puts the selected row on a strong background, with a bar in the
// accent colour, as office's TUI does, and keeps every colour of the row:
// the background is set again after each reset.
func selectLine(line string, w int) string {
	probe := lipgloss.NewStyle().Background(cSel).Render("|")
	bg, _, _ := strings.Cut(probe, "|")
	rest := ansi.Cut(line, 1, w)
	body := bg + sgrReset.ReplaceAllStringFunc(rest, func(r string) string { return r + bg })
	if d := w - 1 - ansi.StringWidth(rest); d > 0 {
		body += strings.Repeat(" ", d)
	}
	bar := lipgloss.NewStyle().Foreground(cAccent).Background(cSel).Bold(true).Render("▌")
	return bar + body + "\x1b[m"
}

func sphereColor(s string) color.Color {
	switch s {
	case "perso":
		return cPerso
	case "pro":
		return cPro
	}
	return cOther
}

func tag(s string) string {
	return lipgloss.NewStyle().Foreground(sphereColor(s)).Bold(true).Render(s)
}

// pad truncates or fills a rendered line to exactly w cells.
func pad(s string, w int) string {
	if w <= 0 {
		return ""
	}
	s = ansi.Truncate(s, w, "…")
	if d := w - ansi.StringWidth(s); d > 0 {
		s += strings.Repeat(" ", d)
	}
	return s
}

// block lays lines out as a w×h rectangle.
func block(lines []string, w, h int) string {
	out := make([]string, max(0, h))
	for i := range out {
		s := ""
		if i < len(lines) {
			s = lines[i]
		}
		out[i] = pad(s, w)
	}
	return strings.Join(out, "\n")
}

func size(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1<<20:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	case n < 1<<30:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
}

// keys renders a help line: pairs of key and action.
func helpLine(pairs ...string) string {
	var b []string
	for i := 0; i+1 < len(pairs); i += 2 {
		b = append(b, sKey.Render(pairs[i])+" "+sMuted.Render(pairs[i+1]))
	}
	return strings.Join(b, sMuted.Render(" · "))
}
