package hooks

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// hookContext is everything an integration needs to know about the local environment: where the
// lidwake CLI lives (embedded in the hook commands it writes) and which directories to root
// config paths and detection at. Tests inject temp dirs for all of them.
type hookContext struct {
	cliPath    string
	home       string
	appsDir    string   // "" = /Applications
	searchPath []string // nil = $PATH + standard install locations
}

// mcpServerName is the name lidwake registers its MCP server under in every agent's config. A
// named key is its own idempotency handle, unlike the array-based hook entries.
const mcpServerName = "lidwake"

var plainPath = regexp.MustCompile(`^[A-Za-z0-9_/.-]+$`)

// quotedCLI is the CLI path, shell-quoted unless it is plain enough to need none. POSIX single
// quotes neutralize every shell metacharacter (spaces, $, quotes, backticks).
func (c hookContext) quotedCLI() string {
	if plainPath.MatchString(c.cliPath) {
		return c.cliPath
	}
	return "'" + strings.ReplaceAll(c.cliPath, "'", `'\''`) + "'"
}

// cmdOpts are the optional parts of an acquire/release hook command.
type cmdOpts struct {
	// sessionVar is a positional session key (an env var reference such as
	// $CLAUDE_CODE_SESSION_ID). Empty: the CLI sources session_id from the hook's stdin.
	sessionVar string
	// subagent appends --subagent: the CLI keys the hold on the sub-agent's agent_id from stdin,
	// so a SubagentStart/SubagentStop hold outlives the parent turn's Stop. Sub-agent hooks always
	// read their id from stdin and pass no sessionVar.
	subagent bool
	// ttlSeconds > 0 appends --ttl <n>, expiring the hold as a backstop for agents whose release
	// signal alone can't be trusted to fire (Cursor).
	ttlSeconds int
}

// hookCommand builds an `acquire`/`release` hook command.
func (c hookContext) hookCommand(op, tool string, o cmdOpts) string {
	cmd := c.quotedCLI() + " " + op
	if o.sessionVar != "" {
		cmd += " " + o.sessionVar
	}
	cmd += " --tool " + tool
	if o.subagent {
		cmd += " --subagent"
	}
	if o.ttlSeconds > 0 {
		cmd += " --ttl " + strconv.Itoa(o.ttlSeconds)
	}
	return cmd
}

// backgroundAcquireCommand builds the PreToolUse(Bash) acquire: `acquire --tool <tool>
// --if-background --ttl <n>`. It fires when the agent launches a run_in_background command — the
// one signal for a background task that outlives the turn's Stop (which fires no completion hook),
// so the hold is TTL-bounded. No positional precedes the flags, so --if-background parses as a
// bare flag.
func (c hookContext) backgroundAcquireCommand(tool string, ttlSeconds int) string {
	return c.quotedCLI() + " acquire --tool " + tool + " --if-background --ttl " + strconv.Itoa(ttlSeconds)
}

// mcpEntry is the canonical MCP server entry: spawns `lidwake mcp --tool <agent>` over stdio, so
// holds the agent places carry its name. The path is not shell-quoted — MCP command/args are
// exec'd directly, so a path with spaces is fine as a single argv element.
func (c hookContext) mcpEntry(tool string) map[string]any {
	return map[string]any{
		"type":    "stdio",
		"command": c.cliPath,
		"args":    []any{"mcp", "--tool", tool},
	}
}

// homePath joins elem onto the home directory.
func (c hookContext) homePath(elem ...string) string {
	return filepath.Join(append([]string{c.home}, elem...)...)
}

// applicationsPath joins elem onto the applications directory.
func (c hookContext) applicationsPath(elem ...string) string {
	dir := c.appsDir
	if dir == "" {
		dir = "/Applications"
	}
	return filepath.Join(append([]string{dir}, elem...)...)
}

// binaryOnPath reports whether an executable named name exists in the search path. The default
// search path adds the install locations a GUI/launchd PATH misses (Homebrew, ~/.local/bin). Used
// by binary-based detection where there is no config directory to probe.
func (c hookContext) binaryOnPath(name string) bool {
	dirs := c.searchPath
	if dirs == nil {
		for _, d := range strings.Split(os.Getenv("PATH"), ":") {
			if d != "" {
				dirs = append(dirs, d)
			}
		}
		dirs = append(dirs, "/opt/homebrew/bin", "/usr/local/bin", c.homePath(".local", "bin"))
	}
	for _, dir := range dirs {
		fi, err := os.Stat(filepath.Join(dir, name))
		if err == nil && !fi.IsDir() && fi.Mode().Perm()&0o111 != 0 {
			return true
		}
	}
	return false
}

// exists reports whether path exists (following symlinks).
func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// integration is one agent's wiring: how to detect it, and how to install, uninstall and inspect
// the entries that make it call `lidwake acquire`/`release`. One implementation per agent, each in
// its own file registering itself from init(), so adding an agent is a single new file.
//
// The shared mechanics are the shapes: nestedJSONHookShape and flatJSONHookShape (JSON hook
// files), codexHookShape (Codex's trust-preserving variant), plus the shell-wrapper and
// single-file-plugin helpers for agents without a JSON hook file. Each integration is mostly a
// description of paths and commands delegating to one of them.
type integration interface {
	// agent is the --tool value this integration serves.
	agent() string
	// detected reports whether the agent appears installed (config dir, app bundle or binary).
	detected(ctx hookContext) bool
	// configPath is the file the hooks are written into — the one a "reveal" affordance shows.
	configPath(ctx hookContext) string
	install(ctx hookContext, dryRun bool) (Result, error)
	uninstall(ctx hookContext, dryRun bool) (Result, error)
	state(ctx hookContext) InstallState
}

// mcpIntegration is implemented by integrations that can register lidwake's MCP server. A nil
// shape means no (verified) MCP support. Separate from the hooks: hooks track when the agent is
// working; the MCP server lets the agent deliberately hold sleep past its turn.
type mcpIntegration interface {
	mcpShape(ctx hookContext) *mcpServerShape
}

// backgroundBashIntegration is implemented by integrations with a clean run_in_background
// pre-tool signal. Like MCP it is toggled on its own, so flipping it never rewrites the core
// acquire/release set (and, for a trust-gated agent, never re-triggers approval).
type backgroundBashIntegration interface {
	backgroundBashShape(ctx hookContext) *backgroundBashHookShape
}

// agentOrder is the canonical agent order (the order agents are listed and detected in).
var agentOrder = []string{
	"claude-code", "codex", "cursor", "gemini-cli", "aider", "hermes", "opencode", "cline", "pi",
}

// registry maps every agent to its integration. Written only from init(), read-only afterwards.
var registry = map[string]integration{}

// register adds an integration; each integration file calls it from init(). Registering an agent
// twice is a programming error caught at startup.
func register(i integration) {
	if _, dup := registry[i.agent()]; dup {
		panic("hooks: duplicate integration for " + i.agent())
	}
	registry[i.agent()] = i
}

func lookup(agent string) (integration, bool) {
	i, ok := registry[agent]
	return i, ok
}

// Agents lists the agents with a registered integration, in canonical order (agents outside it
// last, by name).
func Agents() []string {
	out := make([]string, 0, len(registry))
	for agent := range registry {
		out = append(out, agent)
	}
	rank := func(a string) int {
		if i := slices.Index(agentOrder, a); i >= 0 {
			return i
		}
		return len(agentOrder)
	}
	slices.SortFunc(out, func(a, b string) int {
		if ra, rb := rank(a), rank(b); ra != rb {
			return ra - rb
		}
		return strings.Compare(a, b)
	})
	return out
}
