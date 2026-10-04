package cli

import (
	"bytes"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nikitaShakhbazyan/lidwake-go/internal/agents"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/hooks"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/install"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/ipc"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/tui"
)

var testNow = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

const testCLIPath = "/usr/local/libexec/lidwake/lidwake"

// fakeDaemon records every request and answers with reply (default: ok).
type fakeDaemon struct {
	mu       sync.Mutex
	requests []ipc.Request
	reply    func(ipc.Request) (ipc.Response, error)
}

func (f *fakeDaemon) send(req ipc.Request) (ipc.Response, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	reply := f.reply
	f.mu.Unlock()
	if reply == nil {
		return ipc.Response{OK: true}, nil
	}
	return reply(req)
}

func (f *fakeDaemon) only(t *testing.T) ipc.Request {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) != 1 {
		t.Fatalf("got %d requests, want 1: %+v", len(f.requests), f.requests)
	}
	return f.requests[0]
}

func (f *fakeDaemon) none(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) != 0 {
		t.Fatalf("sent %+v, want nothing", f.requests)
	}
}

func replyWith(resp ipc.Response, err error) func(ipc.Request) (ipc.Response, error) {
	return func(ipc.Request) (ipc.Response, error) { return resp, err }
}

// proc is one process in the fake process table.
type proc struct {
	path string
	ppid int
	argv []string
}

var errNoProcess = errors.New("no such process")

// fakeResolver walks a fake process table starting at ppid.
func fakeResolver(ppid int, table map[int]proc) *agents.Resolver {
	return &agents.Resolver{
		Getppid: func() int { return ppid },
		ProcessPath: func(pid int) (string, error) {
			if p, ok := table[pid]; ok {
				return p.path, nil
			}
			return "", errNoProcess
		},
		ParentPID: func(pid int) (int, error) {
			if p, ok := table[pid]; ok {
				return p.ppid, nil
			}
			return 0, errNoProcess
		},
		ProcessArgs: func(pid int) ([]string, error) {
			if p, ok := table[pid]; ok && p.argv != nil {
				return p.argv, nil
			}
			return nil, errNoProcess
		},
		ProcessAlive: func(pid int) bool { _, ok := table[pid]; return ok },
		AllPIDs:      func() ([]int, error) { return nil, errNoProcess },
		ProcessTree:  func() (map[int][]int, error) { return nil, errNoProcess },
		CPUTime:      func(int) (time.Duration, error) { return 0, errNoProcess },
	}
}

// execCall is one recorded exec.
type execCall struct {
	path string
	argv []string
	env  []string
}

type harness struct {
	*app
	out, errOut *bytes.Buffer
	daemon      *fakeDaemon
	tty         bool
	env         []string
	execs       []execCall
	statsCalls  []bool
	statsAction tui.Actions
	hookHome    string
	commands    []install.Command
	euid        int
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{out: &bytes.Buffer{}, errOut: &bytes.Buffer{}, daemon: &fakeDaemon{}, euid: 501}
	h.hookHome = t.TempDir()
	h.app = &app{
		stdout:     h.out,
		stderr:     h.errOut,
		stdin:      agents.NewStdin(agents.PayloadReader{}),
		stdinIsTTY: func() bool { return h.tty },
		send:       h.daemon.send,
		resolver:   fakeResolver(100, nil),
		environ:    func() []string { return h.env },
		getpid:     func() int { return 4242 },
		now:        func() time.Time { return testNow },
		home:       h.hookHome,
		configPath: filepath.Join(t.TempDir(), "config.json"),
		lookPath:   func(file string) (string, error) { return "/usr/bin/" + filepath.Base(file), nil },
		exec: func(path string, argv, env []string) error {
			h.execs = append(h.execs, execCall{path, argv, env})
			return errors.New("exec stubbed")
		},
		hooks: func() *hooks.Installer {
			return &hooks.Installer{
				CLIPath:         testCLIPath,
				Home:            h.hookHome,
				ApplicationsDir: filepath.Join(h.hookHome, "Applications"),
				SearchPath:      []string{},
			}
		},
		stats: func(actions tui.Actions, once bool) int {
			h.statsCalls = append(h.statsCalls, once)
			h.statsAction = actions
			return 0
		},
		mcpIn: strings.NewReader(""),
		installer: func() *install.Installer {
			return &install.Installer{
				Layout:     install.Layout{},
				Run:        func(c install.Command) error { h.commands = append(h.commands, c); return nil },
				Out:        h.out,
				Err:        h.errOut,
				UID:        501,
				Username:   "tester",
				Geteuid:    func() int { return h.euid },
				Executable: func() (string, error) { return "/tmp/lidwake", nil },
				Chown:      func(string, int, int) error { return nil },
				IsRootOnly: func(string) bool { return true },
				Sudo:       "/usr/bin/sudo",
				Launchctl:  "/bin/launchctl",
				Codesign:   "/usr/bin/codesign",
				Pmset:      "/usr/bin/pmset",
			}
		},
		log: slog.New(slog.DiscardHandler),
	}
	return h
}

// withPayload makes stdin carry a hook payload.
func (h *harness) withPayload(payload string) *harness {
	h.stdin = agents.NewStdin(agents.PayloadReader{In: strings.NewReader(payload), IsTerminal: func() bool { return false }})
	return h
}

// atTTY makes stdin a terminal: no payload, and hook failures exit 2.
func (h *harness) atTTY() *harness {
	h.tty = true
	h.stdin = agents.NewStdin(agents.PayloadReader{In: strings.NewReader(""), IsTerminal: func() bool { return true }})
	return h
}

func (h *harness) do(t *testing.T, args ...string) int {
	t.Helper()
	return h.run(args)
}

func expectCode(t *testing.T, got, want int) {
	t.Helper()
	if got != want {
		t.Fatalf("exit code %d, want %d", got, want)
	}
}

func expectOutput(t *testing.T, label string, got *bytes.Buffer, want string) {
	t.Helper()
	if got.String() != want {
		t.Fatalf("%s:\n got %q\nwant %q", label, got.String(), want)
	}
}

func expectContains(t *testing.T, label string, got *bytes.Buffer, want string) {
	t.Helper()
	if !strings.Contains(got.String(), want) {
		t.Fatalf("%s %q does not contain %q", label, got.String(), want)
	}
}

func f64(v float64) *float64 { return &v }
