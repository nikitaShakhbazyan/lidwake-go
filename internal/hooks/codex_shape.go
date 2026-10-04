package hooks

import "strconv"

// codexHookShape writes Codex's hooks.json: each event maps to an array of matcher groups, each
// wrapping an inner "hooks" array of {"type": "command", "command": …} handlers — the same nested
// layout Claude Code uses. Codex silently ignores a flat (no-wrapper) array: /hooks lists nothing.
// No matcher is written: it narrows which tool a Pre/PostToolUse hook fires for, and these
// lifecycle events have no tool.
//
//	{ "hooks": { "UserPromptSubmit": [ { "hooks": [ { "type": "command", "command": "lidwake acquire …" } ] } ],
//	             "Stop":             [ { "hooks": [ { "type": "command", "command": "lidwake release …" } ] } ] } }
//
// Acquire on UserPromptSubmit, release on Stop: Codex's Stop fires at turn completion, when the
// model is done and control returns to the user, and carries the same session_id on stdin that
// acquire keys on — so each turn is bracketed exactly like Claude Code's. An Esc-interrupt is the
// one turn end that skips Stop; the daemon's CPU-idle sweep and process-exit watcher cover it.
//
// Two Codex constraints shape this type:
//
//  1. No marker key. Codex deserializes handlers strictly, so an extra "_lidwake" field risks an
//     "unexpected key" rejection. Our handlers are recognized by their command calling the CLI.
//  2. Preserve trust. Trust isn't stored in hooks.json: when the user trusts our hooks via /hooks,
//     Codex records a trusted_hash in config.toml keyed per handler by its position and a hash of
//     its command. Re-installing must keep each handler at a stable index with an unchanged
//     command, or the user must approve again. So a correct handler is left untouched (sibling
//     keys included), only a drifted command is rewritten, and a fresh group is appended when ours
//     is absent. Each event is trusted on its own, so adding a handler asks for approval of only
//     that one.
type codexHookShape struct {
	configPath     string
	acquireEvent   string
	acquireCommand string
	// releaseEvent and releaseCommand are set together, or both "" to release solely via the
	// daemon's process-exit and CPU-idle nets.
	releaseEvent   string
	releaseCommand string
	// obsoleteEvents are events lidwake used to wire but no longer does (SessionStart, which a
	// resume skips). Install strips our handlers from them so an upgrade self-heals.
	obsoleteEvents []string
	// extraHandlers beyond the core pair: the SubagentStart → acquire --subagent and
	// SubagentStop → release --subagent hooks keep the Mac awake for a backgrounded sub-agent that
	// outlives the parent turn's Stop (Codex carries the sub-agent's id in agent_id).
	extraHandlers []eventCommand
}

// managedHandlers are the handlers we own, in install order.
func (s codexHookShape) managedHandlers() []eventCommand {
	out := []eventCommand{{s.acquireEvent, s.acquireCommand}}
	if s.releaseEvent != "" && s.releaseCommand != "" {
		out = append(out, eventCommand{s.releaseEvent, s.releaseCommand})
	}
	return append(out, s.extraHandlers...)
}

func (s codexHookShape) managedEvents() map[string]bool {
	m := map[string]bool{}
	for _, h := range s.managedHandlers() {
		m[h.event] = true
	}
	return m
}

func (s codexHookShape) install(dryRun bool) (Result, error) {
	existing, err := readJSONForUpdate(s.configPath)
	if err != nil {
		return Result{}, err
	}
	before := orEmpty(existing)
	after := deepCopyObject(before)
	hooks := objectField(after, "hooks")
	handlers := s.managedHandlers()
	for _, h := range handlers {
		mergeCodexHandler(hooks, h.event, h.command)
	}
	managed := s.managedEvents()
	for _, ev := range s.obsoleteEvents {
		if !managed[ev] {
			stripCodexHandlers(hooks, ev)
		}
	}
	after["hooks"] = hooks

	diff := makeDiff(before, after)
	if !dryRun {
		if err := saveJSON(after, s.configPath, existing, diff); err != nil {
			return Result{}, err
		}
	}
	var wired string
	if s.releaseEvent != "" {
		wired = s.acquireEvent + " acquire + " + s.releaseEvent + " release hooks"
	} else {
		wired = s.acquireEvent + " hook (release via process-exit watcher)"
	}
	if n := len(s.extraHandlers); n > 0 {
		wired += " plus " + strconv.Itoa(n) + " sub-agent hook" + plural(n)
	}
	// Trust is per handler in Codex, so each hook we add is one more one-time /hooks approval.
	trustNote := "trust it in Codex with /hooks"
	if len(handlers) > 1 {
		trustNote = "trust each in Codex with /hooks"
	}
	return Result{Summary: "wired " + wired + "; " + trustNote, Diff: diff}, nil
}

func (s codexHookShape) uninstall(dryRun bool) (Result, error) {
	existing, err := readJSONForUpdate(s.configPath)
	if err != nil {
		return Result{}, err
	}
	if _, ok := existing["hooks"].(map[string]any); !ok {
		return nothingRemoved(), nil
	}
	after := deepCopyObject(existing)
	hooks := objectField(after, "hooks")
	// Strip our handlers from every event — including one the user moved them to.
	for ev := range hooks {
		stripCodexHandlers(hooks, ev)
	}
	after["hooks"] = hooks
	diff := makeDiff(existing, after)
	if !dryRun {
		if err := saveJSON(after, s.configPath, existing, diff); err != nil {
			return Result{}, err
		}
	}
	return Result{Summary: "removed Codex hook entries", Diff: diff}, nil
}

func (s codexHookShape) state() InstallState {
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
	// Any of our handlers anywhere — under an obsolete event or one the user moved it to — is a
	// partial install, never not installed.
	anyOurs := false
	for _, v := range hooks {
		if groups, ok := v.([]any); ok && anyObject(groups, codexGroupIsOurs) {
			anyOurs = true
			break
		}
	}
	// Every managed handler must be present with its canonical command; a missing or drifted one
	// (an upgrade from a build that predated it) is a partial install.
	for _, h := range s.managedHandlers() {
		if cmd, ok := codexOurCommand(hooks, h.event); !ok || cmd != h.command {
			if anyOurs {
				return StateModifiedExternally
			}
			return StateNotInstalled
		}
	}
	managed := s.managedEvents()
	for _, ev := range s.obsoleteEvents {
		if groups, ok := hooks[ev].([]any); ok && !managed[ev] && anyObject(groups, codexGroupIsOurs) {
			return StateModifiedExternally
		}
	}
	// Trust is the user's step via /hooks, not something we apply, so present-and-correct handlers
	// read as installed whatever the trust state.
	return StateInstalled
}

// mergeCodexHandler inserts (or repairs) our handler under event while preserving trust: a correct
// handler is left untouched so the config.toml trust hash keeps matching; only a drifted command
// is rewritten (keeping sibling keys Codex added); a fresh group is appended when ours is absent.
func mergeCodexHandler(hooks map[string]any, event, command string) {
	groups, _ := hooks[event].([]any)
	if g := firstObject(groups, codexGroupIsOurs); g >= 0 {
		group := groups[g].(map[string]any)
		handlers, _ := group["hooks"].([]any)
		if h := firstObject(handlers, codexHandlerIsOurs); h >= 0 {
			handler := handlers[h].(map[string]any)
			if cmd, _ := stringField(handler, "command"); cmd != command {
				handler["type"] = "command"
				handler["command"] = command
			}
		} else {
			group["hooks"] = append(handlers, codexHandler(command))
		}
	} else {
		groups = append(groups, map[string]any{"hooks": []any{codexHandler(command)}})
	}
	hooks[event] = groups
}

// stripCodexHandlers removes our handlers from every group under event, dropping a group left
// with no handlers but keeping groups that still hold the user's hooks, and dropping the event key
// once nothing is left so no `"<event>": []` residue remains.
func stripCodexHandlers(hooks map[string]any, event string) {
	groups, ok := hooks[event].([]any)
	if !ok {
		return
	}
	pruned := make([]any, 0, len(groups))
	for _, v := range groups {
		group, ok := v.(map[string]any)
		if !ok || !codexGroupIsOurs(group) {
			pruned = append(pruned, v)
			continue
		}
		handlers, _ := group["hooks"].([]any)
		handlers = withoutObjects(handlers, codexHandlerIsOurs)
		if len(handlers) == 0 {
			continue
		}
		group["hooks"] = handlers
		pruned = append(pruned, group)
	}
	if len(pruned) == 0 {
		delete(hooks, event)
	} else {
		hooks[event] = pruned
	}
}

// codexOurCommand is the command of our handler under event.
func codexOurCommand(hooks map[string]any, event string) (string, bool) {
	groups, _ := hooks[event].([]any)
	g := firstObject(groups, codexGroupIsOurs)
	if g < 0 {
		return "", false
	}
	handlers, _ := groups[g].(map[string]any)["hooks"].([]any)
	h := firstObject(handlers, codexHandlerIsOurs)
	if h < 0 {
		return "", false
	}
	return stringField(handlers[h].(map[string]any), "command")
}

func codexHandler(command string) map[string]any {
	return map[string]any{"type": "command", "command": command}
}

// codexGroupIsOurs: the group's inner hooks array holds a handler calling our CLI.
func codexGroupIsOurs(group map[string]any) bool {
	handlers, ok := group["hooks"].([]any)
	return ok && anyObject(handlers, codexHandlerIsOurs)
}

func codexHandlerIsOurs(handler map[string]any) bool {
	cmd, _ := stringField(handler, "command")
	return commandInvokesLidwakeCLI(cmd)
}
