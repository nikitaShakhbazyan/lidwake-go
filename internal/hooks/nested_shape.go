package hooks

import "strconv"

// nestedJSONHookShape is the hook layout shared by Claude Code and Gemini CLI: one JSON file with
// a top-level "hooks" object keyed by event name, each value an array of groups that wrap an inner
// "hooks" array of {"type": "command", "command": …} handlers.
//
//	{ "hooks": { "SessionStart": [ { "hooks": [ { "type": "command", "command": "lidwake acquire …" } ] } ] } }
//
// lidwake's handlers are tagged "_lidwake": true, so install is self-healing (a stale handler is
// replaced in place) and uninstall removes only ours. Repairs operate on the inner handler, never
// the whole group, so user handlers sharing a group with ours survive every install and uninstall.
type nestedJSONHookShape struct {
	configPath string
	startEvent string
	// endEvent is the release event, or "" to release via the daemon's process-exit watcher.
	endEvent       string
	acquireCommand string
	releaseCommand string
	// obsoleteEvents are events this integration used to wire but no longer does. Install strips
	// any lidwake handler from them so an upgrade self-heals: a lingering session-scoped acquire
	// would otherwise bring back the whole-session hold.
	obsoleteEvents []string
	// extraHandlers are hooks beyond the core acquire/release pair, installed, uninstalled and
	// verified exactly like the pair.
	extraHandlers []extraHandler
}

// extraHandler is a hook on its own event carrying its own command, optionally narrowed by a
// matcher. Uses: Claude Code's Notification matched to idle_prompt (a release after an
// Esc-interrupt skipped Stop), SubagentStart/SubagentStop with the --subagent commands (a hold
// keyed on the sub-agent's own agent_id that outlives the parent turn's Stop), and the
// SessionEnd/SessionStart(clear) pair around in-process session retirement.
type extraHandler struct {
	event   string
	command string
	matcher string // "" = no matcher
}

func (s nestedJSONHookShape) managedEvents() map[string]bool {
	m := map[string]bool{s.startEvent: true}
	if s.endEvent != "" {
		m[s.endEvent] = true
	}
	for _, x := range s.extraHandlers {
		m[x.event] = true
	}
	return m
}

func (s nestedJSONHookShape) install(dryRun bool) (Result, error) {
	existing, err := readJSONForUpdate(s.configPath)
	if err != nil {
		return Result{}, err
	}
	before := orEmpty(existing)
	after := deepCopyObject(before)
	hooks := objectField(after, "hooks")
	mergeTaggedHandler(hooks, s.startEvent, s.acquireCommand, "", nestedEntryIsOurs)
	if s.endEvent != "" {
		mergeTaggedHandler(hooks, s.endEvent, s.releaseCommand, "", nestedEntryIsOurs)
	}
	for _, x := range s.extraHandlers {
		mergeTaggedHandler(hooks, x.event, x.command, x.matcher, nestedEntryIsOurs)
	}
	managed := s.managedEvents()
	for _, ev := range s.obsoleteEvents {
		if !managed[ev] {
			stripTaggedHandlers(hooks, ev, nestedEntryIsOurs)
		}
	}
	after["hooks"] = hooks

	diff := makeDiff(before, after)
	if !dryRun {
		if err := saveJSON(after, s.configPath, existing, diff); err != nil {
			return Result{}, err
		}
	}
	var summary string
	if s.endEvent == "" {
		summary = "wired " + s.startEvent + " hook (release via process-exit watcher)"
	} else {
		summary = "wired " + s.startEvent + " acquire + " + s.endEvent + " release hooks"
	}
	if n := len(s.extraHandlers); n > 0 {
		summary += " (+ " + strconv.Itoa(n) + " lifecycle hook" + plural(n) + ")"
	}
	return Result{Summary: summary, Diff: diff}, nil
}

func (s nestedJSONHookShape) uninstall(dryRun bool) (Result, error) {
	existing, err := readJSONForUpdate(s.configPath)
	if err != nil {
		return Result{}, err
	}
	if _, ok := existing["hooks"].(map[string]any); !ok {
		return nothingRemoved(), nil
	}
	after := deepCopyObject(existing)
	hooks := objectField(after, "hooks")
	// Clean our handlers from every event — including one the user moved ours to — not just the
	// ones we manage.
	for ev := range hooks {
		stripTaggedHandlers(hooks, ev, nestedEntryIsOurs)
	}
	after["hooks"] = hooks
	diff := makeDiff(existing, after)
	if !dryRun {
		if err := saveJSON(after, s.configPath, existing, diff); err != nil {
			return Result{}, err
		}
	}
	return Result{Summary: "removed hook entries", Diff: diff}, nil
}

func (s nestedJSONHookShape) state() InstallState {
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

	// Any lidwake entry anywhere — under a managed event, an obsolete one, or one the user moved
	// it to — means our hooks are (partly) present. A partial or drifted set must never read as
	// not installed: live entries would be invisible, with no way offered to remove them.
	anyOurs := false
	for _, v := range hooks {
		if arr, ok := v.([]any); ok && anyObject(arr, nestedEntryIsOurs) {
			anyOurs = true
			break
		}
	}

	installedAcquire, ok := taggedCommand(hooks[s.startEvent])
	if !ok {
		if anyOurs {
			return StateModifiedExternally
		}
		return StateNotInstalled
	}

	// A leftover handler under an event we migrated away from is a stale, mixed state; reporting
	// it as modified nudges the reinstall that strips it.
	hasObsolete := false
	for _, ev := range s.obsoleteEvents {
		if _, ok := taggedCommand(hooks[ev]); ok {
			hasObsolete = true
			break
		}
	}

	// Every extra hook must carry its canonical command, or the install is partial (an upgrade
	// from a build that predated it).
	extrasInstalled := true
	for _, x := range s.extraHandlers {
		if cmd, ok := taggedCommand(hooks[x.event]); !ok || cmd != x.command {
			extrasInstalled = false
			break
		}
	}

	if s.endEvent == "" {
		if installedAcquire == s.acquireCommand && extrasInstalled && !hasObsolete {
			return StateInstalled
		}
		return StateModifiedExternally
	}
	installedRelease, ok := taggedCommand(hooks[s.endEvent])
	if !ok {
		return StateModifiedExternally
	}
	if installedAcquire == s.acquireCommand && installedRelease == s.releaseCommand &&
		extrasInstalled && !hasObsolete {
		return StateInstalled
	}
	return StateModifiedExternally
}

// nestedEntryIsOurs: a group holding one of our handlers, or a legacy flat entry whose own
// command calls the CLI.
func nestedEntryIsOurs(entry map[string]any) bool {
	if inner, ok := entry["hooks"].([]any); ok {
		return anyObject(inner, taggedOrInvokesCLI)
	}
	cmd, _ := stringField(entry, "command")
	return commandInvokesLidwakeCLI(cmd)
}

// mergeTaggedHandler inserts (or repairs) our tagged handler under event. Idempotent: an existing
// lidwake handler is replaced with the canonical form in place — only the handler, so user
// handlers sharing the group survive — and re-running install upgrades a stale command. A matcher
// ("" for none) is set on our group. Entries that aren't ours are untouched.
func mergeTaggedHandler(hooks map[string]any, event, command, matcher string, groupIsOurs func(map[string]any) bool) {
	arr, _ := hooks[event].([]any)
	canonical := map[string]any{"type": "command", "command": command, lidwakeTag: true}
	if g := firstObject(arr, groupIsOurs); g >= 0 {
		group := arr[g].(map[string]any)
		inner, _ := group["hooks"].([]any)
		if h := firstObject(inner, taggedOrInvokesCLI); h >= 0 {
			inner[h] = canonical
		} else {
			inner = append(inner, canonical)
		}
		group["hooks"] = inner
		if matcher != "" {
			group["matcher"] = matcher
		}
	} else {
		group := map[string]any{"hooks": []any{canonical}}
		if matcher != "" {
			group["matcher"] = matcher
		}
		arr = append(arr, group)
	}
	hooks[event] = arr
}

// stripTaggedHandlers removes our handlers from one event, leaving the user's hooks untouched: a
// group is dropped only once it holds no other handlers, and the event key once it holds nothing,
// so no `"<event>": []` residue is left.
func stripTaggedHandlers(hooks map[string]any, event string, groupIsOurs func(map[string]any) bool) {
	arr, ok := hooks[event].([]any)
	if !ok {
		return
	}
	pruned := make([]any, 0, len(arr))
	for _, v := range arr {
		group, ok := v.(map[string]any)
		if !ok || !groupIsOurs(group) {
			pruned = append(pruned, v)
			continue
		}
		inner, _ := group["hooks"].([]any)
		inner = withoutObjects(inner, taggedOrInvokesCLI)
		if len(inner) == 0 {
			continue
		}
		group["hooks"] = inner
		pruned = append(pruned, group)
	}
	if len(pruned) == 0 {
		delete(hooks, event)
	} else {
		hooks[event] = pruned
	}
}

// taggedCommand is the command of the first lidwake handler found in an event's groups.
func taggedCommand(event any) (string, bool) {
	arr, _ := event.([]any)
	for _, v := range arr {
		group, ok := v.(map[string]any)
		if !ok {
			continue
		}
		inner, ok := group["hooks"].([]any)
		if !ok {
			continue
		}
		if h := firstObject(inner, taggedOrInvokesCLI); h >= 0 {
			if cmd, ok := stringField(inner[h].(map[string]any), "command"); ok {
				return cmd, true
			}
		}
	}
	return "", false
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
