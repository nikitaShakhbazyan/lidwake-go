package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nikitaShakhbazyan/lidwake-go/internal/activity"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/agents"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/chime"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/darwin"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/ipc"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/model"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/monitor"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/settings"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/store"
)

// waitLimit bounds every wait for something a daemon goroutine should do soon.
const waitLimit = 5 * time.Second

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// eventually fails the test unless cond becomes true within waitLimit.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitLimit)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// stays fails the test if cond turns false within d.
func stays(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if !cond() {
			t.Fatalf("%s did not hold", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// shortTempDir is a temporary directory short enough for a Unix socket path (104 bytes on macOS);
// t.TempDir embeds the test name and can exceed it.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "lwd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// journal is an ordered record of side effects across fakes ("set(true)", "cue:…", "chime", …),
// so a test can check that one happened before another.
type journal struct {
	mu      sync.Mutex
	entries []string
}

func (j *journal) add(s string) {
	j.mu.Lock()
	j.entries = append(j.entries, s)
	j.mu.Unlock()
}

func (j *journal) all() []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]string(nil), j.entries...)
}

// index is the position of the first entry equal to s, or -1.
func (j *journal) index(s string) int {
	for i, e := range j.all() {
		if e == s {
			return i
		}
	}
	return -1
}

func (j *journal) count(s string) int {
	n := 0
	for _, e := range j.all() {
		if e == s {
			n++
		}
	}
	return n
}

// fakeClock is a settable wall clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// fakeTimers records AfterFunc schedules; tests fire them by hand.
type fakeTimers struct {
	mu      sync.Mutex
	entries []*fakeTimer
}

type fakeTimer struct {
	delay   time.Duration
	f       func()
	stopped bool
}

func (ft *fakeTimers) AfterFunc(d time.Duration, f func()) func() bool {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	e := &fakeTimer{delay: d, f: f}
	ft.entries = append(ft.entries, e)
	return func() bool {
		ft.mu.Lock()
		defer ft.mu.Unlock()
		was := !e.stopped
		e.stopped = true
		return was
	}
}

// live is the newest schedule that was not stopped.
func (ft *fakeTimers) live() (*fakeTimer, bool) {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	for i := len(ft.entries) - 1; i >= 0; i-- {
		if !ft.entries[i].stopped {
			return ft.entries[i], true
		}
	}
	return nil, false
}

func (ft *fakeTimers) fire(t *testing.T) {
	t.Helper()
	e, ok := ft.live()
	if !ok {
		t.Fatal("no timer scheduled")
	}
	ft.mu.Lock()
	e.stopped = true
	ft.mu.Unlock()
	e.f()
}

type fakeChimes struct{ j *journal }

func (c fakeChimes) PlayLidCloseChime(volume float64, chimeName string) {
	c.j.add("chime:" + chimeName)
}

func (c fakeChimes) PlaySleepCue(_ context.Context, soundName string, cue chime.Cue, _ float64) {
	c.j.add("cue:" + soundName + ":" + string(cue))
}

// fakeLid is a lid the test opens and closes.
type fakeLid struct {
	mu     sync.Mutex
	closed bool
}

func (l *fakeLid) read() (bool, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closed, true
}

func (l *fakeLid) set(closed bool) {
	l.mu.Lock()
	l.closed = closed
	l.mu.Unlock()
}

type fakeBattery struct {
	mu sync.Mutex
	b  darwin.Battery
	ok bool
}

func (f *fakeBattery) read() (darwin.Battery, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.b, f.ok
}

// none makes the sensor report no battery, as on a desktop Mac.
func (f *fakeBattery) none() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ok = false
}

func (f *fakeBattery) set(percent int, onBattery bool) {
	f.mu.Lock()
	f.b, f.ok = darwin.Battery{Percent: percent, OnBattery: onBattery}, true
	f.mu.Unlock()
}

// fakeSensor is the CPU temperature; open fails while broken.
type fakeSensor struct {
	mu     sync.Mutex
	temp   float64
	broken bool
}

func (s *fakeSensor) open() (monitor.TemperatureSensor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.broken {
		return nil, errors.New("no SMC")
	}
	return s, nil
}

func (s *fakeSensor) CPUTemperature() (float64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.temp, true
}

func (s *fakeSensor) Close() {}

func (s *fakeSensor) set(c float64) {
	s.mu.Lock()
	s.temp = c
	s.mu.Unlock()
}

// fakeExits is a kqueue stand-in: the test reports exits by hand.
type fakeExits struct {
	mu      sync.Mutex
	watched map[int]bool
	ch      chan int
}

func newFakeExits() *fakeExits { return &fakeExits{watched: map[int]bool{}, ch: make(chan int, 16)} }

func (f *fakeExits) Watch(pid int) error {
	f.mu.Lock()
	f.watched[pid] = true
	f.mu.Unlock()
	return nil
}

func (f *fakeExits) Exits() <-chan int { return f.ch }
func (f *fakeExits) Close() error      { return nil }

func (f *fakeExits) isWatched(pid int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.watched[pid]
}

// fakeDisplay records display assertions.
type fakeDisplay struct {
	j    *journal
	mu   sync.Mutex
	only bool
}

type fakeAssertion struct{ j *journal }

func (a fakeAssertion) Release() { a.j.add("display:drop") }

func (f *fakeDisplay) create(string) (monitor.AssertionHandle, error) {
	f.j.add("display:raise")
	return fakeAssertion{f.j}, nil
}

func (f *fakeDisplay) onlyClosedBuiltIn(bool) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.only
}

// fakeProcs is a process table for the sniff sweep and the idle monitor.
type fakeProcs struct {
	mu    sync.Mutex
	paths map[int]string
}

func (p *fakeProcs) set(pid int, path string) {
	p.mu.Lock()
	if path == "" {
		delete(p.paths, pid)
	} else {
		p.paths[pid] = path
	}
	p.mu.Unlock()
}

func (p *fakeProcs) path(pid int) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if path, ok := p.paths[pid]; ok {
		return path, nil
	}
	return "", errors.New("no such process")
}

func (p *fakeProcs) alive(pid int) bool {
	_, err := p.path(pid)
	return err == nil
}

func (p *fakeProcs) resolver() *agents.Resolver {
	return &agents.Resolver{
		Getppid:      func() int { return 1 },
		ProcessPath:  p.path,
		ParentPID:    func(int) (int, error) { return 1, nil },
		ProcessArgs:  func(int) ([]string, error) { return nil, errors.New("no args") },
		ProcessAlive: p.alive,
		AllPIDs: func() ([]int, error) {
			p.mu.Lock()
			defer p.mu.Unlock()
			var pids []int
			for pid := range p.paths {
				pids = append(pids, pid)
			}
			return pids, nil
		},
		ProcessTree: func() (map[int][]int, error) { return map[int][]int{}, nil },
		CPUTime:     func(int) (time.Duration, error) { return 0, nil },
	}
}

// harness runs a daemon against fakes for every machine dependency.
type harness struct {
	t       *testing.T
	dir     string
	cfg     Config
	clock   *fakeClock
	timers  *fakeTimers
	j       *journal
	helper  *fakeHelper
	lid     *fakeLid
	battery *fakeBattery
	sensor  *fakeSensor
	exits   *fakeExits
	display *fakeDisplay
	procs   *fakeProcs
	locks   *journal

	sleepDisabled bool
	d             *Daemon
	cancel        context.CancelFunc
	done          chan error
	stopped       bool
}

var harnessEpoch = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

// newHarness builds the fakes and a Config over them; the test may adjust both before start.
func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{
		t:       t,
		dir:     shortTempDir(t),
		clock:   &fakeClock{t: harnessEpoch},
		timers:  &fakeTimers{},
		j:       &journal{},
		lid:     &fakeLid{},
		battery: &fakeBattery{},
		sensor:  &fakeSensor{temp: 50},
		exits:   newFakeExits(),
		procs:   &fakeProcs{paths: map[int]string{}},
		locks:   &journal{},
	}
	h.helper = newFakeHelper(h.j)
	h.display = &fakeDisplay{j: h.j}
	h.battery.set(80, false)
	h.cfg = Config{
		SupportDir:   h.dir,
		SettingsPath: filepath.Join(h.dir, "config.json"),
		Socket:       filepath.Join(h.dir, "cli.sock"),
		Helper:       h.helper,
		Chimes:       fakeChimes{h.j},
		Lid:          &monitor.LidMonitor{Read: h.lid.read, Interval: 2 * time.Millisecond},
		Wake:         &monitor.WakeDetector{Period: time.Hour},
		Battery:      &monitor.BatteryMonitor{Read: h.battery.read, Interval: 5 * time.Millisecond},
		Thermal:      &monitor.ThermalMonitor{Open: h.sensor.open, Interval: 5 * time.Millisecond},
		Idle: &monitor.IdleMonitor{
			Procs:    h.procs.resolver(),
			WakePIDs: func() map[int]bool { return nil },
			Interval: time.Hour,
		},
		Session: &monitor.SessionStatusMonitor{
			Dir:          filepath.Join(h.dir, "sessions"),
			ProcessAlive: h.procs.alive,
			Interval:     time.Hour,
		},
		Processes: &monitor.ProcessWatcher{
			NewSource:    func() (monitor.ExitSource, error) { return h.exits, nil },
			ProcessAlive: func(int) bool { return true },
			Interval:     time.Hour,
		},
		Display: &monitor.DisplayHold{
			Create:                     h.display.create,
			DeclareUserActivity:        func(string) error { h.j.add("display:wake"); return nil },
			OnlyDisplayIsClosedBuiltIn: h.display.onlyClosedBuiltIn,
		},
		Procs:         h.procs.resolver(),
		Now:           h.clock.Now,
		AfterFunc:     h.timers.AfterFunc,
		BootTime:      func() (time.Time, error) { return harnessEpoch.Add(-24 * time.Hour), nil },
		ProcessPath:   h.procs.path,
		ProcessAlive:  h.procs.alive,
		SleepDisabled: func() bool { return h.sleepDisabled },
		ThermalState:  func() int { return 1 },
		Capabilities:  func() darwin.DeviceCapabilities { return darwin.DeviceCapabilities{HasLid: true, HasBattery: true} },
		LockScreen:    func() { h.locks.add("lock") },
		Log:           quiet(),

		LatchRecheckInterval: 10 * time.Millisecond,
		HelperRetryBase:      5 * time.Millisecond,
		HelperRetryMax:       20 * time.Millisecond,
		ShutdownTimeout:      time.Second,
		HelperWait:           2 * time.Second,
	}
	t.Cleanup(func() {
		if h.d != nil && !h.stopped {
			h.stop()
		}
	})
	return h
}

// writeSettings saves the defaults changed by edit as config.json.
func (h *harness) writeSettings(edit func(*settings.Settings)) {
	h.t.Helper()
	s := settings.Defaults()
	if edit != nil {
		edit(&s)
	}
	if err := s.Save(h.cfg.SettingsPath); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) writeState(st model.PersistedState) {
	h.t.Helper()
	if err := store.NewStateStore(h.dir).Save(st); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) readState() model.PersistedState {
	h.t.Helper()
	st, ok := store.NewStateStore(h.dir).Load()
	if !ok {
		h.t.Fatal("no state file")
	}
	return st
}

// start runs Serve and waits for the socket.
func (h *harness) start() {
	h.t.Helper()
	h.d = New(h.cfg)
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	h.done = make(chan error, 1)
	go func() { h.done <- h.d.Serve(ctx) }()
	eventually(h.t, "the CLI socket", func() bool { return listening(h.cfg.Socket) })
	// The initial sync reaches the helper.
	eventually(h.t, "the initial helper sync", func() bool { return h.helper.calls() >= 1 })
}

// stop cancels Serve and returns its result.
func (h *harness) stop() error {
	h.t.Helper()
	h.stopped = true
	h.cancel()
	select {
	case err := <-h.done:
		return err
	case <-time.After(waitLimit):
		h.t.Fatal("Serve did not return after cancel")
		return nil
	}
}

func (h *harness) send(req ipc.Request) ipc.Response {
	h.t.Helper()
	resp, err := ipc.SendTo(h.cfg.Socket, req, waitLimit)
	if err != nil {
		h.t.Fatalf("%s: %v", req.Op, err)
	}
	return resp
}

func (h *harness) acquire(key string) ipc.Response {
	h.t.Helper()
	return h.send(ipc.Request{Op: ipc.OpAcquire, Key: key, Tool: "claude-code"})
}

// idleSweep hands releases to the daemon as an idle sweep does: decided on the snapshot the sweep
// took through the monitor's Assertions.
func (h *harness) idleSweep(releases ...activity.Release) {
	h.t.Helper()
	h.d.idle.Assertions()
	h.d.onIdleRelease(releases)
}

func (h *harness) status() model.Status {
	h.t.Helper()
	resp := h.send(ipc.Request{Op: ipc.OpStatus})
	if !resp.OK || resp.Status == nil {
		h.t.Fatalf("status: %+v", resp)
	}
	return *resp.Status
}

// events reads events.log.
func (h *harness) events() []model.Event {
	h.t.Helper()
	f, err := os.Open(filepath.Join(h.dir, store.EventLogFileName))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		h.t.Fatal(err)
	}
	defer f.Close()
	var out []model.Event
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var line struct {
			Event model.Event `json:"event"`
		}
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			h.t.Fatalf("bad event line %q: %v", sc.Text(), err)
		}
		out = append(out, line.Event)
	}
	return out
}

func (h *harness) hasEvent(e model.Event) bool {
	for _, got := range h.events() {
		if got == e {
			return true
		}
	}
	return false
}

// helperBlocked waits until the helper's latest state is blocked.
func (h *harness) waitHelper(blocked bool) {
	h.t.Helper()
	eventually(h.t, fmt.Sprintf("helper set(%v)", blocked), func() bool {
		got, ok := h.helper.lastSet()
		return ok && got == blocked
	})
}

// waitLid waits until the daemon has seen the lid in state closed.
func (h *harness) waitLid(closed bool) {
	h.t.Helper()
	want := model.EventLidOpened
	if closed {
		want = model.EventLidClosed
	}
	before := h.countEvents(want)
	h.lid.set(closed)
	eventually(h.t, "the lid event", func() bool { return h.countEvents(want) > before })
	// The handler logs first and finishes under the daemon's lock: wait it out.
	h.d.mu.Lock()
	h.d.mu.Unlock()
}

func (h *harness) countEvents(e model.Event) int {
	n := 0
	for _, got := range h.events() {
		if got == e {
			n++
		}
	}
	return n
}

func keysOf(as []model.Assertion) []string {
	out := make([]string, len(as))
	for i, a := range as {
		out[i] = a.Key
	}
	return out
}

func joinKeys(as []model.Assertion) string { return strings.Join(keysOf(as), ",") }
