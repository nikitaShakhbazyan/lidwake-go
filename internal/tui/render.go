// Package tui is `lidwake stats`: a live terminal dashboard of on/off, lid-close sleep, the off
// timer, battery, CPU temperature against the cutout, thermal pressure, the agents keeping the
// Mac awake and what happened while the lid was closed. Render is pure, so the layout is
// testable; Model drives it with bubbletea and talks to the daemon through Actions.
package tui

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nikitaShakhbazyan/lidwake-go/internal/model"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/settings"
)

// Frame is everything one dashboard frame depends on.
type Frame struct {
	// Status is the daemon's status; nil draws the "daemon not running" screen.
	Status *model.Status
	// DaemonError explains a missing Status.
	DaemonError error
	Now         time.Time
	// Width is the terminal's width; the frame uses 48…96 columns of it.
	Width int
	// Color emits ANSI styles; false yields text without a single escape sequence.
	Color bool
	// Flash is one line of feedback for the last key press ("timer set", "released 2 holds").
	Flash string
}

const (
	minWidth   = 48
	maxWidth   = 96
	labelWidth = 17
	// tempLow…tempHigh is the temperature axis of the bar, in °C.
	tempLow     = 30.0
	tempHigh    = 100.0
	maxAgents   = 8
	maxWarnings = 3
)

// TempZone is how close a temperature is to the thermal cutout.
type TempZone string

const (
	TempNormal   TempZone = "normal"
	TempWarm     TempZone = "warm"
	TempHot      TempZone = "hot"
	TempCritical TempZone = "critical"
)

// TempZoneFor is normal well below the cutout, warm approaching it, hot within 5 °C and critical
// at the cutout.
func TempZoneFor(celsius, cutout float64) TempZone {
	switch {
	case celsius >= cutout:
		return TempCritical
	case celsius >= cutout-5:
		return TempHot
	case celsius >= cutout-20:
		return TempWarm
	}
	return TempNormal
}

// ThermalStateNames are NSProcessInfo.thermalState's levels, in order.
var ThermalStateNames = []string{"nominal", "fair", "serious", "critical"}

// CutoutName is the human name of a latched cutout cause.
func CutoutName(raw string) string {
	switch raw {
	case "thermal":
		return "overheating"
	case "lowBattery":
		return "low battery"
	case "onBattery":
		return "on battery (AC only)"
	}
	return raw
}

// FormatDuration renders `1h 05m`, `12m 03s` (with seconds) or `12m`, `45s`.
func FormatDuration(d time.Duration, seconds bool) string {
	total := int(math.Floor(d.Seconds()))
	h, m, s := total/3600, total%3600/60, total%60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh %02dm", h, m)
	case m > 0 && seconds:
		return fmt.Sprintf("%dm %02ds", m, s)
	case m > 0:
		return fmt.Sprintf("%dm", m)
	}
	return fmt.Sprintf("%ds", s)
}

// Render draws one frame. Every line fits the frame width (48…96 columns of f.Width), measured
// in terminal cells with escape sequences ignored.
func Render(f Frame) []string {
	d := dashboard{width: max(minWidth, min(f.Width, maxWidth)), color: f.Color, flash: sanitize(f.Flash)}
	return d.render(f.Status, f.Now, f.DaemonError)
}

type dashboard struct {
	width int
	color bool
	flash string
}

func (d dashboard) render(s *model.Status, now time.Time, daemonErr error) []string {
	clock := now.Format("15:04:05")
	var out []line
	if s == nil {
		out = append(out,
			line{bold("lidwake")}.pad(d.width, dim(clock)),
			d.rule(),
			line{red("  ✕ daemon not running")})
		if daemonErr != nil {
			out = append(out, line{dim("    " + sanitize(daemonErr.Error()))})
		}
		out = append(out,
			line{dim("    install it with lidwake setup, or check: lidwake daemon-status")},
			d.rule(),
			d.keyHelp())
		return d.finish(out)
	}

	cfg := s.Settings
	if cfg == (settings.Settings{}) {
		// A status without settings (an older daemon): judge against the defaults rather than a
		// 0 °C cutout and a 0 % floor.
		cfg = settings.Defaults()
	}
	cutout := cfg.ThermalThresholdCelsius
	cells := d.barCells()

	out = append(out, append(line{bold("lidwake  ")}, d.headline(s)...).pad(d.width, dim(clock)))
	out = append(out, d.rule())

	if s.SleepDisabled {
		out = append(out, d.row("Lid-close sleep", green("blocked"), plain(" — closing the lid won't sleep the Mac")))
	} else {
		out = append(out, d.row("Lid-close sleep", plain("allowed"), dim(" — the Mac sleeps when the lid closes")))
	}
	lid := "open"
	if s.LidClosed {
		lid = "closed"
	}
	out = append(out, d.row("Lid", plain(lid)))

	switch {
	case s.Paused:
		out = append(out, d.row("Off timer", dim("— (lidwake is off)")))
	case s.OffAt != nil:
		left := max(0, s.OffAt.Sub(now))
		out = append(out, d.row("Off timer",
			yellow(FormatDuration(left, true)),
			plain("  until "+s.OffAt.In(now.Location()).Format("15:04"))))
	default:
		out = append(out, d.row("Off timer", dim("not set — press t")))
	}

	if s.BatteryPercent != nil {
		pct, floor := *s.BatteryPercent, cfg.LowBatteryThresholdPercent
		source := "on AC"
		if s.OnBattery != nil && *s.OnBattery {
			source = "on battery"
		}
		segs := d.batteryBar(pct, floor, cells)
		segs = append(segs, plain(fmt.Sprintf("  %d%% %s", pct, source)))
		if cfg.RequireACPower {
			segs = append(segs, dim(" · AC only"))
		} else if cfg.LowBatteryCutoutEnabled {
			segs = append(segs, dim(fmt.Sprintf(" · stop at %d%%", floor)))
		}
		out = append(out, d.row("Battery", segs...))
	} else {
		out = append(out, d.row("Power", plain("AC (no battery)")))
	}

	if t, ok := finite(s.CPUTemperature); ok {
		zone := TempZoneFor(t, cutout)
		segs := d.temperatureBar(t, cutout, cells)
		segs = append(segs, seg{fmt.Sprintf("  %.0f°C %s", t, zone), zoneStyle(zone)})
		if !cfg.ThermalCutoutEnabled {
			segs = append(segs, dim(" · cutout off"))
		}
		out = append(out, d.row("CPU temp", segs...), d.row("", d.temperatureLegend(cutout, cells)...))
	} else {
		out = append(out, d.row("CPU temp", dim("unreadable")))
	}

	var thermal line
	for i, name := range ThermalStateNames {
		st := styleRed
		if i <= 0 {
			st = styleGreen
		} else if i == 1 {
			st = styleYellow
		}
		if i == s.ThermalState {
			thermal = append(thermal, seg{"● " + name, st})
		} else {
			thermal = append(thermal, dim("○ "+name))
		}
		if i < len(ThermalStateNames)-1 {
			thermal = append(thermal, plain("  "))
		}
	}
	out = append(out, d.row("Thermal", thermal...))

	out = append(out, d.rule())
	agents := slices.Clone(s.Assertions)
	slices.SortStableFunc(agents, func(a, b model.Assertion) int { return a.AcquiredAt.Compare(b.AcquiredAt) })
	tally := "none working"
	if len(agents) > 0 {
		tally = fmt.Sprintf("%d keeping the Mac awake", len(agents))
	}
	out = append(out, line{bold("  AGENTS  "), dim(tally)})
	for _, a := range agents[:min(len(agents), maxAgents)] {
		out = append(out, d.agentLine(a, now))
	}
	if len(agents) > maxAgents {
		out = append(out, line{dim(fmt.Sprintf("  … and %d more", len(agents)-maxAgents))})
	}

	if s.AwaySummary != nil {
		out = append(out, d.awaySection(s.AwaySummary, cutout, now)...)
	}

	warnings := slices.Clone(s.Warnings)
	if !s.HelperConnected {
		warnings = append(warnings, "The privileged helper is not connected — lid-close sleep can't be blocked.")
	}
	if len(warnings) > 0 || d.flash != "" {
		out = append(out, d.rule())
	}
	for _, w := range warnings[:min(len(warnings), maxWarnings)] {
		out = append(out, line{yellow("  ! " + truncate(sanitize(w), d.width-4))})
	}
	if d.flash != "" {
		out = append(out, line{cyan("  " + truncate(d.flash, d.width-2))})
	}

	out = append(out, d.rule(), d.keyHelp())
	return d.finish(out)
}

// awaySection recaps the last closed-lid period that had work going: how long it lasted, which
// agents finished and which are still at it, how hot the Mac got and which cutouts fired.
func (d dashboard) awaySection(a *model.AwaySummary, cutout float64, now time.Time) []line {
	loc := now.Location()
	out := []line{
		d.rule(),
		{bold("  While the lid was closed  "), dim(fmt.Sprintf("%s · %s → %s",
			FormatDuration(a.OpenedAt.Sub(a.ClosedAt), false),
			a.ClosedAt.In(loc).Format("15:04"), a.OpenedAt.In(loc).Format("15:04")))},
	}
	if len(a.Finished) == 0 {
		out = append(out, d.row("Finished", dim("none")))
	} else {
		out = append(out, d.row("Finished", agentTally(a.Finished, styleGreen)...))
	}
	if len(a.StillActive) > 0 {
		out = append(out, d.row("Still working", agentTally(a.StillActive, styleCyan)...))
	}
	if t, ok := finite(a.PeakTemperature); ok {
		zone := TempZoneFor(t, cutout)
		out = append(out, d.row("Peak CPU temp", seg{fmt.Sprintf("%.0f°C %s", t, zone), zoneStyle(zone)}))
	}
	var fired []string
	if a.ThermalCutout {
		fired = append(fired, CutoutName("thermal"))
	}
	if a.LowBatteryCutout {
		fired = append(fired, CutoutName("lowBattery"))
	}
	if len(fired) == 0 {
		out = append(out, d.row("Safety cutouts", dim("none fired")))
	} else {
		out = append(out, d.row("Safety cutouts", red(strings.Join(fired, ", ")), plain(" fired")))
	}
	return out
}

func agentTally(agents []model.FinishedAgent, st style) line {
	noun := "agents"
	if len(agents) == 1 {
		noun = "agent"
	}
	names := make([]string, 0, len(agents))
	for _, a := range agents {
		name := a.DisplayName
		if name == "" {
			name = a.Tool
		}
		names = append(names, sanitize(name)+" "+FormatDuration(a.Duration, false))
	}
	return line{seg{fmt.Sprintf("%d %s", len(agents), noun), st}, plain("  " + strings.Join(names, ", "))}
}

// finish cuts every line to the frame width and renders it.
func (d dashboard) finish(lines []line) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = l.fit(d.width).render(d.color)
	}
	return out
}

// barCells is the room left for the bars after the label column and the text that follows them.
func (d dashboard) barCells() int {
	return max(8, d.width-2-labelWidth-2-30)
}

func (d dashboard) rule() line {
	return line{dim(strings.Repeat("─", d.width))}
}

func (d dashboard) keyHelp() line {
	return line{dim("  [space] on/off  [t] timer  [+/-] ±15m  [r] release all  [q] quit")}
}

func (d dashboard) headline(s *model.Status) line {
	switch {
	case s.Paused:
		return line{dim("○ OFF"), dim(" · the Mac sleeps normally")}
	case len(s.ActiveCutouts) > 0:
		names := make([]string, len(s.ActiveCutouts))
		for i, c := range s.ActiveCutouts {
			names[i] = sanitize(CutoutName(c))
		}
		return line{red("▲ CUTOUT"), plain(" · " + strings.Join(names, ", "))}
	case s.Blocking:
		return line{green("● ON"), plain(" · keeping the Mac awake")}
	}
	return line{cyan("● ON"), plain(" · idle, no agents working")}
}

func (d dashboard) row(label string, value ...seg) line {
	return append(line{dim("  " + padRight(label, labelWidth))}, value...)
}

func (d dashboard) agentLine(a model.Assertion, now time.Time) line {
	var state seg
	switch {
	case a.WaitingFor != "":
		state = yellow("waiting: " + sanitize(a.WaitingFor))
	case a.Origin == model.OriginManual:
		state = cyan("hold")
	default:
		state = green("working")
	}
	pid := ""
	if a.PID > 0 {
		pid = fmt.Sprintf("pid %d", a.PID)
	}
	segs := line{
		plain("  " + padRight(truncate(sanitize(a.Tool), 14), 14)),
		dim(padRight(pid, 11)),
		state,
		dim("  " + FormatDuration(now.Sub(a.AcquiredAt), false)),
	}
	if a.ExpiresAt != nil {
		segs = append(segs, dim(", "+FormatDuration(max(0, a.ExpiresAt.Sub(now)), false)+" left"))
	}
	if a.Reason != "" {
		used := segs.width()
		segs = append(segs, dim("  "+truncate(sanitize(a.Reason), max(8, d.width-used-2))))
	}
	return segs
}

func zoneStyle(z TempZone) style {
	switch z {
	case TempNormal:
		return styleGreen
	case TempWarm:
		return styleYellow
	}
	return styleRed
}

// temperatureBar is `▕████████▒▒░░░░┃░░▏`: filled up to the reading, each cell coloured by the
// zone it stands for, with the cutout marked.
func (d dashboard) temperatureBar(t, cutout float64, cells int) line {
	span := tempHigh - tempLow
	cutoutCell := int(math.Round((cutout - tempLow) / span * float64(cells)))
	filled := int(math.Round((min(max(t, tempLow), tempHigh) - tempLow) / span * float64(cells)))
	segs := line{dim("▕")}
	for i := range cells {
		cellTemp := tempLow + (float64(i)+0.5)/float64(cells)*span
		st := zoneStyle(TempZoneFor(cellTemp, cutout))
		switch {
		case i == cutoutCell:
			segs = append(segs, red("┃"))
		case i < filled:
			segs = append(segs, seg{"█", st})
		case d.color:
			segs = append(segs, seg{"░", st.dimmed()})
		default:
			segs = append(segs, plain("░"))
		}
	}
	return append(segs, dim("▏"))
}

// temperatureLegend puts the scale's ends and the cutout under the temperature bar.
func (d dashboard) temperatureLegend(cutout float64, cells int) line {
	span := tempHigh - tempLow
	cutoutCell := int(math.Round((cutout - tempLow) / span * float64(cells)))
	text := []rune(strings.Repeat(" ", cells+2))
	put := func(s string, col int) {
		for i, r := range []rune(s) {
			if col+i >= 0 && col+i < len(text) {
				text[col+i] = r
			}
		}
	}
	put(fmt.Sprintf("%d°", int(tempLow)), 0)
	label := fmt.Sprintf("%d° cutout", int(cutout))
	n := utf8.RuneCountInString(label)
	put(label, min(max(1, cutoutCell+1-n/2), cells+2-n))
	top := fmt.Sprintf("%d°", int(tempHigh))
	if tn := utf8.RuneCountInString(top); cutoutCell+n/2+2 < cells+2-tn {
		put(top, cells+2-tn)
	}
	return line{dim(string(text))}
}

func (d dashboard) batteryBar(pct, floor, cells int) line {
	filled := int(math.Round(float64(min(max(pct, 0), 100)) / 100 * float64(cells)))
	floorCell := int(math.Round(float64(floor) / 100 * float64(cells)))
	st := styleGreen
	if pct <= floor {
		st = styleRed
	} else if pct <= floor+15 {
		st = styleYellow
	}
	segs := line{dim("▕")}
	for i := range cells {
		switch {
		case i == floorCell && floor > 0:
			segs = append(segs, red("┃"))
		case i < filled:
			segs = append(segs, seg{"█", st})
		default:
			segs = append(segs, dim("░"))
		}
	}
	return append(segs, dim("▏"))
}

func finite(v *float64) (float64, bool) {
	if v == nil || math.IsNaN(*v) || math.IsInf(*v, 0) {
		return 0, false
	}
	return *v, true
}
