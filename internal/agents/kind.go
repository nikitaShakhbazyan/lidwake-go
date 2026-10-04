// Package agents knows the agentic tools lidwake integrates with: how to recognize their
// processes, how to find the process that owns a hook invocation, and how to read the identity
// fields out of a hook's stdin payload.
package agents

import "strings"

// Kind is an agentic tool lidwake knows about. The value is the kebab-case id used in hook
// commands (`--tool <id>`), assertion keys and the event log.
type Kind string

const (
	ClaudeCode Kind = "claude-code"
	Codex      Kind = "codex"
	Cursor     Kind = "cursor"
	GeminiCLI  Kind = "gemini-cli"
	Aider      Kind = "aider"
	Hermes     Kind = "hermes"
	OpenCode   Kind = "opencode"
	Cline      Kind = "cline"
	Pi         Kind = "pi"
)

// All lists every known agent, in declaration order.
func All() []Kind {
	return []Kind{ClaudeCode, Codex, Cursor, GeminiCLI, Aider, Hermes, OpenCode, Cline, Pi}
}

// Parse returns the Kind for its id; ok is false for an unknown id.
func Parse(raw string) (Kind, bool) {
	k := Kind(raw)
	if _, known := kindInfo[k]; !known {
		return "", false
	}
	return k, true
}

// Known reports whether k is one of All.
func (k Kind) Known() bool {
	_, ok := kindInfo[k]
	return ok
}

type info struct {
	displayName string
	binaryNames []string
	argvMarkers [][]string
	gatewayPID  string
	tier        int
}

var kindInfo = map[Kind]info{
	ClaudeCode: {displayName: "Claude Code", binaryNames: []string{"claude"}, tier: 1},
	// Homebrew's cask symlinks `codex` → the triple-suffixed real binary
	// (`codex-aarch64-apple-darwin`), and proc_pidpath resolves the symlink, so the process
	// basename the daemon sees is the suffixed name, not `codex`. Without these the owning-PID
	// walk and the sniff sweep miss every Homebrew install — the assertion is placed with no PID,
	// so neither the process-exit watcher nor the CPU-idle sweep can release it, leaving the hold
	// pinned until the 24h backstop. (npm spawns a native binary actually named `codex`.)
	Codex: {
		displayName: "Codex",
		binaryNames: []string{"codex", "codex-aarch64-apple-darwin", "codex-x86_64-apple-darwin"},
		tier:        1,
	},
	Cursor:    {displayName: "Cursor", binaryNames: []string{"cursor", "Cursor"}, tier: 1},
	GeminiCLI: {displayName: "Gemini CLI", binaryNames: []string{"gemini"}, tier: 1},
	Aider:     {displayName: "Aider", binaryNames: []string{"aider"}, tier: 2},
	// Hermes runs as `python -m hermes_cli.main {gateway run | dashboard …}`: only argv reveals
	// it, and it is one shared gateway process rather than one process per session.
	Hermes: {
		displayName: "Hermes",
		binaryNames: []string{"hermes"},
		argvMarkers: [][]string{{"hermes_cli.main", "gateway"}, {"hermes_cli.main", "dashboard"}},
		gatewayPID:  ".hermes/gateway.pid",
		tier:        2,
	},
	// npm's `opencode-ai` package maps its bin entry to `bin/opencode.exe` (yes, on macOS), and
	// Homebrew symlinks `opencode` → that file, so proc_pidpath sees basename `opencode.exe`.
	// Without it the owning-PID walk binds the plugin's hold to whatever agent ancestor spawned
	// opencode, and the sniff sweep never sees the process at all. (Direct npm installs spawn a
	// wrapper actually named `opencode`.)
	OpenCode: {displayName: "OpenCode", binaryNames: []string{"opencode", "opencode.exe"}, tier: 2},
	Cline:    {displayName: "Cline", binaryNames: []string{"cline"}, tier: 2},
	// `pi` runs as a Node process (argv0 often "node"), so the sniffer rarely matches it — the TS
	// extension hook is the real integration; this is a weak best-effort fallback.
	Pi: {displayName: "Pi", binaryNames: []string{"pi"}, tier: 2},
}

// DisplayName is the human-readable name. An unknown kind displays as its raw id.
func (k Kind) DisplayName() string {
	if i, ok := kindInfo[k]; ok {
		return i.displayName
	}
	return string(k)
}

// BinaryNames are the executable basenames the process sniffer and the owning-PID walk match.
// Returns a fresh slice.
func (k Kind) BinaryNames() []string {
	return append([]string(nil), kindInfo[k].binaryNames...)
}

// ArgvMarkers are argv substring markers for agents that run under a generic interpreter, where
// the executable path (`python`, `node`) reveals nothing. A process matches when its argv
// contains every marker of any one group (groups are alternatives). Hermes runs as
// `python -m hermes_cli.main {gateway run | dashboard …}`, so both the long-lived gateway and the
// desktop app's embedded dashboard are recognized. nil for agents identifiable by path.
func (k Kind) ArgvMarkers() [][]string {
	src := kindInfo[k].argvMarkers
	if src == nil {
		return nil
	}
	out := make([][]string, len(src))
	for i, g := range src {
		out[i] = append([]string(nil), g...)
	}
	return out
}

// GatewayPIDFileRelativePath is, for an agent that runs as a single long-lived shared process
// (a gateway) multiplexing many logical sessions, the home-relative path of the pid-file that
// process writes; "" for the normal one-process-per-session agents.
//
// Such agents need different hold bookkeeping: (1) their per-session start/end hooks don't
// bracket process lifetime, so the hold is keyed to one fixed gateway scope rather than per
// session, and (2) the executable is a generic interpreter, so the owning-PID parent walk can't
// identify it — the watched PID is read from this file instead. With a real PID attached, the
// daemon's CPU-idle and dead-process release nets apply to the gateway tree.
func (k Kind) GatewayPIDFileRelativePath() string { return kindInfo[k].gatewayPID }

// IsGatewayScoped reports whether the agent runs as a shared gateway process.
func (k Kind) IsGatewayScoped() bool { return kindInfo[k].gatewayPID != "" }

// Tier is the integration tier: 1 = full hooks, 2 = partial/wrapper/plugin needed. 0 for an
// unknown kind.
func (k Kind) Tier() int { return kindInfo[k].tier }

// componentMatchedBinaryNames may also match a directory component of an executable path, not
// just its basename. Reserved for versioned install layouts where the basename is a version
// string (`…/claude/versions/2.1.156`). Generic names must never component-match — `pi` would
// claim anything under a Raspberry Pi project folder, and any executable inside a user's
// `~/src/cline/` would read as that agent.
var componentMatchedBinaryNames = map[string]bool{"claude": true}

// byBinaryName is the reverse lookup from a binary name to its agent. Static.
var byBinaryName = func() map[string]Kind {
	m := map[string]Kind{}
	for _, k := range All() {
		for _, n := range kindInfo[k].binaryNames {
			m[n] = k
		}
	}
	return m
}()

// AllBinaryNames is every known binary name across all agents, as a fresh set.
func AllBinaryNames() map[string]bool {
	out := make(map[string]bool, len(byBinaryName))
	for n := range byBinaryName {
		out[n] = true
	}
	return out
}

// ComponentMatchedBinaryNames returns a fresh copy of the names allowed to match a path
// component (see PathMatchesAgent).
func ComponentMatchedBinaryNames() map[string]bool {
	out := make(map[string]bool, len(componentMatchedBinaryNames))
	for n := range componentMatchedBinaryNames {
		out[n] = true
	}
	return out
}

// ByBinaryName returns the agent owning a binary name.
func ByBinaryName(name string) (Kind, bool) {
	k, ok := byBinaryName[name]
	return k, ok
}

// ForRunningProcess identifies the agent owning a running process: by basename first, then —
// only for the names in componentMatchedBinaryNames — by path component, so versioned installs
// (`…/claude/versions/2.1.156`, basename `2.1.156`) are still recognized by their `claude`
// segment.
func ForRunningProcess(name, path string) (Kind, bool) {
	if k, ok := byBinaryName[name]; ok {
		return k, true
	}
	for _, c := range pathComponents(path) {
		if componentMatchedBinaryNames[c] {
			if k, ok := byBinaryName[c]; ok {
				return k, true
			}
		}
	}
	return "", false
}

// ArgvMatchedAgents are the agents identifiable only by argv (see ArgvMarkers).
func ArgvMatchedAgents() []Kind {
	var out []Kind
	for _, k := range All() {
		if kindInfo[k].argvMarkers != nil {
			out = append(out, k)
		}
	}
	return out
}

// ForArgv identifies the agent an argument vector belongs to, for interpreter-hosted agents the
// path-based ForRunningProcess can't recognize. A marker matches if it is a substring of any
// argv element; an agent matches if all markers of one of its groups do.
func ForArgv(argv []string) (Kind, bool) {
	if len(argv) == 0 {
		return "", false
	}
	for _, k := range ArgvMatchedAgents() {
		for _, group := range kindInfo[k].argvMarkers {
			if allMarkersPresent(group, argv) {
				return k, true
			}
		}
	}
	return "", false
}

func allMarkersPresent(group, argv []string) bool {
	for _, marker := range group {
		found := false
		for _, arg := range argv {
			if strings.Contains(arg, marker) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// EnvironmentMarksPi reports whether the environment carries the markers Pi's CLI and RPC entry
// points set in their own process at startup (`AI_AGENT=pi`, `PI_CODING_AGENT=true`), inherited
// by every child Pi spawns — including the CLI its extension shells out to. Their presence
// proves Pi is an ancestor of this process, which is what authorizes the argv-based owner walk
// (ArgvIsPi): without the gate, that walk could bind a hold to an arbitrary Node ancestor of a
// non-Pi invocation.
func EnvironmentMarksPi(env map[string]string) bool {
	return env["PI_CODING_AGENT"] == "true" || env["AI_AGENT"] == "pi"
}

// ArgvIsPi reports whether an argument vector identifies a Node-hosted Pi process. Pi's bin
// entry is a `#!/usr/bin/env node` script, so the running process is `node <script> …` — its
// executable path is Node's, and only argv reveals the agent. `<script>` is the launcher path the
// user invoked: an npm/Homebrew symlink with basename `pi`, or (for Nix-style wrappers that exec
// the real entry point) a path inside the `pi-coding-agent` package. argv[0] — the interpreter —
// is skipped; a standalone binary actually named `pi` is matched by the executable-path walk
// instead. Callers must gate on EnvironmentMarksPi: these patterns alone are too weak to
// identify Pi among arbitrary processes.
func ArgvIsPi(argv []string) bool {
	if len(argv) < 2 {
		return false
	}
	for _, arg := range argv[1:] {
		if lastPathComponent(arg) == "pi" || strings.Contains(arg, "pi-coding-agent") {
			return true
		}
	}
	return false
}

// lastPathComponent is the final element of a slash-separated path, ignoring trailing slashes;
// "" for "", "/" for a path of only slashes.
func lastPathComponent(p string) string {
	if p == "" {
		return ""
	}
	trimmed := strings.TrimRight(p, "/")
	if trimmed == "" {
		return "/"
	}
	if i := strings.LastIndexByte(trimmed, '/'); i >= 0 {
		return trimmed[i+1:]
	}
	return trimmed
}

// pathComponents splits a path into its non-empty slash-separated components.
func pathComponents(p string) []string {
	var out []string
	for _, c := range strings.Split(p, "/") {
		if c != "" {
			out = append(out, c)
		}
	}
	return out
}
