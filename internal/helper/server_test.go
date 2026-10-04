package helper

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nikitaShakhbazyan/lidwake-go/internal/ipc"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/paths"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/policy"
)

const waitLimit = 5 * time.Second

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

// shortTempDir is a temporary directory short enough for a Unix socket path (104 bytes on
// macOS); t.TempDir embeds the test name and can exceed it.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "lwh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// ---- fake mechanisms (synchronized: the test goroutine reads them while the server runs) ----

type fakeIdle struct {
	mu       sync.Mutex
	held     bool
	acquires int
}

func (f *fakeIdle) IsHeld() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.held
}

func (f *fakeIdle) Acquire() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.held {
		f.acquires++
		f.held = true
	}
}

func (f *fakeIdle) acquireCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.acquires
}

func (f *fakeIdle) Release() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.held = false
}

type fakeClamshell struct {
	mu       sync.Mutex
	disabled bool
	calls    []bool
	// failOn makes SetDisabled(value) fail for that value.
	failOn map[bool]error
}

func (f *fakeClamshell) IsDisabled() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.disabled
}

func (f *fakeClamshell) SetDisabled(d bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failOn[d]; err != nil {
		return err
	}
	f.calls = append(f.calls, d)
	f.disabled = d
	return nil
}

func (f *fakeClamshell) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// manualClock replaces time.AfterFunc: scheduled checks run only when the test fires them.
type manualClock struct {
	mu        sync.Mutex
	pending   []func()
	graces    []time.Duration
	scheduled chan struct{}
}

func newManualClock() *manualClock { return &manualClock{scheduled: make(chan struct{}, 64)} }

func (c *manualClock) AfterFunc(d time.Duration, f func()) {
	c.mu.Lock()
	c.pending = append(c.pending, f)
	c.graces = append(c.graces, d)
	c.mu.Unlock()
	c.scheduled <- struct{}{}
}

// waitScheduled waits for the next dead-man check to be scheduled and returns its index.
func (c *manualClock) waitScheduled(t *testing.T) int {
	t.Helper()
	select {
	case <-c.scheduled:
	case <-time.After(waitLimit):
		t.Fatal("no dead-man check was scheduled")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.pending) - 1
}

// expectNoneScheduled asserts that nothing gets scheduled for a while.
func (c *manualClock) expectNoneScheduled(t *testing.T) {
	t.Helper()
	select {
	case <-c.scheduled:
		t.Fatal("a dead-man check was scheduled")
	case <-time.After(150 * time.Millisecond):
	}
}

func (c *manualClock) grace(i int) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.graces[i]
}

func (c *manualClock) fire(i int) {
	c.mu.Lock()
	f := c.pending[i]
	c.mu.Unlock()
	f()
}

// ---- harness ---------------------------------------------------------------------------------

// testUID is the user every connection belongs to unless a test says otherwise.
const testUID = 501

// allowAll authorizes every connection as testUID's daemon.
func allowAll(*net.UnixConn) (int, bool) { return testUID, true }

type harness struct {
	t         *testing.T
	dir       string
	socket    string
	idle      *fakeIdle
	clamshell *fakeClamshell
	original  originalFile
	blocker   SleepBlocker
	clock     *manualClock
	cancel    context.CancelFunc
	done      chan error
	// uids feeds asUsers.
	uids chan int
}

type option func(*harness, *Config)

// start runs Serve with the real sleep-block policy over fake mechanisms and the real
// original-setting file in a temporary directory.
func start(t *testing.T, opts ...option) *harness {
	t.Helper()
	dir := shortTempDir(t)
	h := &harness{
		t:         t,
		dir:       dir,
		socket:    filepath.Join(dir, "run", "h.sock"),
		idle:      &fakeIdle{},
		clamshell: &fakeClamshell{},
		original:  originalFile{path: filepath.Join(dir, "db", "sleep-disabled-before"), log: discard},
		clock:     newManualClock(),
		done:      make(chan error, 1),
	}
	cfg := Config{
		Socket:    h.socket,
		RunDir:    filepath.Join(dir, "run"),
		Authorize: allowAll,
		AfterFunc: h.clock.AfterFunc,
		Version:   "9.9.9-test",
		Logger:    discard,
	}
	for _, o := range opts {
		o(h, &cfg)
	}
	if cfg.Blocker == nil {
		cfg.Blocker = policy.NewSleepBlockPolicy(h.idle, h.clamshell, h.original)
	}
	h.blocker = cfg.Blocker
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() { h.done <- Serve(ctx, cfg) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-h.done:
		case <-time.After(waitLimit):
			t.Error("Serve did not return after cancellation")
		}
	})
	h.waitListening()
	return h
}

func (h *harness) waitListening() {
	h.t.Helper()
	deadline := time.Now().Add(waitLimit)
	for time.Now().Before(deadline) {
		select {
		case err := <-h.done:
			h.t.Fatalf("Serve returned early: %v", err)
		default:
		}
		// Serve chmods its socket 0666 once it accepts; a stale file at the path never has that
		// mode. (A probe dial would consume an authorization decision.)
		if info, err := os.Lstat(h.socket); err == nil && info.Mode()&os.ModeSocket != 0 && info.Mode().Perm() == 0o666 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.t.Fatal("the helper never started listening")
}

// stop cancels Serve and returns its result.
func (h *harness) stop() error {
	h.t.Helper()
	h.cancel()
	return h.wait()
}

func (h *harness) wait() error {
	h.t.Helper()
	select {
	case err := <-h.done:
		h.done <- err // keep it for the cleanup
		return err
	case <-time.After(waitLimit):
		h.t.Fatal("Serve did not return")
		return nil
	}
}

func (h *harness) dial() net.Conn {
	h.t.Helper()
	conn, err := net.DialTimeout("unix", h.socket, time.Second)
	if err != nil {
		h.t.Fatalf("dial: %v", err)
	}
	h.t.Cleanup(func() { conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(waitLimit))
	return conn
}

// asUsers authorizes each connection as the UID dialAs queued for it.
func asUsers(h *harness, c *Config) {
	h.uids = make(chan int, 4)
	c.Authorize = func(*net.UnixConn) (int, bool) {
		select {
		case uid := <-h.uids:
			return uid, true
		case <-time.After(waitLimit):
			return 0, false
		}
	}
}

// dialAs connects as uid's daemon (the harness needs asUsers). The round trip proves the
// connection was authorized as uid before the next dial can take the next UID.
func (h *harness) dialAs(uid int) net.Conn {
	h.t.Helper()
	h.uids <- uid
	conn := h.dial()
	state(h.t, conn)
	return conn
}

func roundTrip(t *testing.T, conn net.Conn, req ipc.HelperRequest) ipc.HelperResponse {
	t.Helper()
	if err := ipc.WriteFrame(conn, req); err != nil {
		t.Fatalf("write %v: %v", req, err)
	}
	var resp ipc.HelperResponse
	if err := ipc.ReadFrame(conn, &resp); err != nil {
		t.Fatalf("read reply to %v: %v", req, err)
	}
	return resp
}

func set(t *testing.T, conn net.Conn, blocked bool) ipc.HelperResponse {
	t.Helper()
	return roundTrip(t, conn, ipc.HelperRequest{Op: ipc.HelperSet, Blocked: blocked})
}

func state(t *testing.T, conn net.Conn) bool {
	t.Helper()
	resp := roundTrip(t, conn, ipc.HelperRequest{Op: ipc.HelperState})
	if !resp.OK {
		t.Fatalf("state: %+v", resp)
	}
	return resp.Blocked
}

// expectClosed asserts that the server closed conn (a read sees EOF or a reset).
func expectClosed(t *testing.T, conn net.Conn) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(waitLimit))
	var resp ipc.HelperResponse
	err := ipc.ReadFrame(conn, &resp)
	if err == nil {
		t.Fatalf("got a reply %+v on a connection that should be closed", resp)
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		t.Fatal("the connection was not closed")
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitLimit)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// ---- requests --------------------------------------------------------------------------------

func TestServeRequests(t *testing.T) {
	t.Run("set state and version round trip", func(t *testing.T) {
		h := start(t)
		conn := h.dial()

		if state(t, conn) {
			t.Fatal("blocked before any set")
		}
		resp := set(t, conn, true)
		if !resp.OK || !resp.Blocked || resp.Error != "" {
			t.Fatalf("set(true) = %+v", resp)
		}
		if !h.clamshell.IsDisabled() || !h.idle.IsHeld() {
			t.Fatal("set(true) did not apply both mechanisms")
		}
		if saved, ok := h.original.Load(); !ok || saved {
			t.Fatalf("original setting while blocked = (%v, %v), want (false, true)", saved, ok)
		}
		if !state(t, conn) {
			t.Fatal("state after set(true) = false")
		}
		v := roundTrip(t, conn, ipc.HelperRequest{Op: ipc.HelperVersion})
		if !v.OK || v.Version != "9.9.9-test" || !v.Blocked {
			t.Fatalf("version = %+v", v)
		}
		resp = set(t, conn, false)
		if !resp.OK || resp.Blocked {
			t.Fatalf("set(false) = %+v", resp)
		}
		if h.clamshell.IsDisabled() || h.idle.IsHeld() {
			t.Fatal("set(false) did not release both mechanisms")
		}
		if _, ok := h.original.Load(); ok {
			t.Fatal("the original setting is still saved after the release")
		}
	})

	t.Run("a repeated set(true) re-applies the block", func(t *testing.T) {
		h := start(t)
		conn := h.dial()
		set(t, conn, true)
		set(t, conn, true)
		if got := h.clamshell.callCount(); got != 2 {
			t.Fatalf("clamshell SetDisabled calls = %d, want 2", got)
		}
		if n := h.idle.acquireCount(); n != 1 {
			t.Fatalf("idle acquires = %d, want 1", n)
		}
	})

	t.Run("the ipc helper client talks to the server", func(t *testing.T) {
		h := start(t)
		c := ipc.NewHelperClient(h.socket)
		defer c.Close()
		if blocked, err := c.SetBlocked(true); err != nil || !blocked {
			t.Fatalf("SetBlocked(true) = %v, %v", blocked, err)
		}
		if blocked, err := c.State(); err != nil || !blocked {
			t.Fatalf("State() = %v, %v", blocked, err)
		}
		if v, err := c.Version(); err != nil || v != "9.9.9-test" {
			t.Fatalf("Version() = %q, %v", v, err)
		}
		if blocked, err := c.SetBlocked(false); err != nil || blocked {
			t.Fatalf("SetBlocked(false) = %v, %v", blocked, err)
		}
	})

	t.Run("version defaults to paths.Version", func(t *testing.T) {
		h := start(t, func(_ *harness, c *Config) { c.Version = "" })
		v := roundTrip(t, h.dial(), ipc.HelperRequest{Op: ipc.HelperVersion})
		if v.Version != paths.Version {
			t.Fatalf("version = %q, want %q", v.Version, paths.Version)
		}
	})

	t.Run("a failed block is reported with the resulting state", func(t *testing.T) {
		boom := errors.New("pmset exited 1")
		h := start(t, func(h *harness, _ *Config) { h.clamshell.failOn = map[bool]error{true: boom} })
		conn := h.dial()
		resp := set(t, conn, true)
		if resp.OK || resp.Blocked || !strings.Contains(resp.Error, "pmset exited 1") {
			t.Fatalf("set(true) with a failing pmset = %+v", resp)
		}
		// Partial protection: the idle assertion stays while the clamshell block is retried.
		if !h.idle.IsHeld() {
			t.Fatal("the idle assertion was dropped")
		}
		if state(t, conn) {
			t.Fatal("state reports blocked after a failed block")
		}
	})

	t.Run("an unknown op is an error and the connection stays usable", func(t *testing.T) {
		h := start(t)
		conn := h.dial()
		resp := roundTrip(t, conn, ipc.HelperRequest{Op: "explode"})
		if resp.OK || !strings.Contains(resp.Error, "explode") {
			t.Fatalf("unknown op = %+v", resp)
		}
		if state(t, conn) {
			t.Fatal("unexpected block")
		}
		if h.clamshell.callCount() != 0 {
			t.Fatal("an unknown op touched the clamshell setting")
		}
	})

	t.Run("a malformed frame closes the connection with no effect", func(t *testing.T) {
		h := start(t)
		conn := h.dial()
		if _, err := conn.Write([]byte{0, 0, 0, 3, '{', 'x', '}'}); err != nil {
			t.Fatal(err)
		}
		expectClosed(t, conn)
		if h.clamshell.callCount() != 0 || h.idle.IsHeld() {
			t.Fatal("a malformed frame had an effect")
		}
		if state(t, h.dial()) {
			t.Fatal("unexpected block")
		}
	})

	t.Run("a set is refused once the helper is closing", func(t *testing.T) {
		clamshell := &fakeClamshell{}
		s := newServer(Config{Blocker: policy.NewSleepBlockPolicy(&fakeIdle{}, clamshell, &memOriginal{}), Logger: discard}, func(error) {})
		s.closing = true
		resp, _ := s.respond(testUID, ipc.HelperRequest{Op: ipc.HelperSet, Blocked: true})
		if resp.OK || resp.Blocked || !strings.Contains(resp.Error, "shutting down") {
			t.Fatalf("set while closing = %+v", resp)
		}
		if clamshell.callCount() != 0 {
			t.Fatal("a refused set touched the clamshell setting")
		}
	})

	t.Run("an unblock while closing reports the block already cleared", func(t *testing.T) {
		clamshell := &fakeClamshell{}
		s := newServer(Config{Blocker: policy.NewSleepBlockPolicy(&fakeIdle{}, clamshell, &memOriginal{}), Logger: discard}, func(error) {})
		s.closing = true
		resp, mayRelaunch := s.respond(testUID, ipc.HelperRequest{Op: ipc.HelperSet, Blocked: false})
		if !resp.OK || resp.Blocked || resp.Error != "" || mayRelaunch {
			t.Fatalf("unblock while closing = %+v (relaunch %v), want a plain OK", resp, mayRelaunch)
		}
		if clamshell.callCount() != 0 {
			t.Fatal("an unblock while closing touched the clamshell setting")
		}
	})
}

// memOriginal is an in-memory original-setting store for tests that need no file.
type memOriginal struct {
	mu    sync.Mutex
	value *bool
}

func (m *memOriginal) Load() (bool, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.value == nil {
		return false, false
	}
	return *m.value, true
}

func (m *memOriginal) Save(d bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.value = &d
	return nil
}

func (m *memOriginal) Clear() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.value = nil
}

// ---- authorization ---------------------------------------------------------------------------

// decisions authorizes connections in arrival order from a queue the test fills before dialing.
func decisions(queue chan bool) option {
	return func(_ *harness, c *Config) {
		c.Authorize = func(*net.UnixConn) (int, bool) {
			select {
			case ok := <-queue:
				return testUID, ok
			case <-time.After(waitLimit):
				return 0, false
			}
		}
	}
}

func TestServeAuthorization(t *testing.T) {
	t.Run("an unauthorized connection is closed with no effect", func(t *testing.T) {
		queue := make(chan bool, 4)
		h := start(t, decisions(queue))
		queue <- false
		intruder := h.dial()
		_ = ipc.WriteFrame(intruder, ipc.HelperRequest{Op: ipc.HelperSet, Blocked: true}) // may fail: already closed
		expectClosed(t, intruder)
		if h.clamshell.callCount() != 0 || h.idle.IsHeld() || h.blocker.Blocked() {
			t.Fatal("an unauthorized request had an effect")
		}
		if _, ok := h.original.Load(); ok {
			t.Fatal("an unauthorized request saved an original setting")
		}
		queue <- true
		if state(t, h.dial()) {
			t.Fatal("unexpected block")
		}
	})

	t.Run("an unauthorized connection does not keep a dead daemon's block alive", func(t *testing.T) {
		queue := make(chan bool, 4)
		h := start(t, decisions(queue))
		queue <- true
		daemon := h.dial()
		set(t, daemon, true)
		queue <- false
		intruder := h.dial()
		expectClosed(t, intruder)

		daemon.Close()
		i := h.clock.waitScheduled(t)
		h.clock.fire(i)
		if h.blocker.Blocked() || h.clamshell.IsDisabled() {
			t.Fatal("the dead-man switch did not clear the block")
		}
	})

	t.Run("the production authorizer rejects a process that is not the installed daemon", func(t *testing.T) {
		// The peer is this test binary: not paths.InstalledBin, not hardened.
		if _, ok := authorizer(discard)(selfPeerConn(t)); ok {
			t.Fatal("the test binary was authorized")
		}
	})

	t.Run("the production authorizer logs a burst of rejections once", func(t *testing.T) {
		var buf strings.Builder
		authorize := authorizer(slog.New(slog.NewTextHandler(&buf, nil)))
		for range 3 {
			if _, ok := authorize(selfPeerConn(t)); ok {
				t.Fatal("the test binary was authorized")
			}
		}
		if lines := strings.Count(buf.String(), "\n"); lines != 1 || !strings.Contains(buf.String(), "rejected a connection") {
			t.Fatalf("log after three quick rejections = %q, want one rejection line", buf.String())
		}
	})

	t.Run("at most maxAuthorizing connections are authorized at once", func(t *testing.T) {
		entered := make(chan struct{}, maxAuthorizing+2)
		release := make(chan struct{})
		var once sync.Once
		unblock := func() { once.Do(func() { close(release) }) }
		var inFlight, peak atomic.Int32
		h := start(t, func(_ *harness, c *Config) {
			c.Authorize = func(*net.UnixConn) (int, bool) {
				n := inFlight.Add(1)
				for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
				}
				entered <- struct{}{}
				<-release
				inFlight.Add(-1)
				return testUID, true
			}
		})
		t.Cleanup(unblock) // after start's, so it runs first and the helper can stop
		conns := make([]net.Conn, maxAuthorizing+2)
		for i := range conns {
			conns[i] = h.dial()
		}
		for range maxAuthorizing {
			select {
			case <-entered:
			case <-time.After(waitLimit):
				t.Fatal("the first connections were not authorized")
			}
		}
		select {
		case <-entered:
			t.Fatalf("more than %d connections were authorized at once", maxAuthorizing)
		case <-time.After(150 * time.Millisecond):
		}
		unblock()
		for _, conn := range conns {
			state(t, conn) // every queued connection is served once a slot frees
		}
		if p := peak.Load(); p > maxAuthorizing {
			t.Fatalf("peak concurrent authorizations = %d, want at most %d", p, maxAuthorizing)
		}
	})
}

// selfPeerConn is the server end of a connection whose peer is this test binary.
func selfPeerConn(t *testing.T) *net.UnixConn {
	t.Helper()
	dir := shortTempDir(t)
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(dir, "a.sock"), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	client, err := net.Dial("unix", filepath.Join(dir, "a.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	server, err := ln.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Close() })
	return server
}

func TestRejectLog(t *testing.T) {
	var buf strings.Builder
	now := time.Unix(1_000_000, 0)
	r := &rejectLog{
		log:      slog.New(slog.NewTextHandler(&buf, nil)),
		now:      func() time.Time { return now },
		interval: time.Minute,
	}
	r.Error("rejected", "pid", 1)
	r.Error("rejected", "pid", 2)
	now = now.Add(59 * time.Second)
	r.Error("rejected", "pid", 3)
	if got := buf.String(); strings.Count(got, "\n") != 1 || !strings.Contains(got, "pid=1") {
		t.Fatalf("log within the interval = %q, want only the first rejection", got)
	}
	now = now.Add(time.Second)
	r.Error("rejected", "pid", 4)
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[1], "pid=4") || !strings.Contains(lines[1], "suppressedSinceLast=2") {
		t.Fatalf("log after the interval = %q, want a second line counting the two held back", buf.String())
	}
	now = now.Add(time.Minute)
	r.Error("rejected", "pid", 5)
	if last := buf.String(); strings.Contains(last[strings.LastIndex(strings.TrimSpace(last), "\n"):], "suppressed") {
		t.Fatalf("a line after a quiet interval reports held-back rejections: %q", last)
	}
}

// ---- dead-man switch -------------------------------------------------------------------------

func TestDeadManSwitch(t *testing.T) {
	t.Run("fires after the grace when no daemon reconnects", func(t *testing.T) {
		h := start(t, func(_ *harness, c *Config) {
			c.AfterFunc = nil // the real timer
			c.DeadManGrace = 30 * time.Millisecond
		})
		conn := h.dial()
		set(t, conn, true)
		conn.Close()
		eventually(t, "the dead-man unblock", func() bool { return !h.blocker.Blocked() })
		if h.clamshell.IsDisabled() || h.idle.IsHeld() {
			t.Fatal("the dead-man switch left a mechanism applied")
		}
		if _, ok := h.original.Load(); ok {
			t.Fatal("the original setting was not restored and cleared")
		}
	})

	t.Run("a failed dead-man unblock is retried until it takes", func(t *testing.T) {
		h := start(t)
		conn := h.dial()
		set(t, conn, true)
		h.clamshell.mu.Lock()
		h.clamshell.failOn = map[bool]error{false: errors.New("pmset wedged")}
		h.clamshell.mu.Unlock()
		conn.Close()
		h.clock.fire(h.clock.waitScheduled(t))
		if !h.clamshell.IsDisabled() {
			t.Fatal("the failing unblock cleared the flag")
		}
		retry := h.clock.waitScheduled(t)
		h.clamshell.mu.Lock()
		h.clamshell.failOn = nil
		h.clamshell.mu.Unlock()
		h.clock.fire(retry)
		if h.clamshell.IsDisabled() {
			t.Fatal("the retry did not clear the block")
		}
		h.clock.expectNoneScheduled(t)
	})

	t.Run("fires with the configured grace, 60s by default", func(t *testing.T) {
		h := start(t)
		conn := h.dial()
		set(t, conn, true)
		conn.Close()
		i := h.clock.waitScheduled(t)
		if g := h.clock.grace(i); g != 60*time.Second {
			t.Fatalf("grace = %v, want 60s", g)
		}
		h.clock.fire(i)
		if h.blocker.Blocked() || h.clamshell.IsDisabled() {
			t.Fatal("the dead-man check did not clear the block")
		}
	})

	t.Run("does not fire when a daemon reconnects in time", func(t *testing.T) {
		h := start(t)
		first := h.dial()
		set(t, first, true)
		first.Close()
		i := h.clock.waitScheduled(t)

		second := h.dial()
		if !state(t, second) { // the round trip proves the reconnect was counted
			t.Fatal("the block was lost on disconnect")
		}
		h.clock.fire(i)
		if !state(t, second) || !h.clamshell.IsDisabled() {
			t.Fatal("the dead-man check cleared a block a reconnected daemon owns")
		}
	})

	t.Run("a stale check does nothing after a reconnect and a later drop", func(t *testing.T) {
		h := start(t)
		first := h.dial()
		set(t, first, true)
		first.Close()
		stale := h.clock.waitScheduled(t)

		second := h.dial()
		state(t, second)
		second.Close()
		current := h.clock.waitScheduled(t)

		h.clock.fire(stale)
		if !h.blocker.Blocked() {
			t.Fatal("a stale dead-man check cleared the block")
		}
		h.clock.fire(current)
		if h.blocker.Blocked() {
			t.Fatal("the current dead-man check did not clear the block")
		}
	})

	t.Run("waits for the last of several daemon connections", func(t *testing.T) {
		h := start(t)
		a, b := h.dial(), h.dial()
		set(t, a, true)
		state(t, b)
		a.Close()
		h.clock.expectNoneScheduled(t)
		b.Close()
		h.clock.fire(h.clock.waitScheduled(t))
		if h.blocker.Blocked() {
			t.Fatal("the block survived the last daemon")
		}
	})

	t.Run("is not armed when nothing is blocked", func(t *testing.T) {
		h := start(t)
		conn := h.dial()
		set(t, conn, true)
		set(t, conn, false)
		conn.Close()
		h.clock.expectNoneScheduled(t)
	})

	t.Run("an unauthorized connection does not postpone a pending check", func(t *testing.T) {
		queue := make(chan bool, 4)
		h := start(t, decisions(queue))
		queue <- true
		daemon := h.dial()
		set(t, daemon, true)
		daemon.Close()
		i := h.clock.waitScheduled(t)
		queue <- false
		expectClosed(t, h.dial())
		h.clock.expectNoneScheduled(t)
		h.clock.fire(i)
		if h.blocker.Blocked() || h.clamshell.IsDisabled() {
			t.Fatal("an intruder kept the dead daemon's block alive")
		}
	})

	t.Run("a pending check does nothing once the helper has shut down", func(t *testing.T) {
		b := &stickyBlocker{}
		h := start(t, func(_ *harness, c *Config) { c.Blocker = b })
		conn := h.dial()
		set(t, conn, true)
		conn.Close()
		i := h.clock.waitScheduled(t)
		if err := h.stop(); err != nil {
			t.Fatal(err)
		}
		sets := b.sets.Load()
		h.clock.fire(i)
		if b.sets.Load() != sets {
			t.Fatal("a dead-man check ran after shutdown")
		}
	})

	t.Run("a daemon that unblocked in the meantime leaves nothing to do", func(t *testing.T) {
		h := start(t)
		first := h.dial()
		set(t, first, true)
		first.Close()
		i := h.clock.waitScheduled(t)
		second := h.dial()
		set(t, second, false)
		second.Close()
		calls := h.clamshell.callCount()
		h.clock.fire(i)
		if h.clamshell.callCount() != calls {
			t.Fatal("the dead-man check touched an unblocked Mac")
		}
	})
}

// ---- several users ---------------------------------------------------------------------------

func TestSeveralUsers(t *testing.T) {
	const alice, bob = 501, 502
	blockedNow := func(h *harness) bool {
		return h.blocker.Blocked() && h.clamshell.IsDisabled() && h.idle.IsHeld()
	}
	unblockedNow := func(h *harness) bool {
		_, saved := h.original.Load()
		return !h.blocker.Blocked() && !h.clamshell.IsDisabled() && !h.idle.IsHeld() && !saved
	}

	t.Run("another user's unblock leaves a working daemon's block in place", func(t *testing.T) {
		h := start(t, asUsers)
		a := h.dialAs(alice)
		set(t, a, true)
		b := h.dialAs(bob)
		if state(t, b) {
			t.Fatal("another user's daemon sees the block as its own")
		}
		// Bob's daemon starting up, or his agent finishing a turn.
		if resp := set(t, b, false); !resp.OK || resp.Blocked || resp.Error != "" {
			t.Fatalf("bob's set(false) = %+v, want a plain OK", resp)
		}
		if !blockedNow(h) || !state(t, a) {
			t.Fatal("another user's unblock cleared the block")
		}
		if resp := set(t, a, false); !resp.OK || resp.Blocked {
			t.Fatalf("alice's set(false) = %+v", resp)
		}
		if !unblockedNow(h) {
			t.Fatal("the owner's unblock left the block")
		}
	})

	t.Run("the block holds while any user's daemon asks for it", func(t *testing.T) {
		h := start(t, asUsers)
		a, b := h.dialAs(alice), h.dialAs(bob)
		set(t, a, true)
		set(t, b, true)
		if resp := set(t, a, false); !resp.OK || resp.Blocked {
			t.Fatalf("alice's set(false) = %+v", resp)
		}
		if state(t, a) || !state(t, b) || !blockedNow(h) {
			t.Fatal("alice's unblock cleared bob's block")
		}
		set(t, b, false)
		if !unblockedNow(h) {
			t.Fatal("the last unblock left the block")
		}
	})

	t.Run("another user's connection does not keep a dead daemon's block alive", func(t *testing.T) {
		h := start(t, asUsers)
		a := h.dialAs(alice)
		set(t, a, true)
		b := h.dialAs(bob) // idle: never sends another set
		a.Close()          // alice's daemon SIGKILLed at logout
		h.clock.fire(h.clock.waitScheduled(t))
		if !unblockedNow(h) {
			t.Fatal("the dead daemon's block outlived it")
		}
		if state(t, b) {
			t.Fatal("unexpected block")
		}
	})

	t.Run("another user's daemon connecting does not cancel a pending check", func(t *testing.T) {
		h := start(t, asUsers)
		a := h.dialAs(alice)
		set(t, a, true)
		a.Close()
		i := h.clock.waitScheduled(t)
		h.dialAs(bob)
		h.clock.fire(i)
		if !unblockedNow(h) {
			t.Fatal("bob's connection kept alice's dead block alive")
		}
	})

	t.Run("a dead daemon's request is dropped while another user's block stays", func(t *testing.T) {
		h := start(t, asUsers)
		a, b := h.dialAs(alice), h.dialAs(bob)
		set(t, a, true)
		set(t, b, true)
		a.Close()
		h.clock.fire(h.clock.waitScheduled(t))
		if !blockedNow(h) || !state(t, b) {
			t.Fatal("the dead-man check cleared a block another daemon still wants")
		}
		set(t, b, false)
		if !unblockedNow(h) {
			t.Fatal("the dead daemon's request still holds the block")
		}
	})

	t.Run("a dead daemon whose block failed is written off too", func(t *testing.T) {
		boom := errors.New("pmset exited 1")
		h := start(t, asUsers, func(h *harness, _ *Config) { h.clamshell.failOn = map[bool]error{true: boom} })
		a := h.dialAs(alice)
		if resp := set(t, a, true); resp.OK {
			t.Fatalf("set(true) with a failing pmset = %+v", resp)
		}
		b := h.dialAs(bob)
		a.Close()
		h.clock.fire(h.clock.waitScheduled(t))
		if h.idle.IsHeld() {
			t.Fatal("the dead daemon's idle assertion outlived it")
		}
		h.clamshell.mu.Lock()
		h.clamshell.failOn = nil
		h.clamshell.mu.Unlock()
		set(t, b, true)
		set(t, b, false)
		if !unblockedNow(h) {
			t.Fatal("the dead daemon's failed request still holds the block")
		}
	})

	t.Run("a block another user's daemon wants holds off adopting an update", func(t *testing.T) {
		var replaced atomic.Bool
		boom := errors.New("pmset exited 1")
		h := start(t, asUsers, func(h *harness, c *Config) {
			c.Replaced = replaced.Load
			h.clamshell.failOn = map[bool]error{true: boom}
		})
		a := h.dialAs(alice)
		set(t, a, true) // fails, so only the idle assertion holds and Blocked reports false
		b := h.dialAs(bob)
		replaced.Store(true)
		roundTrip(t, b, ipc.HelperRequest{Op: ipc.HelperVersion})
		set(t, b, false)
		select {
		case err := <-h.done:
			t.Fatalf("the helper exited while a daemon wants the block: %v", err)
		case <-time.After(100 * time.Millisecond):
		}
		if !h.idle.IsHeld() {
			t.Fatal("alice's partial block was dropped")
		}
	})
}

// ---- shutdown --------------------------------------------------------------------------------

func TestShutdown(t *testing.T) {
	for _, tc := range []struct {
		name     string
		original bool
	}{
		{"cancellation unblocks and restores sleep allowed", false},
		{"cancellation keeps a Mac its owner set never to sleep", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := start(t, func(h *harness, _ *Config) { h.clamshell.disabled = tc.original })
			conn := h.dial()
			set(t, conn, true)

			if err := h.stop(); err != nil {
				t.Fatalf("Serve after cancellation = %v, want nil", err)
			}
			if h.blocker.Blocked() || h.idle.IsHeld() {
				t.Fatal("still blocked after shutdown")
			}
			if got := h.clamshell.IsDisabled(); got != tc.original {
				t.Fatalf("disablesleep after shutdown = %v, want the original %v", got, tc.original)
			}
			if _, ok := h.original.Load(); ok {
				t.Fatal("the saved original survived a clean shutdown")
			}
			if _, err := os.Lstat(h.socket); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the socket is still there: %v", err)
			}
			expectClosed(t, conn)
		})
	}

	t.Run("cancellation with nothing blocked changes nothing", func(t *testing.T) {
		h := start(t)
		state(t, h.dial())
		if err := h.stop(); err != nil {
			t.Fatal(err)
		}
		if h.clamshell.callCount() != 0 {
			t.Fatal("shutdown touched disablesleep though nothing was blocked")
		}
	})

	t.Run("shutdown waits for an in-flight block, then clears it", func(t *testing.T) {
		b := &gatedBlocker{entered: make(chan struct{}, 1), release: make(chan struct{})}
		h := start(t, func(_ *harness, c *Config) { c.Blocker = b })
		conn := h.dial()
		if err := ipc.WriteFrame(conn, ipc.HelperRequest{Op: ipc.HelperSet, Blocked: true}); err != nil {
			t.Fatal(err)
		}
		select {
		case <-b.entered:
		case <-time.After(waitLimit):
			t.Fatal("the block never started")
		}
		h.cancel()
		select {
		case err := <-h.done:
			t.Fatalf("Serve returned with a block still being applied: %v", err)
		case <-time.After(100 * time.Millisecond):
		}
		close(b.release)
		if err := h.wait(); err != nil {
			t.Fatalf("Serve = %v, want nil", err)
		}
		if got := b.history(); len(got) != 2 || !got[0] || got[1] {
			t.Fatalf("sets = %v, want the block and then the final unblock", got)
		}
		if b.Blocked() {
			t.Fatal("still blocked after shutdown")
		}
	})

	t.Run("no new connection is accepted after shutdown", func(t *testing.T) {
		h := start(t)
		if err := h.stop(); err != nil {
			t.Fatal(err)
		}
		if _, err := net.DialTimeout("unix", h.socket, time.Second); err == nil {
			t.Fatal("dialed a stopped helper")
		}
	})
}

// ---- serialization ---------------------------------------------------------------------------

// serialBlocker is deliberately unsynchronized: the race detector and the overlap counter catch
// any call the server fails to serialize.
type serialBlocker struct {
	blocked  bool
	inFlight atomic.Int32
	overlaps atomic.Int32
	sets     int
}

func (b *serialBlocker) enter() func() {
	if b.inFlight.Add(1) > 1 {
		b.overlaps.Add(1)
	}
	return func() { b.inFlight.Add(-1) }
}

func (b *serialBlocker) Set(blocked bool) error {
	defer b.enter()()
	time.Sleep(time.Millisecond)
	b.blocked = blocked
	b.sets++
	return nil
}

func (b *serialBlocker) Blocked() bool {
	defer b.enter()()
	return b.blocked
}

// stickyBlocker reports blocked whatever it is asked: the case only the closing gate guards.
type stickyBlocker struct{ sets atomic.Int32 }

func (b *stickyBlocker) Set(bool) error { b.sets.Add(1); return nil }
func (b *stickyBlocker) Blocked() bool  { return true }

// gatedBlocker holds every block until the test closes release, like a slow pmset.
type gatedBlocker struct {
	entered chan struct{}
	release chan struct{}
	mu      sync.Mutex
	blocked bool
	sets    []bool
}

func (b *gatedBlocker) Set(blocked bool) error {
	if blocked {
		b.entered <- struct{}{}
		<-b.release
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.blocked = blocked
	b.sets = append(b.sets, blocked)
	return nil
}

func (b *gatedBlocker) Blocked() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.blocked
}

func (b *gatedBlocker) history() []bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]bool(nil), b.sets...)
}

// failingUnblock blocks normally but reports an error for every unblock, which still leaves it
// unblocked.
type failingUnblock struct {
	mu      sync.Mutex
	blocked bool
}

func (b *failingUnblock) Set(blocked bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.blocked = blocked
	if !blocked {
		return errors.New("restore failed")
	}
	return nil
}

func (b *failingUnblock) Blocked() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.blocked
}

func TestServeSerializesConnections(t *testing.T) {
	blocker := &serialBlocker{}
	h := start(t, func(_ *harness, c *Config) { c.Blocker = blocker })
	const clients, requests = 6, 8
	var wg sync.WaitGroup
	errs := make(chan string, clients*requests)
	for c := range clients {
		conn := h.dial()
		wg.Go(func() {
			for r := range requests {
				req := ipc.HelperRequest{Op: ipc.HelperSet, Blocked: (c+r)%2 == 0}
				if r%3 == 2 {
					req = ipc.HelperRequest{Op: ipc.HelperState}
				}
				if err := ipc.WriteFrame(conn, req); err != nil {
					errs <- err.Error()
					return
				}
				var resp ipc.HelperResponse
				if err := ipc.ReadFrame(conn, &resp); err != nil {
					errs <- err.Error()
					return
				}
				if !resp.OK {
					errs <- resp.Error
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	if n := blocker.overlaps.Load(); n != 0 {
		t.Fatalf("%d policy calls overlapped", n)
	}
	if err := h.stop(); err != nil {
		t.Fatal(err)
	}
	// Every set from every client plus the final unblock.
	if want := clients*(requests-requests/3) + 1; blocker.sets != want {
		t.Fatalf("sets = %d, want %d", blocker.sets, want)
	}
}

// ---- adopting an updated binary --------------------------------------------------------------

func TestRelaunchAfterReplacement(t *testing.T) {
	replacedOpt := func(flag *atomic.Bool) option {
		return func(_ *harness, c *Config) { c.Replaced = flag.Load }
	}

	t.Run("a version probe while idle stops the helper after replying", func(t *testing.T) {
		var replaced atomic.Bool
		h := start(t, replacedOpt(&replaced))
		conn := h.dial()
		replaced.Store(true)
		if v := roundTrip(t, conn, ipc.HelperRequest{Op: ipc.HelperVersion}); !v.OK || v.Version == "" {
			t.Fatalf("version = %+v", v)
		}
		if err := h.wait(); !errors.Is(err, ErrExecutableReplaced) {
			t.Fatalf("Serve = %v, want ErrExecutableReplaced", err)
		}
		if _, err := os.Lstat(h.socket); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("the socket survived the relaunch exit")
		}
	})

	t.Run("never while blocking, then at the unblock", func(t *testing.T) {
		var replaced atomic.Bool
		h := start(t, replacedOpt(&replaced))
		conn := h.dial()
		set(t, conn, true)
		replaced.Store(true)
		roundTrip(t, conn, ipc.HelperRequest{Op: ipc.HelperVersion})
		if !state(t, conn) {
			t.Fatal("the helper stopped blocking")
		}
		select {
		case err := <-h.done:
			t.Fatalf("the helper exited while blocking: %v", err)
		default:
		}
		if resp := set(t, conn, false); !resp.OK || resp.Blocked {
			t.Fatalf("set(false) = %+v", resp)
		}
		if err := h.wait(); !errors.Is(err, ErrExecutableReplaced) {
			t.Fatalf("Serve = %v, want ErrExecutableReplaced", err)
		}
		if h.clamshell.IsDisabled() {
			t.Fatal("disablesleep left on")
		}
	})

	t.Run("a failed unblock still adopts the update when nothing is blocked", func(t *testing.T) {
		var replaced atomic.Bool
		h := start(t, replacedOpt(&replaced), func(_ *harness, c *Config) { c.Blocker = &failingUnblock{} })
		conn := h.dial()
		set(t, conn, true)
		replaced.Store(true)
		if resp := set(t, conn, false); resp.OK || resp.Blocked || !strings.Contains(resp.Error, "restore failed") {
			t.Fatalf("set(false) = %+v", resp)
		}
		if err := h.wait(); !errors.Is(err, ErrExecutableReplaced) {
			t.Fatalf("Serve = %v, want ErrExecutableReplaced", err)
		}
	})

	t.Run("an unchanged binary keeps serving", func(t *testing.T) {
		var replaced atomic.Bool
		h := start(t, replacedOpt(&replaced))
		conn := h.dial()
		roundTrip(t, conn, ipc.HelperRequest{Op: ipc.HelperVersion})
		set(t, conn, true)
		set(t, conn, false)
		if state(t, conn) {
			t.Fatal("unexpected block")
		}
		select {
		case err := <-h.done:
			t.Fatalf("the helper exited: %v", err)
		default:
		}
	})
}

// ---- listening -------------------------------------------------------------------------------

func TestServeSocket(t *testing.T) {
	t.Run("any local user may connect and the run directory is 0755", func(t *testing.T) {
		h := start(t)
		info, err := os.Lstat(h.socket)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0o666 {
			t.Fatalf("socket mode = %v, want a 0666 socket", info.Mode())
		}
		dir, err := os.Stat(filepath.Dir(h.socket))
		if err != nil {
			t.Fatal(err)
		}
		if dir.Mode().Perm() != 0o755 {
			t.Fatalf("run directory mode = %v, want 0755", dir.Mode().Perm())
		}
	})

	t.Run("a stale socket left by a dead helper is replaced", func(t *testing.T) {
		dir := shortTempDir(t)
		socket := filepath.Join(dir, "run", "h.sock")
		if err := os.MkdirAll(filepath.Dir(socket), 0o755); err != nil {
			t.Fatal(err)
		}
		old, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
		if err != nil {
			t.Fatal(err)
		}
		old.SetUnlinkOnClose(false)
		old.Close()

		h := start(t, func(h *harness, c *Config) {
			h.socket, c.Socket, c.RunDir = socket, socket, filepath.Dir(socket)
		})
		if state(t, h.dial()) {
			t.Fatal("unexpected block")
		}
	})

	t.Run("a leftover regular file at the socket path is replaced", func(t *testing.T) {
		dir := shortTempDir(t)
		socket := filepath.Join(dir, "h.sock")
		if err := os.WriteFile(socket, []byte("junk"), 0o644); err != nil {
			t.Fatal(err)
		}
		h := start(t, func(h *harness, c *Config) { h.socket, c.Socket, c.RunDir = socket, socket, "" })
		state(t, h.dial())
	})

	t.Run("a live helper's socket is not taken over", func(t *testing.T) {
		h := start(t)
		err := Serve(context.Background(), Config{
			Socket:    h.socket,
			Authorize: allowAll,
			Blocker:   &serialBlocker{},
			Logger:    discard,
		})
		if err == nil || !strings.Contains(err.Error(), "already listening") {
			t.Fatalf("second Serve = %v, want an already-listening error", err)
		}
		conn := h.dial()
		set(t, conn, true)
		if !state(t, conn) {
			t.Fatal("the first helper stopped working")
		}
	})

	t.Run("a config without its required parts is rejected", func(t *testing.T) {
		for name, cfg := range map[string]Config{
			"no socket":    {Authorize: allowAll, Blocker: &serialBlocker{}},
			"no authorize": {Socket: "/nonexistent/s.sock", Blocker: &serialBlocker{}},
			"no blocker":   {Socket: "/nonexistent/s.sock", Authorize: allowAll},
		} {
			if err := Serve(context.Background(), cfg); err == nil {
				t.Errorf("%s: Serve accepted it", name)
			}
		}
	})
}
