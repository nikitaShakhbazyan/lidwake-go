package agents

import (
	"encoding/json"
	"math"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/nikitaShakhbazyan/lidwake-go/internal/darwin"
)

// maxWalkDepth bounds the parent walk: an agent sits a few levels above the hook's shell, and a
// corrupt or cyclic parent chain must not spin.
const maxWalkDepth = 16

// Process is one running process as the daemon's sniff sweep sees it.
type Process struct {
	PID  int
	Name string // executable basename
	Path string // full executable path
}

// Resolver holds the process-tree helpers shared by the CLI (to find which agent owns an
// `acquire`) and the daemon (to sweep for running agents, and to sum a tree's CPU time).
//
// The CLI is invoked from a hook command, typically as `agent → /bin/sh -c "lidwake …" →
// lidwake`. The parent PID is therefore the shell, which exits the instant lidwake returns:
// watching it would force-release the assertion immediately, while the agent is still working.
// OwningAgentPID walks up the parent chain to the real agent process so the daemon watches the
// process that actually matters.
//
// The fields are the system calls the logic needs; System wires them to package darwin and
// tests substitute fakes. Every field must be set. ProcessTree maps each parent PID to its
// direct children from one process-table snapshot.
type Resolver struct {
	Getppid      func() int
	ProcessPath  func(pid int) (string, error)
	ParentPID    func(pid int) (int, error)
	ProcessArgs  func(pid int) ([]string, error)
	ProcessAlive func(pid int) bool
	AllPIDs      func() ([]int, error)
	ProcessTree  func() (map[int][]int, error)
	CPUTime      func(pid int) (time.Duration, error)
}

// System returns a Resolver backed by the real system calls.
func System() *Resolver {
	return &Resolver{
		Getppid:      os.Getppid,
		ProcessPath:  darwin.ProcessPath,
		ParentPID:    darwin.ParentPID,
		ProcessArgs:  darwin.ProcessArgs,
		ProcessAlive: darwin.ProcessAlive,
		AllPIDs:      darwin.AllPIDs,
		ProcessTree:  darwin.ChildMap,
		CPUTime:      darwin.CPUTime,
	}
}

// OwningAgentPID walks up the parent chain from this process looking for an ancestor whose
// executable matches names (see PathMatchesAgent). Returns that PID, or -1 if none is found —
// the daemon must then not process-watch, which would risk a premature release.
func (r *Resolver) OwningAgentPID(names map[string]bool) int {
	return r.walk(func(pid int) bool {
		p, ok := r.Path(pid)
		return ok && PathMatchesAgent(p, names)
	})
}

// OwningAgentPIDByArgv walks up the parent chain matching ancestors by argument vector rather
// than executable path — for interpreter-hosted agents (Pi under Node), where the executable is
// the interpreter and only argv reveals the agent. Same contract as OwningAgentPID. An ancestor
// whose argv can't be read (permission) is skipped, not matched.
func (r *Resolver) OwningAgentPIDByArgv(matches func(argv []string) bool) int {
	return r.walk(func(pid int) bool {
		argv, ok := r.Arguments(pid)
		return ok && matches(argv)
	})
}

func (r *Resolver) walk(match func(pid int) bool) int {
	pid := r.Getppid()
	for depth := 0; pid > 1 && depth < maxWalkDepth; depth++ {
		if match(pid) {
			return pid
		}
		parent := r.Parent(pid)
		if parent <= 0 || parent == pid {
			break
		}
		pid = parent
	}
	return -1
}

// PathMatchesAgent reports whether an executable path belongs to a known agent. The basename is
// checked against names; path components are additionally checked, but only for names that are
// also in ComponentMatchedBinaryNames — versioned installs have a version string as their
// basename (`~/.local/share/claude/versions/2.1.156`, recognizable only by its `claude`
// segment; basename-only matching missed Claude entirely, leaving its assertion unwatchable),
// while generic names like `pi` would otherwise claim any executable under a same-named project
// directory.
func PathMatchesAgent(p string, names map[string]bool) bool {
	if names[lastPathComponent(p)] {
		return true
	}
	for _, c := range pathComponents(p) {
		if names[c] && componentMatchedBinaryNames[c] {
			return true
		}
	}
	return false
}

// RunningProcesses lists every running process whose executable path can be read. Used by the
// daemon's periodic sniff sweep. Best-effort: unreadable processes are skipped, and a failed
// listing yields nil.
func (r *Resolver) RunningProcesses() []Process {
	pids, err := r.AllPIDs()
	if err != nil {
		return nil
	}
	out := make([]Process, 0, len(pids))
	for _, pid := range pids {
		if pid <= 0 {
			continue
		}
		p, ok := r.Path(pid)
		if !ok {
			continue
		}
		out = append(out, Process{PID: pid, Name: lastPathComponent(p), Path: p})
	}
	return out
}

// Path is pid's full executable path; ok is false when it can't be read.
func (r *Resolver) Path(pid int) (string, bool) {
	if pid <= 0 {
		return "", false
	}
	p, err := r.ProcessPath(pid)
	if err != nil || p == "" {
		return "", false
	}
	return p, true
}

// Name is pid's executable basename; ok is false when the path can't be read.
func (r *Resolver) Name(pid int) (string, bool) {
	p, ok := r.Path(pid)
	if !ok {
		return "", false
	}
	return lastPathComponent(p), true
}

// Parent is pid's parent PID, or -1 when it can't be read.
func (r *Resolver) Parent(pid int) int {
	if pid <= 0 {
		return -1
	}
	ppid, err := r.ParentPID(pid)
	if err != nil {
		return -1
	}
	return ppid
}

// Arguments is pid's argument vector; ok is false when it can't be read (permission, exited)
// or is empty. This is how interpreter-hosted agents are identified: a Hermes backend runs as a
// generic `python -m hermes_cli.main gateway run`, so only argv reveals what it is.
func (r *Resolver) Arguments(pid int) ([]string, bool) {
	if pid <= 0 {
		return nil, false
	}
	argv, err := r.ProcessArgs(pid)
	if err != nil || len(argv) == 0 {
		return nil, false
	}
	return argv, true
}

// CPU is pid's cumulative user+system CPU time; ok is false when it can't be read (gone).
func (r *Resolver) CPU(pid int) (time.Duration, bool) {
	if pid <= 0 {
		return 0, false
	}
	d, err := r.CPUTime(pid)
	if err != nil {
		return 0, false
	}
	return d, true
}

// ChildMap maps each parent PID to its direct children, from a single snapshot of the process
// table — the source `ps` and `pgrep -P` use. Tree walks build on this rather than per-process
// child listing, which is unreliable on current macOS (truncated results, so children are
// missed), and rather than one parent lookup per process, which costs a system call each and
// mixes moments. Best-effort: a process spawned during the snapshot may be missing until the
// next one, and a failed snapshot yields an empty map.
func (r *Resolver) ChildMap() map[int][]int {
	m, err := r.ProcessTree()
	if err != nil || m == nil {
		return map[int][]int{}
	}
	return m
}

// TreeCPUTime is the cumulative CPU time of root plus every descendant in childMap. Summing the
// tree — not just the root — is what keeps a long tool call active in the reading: the agent
// waits while a busy child does the work. Descendants that can't be read contribute nothing;
// cycles in childMap are tolerated. ok is false only when the root itself is unreadable (gone).
func (r *Resolver) TreeCPUTime(root int, childMap map[int][]int) (time.Duration, bool) {
	total, ok := r.CPU(root)
	if !ok {
		return 0, false
	}
	seen := map[int]bool{root: true}
	stack := append([]int(nil), childMap[root]...)
	for len(stack) > 0 {
		pid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[pid] {
			continue
		}
		seen[pid] = true
		if t, ok := r.CPU(pid); ok {
			total += t
		}
		stack = append(stack, childMap[pid]...)
	}
	return total, true
}

// GatewayPID resolves the live PID of a gateway-style agent from its pid-file (see
// Kind.GatewayPIDFileRelativePath). The file holds either a bare integer or a JSON object with
// a "pid" field — Hermes writes `{"pid": 1006, "kind": "hermes-gateway", …}`. Returns the PID
// only if it parses and the process is alive, otherwise -1 (the caller then asks the daemon not
// to process-watch, as with an unresolved agent). The liveness check keeps a stale pid-file
// (gateway exited without cleaning up) from binding a hold to a dead or, worse, recycled PID.
func (r *Resolver) GatewayPID(pidFilePath string) int {
	data, err := os.ReadFile(pidFilePath)
	if err != nil {
		return -1
	}
	pid, ok := parsePIDFile(data)
	if !ok || pid <= 0 || !r.ProcessAlive(pid) {
		return -1
	}
	return pid
}

// GatewayPIDInHome resolves a live gateway PID from the default pid-file and any per-profile
// ones. Hermes runs one gateway per profile — named profiles live at
// `<home>/.hermes/profiles/<name>/`, each a full home writing its own gateway.pid — and the
// desktop app is just a UI launcher over one of those gateways. Relying on the default path alone
// would miss a profile gateway (and the desktop's), degrading the hold to the end hook and the
// 24h backstop. Mirrors Hermes' own gateway discovery: the default profile first, then the first
// live named profile in name order; -1 if none is alive.
//
// pidFileRelativePath is the home-relative default pid-file, `<dir>/<file>` (e.g.
// `.hermes/gateway.pid`); the profiles are looked up as `<dir>/profiles/*/<file>`.
func (r *Resolver) GatewayPIDInHome(homeRoot, pidFileRelativePath string) int {
	if pid := r.GatewayPID(filepath.Join(homeRoot, pidFileRelativePath)); pid > 0 {
		return pid
	}
	dir := path.Dir(pidFileRelativePath)
	file := lastPathComponent(pidFileRelativePath)
	profiles := filepath.Join(homeRoot, dir, "profiles")
	entries, err := os.ReadDir(profiles) // sorted by name
	if err != nil {
		return -1
	}
	for _, e := range entries {
		if pid := r.GatewayPID(filepath.Join(profiles, e.Name(), file)); pid > 0 {
			return pid
		}
	}
	return -1
}

// parsePIDFile reads a JSON object's integral "pid", or else the whole trimmed text as a 32-bit
// integer. Values outside the PID range are rejected rather than truncated.
func parsePIDFile(data []byte) (int, bool) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err == nil {
		if raw, ok := obj["pid"]; ok {
			var f float64
			if err := json.Unmarshal(raw, &f); err == nil && f == math.Trunc(f) &&
				f >= math.MinInt32 && f <= math.MaxInt32 {
				return int(f), true
			}
		}
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 32)
	if err != nil {
		return 0, false
	}
	return int(n), true
}
