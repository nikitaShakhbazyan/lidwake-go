package tui

import (
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/nikitaShakhbazyan/lidwake-go/internal/model"
)

// fakeActions behaves like the daemon: pausing releases every hold, a timer is clamped to
// 1 min … 24 h and turns lidwake back on, and Status reflects all of it.
type fakeActions struct {
	mu          sync.Mutex
	clock       *time.Time
	status      *model.Status
	statusErr   error
	actionErr   error
	statusCalls int
	paused      []bool
	timers      []time.Duration
	releases    int
}

func (f *fakeActions) Status() (*model.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statusCalls++
	if f.statusErr != nil {
		return nil, f.statusErr
	}
	if f.status == nil {
		return nil, nil
	}
	cp := *f.status
	return &cp, nil
}

func (f *fakeActions) SetPaused(p bool) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paused = append(f.paused, p)
	if f.actionErr != nil {
		return 0, f.actionErr
	}
	f.status.Paused = p
	if !p {
		return 0, nil
	}
	n := len(f.status.Assertions)
	f.status.Assertions, f.status.Blocking = nil, false
	return n, nil
}

func (f *fakeActions) SetTimer(d time.Duration) (time.Duration, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.timers = append(f.timers, d)
	if f.actionErr != nil {
		return 0, false, f.actionErr
	}
	if d <= 0 {
		f.status.OffAt = nil
		return 0, false, nil
	}
	d = min(max(d, time.Minute), 24*time.Hour)
	at := f.clock.Add(d)
	f.status.OffAt, f.status.Paused = &at, false
	return d, true, nil
}

func (f *fakeActions) ReleaseAll() (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releases++
	if f.actionErr != nil {
		return 0, f.actionErr
	}
	n := len(f.status.Assertions)
	f.status.Assertions, f.status.Blocking = nil, false
	return n, nil
}

// harness is a dashboard on a fake daemon with a hand-moved clock and no refresh tick, driven
// synchronously: every command runs to completion and its message goes back through Update.
type harness struct {
	t     *testing.T
	clock time.Time
	fake  *fakeActions
	m     *Model
	quit  bool
}

func newHarness(t *testing.T, s *model.Status) *harness {
	h := &harness{t: t, clock: testNow}
	h.fake = &fakeActions{clock: &h.clock, status: s}
	h.m = New(h.fake, Options{Width: 80, Now: func() time.Time { return h.clock }})
	h.m.schedule = func(time.Duration, func(time.Time) tea.Msg) tea.Cmd { return nil }
	h.drive(h.m.Init())
	return h
}

func (h *harness) drive(cmd tea.Cmd) {
	h.t.Helper()
	queue := []tea.Cmd{cmd}
	for steps := 0; len(queue) > 0; steps++ {
		if steps > 200 {
			h.t.Fatal("commands never settle")
		}
		c := queue[0]
		queue = queue[1:]
		if c == nil {
			continue
		}
		switch msg := c().(type) {
		case nil:
		case tea.BatchMsg:
			queue = append(queue, msg...)
		case tea.QuitMsg:
			h.quit = true
		default:
			_, next := h.m.Update(msg)
			queue = append(queue, next)
		}
	}
}

func (h *harness) send(msg tea.Msg) {
	h.t.Helper()
	_, cmd := h.m.Update(msg)
	h.drive(cmd)
}

func runes(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

var space = tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}}

// press sends each key as its own event.
func (h *harness) press(keys ...string) {
	h.t.Helper()
	for _, k := range keys {
		if k == " " {
			h.send(space)
		} else {
			h.send(runes(k))
		}
	}
}

func (h *harness) flash() string { return h.m.flash }

func TestDashboardKeys(t *testing.T) {
	t.Run("space and o turn lidwake off and on", func(t *testing.T) {
		h := newHarness(t, testStatus(withAgents(testAgent("claude-code", ""), testAgent("codex", ""))))
		h.press(" ")
		if got := h.flash(); got != "lidwake is off — released 2 holds" {
			t.Errorf("flash %q", got)
		}
		h.press("o")
		if got := h.flash(); got != "lidwake is on" {
			t.Errorf("flash %q", got)
		}
		h.press("O")
		if got := h.flash(); got != "lidwake is off" {
			t.Errorf("flash %q", got)
		}
		if want := []bool{true, false, true}; !slices.Equal(h.fake.paused, want) {
			t.Errorf("paused %v, want %v", h.fake.paused, want)
		}
	})

	t.Run("on/off needs a status", func(t *testing.T) {
		h := newHarness(t, testStatus())
		h.fake.statusErr = errors.New("lidwake daemon is not running")
		h.send(tickMsg{})
		h.press(" ")
		if len(h.fake.paused) != 0 || h.flash() != "" {
			t.Errorf("toggled without a status: %v, flash %q", h.fake.paused, h.flash())
		}
		if !strings.Contains(h.m.View(), "daemon not running") {
			t.Error("the view does not say the daemon is down")
		}
	})

	t.Run("t steps through the presets and back to off", func(t *testing.T) {
		h := newHarness(t, testStatus())
		h.press("t", "t", "T", "t", "t", "t")
		want := []time.Duration{15 * time.Minute, 30 * time.Minute, time.Hour, 2 * time.Hour, 4 * time.Hour, 0}
		if !slices.Equal(h.fake.timers, want) {
			t.Errorf("timers %v, want %v", h.fake.timers, want)
		}
		if got := h.flash(); got != "off timer cancelled" {
			t.Errorf("flash %q", got)
		}
	})

	t.Run("t steps from what is left, not from the last preset", func(t *testing.T) {
		h := newHarness(t, testStatus(offIn(50*time.Minute)))
		h.press("t")
		h.fake.status.OffAt = ptr(testNow.Add(29*time.Minute + 45*time.Second))
		h.send(tickMsg{})
		h.press("t")
		if want := []time.Duration{time.Hour, time.Hour}; !slices.Equal(h.fake.timers, want) {
			t.Errorf("timers %v, want %v", h.fake.timers, want)
		}
	})

	t.Run("t on a paused lidwake starts at 15 minutes", func(t *testing.T) {
		h := newHarness(t, testStatus(paused(), offIn(2*time.Hour)))
		h.press("t")
		if want := []time.Duration{15 * time.Minute}; !slices.Equal(h.fake.timers, want) {
			t.Errorf("timers %v, want %v", h.fake.timers, want)
		}
	})

	t.Run("the timer flash says when lidwake turns off", func(t *testing.T) {
		h := newHarness(t, testStatus())
		h.press("t")
		want := "lidwake turns off at " + testNow.Add(15*time.Minute).Format("15:04") + " (in 15m)"
		if got := h.flash(); got != want {
			t.Errorf("flash %q, want %q", got, want)
		}
	})

	t.Run("plus adds and minus subtracts 15 minutes", func(t *testing.T) {
		h := newHarness(t, testStatus())
		h.press("+", "=", "-", "_")
		want := []time.Duration{15 * time.Minute, 30 * time.Minute, 15 * time.Minute, 0}
		if !slices.Equal(h.fake.timers, want) {
			t.Errorf("timers %v, want %v", h.fake.timers, want)
		}
	})

	t.Run("minus cancels below a minute", func(t *testing.T) {
		h := newHarness(t, testStatus(offIn(16*time.Minute)))
		h.press("-")
		h.fake.status.OffAt = ptr(testNow.Add(15*time.Minute + 59*time.Second))
		h.send(tickMsg{})
		h.press("-")
		if want := []time.Duration{time.Minute, 0}; !slices.Equal(h.fake.timers, want) {
			t.Errorf("timers %v, want %v", h.fake.timers, want)
		}
	})

	t.Run("r releases all holds", func(t *testing.T) {
		h := newHarness(t, testStatus(withAgents(testAgent("claude-code", ""), testAgent("codex", ""))))
		h.press("r")
		if got := h.flash(); got != "released 2 holds" {
			t.Errorf("flash %q", got)
		}
		h.press("R")
		if got := h.flash(); got != "nothing to release" {
			t.Errorf("flash %q", got)
		}
		h.fake.status.Assertions = []model.Assertion{testAgent("cursor", "")}
		h.press("r")
		if got := h.flash(); got != "released 1 hold" {
			t.Errorf("flash %q", got)
		}
	})

	t.Run("q, Esc and Ctrl-C quit", func(t *testing.T) {
		for _, k := range []tea.KeyMsg{runes("q"), runes("Q"), {Type: tea.KeyEsc}, {Type: tea.KeyCtrlC}} {
			h := newHarness(t, testStatus())
			h.send(k)
			if !h.quit || h.m.View() != "" {
				t.Errorf("%v did not quit", k)
			}
		}
	})

	t.Run("other keys, pastes and Alt combinations do nothing", func(t *testing.T) {
		h := newHarness(t, testStatus())
		h.send(runes("x"))
		h.send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("t"), Paste: true})
		h.send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q"), Alt: true})
		h.send(tea.KeyMsg{Type: tea.KeyUp})
		if h.quit || len(h.fake.timers) != 0 || h.flash() != "" {
			t.Errorf("quit=%v timers=%v flash=%q", h.quit, h.fake.timers, h.flash())
		}
	})

	t.Run("a failed action says so", func(t *testing.T) {
		h := newHarness(t, testStatus())
		h.fake.actionErr = errors.New("lidwake daemon is not running")
		h.press("r")
		if got := h.flash(); got != "the daemon did not answer" {
			t.Errorf("flash %q", got)
		}
		h.fake.actionErr = &RefusedError{Op: "timer", Message: "bad ttl"}
		h.press("t")
		if got := h.flash(); got != "the daemon refused: bad ttl" {
			t.Errorf("flash %q", got)
		}
		h.fake.actionErr = &RefusedError{Op: "pause"}
		h.press(" ")
		if got := h.flash(); got != "the daemon refused the request" {
			t.Errorf("flash %q", got)
		}
	})

	t.Run("a reply without a status gets the missing-daemon screen", func(t *testing.T) {
		h := newHarness(t, testStatus())
		h.fake.status = nil
		h.send(tickMsg{})
		view := h.m.View()
		if !strings.Contains(view, "daemon not running") || !strings.Contains(view, "the daemon sent no status") {
			t.Errorf("view %q", view)
		}
	})

	t.Run("feedback stays for 4 seconds", func(t *testing.T) {
		h := newHarness(t, testStatus())
		h.press("r")
		h.clock = h.clock.Add(3900 * time.Millisecond)
		if !strings.Contains(h.m.View(), "nothing to release") {
			t.Error("the flash vanished before 4 s")
		}
		h.clock = h.clock.Add(100 * time.Millisecond)
		if strings.Contains(h.m.View(), "nothing to release") {
			t.Error("the flash outlived 4 s")
		}
	})

	t.Run("refreshes every second", func(t *testing.T) {
		h := newHarness(t, testStatus())
		var scheduled []time.Duration
		h.m.schedule = func(d time.Duration, fn func(time.Time) tea.Msg) tea.Cmd {
			scheduled = append(scheduled, d)
			return nil
		}
		before := h.fake.statusCalls
		h.fake.status.LidClosed = false
		h.send(tickMsg{})
		if h.fake.statusCalls != before+1 || !slices.Equal(scheduled, []time.Duration{time.Second}) {
			t.Errorf("status calls %d → %d, scheduled %v", before, h.fake.statusCalls, scheduled)
		}
		if !strings.Contains(lineWith(strings.Split(h.m.View(), "\n"), "Lid "), "open") {
			t.Error("the refreshed status is not shown")
		}
	})

	t.Run("a resize refetches and fits the new size", func(t *testing.T) {
		h := newHarness(t, testStatus(withAgents(testAgent("claude-code", strings.Repeat("reason ", 30)))))
		before := h.fake.statusCalls
		h.send(tea.WindowSizeMsg{Width: 60, Height: 12})
		if h.fake.statusCalls != before+1 {
			t.Error("a resize did not refetch")
		}
		lines := strings.Split(h.m.View(), "\n")
		if len(lines) > 12 {
			t.Errorf("%d lines for a 12-line terminal", len(lines))
		}
		if !strings.Contains(lines[0], "lidwake") {
			t.Errorf("the headline is not first: %q", lines[0])
		}
		for _, l := range lines {
			if w := cellWidth(l); w > 60 {
				t.Errorf("%d > 60: %s", w, l)
			}
		}
	})

	t.Run("nothing is drawn before the first status", func(t *testing.T) {
		m := New(&fakeActions{status: testStatus()}, Options{})
		if m.View() != "" {
			t.Error("a frame before any status arrived")
		}
	})

	t.Run("keys typed together run in order", func(t *testing.T) {
		h := newHarness(t, testStatus())
		h.send(runes("tt+"))
		want := []time.Duration{15 * time.Minute, 30 * time.Minute, 45 * time.Minute}
		if !slices.Equal(h.fake.timers, want) {
			t.Errorf("timers %v, want %v", h.fake.timers, want)
		}
	})

	t.Run("keys pressed during an action wait for it", func(t *testing.T) {
		h := newHarness(t, testStatus())
		_, first := h.m.Update(runes("t"))
		_, second := h.m.Update(runes("t"))
		if second != nil {
			t.Fatal("a second action started while the first was in flight")
		}
		h.drive(first)
		if want := []time.Duration{15 * time.Minute, 30 * time.Minute}; !slices.Equal(h.fake.timers, want) {
			t.Errorf("timers %v, want %v", h.fake.timers, want)
		}
	})

	t.Run("a status fetched before an action does not overwrite it", func(t *testing.T) {
		h := newHarness(t, testStatus())
		_, refresh := h.m.Update(tickMsg{})
		stale := refresh() // the daemon answers before the timer is set …
		h.press("t")       // … and the answer arrives after
		_, cmd := h.m.Update(stale)
		if h.m.status.OffAt == nil {
			t.Fatal("the stale status replaced the timer")
		}
		if cmd == nil {
			t.Error("a discarded status was not refetched")
		}
		h.drive(cmd)
		if h.m.status.OffAt == nil || !h.m.status.OffAt.Equal(testNow.Add(15*time.Minute)) {
			t.Errorf("off at %v", h.m.status.OffAt)
		}
	})
}

func TestNextPreset(t *testing.T) {
	// The timer key's rule: the first preset more than 30 s above what is left; off after 4h.
	for _, c := range []struct {
		left *time.Duration
		want time.Duration
		ok   bool
	}{
		{nil, 15 * time.Minute, true},
		{ptr(900 * time.Second), 30 * time.Minute, true},
		{ptr(3000 * time.Second), time.Hour, true},
		{ptr(14400 * time.Second), 0, false},
		{ptr(0 * time.Second), 15 * time.Minute, true},
	} {
		got, ok := nextPreset(c.left)
		if got != c.want || ok != c.ok {
			t.Errorf("nextPreset(%v) = %v, %v; want %v, %v", c.left, got, ok, c.want, c.ok)
		}
	}
}

func ptr[T any](v T) *T { return &v }
