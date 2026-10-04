// Package hooks wires lidwake into each agent's own configuration: the hook entries that make an
// agent call `lidwake acquire`/`release`, the opt-in background-shell hook, the `lidwake mcp`
// server registration, Codex hook-trust detection, and the copy-paste snippets for agents lidwake
// has no integration for.
//
// The files touched here belong to other programs and hold the user's own content, so every edit
// is read → mutate → write a copy, scoped to entries that are provably lidwake's, and refused
// outright when the file can't be parsed or changed underneath us.
package hooks

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"

	"github.com/nikitaShakhbazyan/lidwake-go/internal/paths"
)

// InstallState is an agent's hook-installation state.
type InstallState string

const (
	// StateInstalled: lidwake's entries are present and match what install would write.
	StateInstalled InstallState = "installed"
	// StateNotInstalled: no lidwake entry exists in the agent's config.
	StateNotInstalled InstallState = "notInstalled"
	// StateModifiedExternally: a lidwake entry exists but was edited, is stale, or is only partly
	// present. Reinstalling repairs it.
	StateModifiedExternally InstallState = "modifiedExternally"
	// StateConfigUnreadable: the config exists but can't be parsed, so the state is unknowable —
	// and installing would risk the user's content.
	StateConfigUnreadable InstallState = "configUnreadable"
)

// SkipReason says why an install or uninstall did not happen.
type SkipReason int

const (
	// SkipNotInstalled: the agent isn't installed on this system.
	SkipNotInstalled SkipReason = iota + 1
	// SkipUnsupported: the capability doesn't exist for this agent; Detail says why.
	SkipUnsupported
	// SkipConfigUnreadable: the config file exists but isn't a parseable JSON object (comments, a
	// syntax error mid-edit, an array root). Refusing is the safe move: treating a read failure as
	// an empty file would make the next write replace the user's entire config. Detail is the path.
	SkipConfigUnreadable
	// SkipConcurrentModification: the config changed on disk between read and write (agents
	// rewrite their own configs during sessions). Retrying re-reads the fresh content. Detail is
	// the path.
	SkipConcurrentModification
)

// SkipError is returned when an install or uninstall was skipped or refused.
type SkipError struct {
	Reason SkipReason
	Detail string
}

func (e *SkipError) Error() string {
	switch e.Reason {
	case SkipNotInstalled:
		return "The agent isn't installed on this system."
	case SkipUnsupported:
		return e.Detail
	case SkipConfigUnreadable:
		return e.Detail + " exists but isn't valid JSON — fix or remove it, then retry."
	case SkipConcurrentModification:
		return e.Detail + " changed while updating it (the agent may be running) — retry."
	default:
		return "skipped: " + e.Detail
	}
}

// IsSkip reports whether err is a *SkipError with the given reason.
func IsSkip(err error, reason SkipReason) bool {
	var s *SkipError
	return errors.As(err, &s) && s.Reason == reason
}

// ErrUnknownAgent is returned for an agent name no integration is registered for.
var ErrUnknownAgent = errors.New("unknown agent")

// Result describes what an install or uninstall did (or, for a dry run, would do).
type Result struct {
	Summary string
	// Diff is a BEFORE/AFTER rendering of the change, or "(unchanged)".
	Diff string
}

const (
	unchangedDiff   = "(unchanged)"
	nothingToRemove = "nothing to remove"
)

func nothingRemoved() Result { return Result{Summary: nothingToRemove, Diff: unchangedDiff} }

// Installer writes (or removes) lidwake's entries inside each agent's config. Agents are named by
// their --tool value ("claude-code", "codex", …). The zero value is not usable; build one with New
// or fill every path field.
type Installer struct {
	// CLIPath is the absolute path of the lidwake binary embedded in hook commands.
	CLIPath string
	// Home roots every ~-relative config path.
	Home string
	// ApplicationsDir is where app-bundle detection looks (Cursor.app); "" means /Applications.
	ApplicationsDir string
	// SearchPath lists the directories binary-based detection probes; nil means $PATH plus the
	// standard install locations a launchd-provided PATH misses.
	SearchPath []string
}

// New returns an installer embedding cliPath in hook commands and rooted at home. An empty cliPath
// means DefaultCLIPath(), an empty home the current user's home directory: $HOME, else the
// account database. An empty Home would make every config path relative to the working
// directory, where a project's own .claude/settings.json could be edited instead.
func New(cliPath, home string) *Installer {
	if cliPath == "" {
		cliPath = DefaultCLIPath()
	}
	if home == "" {
		home = paths.Home()
	}
	if home == "" {
		if u, err := user.Current(); err == nil {
			home = u.HomeDir
		}
	}
	return &Installer{CLIPath: cliPath, Home: home}
}

// DefaultCLIPath is the path hooks should call: paths.InstalledBin when this process is the
// installed binary (however it was reached — the /usr/local/bin symlink, a PATH lookup), else this
// executable with symlinks resolved. A canonical string keeps install-state comparisons stable no
// matter which entry point wrote the hook.
func DefaultCLIPath() string {
	exe, err := os.Executable()
	if err != nil {
		return paths.InstalledBin
	}
	return resolveCLIPath(exe, filepath.EvalSymlinks)
}

func resolveCLIPath(exe string, eval func(string) (string, error)) string {
	resolved := exe
	if r, err := eval(exe); err == nil {
		resolved = r
	}
	if resolved == paths.InstalledBin {
		return paths.InstalledBin
	}
	if installed, err := eval(paths.InstalledBin); err == nil && installed == resolved {
		return paths.InstalledBin
	}
	return resolved
}

func (in *Installer) context() hookContext {
	return hookContext{
		cliPath:    in.CLIPath,
		home:       in.Home,
		appsDir:    in.ApplicationsDir,
		searchPath: in.SearchPath,
	}
}

func unknownAgent(agent string) error {
	return fmt.Errorf("%w %q", ErrUnknownAgent, agent)
}

// Install wires lidwake into agent's config. A SkipError with SkipNotInstalled means the agent
// isn't detected on this system.
func (in *Installer) Install(agent string, dryRun bool) (Result, error) {
	integ, ok := lookup(agent)
	if !ok {
		return Result{}, unknownAgent(agent)
	}
	ctx := in.context()
	if !integ.detected(ctx) {
		return Result{}, &SkipError{Reason: SkipNotInstalled}
	}
	return integ.install(ctx, dryRun)
}

// Uninstall removes lidwake's entries for agent, leaving everything else in place. With dryRun it
// only computes the change.
func (in *Installer) Uninstall(agent string, dryRun bool) (Result, error) {
	integ, ok := lookup(agent)
	if !ok {
		return Result{}, unknownAgent(agent)
	}
	return integ.uninstall(in.context(), dryRun)
}

// DetectedAgents lists the agents that appear installed, in canonical order.
func (in *Installer) DetectedAgents() []string {
	ctx := in.context()
	var out []string
	for _, agent := range Agents() {
		if integ, _ := lookup(agent); integ.detected(ctx) {
			out = append(out, agent)
		}
	}
	return out
}

// ConfigPath is the file lidwake writes agent's hooks into, "" for an unknown agent.
func (in *Installer) ConfigPath(agent string) string {
	integ, ok := lookup(agent)
	if !ok {
		return ""
	}
	return integ.configPath(in.context())
}

// State is agent's hook-installation state.
func (in *Installer) State(agent string) InstallState {
	integ, ok := lookup(agent)
	if !ok {
		return StateNotInstalled
	}
	return integ.state(in.context())
}

// ---- MCP server registration -----------------------------------------------------------------

func (in *Installer) mcp(agent string) *mcpServerShape {
	integ, ok := lookup(agent)
	if !ok {
		return nil
	}
	m, ok := integ.(mcpIntegration)
	if !ok {
		return nil
	}
	return m.mcpShape(in.context())
}

// SupportsMCP reports whether lidwake knows how to register its `lidwake mcp` server with agent.
// Distinct from hook support: only agents whose MCP config format is device-verified qualify.
func (in *Installer) SupportsMCP(agent string) bool { return in.mcp(agent) != nil }

// InstallMCP registers lidwake's MCP server in agent's config so the agent can call keep_awake.
func (in *Installer) InstallMCP(agent string, dryRun bool) (Result, error) {
	shape := in.mcp(agent)
	if shape == nil {
		return Result{}, &SkipError{Reason: SkipUnsupported, Detail: "MCP not supported for " + agent}
	}
	return shape.install(dryRun)
}

// UninstallMCP removes lidwake's MCP server from agent's config; a no-op without MCP support.
func (in *Installer) UninstallMCP(agent string, dryRun bool) (Result, error) {
	shape := in.mcp(agent)
	if shape == nil {
		return nothingRemoved(), nil
	}
	return shape.uninstall(dryRun)
}

// MCPState is the MCP registration state; StateNotInstalled for an agent without MCP support.
func (in *Installer) MCPState(agent string) InstallState {
	shape := in.mcp(agent)
	if shape == nil {
		return StateNotInstalled
	}
	return shape.state()
}

// ---- Background-shell hook (opt-in, toggled separately) --------------------------------------

func (in *Installer) backgroundBash(agent string) *backgroundBashHookShape {
	integ, ok := lookup(agent)
	if !ok {
		return nil
	}
	b, ok := integ.(backgroundBashIntegration)
	if !ok {
		return nil
	}
	return b.backgroundBashShape(in.context())
}

// SupportsBackgroundHold reports whether agent exposes a clean run_in_background pre-tool signal
// for the opt-in background-shell hook (Claude Code only).
func (in *Installer) SupportsBackgroundHold(agent string) bool {
	return in.backgroundBash(agent) != nil
}

// InstallBackgroundHold installs the background-shell PreToolUse(Bash) hook for agent.
func (in *Installer) InstallBackgroundHold(agent string, dryRun bool) (Result, error) {
	shape := in.backgroundBash(agent)
	if shape == nil {
		return Result{}, &SkipError{
			Reason: SkipUnsupported,
			Detail: "Background-shell keep-awake not supported for " + agent,
		}
	}
	return shape.install(dryRun)
}

// UninstallBackgroundHold removes the background-shell hook; a no-op without support.
func (in *Installer) UninstallBackgroundHold(agent string, dryRun bool) (Result, error) {
	shape := in.backgroundBash(agent)
	if shape == nil {
		return nothingRemoved(), nil
	}
	return shape.uninstall(dryRun)
}

// BackgroundHoldState is the background-shell hook's state; StateNotInstalled without support.
func (in *Installer) BackgroundHoldState(agent string) InstallState {
	shape := in.backgroundBash(agent)
	if shape == nil {
		return StateNotInstalled
	}
	return shape.state()
}
