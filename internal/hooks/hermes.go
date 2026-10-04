package hooks

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
)

const agentHermes = "hermes"

// hermesIntegration: a shell hook declared in ~/.hermes/config.yaml under a top-level hooks: map,
// plus an approval in ~/.hermes/shell-hooks-allowlist.json.
//
// Of Hermes's three hook systems, shell hooks are the right fit (verified on a real install):
// declared in config.yaml, they run in both the CLI and the gateway and pipe a JSON payload to the
// command's stdin. Each command must be allowlisted (first-use consent), matched by the exact
// (event, command) pair; without that the hook is silently skipped. The other two systems — Python
// plugins and the gateway-only HOOK.yaml — don't fit.
//
// Hermes is a 24/7 gateway: one shared process multiplexes every session, and its session hooks
// are asymmetric — on_session_start fires once per new conversation (not on continuation) while
// on_session_end fires at the end of every turn. A plain start→acquire / end→release pair would
// protect only a session's first turn. The gateway is therefore treated as one activity unit:
// acquire on both on_session_start and pre_gateway_dispatch (which fires once per incoming message,
// so a turn arriving after the hold was released is protected again), coalesced onto a fixed
// hermes:gateway hold carrying the gateway PID. Release on on_session_end is the fast path back to
// sleep; the daemon's CPU-idle and dead-process nets on the gateway tree are what make a missed or
// asymmetric end hook safe.
//
// The YAML is changed with line-scoped string surgery, never a parse/serialize round trip (which
// would reorder and reformat the user's whole file). Every removal is recognizer-based, so user
// content survives even a damaged marker block.
type hermesIntegration struct{}

func init() { register(hermesIntegration{}) }

const (
	hermesMarkerStart = "  # >>> lidwake (managed)"
	hermesMarkerEnd   = "  # <<< lidwake"
)

// hermesManagedEventKeys are the event keys our block declares, used to recognize our own lines
// during removal.
var hermesManagedEventKeys = []string{"on_session_start:", "pre_gateway_dispatch:", "on_session_end:"}

func (hermesIntegration) agent() string { return agentHermes }

func (hermesIntegration) detected(ctx hookContext) bool { return exists(ctx.homePath(".hermes")) }

func (hermesIntegration) configPath(ctx hookContext) string {
	return ctx.homePath(".hermes", "config.yaml")
}

func hermesAllowlistPath(ctx hookContext) string {
	return ctx.homePath(".hermes", "shell-hooks-allowlist.json")
}

// hermesCommand is the command string after YAML decoding — also the exact string stored in the
// allowlist. The CLI path is shell-quoted so Hermes's shlex.split keeps a spaced path as one
// argument; the session id comes from the hook's stdin session_id, so no positional key is needed.
func hermesCommand(op, cliPath string) string {
	cli := "'" + cliPath + "'"
	if strings.Contains(cliPath, "'") {
		cli = `"` + cliPath + `"`
	}
	return cli + " " + op + " --tool hermes"
}

// yamlEscaper escapes a string for the inside of a YAML double-quoted scalar.
var yamlEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`)

// hermesYAMLCommand is hermesCommand as it appears inside the double-quoted YAML scalar.
func hermesYAMLCommand(op, cliPath string) string {
	return yamlEscaper.Replace(hermesCommand(op, cliPath))
}

// hermesHookBlock is the hooks: block we manage, bracketed by comment markers for clean removal.
// Acquire is wired on both on_session_start (new conversation) and pre_gateway_dispatch (every
// incoming gateway message), so a multi-turn session stays protected; release on on_session_end.
func hermesHookBlock(cliPath string) string {
	acquire := hermesYAMLCommand("acquire", cliPath)
	release := hermesYAMLCommand("release", cliPath)
	return "hooks:\n" +
		hermesMarkerStart + "\n" +
		"  on_session_start:\n" +
		"    - command: \"" + acquire + "\"\n" +
		"  pre_gateway_dispatch:\n" +
		"    - command: \"" + acquire + "\"\n" +
		"  on_session_end:\n" +
		"    - command: \"" + release + "\"\n" +
		hermesMarkerEnd
}

// hermesApproval is one allowlist entry: {"event": …, "command": …}.
type hermesApproval struct{ event, command string }

func hermesApprovals(cliPath string) []hermesApproval {
	acquire, release := hermesCommand("acquire", cliPath), hermesCommand("release", cliPath)
	return []hermesApproval{
		{"on_session_start", acquire},
		{"pre_gateway_dispatch", acquire},
		{"on_session_end", release},
	}
}

func (i hermesIntegration) install(ctx hookContext, dryRun bool) (Result, error) {
	cfgPath := i.configPath(ctx)
	existing, err := readText(cfgPath)
	if err != nil {
		return Result{}, err
	}
	summary := "wired Hermes on_session_start/on_session_end shell hooks"
	var diff strings.Builder
	var updated string
	writeYAML, wroteHooks := false, false

	active := hermesActiveLines(existing)
	hasAcquire := containsLine(active, hermesYAMLCommand("acquire", ctx.cliPath))
	hasRelease := containsLine(active, hermesYAMLCommand("release", ctx.cliPath))
	if strings.Contains(existing, hermesMarkerStart) && hasAcquire && hasRelease {
		summary = "already installed"
		wroteHooks = true // The approvals may still need a top-up (a wiped allowlist).
	} else {
		// A stale or partial block (the binary moved, so the embedded CLI path drifted; a deleted
		// line) is removed first, then reinstalled fresh through the same paths as a clean config.
		working := existing
		if strings.Contains(existing, hermesMarkerStart) {
			working = hermesRemoveManagedBlock(existing)
		}
		lines := strings.Split(working, "\n")
		if idx := slices.Index(lines, "hooks: {}"); idx >= 0 {
			// Fresh or default config: replace the empty top-level hooks map with our block.
			// Exact-line match only — a nested `    hooks: {}` or a `python_hooks: {}` must never
			// be rewritten.
			lines[idx] = hermesHookBlock(ctx.cliPath)
			updated = strings.Join(lines, "\n")
			fmt.Fprintf(&diff, "~ %s: set hooks.on_session_start/on_session_end\n", cfgPath)
			writeYAML, wroteHooks = true, true
		} else if !slices.ContainsFunc(lines, func(l string) bool { return strings.HasPrefix(l, "hooks:") }) {
			// No top-level hooks map yet: append one.
			if working != "" {
				updated = working + "\n"
			}
			updated += hermesHookBlock(ctx.cliPath) + "\n"
			fmt.Fprintf(&diff, "+ %s: hooks block\n", cfgPath)
			writeYAML, wroteHooks = true, true
		} else {
			// A populated hooks: map — a blind YAML merge isn't worth the risk.
			summary = "Hermes config already has a hooks: section — add on_session_start/on_session_end manually (see docs)"
		}
	}

	// Approvals are written only alongside hooks that exist: an approval for a command that isn't
	// in config.yaml is dead weight that survives uninstalls confusingly. The allowlist is read
	// before config.yaml is touched, so an unreadable allowlist refuses the install instead of
	// wiring hooks Hermes would silently skip.
	var allowBefore, allowAfter map[string]any
	allowDiff := unchangedDiff
	if wroteHooks {
		if allowBefore, err = readJSONForUpdate(hermesAllowlistPath(ctx)); err != nil {
			return Result{}, err
		}
		allowAfter = hermesReconciled(allowBefore, hermesApprovals(ctx.cliPath))
		if allowDiff = makeDiff(allowBefore, allowAfter); allowDiff != unchangedDiff {
			fmt.Fprintf(&diff, "~ %s: approve acquire/release", hermesAllowlistPath(ctx))
		}
	}

	if !dryRun {
		if writeYAML {
			if err := ensureParentDir(cfgPath); err != nil {
				return Result{}, err
			}
			if err := writeString(updated, cfgPath); err != nil {
				return Result{}, err
			}
		}
		if err := saveJSON(allowAfter, hermesAllowlistPath(ctx), allowBefore, allowDiff); err != nil {
			return Result{}, err
		}
	}
	if diff.Len() == 0 {
		return Result{Summary: summary, Diff: unchangedDiff}, nil
	}
	return Result{Summary: summary, Diff: diff.String()}, nil
}

func (i hermesIntegration) uninstall(ctx hookContext, dryRun bool) (Result, error) {
	cfgPath := i.configPath(ctx)
	var diff strings.Builder
	if data, err := os.ReadFile(cfgPath); err == nil && strings.Contains(string(data), hermesMarkerStart) {
		if !dryRun {
			if err := writeString(hermesRemoveManagedBlock(string(data)), cfgPath); err != nil {
				return Result{}, err
			}
		}
		fmt.Fprintf(&diff, "~ %s: removed lidwake hooks\n", cfgPath)
	}

	allowPath := hermesAllowlistPath(ctx)
	before, err := readJSONForUpdate(allowPath)
	if err != nil {
		return Result{}, err
	}
	if before != nil {
		after, changed := hermesRevoked(before)
		if changed {
			if !dryRun {
				if err := writeJSON(after, allowPath, before); err != nil {
					return Result{}, err
				}
			}
			fmt.Fprintf(&diff, "~ %s: revoke lidwake approvals", allowPath)
		}
	}

	if diff.Len() == 0 {
		return nothingRemoved(), nil
	}
	return Result{Summary: "removed Hermes hooks", Diff: diff.String()}, nil
}

func (i hermesIntegration) state(ctx hookContext) InstallState {
	data, err := os.ReadFile(i.configPath(ctx))
	if errors.Is(err, fs.ErrNotExist) {
		return StateNotInstalled
	}
	if err != nil {
		return StateConfigUnreadable
	}
	allow, status := readConfig(hermesAllowlistPath(ctx))
	if status == readUnparseable {
		return StateConfigUnreadable
	}

	text := string(data)
	active := hermesActiveLines(text)
	hasAcquire := containsLine(active, hermesYAMLCommand("acquire", ctx.cliPath))
	hasRelease := containsLine(active, hermesYAMLCommand("release", ctx.cliPath))
	if !hasAcquire && !hasRelease {
		// Our marker block, or any lidwake hook command (a stale CLI path after the binary moved),
		// means our wiring is present but broken — never notInstalled, which would offer to
		// connect while dead hooks linger.
		ours := strings.Contains(text, hermesMarkerStart) || slices.ContainsFunc(active, func(l string) bool {
			return commandInvokesLidwakeCLI(l) && strings.Contains(l, "--tool hermes")
		})
		if ours {
			return StateModifiedExternally
		}
		return StateNotInstalled
	}
	// Installed only when both commands are present and every (event, command) pair is
	// allowlisted; an unapproved hook is silently skipped.
	entries := hermesApprovalEntries(allow)
	for _, a := range hermesApprovals(ctx.cliPath) {
		if !anyObject(entries, a.matches) {
			return StateModifiedExternally
		}
	}
	if hasAcquire && hasRelease {
		return StateInstalled
	}
	return StateModifiedExternally
}

// ---- YAML surgery ----

// hermesActiveLines are the lines Hermes actually evaluates: comment lines (which include our own
// markers) don't count when checking whether a hook command is wired.
func hermesActiveLines(text string) []string {
	var out []string
	for _, l := range strings.Split(text, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(l), "#") {
			out = append(out, l)
		}
	}
	return out
}

func containsLine(lines []string, sub string) bool {
	return slices.ContainsFunc(lines, func(l string) bool { return strings.Contains(l, sub) })
}

// hermesRemoveManagedBlock removes our managed block. It is bounded by both markers when they are
// intact; when the end marker was deleted, only lines recognizably ours (our event keys, our
// command lines, blank lines) are removed, and removal stops at the first foreign line — the
// user's YAML after a damaged block is never swallowed. A top-level `hooks:` line left childless
// (its next non-blank line isn't indented) is restored to `hooks: {}`; a populated hooks map is
// never touched.
func hermesRemoveManagedBlock(text string) string {
	lines := strings.Split(text, "\n")
	kept := make([]string, 0, len(lines))
	inBlock := false
	for _, line := range lines {
		if strings.Contains(line, hermesMarkerStart) {
			inBlock = true
			continue
		}
		if strings.Contains(line, hermesMarkerEnd) {
			inBlock = false
			continue
		}
		if inBlock {
			trimmed := strings.TrimSpace(line)
			ours := trimmed == "" ||
				slices.ContainsFunc(hermesManagedEventKeys, func(k string) bool { return strings.HasPrefix(trimmed, k) }) ||
				(strings.HasPrefix(trimmed, "- command:") && strings.Contains(trimmed, "lidwake"))
			if ours {
				continue
			}
			inBlock = false
		}
		kept = append(kept, line)
	}
	for i, line := range kept {
		if line != "hooks:" {
			continue
		}
		next := ""
		for _, l := range kept[i+1:] {
			if strings.TrimSpace(l) != "" {
				next = l
				break
			}
		}
		if next == "" || !strings.HasPrefix(next, " ") {
			kept[i] = "hooks: {}"
		}
	}
	return strings.Join(kept, "\n")
}

// ---- Allowlist: {"approvals": [{"event": …, "command": …}, …]} ----
//
// Hermes owns this file and may add top-level keys beyond approvals; the whole object is
// round-tripped so they survive.

func (a hermesApproval) matches(entry map[string]any) bool {
	event, _ := stringField(entry, "event")
	command, _ := stringField(entry, "command")
	return event == a.event && command == a.command
}

// hermesApprovalEntries is the approvals array (empty when missing or not an array).
func hermesApprovalEntries(doc map[string]any) []any {
	if arr, ok := doc["approvals"].([]any); ok {
		return arr
	}
	return []any{}
}

// isOurHermesApproval reports whether an allowlist entry approves a lidwake hermes command (with
// any CLI path, current or stale).
func isOurHermesApproval(entry map[string]any) bool {
	cmd, ok := stringField(entry, "command")
	return ok && commandInvokesLidwakeCLI(cmd) && strings.Contains(cmd, "--tool hermes")
}

// hermesReconciled adds approvals for the current commands and prunes ours that no longer match
// (a stale CLI path), leaving the user's own approvals untouched.
func hermesReconciled(before map[string]any, wanted []hermesApproval) map[string]any {
	doc := orEmpty(deepCopyObject(before))
	entries := withoutObjects(hermesApprovalEntries(doc), func(e map[string]any) bool {
		return isOurHermesApproval(e) && !slices.ContainsFunc(wanted, func(a hermesApproval) bool { return a.matches(e) })
	})
	for _, a := range wanted {
		if !anyObject(entries, a.matches) {
			entries = append(entries, map[string]any{"event": a.event, "command": a.command})
		}
	}
	doc["approvals"] = entries
	return doc
}

// hermesRevoked removes every lidwake hermes approval; changed is false when there was none.
func hermesRevoked(before map[string]any) (after map[string]any, changed bool) {
	entries := hermesApprovalEntries(before)
	kept := withoutObjects(entries, isOurHermesApproval)
	if len(kept) == len(entries) {
		return before, false
	}
	after = deepCopyObject(before)
	after["approvals"] = kept
	return after, true
}
