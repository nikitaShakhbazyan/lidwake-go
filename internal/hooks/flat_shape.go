package hooks

// flatJSONHookShape is a flatter layout than nestedJSONHookShape: each event maps to an array of
// {"command": …, "_lidwake": true} objects directly, with no inner "hooks" wrapper.
//
//	{ "hooks": { "beforeSubmitPrompt": [ { "command": "lidwake acquire …", "_lidwake": true } ] } }
//
// Used by Cursor. The integration supplies the (event, command) pairs it manages and an optional
// seed document for a fresh file.
type flatJSONHookShape struct {
	configPath string
	// entries are the events and commands this integration owns, in write order.
	entries []eventCommand
	// baseDocument seeds the file when it doesn't exist yet.
	baseDocument     map[string]any
	installSummary   string
	uninstallSummary string
}

// eventCommand is one managed (event, command) pair.
type eventCommand struct {
	event   string
	command string
}

func (s flatJSONHookShape) install(dryRun bool) (Result, error) {
	existing, err := readJSONForUpdate(s.configPath)
	if err != nil {
		return Result{}, err
	}
	before := orEmpty(existing)
	var after map[string]any
	if existing != nil {
		after = deepCopyObject(existing)
	} else {
		after = orEmpty(deepCopyObject(s.baseDocument))
	}
	hooks := objectField(after, "hooks")
	// Canonicalize, don't just merge: strip our entries from every event first, so one left under
	// an event this build no longer uses (an older session-scoped shape) can't survive the
	// reinstall and keep firing.
	removeFlatEntries(hooks)
	for _, e := range s.entries {
		arr, _ := hooks[e.event].([]any)
		canonical := map[string]any{"command": e.command, lidwakeTag: true}
		if i := firstObject(arr, taggedOrInvokesCLI); i >= 0 {
			arr[i] = canonical
		} else {
			arr = append(arr, canonical)
		}
		hooks[e.event] = arr
	}
	after["hooks"] = hooks

	diff := makeDiff(before, after)
	if !dryRun {
		if err := saveJSON(after, s.configPath, existing, diff); err != nil {
			return Result{}, err
		}
	}
	return Result{Summary: s.installSummary, Diff: diff}, nil
}

func (s flatJSONHookShape) uninstall(dryRun bool) (Result, error) {
	existing, err := readJSONForUpdate(s.configPath)
	if err != nil {
		return Result{}, err
	}
	if _, ok := existing["hooks"].(map[string]any); !ok {
		return nothingRemoved(), nil
	}
	after := deepCopyObject(existing)
	hooks := objectField(after, "hooks")
	removeFlatEntries(hooks)
	after["hooks"] = hooks
	diff := makeDiff(existing, after)
	if !dryRun {
		if err := saveJSON(after, s.configPath, existing, diff); err != nil {
			return Result{}, err
		}
	}
	return Result{Summary: s.uninstallSummary, Diff: diff}, nil
}

func (s flatJSONHookShape) state() InstallState {
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
	// Any lidwake entry anywhere means our hooks are (partly) present; a partial set must read as
	// drifted, not as not installed, so it can be cleaned up.
	anyOurs := false
	for _, v := range hooks {
		if arr, ok := v.([]any); ok && anyObject(arr, taggedOrInvokesCLI) {
			anyOurs = true
			break
		}
	}
	for _, e := range s.entries {
		arr, _ := hooks[e.event].([]any)
		installed, ok := "", false
		if i := firstObject(arr, taggedOrInvokesCLI); i >= 0 {
			installed, ok = stringField(arr[i].(map[string]any), "command")
		}
		if !ok {
			if anyOurs {
				return StateModifiedExternally
			}
			return StateNotInstalled
		}
		if installed != e.command {
			return StateModifiedExternally
		}
	}
	return StateInstalled
}

// removeFlatEntries strips our entries from every event — including one the user moved them to,
// or one an older build wrote — and drops emptied event arrays so no `"<event>": []` residue is
// left.
func removeFlatEntries(hooks map[string]any) {
	for ev, v := range hooks {
		arr, ok := v.([]any)
		if !ok {
			continue
		}
		pruned := withoutObjects(arr, taggedOrInvokesCLI)
		if len(pruned) == 0 {
			delete(hooks, ev)
		} else {
			hooks[ev] = pruned
		}
	}
}
