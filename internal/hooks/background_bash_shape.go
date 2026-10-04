package hooks

import (
	"time"

	"github.com/nikitaShakhbazyan/lidwake-go/internal/activity"
)

// backgroundBashHookShape installs (or removes) the opt-in background-shell hook: a PreToolUse
// hook matched to the Bash tool that runs `acquire --if-background`, placing a TTL-bounded hold
// whenever the agent launches a run_in_background command — the only signal such a command gives.
//
// It is a separate install path, the MCP registration's sibling, not part of the core hook shape:
// a default-off capability toggled on its own. Folding it into nestedJSONHookShape would rewrite
// the whole acquire/release/sub-agent set on every flip of the setting. So it is scoped to the
// single PreToolUse event: install upserts our one handler (in a matcher group of our own),
// uninstall removes only it, and neither touches the core hooks.
//
// One coupling remains: the core uninstall strips every lidwake-tagged handler from every event,
// this one included, so disconnecting an agent drops the background hook too. A plain reinstall of
// the core hooks leaves PreToolUse alone; whoever reconnects an agent re-applies this hook when the
// setting is on.
//
//	{ "hooks": { "PreToolUse": [ { "matcher": "Bash",
//	    "hooks": [ { "type": "command", "command": "lidwake acquire … --if-background --ttl …", "_lidwake": true } ] } ] } }
type backgroundBashHookShape struct {
	configPath string
	// matcher narrows the hook to the shell tool (Claude Code matches it against tool_name).
	matcher string
	// command is the `acquire … --if-background --ttl …` the handler runs.
	command string
}

// backgroundBashEvent is the single event this shape owns.
const backgroundBashEvent = "PreToolUse"

// backgroundBashTTLSeconds is the --ttl the installed hook requests: the 24 h ceiling of a
// background-shell hold, which the daemon clamps down to the live manualHoldMaxHours. It is the
// same default the CLI applies when the hook carries no --ttl, so the two can't drift apart.
const backgroundBashTTLSeconds = int(activity.BackgroundDefaultTTL / time.Second)

func (s backgroundBashHookShape) install(dryRun bool) (Result, error) {
	existing, err := readJSONForUpdate(s.configPath)
	if err != nil {
		return Result{}, err
	}
	before := orEmpty(existing)
	after := deepCopyObject(before)
	hooks := objectField(after, "hooks")
	mergeTaggedHandler(hooks, backgroundBashEvent, s.command, s.matcher, groupHoldsTaggedHandler)
	after["hooks"] = hooks

	diff := makeDiff(before, after)
	if !dryRun {
		if err := saveJSON(after, s.configPath, existing, diff); err != nil {
			return Result{}, err
		}
	}
	return Result{
		Summary: "wired " + backgroundBashEvent + "(" + s.matcher + ") background-shell hook",
		Diff:    diff,
	}, nil
}

func (s backgroundBashHookShape) uninstall(dryRun bool) (Result, error) {
	existing, err := readJSONForUpdate(s.configPath)
	if err != nil {
		return Result{}, err
	}
	if _, ok := existing["hooks"].(map[string]any); !ok {
		return nothingRemoved(), nil
	}
	after := deepCopyObject(existing)
	hooks := objectField(after, "hooks")
	stripTaggedHandlers(hooks, backgroundBashEvent, groupHoldsTaggedHandler)
	after["hooks"] = hooks
	diff := makeDiff(existing, after)
	if !dryRun {
		if err := saveJSON(after, s.configPath, existing, diff); err != nil {
			return Result{}, err
		}
	}
	return Result{Summary: "removed background-shell hook", Diff: diff}, nil
}

func (s backgroundBashHookShape) state() InstallState {
	obj, status := readConfig(s.configPath)
	switch status {
	case readMissing:
		return StateNotInstalled
	case readUnparseable:
		return StateConfigUnreadable
	}
	hooks, ok := obj["hooks"].(map[string]any)
	if !ok {
		return StateNotInstalled
	}
	installed, ok := taggedCommand(hooks[backgroundBashEvent])
	if !ok {
		return StateNotInstalled
	}
	if installed == s.command {
		return StateInstalled
	}
	return StateModifiedExternally
}

// groupHoldsTaggedHandler: the group's inner hooks array holds one of our handlers. Unlike the
// core shape there is no legacy flat-entry form to recognize.
func groupHoldsTaggedHandler(group map[string]any) bool {
	inner, ok := group["hooks"].([]any)
	return ok && anyObject(inner, taggedOrInvokesCLI)
}
