package agents

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"
)

var errNoProc = errors.New("no such process")

// fakeProc is one process in a fake process table. Unset fields read as unreadable.
type fakeProc struct {
	path string
	ppid int
	argv []string
	cpu  time.Duration
	// noCPU makes CPU time unreadable.
	noCPU bool
}

// fakeSystem is a process table the Resolver reads through its func fields.
type fakeSystem struct {
	self  int
	procs map[int]fakeProc
	alive map[int]bool // extra live PIDs beyond procs (for pid-file tests)

	mu    sync.Mutex
	calls []string
}

func (f *fakeSystem) record(call string, pid int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("%s(%d)", call, pid))
}

func (f *fakeSystem) resolver() *Resolver {
	return &Resolver{
		Getppid: func() int { return f.procs[f.self].ppid },
		ProcessPath: func(pid int) (string, error) {
			f.record("path", pid)
			p, ok := f.procs[pid]
			if !ok || p.path == "" {
				return "", errNoProc
			}
			return p.path, nil
		},
		ParentPID: func(pid int) (int, error) {
			f.record("ppid", pid)
			p, ok := f.procs[pid]
			if !ok {
				return 0, errNoProc
			}
			return p.ppid, nil
		},
		ProcessArgs: func(pid int) ([]string, error) {
			f.record("args", pid)
			p, ok := f.procs[pid]
			if !ok || p.argv == nil {
				return nil, errNoProc
			}
			return p.argv, nil
		},
		ProcessAlive: func(pid int) bool {
			_, ok := f.procs[pid]
			return ok || f.alive[pid]
		},
		AllPIDs: func() ([]int, error) {
			pids := make([]int, 0, len(f.procs))
			for pid := range f.procs {
				pids = append(pids, pid)
			}
			slices.Sort(pids)
			return pids, nil
		},
		ProcessTree: func() (map[int][]int, error) {
			f.mu.Lock()
			f.calls = append(f.calls, "tree")
			f.mu.Unlock()
			pids := make([]int, 0, len(f.procs))
			for pid := range f.procs {
				pids = append(pids, pid)
			}
			slices.Sort(pids)
			m := map[int][]int{}
			for _, pid := range pids {
				if pid > 0 {
					m[f.procs[pid].ppid] = append(m[f.procs[pid].ppid], pid)
				}
			}
			return m, nil
		},
		CPUTime: func(pid int) (time.Duration, error) {
			f.record("cpu", pid)
			p, ok := f.procs[pid]
			if !ok || p.noCPU {
				return 0, errNoProc
			}
			return p.cpu, nil
		},
	}
}

func (f *fakeSystem) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

const selfPID = 4242

// hookChain models a hook invocation: launchd(1) → terminal(300) → claude(500) → sh(600) →
// lidwake(self). Claude is a versioned install whose basename is its version.
func hookChain() *fakeSystem {
	return &fakeSystem{
		self: selfPID,
		procs: map[int]fakeProc{
			1:       {path: "/sbin/launchd", ppid: 0, argv: []string{"/sbin/launchd"}, cpu: time.Second},
			300:     {path: "/System/Applications/Utilities/Terminal.app/Contents/MacOS/Terminal", ppid: 1, argv: []string{"Terminal"}},
			500:     {path: "/Users/u/.local/share/claude/versions/2.1.156", ppid: 300, argv: []string{"claude", "--resume"}},
			600:     {path: "/bin/sh", ppid: 500, argv: []string{"/bin/sh", "-c", "lidwake acquire"}},
			selfPID: {path: "/usr/local/libexec/lidwake/lidwake", ppid: 600, argv: []string{"/usr/local/libexec/lidwake/lidwake", "acquire"}, cpu: 20 * time.Millisecond},
		},
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// deadPID is a PID no fake process table contains.
const deadPID = 2147483600

func TestProcessResolver(t *testing.T) {
	t.Run("name of current process is non nil", func(t *testing.T) {
		r := hookChain().resolver()
		name, ok := r.Name(selfPID)
		if !ok || name != "lidwake" {
			t.Fatalf("got %q, %v", name, ok)
		}
	})

	t.Run("name of invalid pid is nil", func(t *testing.T) {
		f := hookChain()
		if name, ok := f.resolver().Name(-1); ok {
			t.Fatalf("got %q", name)
		}
		if calls := f.recorded(); len(calls) != 0 {
			t.Fatalf("invalid pid reached the system: %v", calls)
		}
	})

	t.Run("parent of current process is positive", func(t *testing.T) {
		if ppid := hookChain().resolver().Parent(selfPID); ppid != 600 {
			t.Fatalf("Parent = %d", ppid)
		}
	})

	t.Run("owning agent PID with empty binary set is negative", func(t *testing.T) {
		// No candidate names → never matches → -1 (the daemon then declines to process-watch).
		if pid := hookChain().resolver().OwningAgentPID(map[string]bool{}); pid != -1 {
			t.Fatalf("pid = %d", pid)
		}
	})

	t.Run("owning agent PID with unmatchable name is negative", func(t *testing.T) {
		names := map[string]bool{"definitely-not-a-real-binary-xyzzy": true}
		if pid := hookChain().resolver().OwningAgentPID(names); pid != -1 {
			t.Fatalf("pid = %d", pid)
		}
	})

	t.Run("owning agent PID by argv with never-matching predicate is negative", func(t *testing.T) {
		if pid := hookChain().resolver().OwningAgentPIDByArgv(func([]string) bool { return false }); pid != -1 {
			t.Fatalf("pid = %d", pid)
		}
	})

	t.Run("owning agent PID by argv walks real ancestors", func(t *testing.T) {
		// An always-true predicate must bind the first readable ancestor (the hook's shell),
		// proving the walk reads ancestor argv rather than failing before the predicate runs.
		pid := hookChain().resolver().OwningAgentPIDByArgv(func(argv []string) bool { return len(argv) > 0 })
		if pid != 600 {
			t.Fatalf("pid = %d", pid)
		}
	})

	t.Run("path matches agent by basename", func(t *testing.T) {
		if !PathMatchesAgent("/usr/local/bin/codex", map[string]bool{"codex": true}) {
			t.Fatal("false")
		}
	})

	t.Run("path matches agent by path component for versioned install", func(t *testing.T) {
		// Claude installs at …/claude/versions/<x.y.z>; the basename is a version, but the
		// "claude" path component must still match (or Claude is unwatchable, pid = -1).
		if !PathMatchesAgent("/Users/u/.local/share/claude/versions/2.1.156", map[string]bool{"claude": true}) {
			t.Fatal("false")
		}
	})

	t.Run("path matches agent rejects unrelated path", func(t *testing.T) {
		if PathMatchesAgent("/usr/bin/python3", map[string]bool{"claude": true, "codex": true}) {
			t.Fatal("true")
		}
	})

	t.Run("running processes is non empty and includes self", func(t *testing.T) {
		procs := hookChain().resolver().RunningProcesses()
		if len(procs) == 0 {
			t.Fatal("empty")
		}
		want := Process{PID: selfPID, Name: "lidwake", Path: "/usr/local/libexec/lidwake/lidwake"}
		if !slices.Contains(procs, want) {
			t.Fatalf("self missing from %v", procs)
		}
	})

	t.Run("arguments of self includes executable arg", func(t *testing.T) {
		argv, ok := hookChain().resolver().Arguments(selfPID)
		if !ok || len(argv) == 0 || argv[0] != "/usr/local/libexec/lidwake/lidwake" {
			t.Fatalf("got %q, %v", argv, ok)
		}
	})

	t.Run("arguments of invalid pid is nil", func(t *testing.T) {
		f := hookChain()
		if argv, ok := f.resolver().Arguments(-1); ok {
			t.Fatalf("got %q", argv)
		}
		if calls := f.recorded(); len(calls) != 0 {
			t.Fatalf("invalid pid reached the system: %v", calls)
		}
	})

	t.Run("cpu time of self is non negative", func(t *testing.T) {
		d, ok := hookChain().resolver().CPU(selfPID)
		if !ok || d < 0 {
			t.Fatalf("got %v, %v", d, ok)
		}
	})

	t.Run("cpu time of invalid pid is nil", func(t *testing.T) {
		if d, ok := hookChain().resolver().CPU(-1); ok {
			t.Fatalf("got %v", d)
		}
	})

	t.Run("cpu time tracks wall clock under load", func(t *testing.T) {
		// The tick-to-time conversion lives in darwin.CPUTime, which returns a Duration; the
		// resolver must pass it through unscaled (a units slip here would under-report a pinned
		// core below any idle threshold).
		f := hookChain()
		f.procs[selfPID] = fakeProc{path: "/x/lidwake", ppid: 600, cpu: 300 * time.Millisecond}
		before, _ := f.resolver().CPU(selfPID)
		f.procs[selfPID] = fakeProc{path: "/x/lidwake", ppid: 600, cpu: 600 * time.Millisecond}
		after, _ := f.resolver().CPU(selfPID)
		if after-before != 300*time.Millisecond {
			t.Fatalf("delta = %v", after-before)
		}
	})

	t.Run("tree CPU time sums root and children from map", func(t *testing.T) {
		// Synthetic map: self → [childA → [grandchild], childB] with a grandchild → self cycle.
		// The fake descendants are unreadable, so they contribute 0, but the walk must terminate
		// and return the root's own CPU.
		f := hookChain()
		m := map[int][]int{selfPID: {990001, 990002}, 990001: {990003}, 990003: {selfPID}}
		total, ok := f.resolver().TreeCPUTime(selfPID, m)
		if !ok || total != 20*time.Millisecond {
			t.Fatalf("got %v, %v", total, ok)
		}
	})

	t.Run("tree CPU time nil when root unreadable", func(t *testing.T) {
		if d, ok := hookChain().resolver().TreeCPUTime(-1, map[int][]int{}); ok {
			t.Fatalf("got %v", d)
		}
	})

	t.Run("gateway PID missing file is negative", func(t *testing.T) {
		if pid := hookChain().resolver().GatewayPID(filepath.Join(t.TempDir(), "no", "gateway.pid")); pid != -1 {
			t.Fatalf("pid = %d", pid)
		}
	})

	t.Run("gateway PID parses live JSON pid", func(t *testing.T) {
		// Hermes writes {"pid": N, "kind": "hermes-gateway", …}.
		file := filepath.Join(t.TempDir(), "gw.pid")
		writeFile(t, file, fmt.Sprintf(`{"pid": %d, "kind": "hermes-gateway"}`, selfPID))
		if pid := hookChain().resolver().GatewayPID(file); pid != selfPID {
			t.Fatalf("pid = %d", pid)
		}
	})

	t.Run("gateway PID parses live bare int pid", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "gw.pid")
		writeFile(t, file, fmt.Sprintf("%d\n", selfPID))
		if pid := hookChain().resolver().GatewayPID(file); pid != selfPID {
			t.Fatalf("pid = %d", pid)
		}
	})

	t.Run("gateway PID rejects dead pid", func(t *testing.T) {
		// A stale pid-file pointing at a long-dead PID must resolve to -1, not a recycled process.
		file := filepath.Join(t.TempDir(), "gw.pid")
		writeFile(t, file, fmt.Sprintf(`{"pid": %d}`, deadPID))
		if pid := hookChain().resolver().GatewayPID(file); pid != -1 {
			t.Fatalf("pid = %d", pid)
		}
	})

	t.Run("gateway PID profile aware prefers default", func(t *testing.T) {
		home := t.TempDir()
		// The default pid-file is live → used directly, profiles not consulted.
		writeFile(t, filepath.Join(home, ".hermes", "gateway.pid"), fmt.Sprintf(`{"pid": %d}`, selfPID))
		writeFile(t, filepath.Join(home, ".hermes", "profiles", "a", "gateway.pid"), `{"pid": 500}`)
		if pid := hookChain().resolver().GatewayPIDInHome(home, ".hermes/gateway.pid"); pid != selfPID {
			t.Fatalf("pid = %d", pid)
		}
	})

	t.Run("gateway PID profile aware finds profile when default missing", func(t *testing.T) {
		home := t.TempDir()
		// No default pid-file; a named profile has a live gateway → found via the profiles glob.
		writeFile(t, filepath.Join(home, ".hermes", "profiles", "work", "gateway.pid"),
			fmt.Sprintf(`{"pid": %d, "kind": "hermes-gateway"}`, selfPID))
		if pid := hookChain().resolver().GatewayPIDInHome(home, ".hermes/gateway.pid"); pid != selfPID {
			t.Fatalf("pid = %d", pid)
		}
	})

	t.Run("gateway PID profile aware skips dead profile gateways", func(t *testing.T) {
		home := t.TempDir()
		// A profile whose gateway is dead must not match (stale pid-file).
		writeFile(t, filepath.Join(home, ".hermes", "profiles", "dead", "gateway.pid"), fmt.Sprintf(`{"pid": %d}`, deadPID))
		if pid := hookChain().resolver().GatewayPIDInHome(home, ".hermes/gateway.pid"); pid != -1 {
			t.Fatalf("pid = %d", pid)
		}
	})

	t.Run("gateway PID profile aware none returns negative", func(t *testing.T) {
		home := t.TempDir()
		// No pid-files anywhere (not even a profiles dir) → -1.
		if err := os.MkdirAll(filepath.Join(home, ".hermes"), 0o755); err != nil {
			t.Fatal(err)
		}
		if pid := hookChain().resolver().GatewayPIDInHome(home, ".hermes/gateway.pid"); pid != -1 {
			t.Fatalf("pid = %d", pid)
		}
	})
}

func TestProcessResolverWalk(t *testing.T) {
	t.Run("finds the agent above the hook shell", func(t *testing.T) {
		// getppid is the shell, which exits as soon as lidwake returns; the walk must continue to
		// the versioned Claude install two levels up.
		if pid := hookChain().resolver().OwningAgentPID(AllBinaryNames()); pid != 500 {
			t.Fatalf("pid = %d", pid)
		}
	})

	t.Run("finds a node-hosted pi by argv", func(t *testing.T) {
		f := hookChain()
		f.procs[500] = fakeProc{path: "/opt/homebrew/bin/node", ppid: 300, argv: []string{"node", "/opt/homebrew/bin/pi"}}
		r := f.resolver()
		if pid := r.OwningAgentPID(AllBinaryNames()); pid != -1 {
			t.Fatalf("path walk pid = %d, want -1 for a node executable", pid)
		}
		if pid := r.OwningAgentPIDByArgv(ArgvIsPi); pid != 500 {
			t.Fatalf("argv walk pid = %d", pid)
		}
	})

	t.Run("skips ancestors whose argv is unreadable", func(t *testing.T) {
		f := hookChain()
		f.procs[600] = fakeProc{path: "/bin/sh", ppid: 500} // argv unreadable
		pid := f.resolver().OwningAgentPIDByArgv(func(argv []string) bool { return len(argv) > 0 })
		if pid != 500 {
			t.Fatalf("pid = %d", pid)
		}
	})

	t.Run("stops at launchd", func(t *testing.T) {
		f := hookChain()
		var visited []int
		f.resolver().OwningAgentPIDByArgv(func(argv []string) bool {
			visited = append(visited, len(visited))
			return false
		})
		// sh, claude, Terminal — pid 1 is never examined.
		if len(visited) != 3 {
			t.Fatalf("visited %d ancestors", len(visited))
		}
	})

	t.Run("stops when the parent cannot be read or is itself", func(t *testing.T) {
		f := &fakeSystem{self: selfPID, procs: map[int]fakeProc{
			selfPID: {path: "/x/lidwake", ppid: 700},
			700:     {path: "/bin/sh", ppid: 700}, // self-parented
		}}
		if pid := f.resolver().OwningAgentPID(map[string]bool{"claude": true}); pid != -1 {
			t.Fatalf("pid = %d", pid)
		}
		f.procs[700] = fakeProc{path: "/bin/sh", ppid: 701} // 701 does not exist
		if pid := f.resolver().OwningAgentPID(map[string]bool{"claude": true}); pid != -1 {
			t.Fatalf("pid = %d", pid)
		}
	})

	t.Run("is bounded to sixteen ancestors", func(t *testing.T) {
		// A chain of shells 30 deep with the agent at the very top: out of reach.
		f := &fakeSystem{self: selfPID, procs: map[int]fakeProc{selfPID: {path: "/x/lidwake", ppid: 1000}}}
		for i := range 30 {
			f.procs[1000+i] = fakeProc{path: "/bin/sh", ppid: 1000 + i + 1}
		}
		f.procs[1030] = fakeProc{path: "/usr/local/bin/codex", ppid: 1}
		if pid := f.resolver().OwningAgentPID(AllBinaryNames()); pid != -1 {
			t.Fatalf("pid = %d", pid)
		}
		// The 16th ancestor (depth 15) is still examined.
		f.procs[1015] = fakeProc{path: "/usr/local/bin/codex", ppid: 1016}
		if pid := f.resolver().OwningAgentPID(AllBinaryNames()); pid != 1015 {
			t.Fatalf("pid = %d", pid)
		}
	})
}

func TestProcessResolverTables(t *testing.T) {
	t.Run("running processes skip unreadable and non-positive pids", func(t *testing.T) {
		f := hookChain()
		f.procs[0] = fakeProc{path: "/kernel"}
		f.procs[777] = fakeProc{ppid: 1} // path unreadable
		for _, p := range f.resolver().RunningProcesses() {
			if p.PID == 0 || p.PID == 777 {
				t.Fatalf("listed %+v", p)
			}
		}
	})

	t.Run("running processes is nil when the listing fails", func(t *testing.T) {
		r := hookChain().resolver()
		r.AllPIDs = func() ([]int, error) { return nil, errNoProc }
		if procs := r.RunningProcesses(); procs != nil {
			t.Fatalf("got %v", procs)
		}
	})

	t.Run("child map is empty when the snapshot fails", func(t *testing.T) {
		r := hookChain().resolver()
		r.ProcessTree = func() (map[int][]int, error) { return nil, errNoProc }
		if m := r.ChildMap(); m == nil || len(m) != 0 {
			t.Fatalf("got %v", m)
		}
	})

	t.Run("child map groups children by parent", func(t *testing.T) {
		m := hookChain().resolver().ChildMap()
		want := map[int][]int{0: {1}, 1: {300}, 300: {500}, 500: {600}, 600: {selfPID}}
		if len(m) != len(want) {
			t.Fatalf("got %v", m)
		}
		for ppid, kids := range want {
			if !slices.Equal(m[ppid], kids) {
				t.Fatalf("children of %d = %v, want %v", ppid, m[ppid], kids)
			}
		}
	})

	t.Run("child map is one snapshot, not a lookup per process", func(t *testing.T) {
		f := hookChain()
		f.resolver().ChildMap()
		if calls := f.recorded(); !slices.Equal(calls, []string{"tree"}) {
			t.Fatalf("system calls = %v", calls)
		}
	})

	t.Run("tree CPU time sums readable descendants exactly once", func(t *testing.T) {
		f := &fakeSystem{self: selfPID, procs: map[int]fakeProc{
			10: {cpu: 1 * time.Second},
			11: {cpu: 2 * time.Second},
			12: {cpu: 4 * time.Second},
			13: {noCPU: true},
		}}
		m := map[int][]int{10: {11, 12}, 11: {12, 13}, 12: {10}}
		total, ok := f.resolver().TreeCPUTime(10, m)
		if !ok || total != 7*time.Second {
			t.Fatalf("got %v, %v", total, ok)
		}
	})

	t.Run("gateway pid-file formats", func(t *testing.T) {
		r := hookChain().resolver()
		cases := []struct {
			content string
			want    int
		}{
			{fmt.Sprintf(`{"pid": %d.0}`, selfPID), selfPID},
			{fmt.Sprintf("  %d  ", selfPID), selfPID},
			{fmt.Sprintf(`{"pid": "%d"}`, selfPID), -1},
			{fmt.Sprintf(`{"pid": %d.5}`, selfPID), -1},
			{`{"pid": 99999999999}`, -1},
			{`99999999999`, -1},
			{`{"pid": 0}`, -1},
			{`{"pid": -5}`, -1},
			{`{"kind": "hermes-gateway"}`, -1},
			{``, -1},
			{`garbage`, -1},
		}
		for _, c := range cases {
			file := filepath.Join(t.TempDir(), "gw.pid")
			writeFile(t, file, c.content)
			if got := r.GatewayPID(file); got != c.want {
				t.Errorf("GatewayPID(%q) = %d, want %d", c.content, got, c.want)
			}
		}
	})

	t.Run("gateway profiles are tried in name order after a dead default", func(t *testing.T) {
		home := t.TempDir()
		f := hookChain()
		f.alive = map[int]bool{8001: true, 8002: true}
		writeFile(t, filepath.Join(home, ".hermes", "gateway.pid"), fmt.Sprintf("%d", deadPID))
		writeFile(t, filepath.Join(home, ".hermes", "profiles", "b-second", "gateway.pid"), "8002")
		writeFile(t, filepath.Join(home, ".hermes", "profiles", "a-first", "gateway.pid"), "8001")
		writeFile(t, filepath.Join(home, ".hermes", "profiles", "0-stray-file"), "not a dir")
		if pid := f.resolver().GatewayPIDInHome(home, ".hermes/gateway.pid"); pid != 8001 {
			t.Fatalf("pid = %d", pid)
		}
	})
}
