package tui

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/nikitaShakhbazyan/lidwake-go/internal/model"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/settings"
)

var testNow = time.Unix(1_800_000_000, 0)

type statusOpt func(*model.Status)

func paused() statusOpt { return func(s *model.Status) { s.Paused = true } }

func withAgents(a ...model.Assertion) statusOpt {
	return func(s *model.Status) { s.Assertions = append(s.Assertions, a...) }
}

func withCutouts(c ...string) statusOpt {
	return func(s *model.Status) { s.ActiveCutouts = c }
}

func offIn(d time.Duration) statusOpt {
	return func(s *model.Status) {
		at := testNow.Add(d)
		s.OffAt = &at
	}
}

func temperature(t float64) statusOpt {
	return func(s *model.Status) { s.CPUTemperature = &t }
}

func noTemperature() statusOpt { return func(s *model.Status) { s.CPUTemperature = nil } }

func withAway(a model.AwaySummary) statusOpt {
	return func(s *model.Status) { s.AwaySummary = &a }
}

// testStatus is a lid-closed Mac on battery at 64 %, 55 °C, fair thermal pressure, with the
// helper connected and default settings; blocking whenever it has assertions.
func testStatus(opts ...statusOpt) *model.Status {
	t, pct, onBattery := 55.0, 64, true
	s := &model.Status{
		LidClosed:       true,
		HelperConnected: true,
		CPUTemperature:  &t,
		BatteryPercent:  &pct,
		OnBattery:       &onBattery,
		ThermalState:    1,
		Settings:        settings.Defaults(),
	}
	for _, o := range opts {
		o(s)
	}
	s.Blocking = len(s.Assertions) > 0
	s.SleepDisabled = len(s.Assertions) > 0
	return s
}

func testAgent(tool, reason string) model.Assertion {
	return model.New(tool+":1", tool, reason, 4242, tool, testNow.Add(-725*time.Second), nil, model.OriginHook)
}

func render(s *model.Status, width ...int) []string {
	w := 80
	if len(width) > 0 {
		w = width[0]
	}
	return Render(Frame{Status: s, Now: testNow, Width: w})
}

func lineWith(lines []string, sub string) string {
	for _, l := range lines {
		if strings.Contains(l, sub) {
			return l
		}
	}
	return ""
}

func anyContains(lines []string, sub string) bool { return lineWith(lines, sub) != "" }

func TestDashboard(t *testing.T) {
	t.Run("the headline says on, off, idle or cutout", func(t *testing.T) {
		cases := []struct {
			status *model.Status
			want   string
		}{
			{testStatus(paused()), "○ OFF"},
			{testStatus(), "● ON · idle"},
			{testStatus(withAgents(testAgent("claude-code", ""))), "● ON · keeping the Mac awake"},
			{testStatus(withCutouts("lowBattery")), "▲ CUTOUT · low battery"},
		}
		for _, c := range cases {
			if got := render(c.status)[0]; !strings.Contains(got, c.want) {
				t.Errorf("headline %q does not contain %q", got, c.want)
			}
		}
	})

	t.Run("agents show tool, pid, state, age and reason", func(t *testing.T) {
		line := lineWith(render(testStatus(withAgents(testAgent("claude-code", "refactor auth")))), "claude-code")
		for _, want := range []string{"pid 4242", "working", "12m", "refactor auth"} {
			if !strings.Contains(line, want) {
				t.Errorf("agent line %q does not contain %q", line, want)
			}
		}
	})

	t.Run("the off timer counts down to its deadline", func(t *testing.T) {
		if line := lineWith(render(testStatus(offIn(3725*time.Second))), "Off timer"); !strings.Contains(line, "1h 02m") {
			t.Errorf("off timer line %q does not contain 1h 02m", line)
		}
		if !anyContains(render(testStatus()), "not set") {
			t.Error("an unset timer is not shown as not set")
		}
		if !anyContains(render(testStatus(paused(), offIn(600*time.Second))), "lidwake is off") {
			t.Error("a paused lidwake does not say it is off")
		}
	})

	t.Run("temperature zones climb toward the cutout", func(t *testing.T) {
		for _, c := range []struct {
			celsius float64
			want    TempZone
		}{{50, TempNormal}, {60, TempWarm}, {76, TempHot}, {80, TempCritical}} {
			if got := TempZoneFor(c.celsius, 80); got != c.want {
				t.Errorf("TempZoneFor(%v, 80) = %v, want %v", c.celsius, got, c.want)
			}
		}
		if !anyContains(render(testStatus(temperature(81))), "81°C critical") {
			t.Error("81 °C is not shown as critical")
		}
		if !anyContains(render(testStatus(noTemperature())), "unreadable") {
			t.Error("a missing temperature is not shown as unreadable")
		}
	})

	t.Run("thermal pressure marks the current state", func(t *testing.T) {
		line := lineWith(render(testStatus()), "Thermal")
		if !strings.Contains(line, "● fair") || !strings.Contains(line, "○ nominal") {
			t.Errorf("thermal line %q does not mark fair as current", line)
		}
	})

	t.Run("lines fit the terminal width", func(t *testing.T) {
		for _, width := range []int{48, 64, 80, 120} {
			t.Run(fmt.Sprint(width), func(t *testing.T) {
				s := testStatus(withAgents(testAgent("claude-code", strings.Repeat("long reason ", 20))), offIn(600*time.Second))
				limit := max(48, min(width, 96))
				for _, color := range []bool{false, true} {
					for _, line := range Render(Frame{Status: s, Now: testNow, Width: width, Color: color}) {
						if w := lipgloss.Width(line); w > limit {
							t.Errorf("color=%v: %d > %d: %s", color, w, limit, line)
						}
					}
				}
			})
		}
	})

	t.Run("color is all-or-nothing", func(t *testing.T) {
		plainFrame := strings.Join(render(testStatus()), "")
		colored := strings.Join(Render(Frame{Status: testStatus(), Now: testNow, Width: 80, Color: true}), "")
		if strings.Contains(plainFrame, "\x1b[") {
			t.Error("the plain frame contains an escape sequence")
		}
		if !strings.Contains(colored, "\x1b[") {
			t.Error("the colored frame contains no escape sequence")
		}
	})

	t.Run("a missing daemon gets its own screen", func(t *testing.T) {
		if !anyContains(render(nil), "daemon not running") {
			t.Error("no daemon-not-running screen")
		}
	})
}

// The cases below cover what the Go dashboard adds or does differently.

func TestDashboardAwaySummary(t *testing.T) {
	closed := testNow.Add(-3 * time.Hour)
	opened := closed.Add(2*time.Hour + 5*time.Minute)
	peak := 72.0
	summary := model.AwaySummary{
		ClosedAt: closed,
		OpenedAt: opened,
		Finished: []model.FinishedAgent{
			{Key: "claude-code:1", Tool: "claude-code", DisplayName: "Claude Code", Duration: time.Hour + 2*time.Minute},
			{Key: "codex:2", Tool: "codex", Duration: 12 * time.Minute},
		},
		StillActive:     []model.FinishedAgent{{Key: "cursor:3", Tool: "cursor", DisplayName: "Cursor", Duration: 3 * time.Hour}},
		PeakTemperature: &peak,
	}

	t.Run("no section without a summary", func(t *testing.T) {
		if anyContains(render(testStatus()), "While the lid was closed") {
			t.Error("an away section without an away summary")
		}
	})

	t.Run("the header says how long the lid was closed and when", func(t *testing.T) {
		line := lineWith(render(testStatus(withAway(summary))), "While the lid was closed")
		want := fmt.Sprintf("2h 05m · %s → %s", closed.Format("15:04"), opened.Format("15:04"))
		if !strings.Contains(line, want) {
			t.Errorf("header %q does not contain %q", line, want)
		}
	})

	t.Run("finished agents are listed by name with how long they ran", func(t *testing.T) {
		line := lineWith(render(testStatus(withAway(summary)), 96), "Finished")
		for _, want := range []string{"2 agents", "Claude Code 1h 02m", "codex 12m"} {
			if !strings.Contains(line, want) {
				t.Errorf("finished line %q does not contain %q", line, want)
			}
		}
	})

	t.Run("agents still working are listed separately", func(t *testing.T) {
		line := lineWith(render(testStatus(withAway(summary))), "Still working")
		if !strings.Contains(line, "1 agent  Cursor 3h 00m") {
			t.Errorf("still-working line %q", line)
		}
		s := summary
		s.StillActive = nil
		if anyContains(render(testStatus(withAway(s))), "Still working") {
			t.Error("a still-working row with nothing still working")
		}
	})

	t.Run("nothing finished says none", func(t *testing.T) {
		s := summary
		s.Finished = nil
		if line := lineWith(render(testStatus(withAway(s))), "Finished"); !strings.Contains(line, "none") {
			t.Errorf("finished line %q does not say none", line)
		}
	})

	t.Run("the peak temperature is judged against the cutout", func(t *testing.T) {
		if line := lineWith(render(testStatus(withAway(summary))), "Peak CPU temp"); !strings.Contains(line, "72°C warm") {
			t.Errorf("peak line %q", line)
		}
		s := summary
		s.PeakTemperature = nil
		if anyContains(render(testStatus(withAway(s))), "Peak CPU temp") {
			t.Error("a peak row without a peak temperature")
		}
	})

	t.Run("fired cutouts are named", func(t *testing.T) {
		if line := lineWith(render(testStatus(withAway(summary))), "Safety cutouts"); !strings.Contains(line, "none fired") {
			t.Errorf("cutout line %q does not say none fired", line)
		}
		s := summary
		s.ThermalCutout, s.LowBatteryCutout = true, true
		if line := lineWith(render(testStatus(withAway(s))), "Safety cutouts"); !strings.Contains(line, "overheating, low battery fired") {
			t.Errorf("cutout line %q does not name both cutouts", line)
		}
		s.ThermalCutout = false
		if line := lineWith(render(testStatus(withAway(s))), "Safety cutouts"); !strings.Contains(line, "low battery fired") || strings.Contains(line, "overheating") {
			t.Errorf("cutout line %q", line)
		}
	})

	t.Run("the section sits between the agents and the key help", func(t *testing.T) {
		lines := render(testStatus(withAway(summary)))
		agents := slices.IndexFunc(lines, func(l string) bool { return strings.Contains(l, "AGENTS") })
		away := slices.IndexFunc(lines, func(l string) bool { return strings.Contains(l, "While the lid was closed") })
		help := slices.IndexFunc(lines, func(l string) bool { return strings.Contains(l, "[q] quit") })
		if !(agents < away && away < help) {
			t.Errorf("agents at %d, away at %d, help at %d", agents, away, help)
		}
	})

	t.Run("long lists still fit the width", func(t *testing.T) {
		s := summary
		for i := range 12 {
			s.Finished = append(s.Finished, model.FinishedAgent{Key: fmt.Sprint(i), Tool: "an-agent-with-a-long-name"})
		}
		for _, width := range []int{48, 80, 120} {
			for _, color := range []bool{false, true} {
				for _, line := range Render(Frame{Status: testStatus(withAway(s)), Now: testNow, Width: width, Color: color}) {
					if w, limit := lipgloss.Width(line), max(48, min(width, 96)); w > limit {
						t.Errorf("width %d color=%v: %d > %d: %s", width, color, w, limit, line)
					}
				}
			}
		}
	})
}

func TestDashboardDetails(t *testing.T) {
	t.Run("durations read as hours and minutes or minutes and seconds", func(t *testing.T) {
		for _, c := range []struct {
			d       time.Duration
			seconds bool
			want    string
		}{
			{3725 * time.Second, true, "1h 02m"},
			{3725 * time.Second, false, "1h 02m"},
			{723 * time.Second, true, "12m 03s"},
			{723 * time.Second, false, "12m"},
			{45*time.Second + 900*time.Millisecond, false, "45s"},
			{0, true, "0s"},
		} {
			if got := FormatDuration(c.d, c.seconds); got != c.want {
				t.Errorf("FormatDuration(%v, %v) = %q, want %q", c.d, c.seconds, got, c.want)
			}
		}
	})

	t.Run("the missing-daemon screen shows why", func(t *testing.T) {
		lines := Render(Frame{DaemonError: errors.New("lidwake daemon is not running"), Now: testNow, Width: 80})
		if !anyContains(lines, "lidwake daemon is not running") || !anyContains(lines, "lidwake daemon-status") {
			t.Errorf("missing-daemon screen: %q", lines)
		}
	})

	t.Run("text from agents cannot break the frame", func(t *testing.T) {
		a := testAgent("evil\ntool", "line one\nline two\x1b[2J\u202ereversed")
		a.WaitingFor = "a\rb"
		s := testStatus(withAgents(a))
		s.Warnings = []string{"warn\x1b]0;title\x07ing"}
		lines := Render(Frame{Status: s, Now: testNow, Width: 96, Flash: "flash\nmessage"})
		for _, l := range lines {
			if strings.ContainsAny(l, "\n\r\x1b\x07\u202e") {
				t.Errorf("control character in %q", l)
			}
		}
		if !anyContains(lines, "line one line two") {
			t.Errorf("reason not kept readable: %q", lines)
		}
	})

	t.Run("wide characters are measured in cells", func(t *testing.T) {
		s := testStatus(withAgents(testAgent("エージェント名前長い", strings.Repeat("理由", 40))))
		for _, line := range render(s, 64) {
			if w := lipgloss.Width(line); w > 64 {
				t.Errorf("%d > 64: %s", w, line)
			}
		}
	})

	t.Run("a status without settings uses the defaults", func(t *testing.T) {
		s := testStatus()
		s.Settings = settings.Settings{}
		lines := render(s)
		if !anyContains(lines, "stop at 20%") || !anyContains(lines, "80° cutout") {
			t.Errorf("defaults not applied: %q", lines)
		}
	})

	t.Run("battery and power rows follow the settings", func(t *testing.T) {
		s := testStatus()
		s.Settings.RequireACPower = true
		if line := lineWith(render(s), "Battery"); !strings.Contains(line, "64% on battery · AC only") {
			t.Errorf("battery line %q", line)
		}
		s = testStatus()
		s.BatteryPercent = nil
		if !anyContains(render(s), "AC (no battery)") {
			t.Error("a Mac without a battery has no power row")
		}
	})

	t.Run("a missing helper is a warning", func(t *testing.T) {
		s := testStatus()
		s.HelperConnected = false
		if !anyContains(render(s), "privileged helper is not connected") {
			t.Error("no helper warning")
		}
	})

	t.Run("holds, waits and time left are shown per agent", func(t *testing.T) {
		hold := testAgent("manual", "")
		hold.Origin = model.OriginManual
		exp := testNow.Add(30 * time.Minute)
		hold.ExpiresAt = &exp
		waiting := testAgent("codex", "")
		waiting.WaitingFor = "permission"
		lines := render(testStatus(withAgents(hold, waiting)))
		if line := lineWith(lines, "manual"); !strings.Contains(line, "hold") || !strings.Contains(line, "30m left") {
			t.Errorf("hold line %q", line)
		}
		if line := lineWith(lines, "codex"); !strings.Contains(line, "waiting: permission") {
			t.Errorf("waiting line %q", line)
		}
	})

	t.Run("more than eight agents are summarized", func(t *testing.T) {
		var agents []model.Assertion
		for i := range 10 {
			agents = append(agents, testAgent(fmt.Sprintf("agent%d", i), ""))
		}
		lines := render(testStatus(withAgents(agents...)))
		if !anyContains(lines, "10 keeping the Mac awake") || !anyContains(lines, "… and 2 more") {
			t.Errorf("agents: %q", lines)
		}
	})
}
