package tui

import (
	"errors"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/nikitaShakhbazyan/lidwake-go/internal/model"
)

// Actions is how the dashboard talks to the daemon: DaemonActions over the CLI socket in
// production, a fake in tests.
type Actions interface {
	// Status fetches the daemon's status.
	Status() (*model.Status, error)
	// SetPaused turns lidwake off (true) or back on (false). released is how many holds turning
	// it off dropped.
	SetPaused(paused bool) (released int, err error)
	// SetTimer arms the off timer to fire after d, or cancels it when d is zero. left is the time
	// until it fires as the daemon applied it (clamped); armed is false when it was cancelled.
	SetTimer(d time.Duration) (left time.Duration, armed bool, err error)
	// ReleaseAll force-releases every hold.
	ReleaseAll() (released int, err error)
}

// Options configure the dashboard.
type Options struct {
	// Color emits ANSI styles; the caller decides (a terminal, and NO_COLOR unset).
	Color bool
	// Width is the frame width until the terminal reports its size, and RenderOnce's width;
	// zero means 80.
	Width int
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

const (
	refreshInterval = time.Second
	flashTTL        = 4 * time.Second
	timerStep       = 15 * time.Minute
	// minTimer: stepping the timer down below a minute cancels it instead.
	minTimer = time.Minute
	// queueLimit bounds the keys waiting for an action in flight.
	queueLimit = 16
)

// timerPresets are what the timer key steps through.
var timerPresets = []time.Duration{15 * time.Minute, 30 * time.Minute, time.Hour, 2 * time.Hour, 4 * time.Hour}

// nextPreset is off → 15m → 30m → 1h → 2h → 4h → off, starting from whatever is left now (nil:
// no timer). The next preset must exceed what is left by more than 30 s, so a timer that just
// got set to 30m and has ticked a few seconds steps on to 1h rather than back to 30m.
func nextPreset(left *time.Duration) (time.Duration, bool) {
	if left == nil {
		return timerPresets[0], true
	}
	for _, p := range timerPresets {
		if p > *left+30*time.Second {
			return p, true
		}
	}
	return 0, false
}

type (
	tickMsg   struct{}
	statusMsg struct {
		status *model.Status
		err    error
		// gen is the action generation the fetch started in; an older one may predate an action.
		gen int
	}
	actionMsg struct {
		flash string
		// apply mirrors the action's effect on the shown status, so keys queued behind it work
		// from the new state before the next fetch lands.
		apply func(*model.Status)
	}
)

// Model is the bubbletea model of the live dashboard. Keys: space/o on/off · t cycle the off
// timer · +/- move it by 15 min · r release all · q/Esc/Ctrl-C quit. It refreshes every second
// and on resize; a key's feedback stays for 4 s. One action runs at a time and keys pressed
// meanwhile run after it, in order, against the state it left — the same as reading keys one by
// one between round trips.
type Model struct {
	actions  Actions
	now      func() time.Time
	color    bool
	width    int
	height   int
	schedule func(time.Duration, func(time.Time) tea.Msg) tea.Cmd

	loaded   bool
	status   *model.Status
	err      error
	fetching bool
	gen      int

	busy  bool
	queue []string

	flash    string
	flashAt  time.Time
	quitting bool
}

// New returns the dashboard model.
func New(a Actions, opts Options) *Model {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	width := opts.Width
	if width <= 0 {
		width = 80
	}
	return &Model{actions: a, now: now, color: opts.Color, width: width, schedule: tea.Tick}
}

// Init fetches the first status and starts the refresh tick.
func (m *Model) Init() tea.Cmd {
	return tea.Batch(m.fetch(), m.tick())
}

// Update handles keys, refresh ticks, resizes and the results of fetches and actions.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, m.fetch()
	case tickMsg:
		return m, tea.Batch(m.fetch(), m.tick())
	case statusMsg:
		m.fetching = false
		if msg.gen != m.gen {
			// Started before the last action landed: it may show the state the action replaced.
			return m, m.fetch()
		}
		m.loaded = true
		m.status, m.err = nil, msg.err
		if msg.status != nil {
			cp := *msg.status
			m.status = &cp
		} else if m.err == nil {
			m.err = errNoStatus
		}
		return m, nil
	case actionMsg:
		return m, m.finishAction(msg)
	case tea.KeyMsg:
		return m, m.keys(keysOf(msg))
	}
	return m, nil
}

// View renders the current frame, cut to the terminal's height so the headline stays on screen.
func (m *Model) View() string {
	if m.quitting || !m.loaded {
		return ""
	}
	now := m.now()
	flash := ""
	if m.flash != "" && now.Sub(m.flashAt) < flashTTL {
		flash = m.flash
	}
	lines := Render(Frame{Status: m.status, DaemonError: m.err, Now: now, Width: m.width, Color: m.color, Flash: flash})
	if m.height > 0 && len(lines) > m.height {
		lines = lines[:m.height]
	}
	return strings.Join(lines, "\n")
}

// keysOf splits a key event into the keys the dashboard knows. Runes typed faster than one read
// arrive together and are handled one by one; pasted text and Alt combinations are not keys.
func keysOf(k tea.KeyMsg) []string {
	if k.Paste || k.Alt {
		return nil
	}
	switch k.Type {
	case tea.KeyCtrlC:
		return []string{"ctrl+c"}
	case tea.KeyEsc:
		return []string{"esc"}
	case tea.KeySpace:
		return []string{" "}
	case tea.KeyRunes:
		out := make([]string, len(k.Runes))
		for i, r := range k.Runes {
			out[i] = string(r)
		}
		return out
	}
	return nil
}

func isQuit(k string) bool {
	switch k {
	case "q", "Q", "esc", "ctrl+c":
		return true
	}
	return false
}

func (m *Model) keys(keys []string) tea.Cmd {
	var cmds []tea.Cmd
	for _, k := range keys {
		if isQuit(k) {
			m.quitting = true
			return tea.Quit
		}
		if m.busy {
			if len(m.queue) < queueLimit {
				m.queue = append(m.queue, k)
			}
			continue
		}
		if cmd := m.act(k); cmd != nil {
			m.busy = true
			cmds = append(cmds, cmd)
		}
	}
	return tea.Batch(cmds...)
}

func (m *Model) finishAction(msg actionMsg) tea.Cmd {
	m.busy = false
	m.flash, m.flashAt = msg.flash, m.now()
	if msg.apply != nil && m.status != nil {
		msg.apply(m.status)
	}
	m.gen++
	cmds := []tea.Cmd{m.fetch()}
	for len(m.queue) > 0 && !m.busy {
		k := m.queue[0]
		m.queue = m.queue[1:]
		if cmd := m.act(k); cmd != nil {
			m.busy = true
			cmds = append(cmds, cmd)
		}
	}
	return tea.Batch(cmds...)
}

// act starts the action bound to k, or returns nil for a key that does nothing now.
func (m *Model) act(k string) tea.Cmd {
	switch k {
	case " ", "o", "O":
		return m.togglePause()
	case "t", "T":
		if p, ok := nextPreset(m.remaining()); ok {
			return m.setTimer(p)
		}
		return m.setTimer(0)
	case "+", "=":
		return m.setTimer(m.left() + timerStep)
	case "-", "_":
		left := m.left() - timerStep
		if left < minTimer {
			left = 0
		}
		return m.setTimer(left)
	case "r", "R":
		return m.releaseAll()
	}
	return nil
}

// remaining is the time until the off timer fires, nil when none runs (or lidwake is off).
func (m *Model) remaining() *time.Duration {
	if m.status == nil || m.status.Paused || m.status.OffAt == nil {
		return nil
	}
	left := max(0, m.status.OffAt.Sub(m.now()))
	return &left
}

func (m *Model) left() time.Duration {
	if r := m.remaining(); r != nil {
		return *r
	}
	return 0
}

func (m *Model) togglePause() tea.Cmd {
	if m.status == nil {
		return nil
	}
	pause := !m.status.Paused
	a := m.actions
	return func() tea.Msg {
		n, err := a.SetPaused(pause)
		if err != nil {
			return actionMsg{flash: failure(err)}
		}
		text := "lidwake is on"
		if pause {
			text = "lidwake is off"
			if n > 0 {
				text += " — released " + holds(n)
			}
		}
		return actionMsg{flash: text, apply: func(s *model.Status) { s.Paused = pause }}
	}
}

func (m *Model) setTimer(d time.Duration) tea.Cmd {
	a, now := m.actions, m.now
	return func() tea.Msg {
		left, armed, err := a.SetTimer(d)
		if err != nil {
			return actionMsg{flash: failure(err)}
		}
		if !armed {
			return actionMsg{flash: "off timer cancelled", apply: func(s *model.Status) { s.OffAt = nil }}
		}
		at := now().Add(left)
		return actionMsg{
			flash: fmt.Sprintf("lidwake turns off at %s (in %s)", at.Format("15:04"), FormatDuration(left, false)),
			// Setting a timer also turns a paused lidwake back on.
			apply: func(s *model.Status) { s.OffAt, s.Paused = &at, false },
		}
	}
}

func (m *Model) releaseAll() tea.Cmd {
	a := m.actions
	return func() tea.Msg {
		n, err := a.ReleaseAll()
		if err != nil {
			return actionMsg{flash: failure(err)}
		}
		if n == 0 {
			return actionMsg{flash: "nothing to release"}
		}
		return actionMsg{flash: "released " + holds(n)}
	}
}

func (m *Model) fetch() tea.Cmd {
	if m.fetching {
		return nil
	}
	m.fetching = true
	a, gen := m.actions, m.gen
	return func() tea.Msg {
		s, err := a.Status()
		return statusMsg{status: s, err: err, gen: gen}
	}
}

func (m *Model) tick() tea.Cmd {
	return m.schedule(refreshInterval, func(time.Time) tea.Msg { return tickMsg{} })
}

func holds(n int) string {
	if n == 1 {
		return "1 hold"
	}
	return fmt.Sprintf("%d holds", n)
}

// failure is the feedback for an action the daemon did not carry out.
func failure(err error) string {
	if r, ok := errors.AsType[*RefusedError](err); ok {
		if r.Message == "" {
			return "the daemon refused the request"
		}
		return "the daemon refused: " + r.Message
	}
	return "the daemon did not answer"
}
