package hooks

import (
	"path/filepath"
	"strconv"
	"testing"
)

// allAgentNames is every agent name, registered here or not: capability checks must say no for
// any agent without the capability.
var allAgentNames = []string{
	"claude-code", "codex", "cursor", "gemini-cli", "aider", "cline", "hermes", "opencode", "pi",
}

// backgroundCommandIn is our single PreToolUse handler's command, "" when absent.
func backgroundCommandIn(t *testing.T, home string) string {
	t.Helper()
	hooks := hooksIn(t, filepath.Join(home, ".claude", "settings.json"))
	groups, ok := hooks["PreToolUse"].([]any)
	if !ok {
		return ""
	}
	for _, g := range groups {
		inner, _ := g.(map[string]any)["hooks"].([]any)
		for _, h := range inner {
			if m, ok := h.(map[string]any); ok && isTagged(m) {
				cmd, _ := m["command"].(string)
				return cmd
			}
		}
	}
	return ""
}

// TestBackgroundBashHookShape covers the opt-in background-shell hook, driven through the
// installer's background API. Like MCP registration it is gated on the capability, not detection,
// so a bare temp home suffices; the coexistence cases create ~/.claude so the core install runs.
func TestBackgroundBashHookShape(t *testing.T) {
	settings := func(home string) string { return filepath.Join(home, ".claude", "settings.json") }

	// ---- Capability gating ----

	t.Run("supports background hold only for Claude Code", func(t *testing.T) {
		in := testInstaller(testCLI, fakeHome(t))
		if !in.SupportsBackgroundHold(agentClaudeCode) {
			t.Error("claude-code must support the background hold")
		}
		for _, agent := range allAgentNames[1:] {
			if in.SupportsBackgroundHold(agent) {
				t.Errorf("%s has no clean run_in_background signal", agent)
			}
		}
	})

	t.Run("install throws for an unsupported agent", func(t *testing.T) {
		_, err := testInstaller(testCLI, fakeHome(t)).InstallBackgroundHold(agentCodex, false)
		if s := requireSkip(t, err); s.Reason != SkipUnsupported {
			t.Errorf("reason = %v", s.Reason)
		}
	})

	// ---- Install ----

	t.Run("install writes a PreToolUse Bash acquire --if-background hook", func(t *testing.T) {
		home := fakeHome(t)
		must[Result](t)(testInstaller(testCLI, home).InstallBackgroundHold(agentClaudeCode, false))

		group := objects(t, hooksIn(t, settings(home))["PreToolUse"], "PreToolUse")[0]
		if group["matcher"] != "Bash" {
			t.Error("the hook must be narrowed to the Bash tool")
		}
		want := "/usr/local/bin/lidwake acquire --tool claude-code --if-background --ttl " + strconv.Itoa(backgroundBashTTLSeconds)
		if got := backgroundCommandIn(t, home); got != want {
			t.Errorf("command = %q, want %q", got, want)
		}
		if backgroundBashTTLSeconds != 86400 {
			t.Errorf("TTL = %d, want the 24 h ceiling", backgroundBashTTLSeconds)
		}
	})

	t.Run("install preserves the user's other PreToolUse hooks", func(t *testing.T) {
		home := fakeHome(t, ".claude")
		writeJSONFile(t, map[string]any{
			"model": "opus",
			"hooks": map[string]any{"PreToolUse": []any{map[string]any{
				"matcher": "Write", "hooks": []any{map[string]any{"type": "command", "command": "my-linter"}},
			}}},
		}, settings(home))

		must[Result](t)(testInstaller(testCLI, home).InstallBackgroundHold(agentClaudeCode, false))

		dict := readJSONFile(t, settings(home))
		if dict["model"] != "opus" {
			t.Error("unrelated top-level keys must survive")
		}
		found := false
		for _, g := range objects(t, object(t, dict["hooks"], "hooks")["PreToolUse"], "PreToolUse") {
			if g["matcher"] == "Write" {
				found = true
			}
		}
		if !found {
			t.Error("the user's own PreToolUse hook must survive")
		}
		if backgroundCommandIn(t, home) == "" {
			t.Error("ours is added alongside")
		}
	})

	t.Run("install is idempotent", func(t *testing.T) {
		home := fakeHome(t)
		in := testInstaller(testCLI, home)
		must[Result](t)(in.InstallBackgroundHold(agentClaudeCode, false))
		must[Result](t)(in.InstallBackgroundHold(agentClaudeCode, false))

		ours := 0
		for _, g := range objects(t, hooksIn(t, settings(home))["PreToolUse"], "PreToolUse") {
			for _, h := range objects(t, g["hooks"], "inner") {
				if isTagged(h) {
					ours++
				}
			}
		}
		if ours != 1 {
			t.Errorf("re-install must not duplicate our group: %d", ours)
		}
		wantState(t, in.BackgroundHoldState(agentClaudeCode), StateInstalled, "idempotent")
	})

	t.Run("dry run does not touch disk", func(t *testing.T) {
		home := fakeHome(t)
		result := must[Result](t)(testInstaller(testCLI, home).InstallBackgroundHold(agentClaudeCode, true))
		if result.Diff == "" {
			t.Error("diff must not be empty")
		}
		if fileExists(settings(home)) {
			t.Error("dry run wrote the config")
		}
	})

	// ---- Uninstall & state ----

	t.Run("uninstall removes only our hook", func(t *testing.T) {
		home := fakeHome(t, ".claude")
		writeJSONFile(t, map[string]any{
			"hooks": map[string]any{"PreToolUse": []any{map[string]any{
				"matcher": "Write", "hooks": []any{map[string]any{"type": "command", "command": "my-linter"}},
			}}},
		}, settings(home))

		in := testInstaller(testCLI, home)
		must[Result](t)(in.InstallBackgroundHold(agentClaudeCode, false))
		must[Result](t)(in.UninstallBackgroundHold(agentClaudeCode, false))

		if backgroundCommandIn(t, home) != "" {
			t.Error("our hook must be gone")
		}
		groups := objects(t, hooksIn(t, settings(home))["PreToolUse"], "PreToolUse")
		if len(groups) != 1 || groups[0]["matcher"] != "Write" {
			t.Errorf("the user's own hook must remain: %v", groups)
		}
	})

	t.Run("uninstall is a no-op when absent", func(t *testing.T) {
		result := must[Result](t)(testInstaller(testCLI, fakeHome(t)).UninstallBackgroundHold(agentClaudeCode, false))
		if result.Diff != unchangedDiff {
			t.Errorf("diff = %q", result.Diff)
		}
	})

	t.Run("state transitions and toggle on-off-on is consistent", func(t *testing.T) {
		in := testInstaller(testCLI, fakeHome(t))
		wantState(t, in.BackgroundHoldState(agentClaudeCode), StateNotInstalled, "initial")
		must[Result](t)(in.InstallBackgroundHold(agentClaudeCode, false))
		wantState(t, in.BackgroundHoldState(agentClaudeCode), StateInstalled, "on")
		must[Result](t)(in.UninstallBackgroundHold(agentClaudeCode, false))
		wantState(t, in.BackgroundHoldState(agentClaudeCode), StateNotInstalled, "off")
		must[Result](t)(in.InstallBackgroundHold(agentClaudeCode, false))
		wantState(t, in.BackgroundHoldState(agentClaudeCode), StateInstalled, "re-enabling must restore cleanly")
	})

	t.Run("state is modified when the embedded TTL drifts", func(t *testing.T) {
		home := fakeHome(t)
		in := testInstaller(testCLI, home)
		must[Result](t)(in.InstallBackgroundHold(agentClaudeCode, false))

		dict := readJSONFile(t, settings(home))
		dict["hooks"] = map[string]any{"PreToolUse": []any{map[string]any{"matcher": "Bash", "hooks": []any{map[string]any{
			"type": "command", "command": "/usr/local/bin/lidwake acquire --tool claude-code --if-background --ttl 999", "_lidwake": true,
		}}}}}
		writeJSONFile(t, dict, settings(home))
		wantState(t, in.BackgroundHoldState(agentClaudeCode), StateModifiedExternally, "drifted TTL")

		must[Result](t)(in.InstallBackgroundHold(agentClaudeCode, false))
		wantState(t, in.BackgroundHoldState(agentClaudeCode), StateInstalled, "repaired")
	})

	// ---- Coexistence with the core hooks ----

	t.Run("the background hook coexists with the core hooks without disturbing them", func(t *testing.T) {
		in := testInstaller(testCLI, fakeHome(t, ".claude"))
		mustInstall(t, in, agentClaudeCode)
		wantState(t, in.State(agentClaudeCode), StateInstalled, "core")
		must[Result](t)(in.InstallBackgroundHold(agentClaudeCode, false))

		wantState(t, in.State(agentClaudeCode), StateInstalled, "core hooks stay connected")
		wantState(t, in.BackgroundHoldState(agentClaudeCode), StateInstalled, "background")

		must[Result](t)(in.UninstallBackgroundHold(agentClaudeCode, false))
		wantState(t, in.State(agentClaudeCode), StateInstalled, "core hooks survive a background toggle-off")
		wantState(t, in.BackgroundHoldState(agentClaudeCode), StateNotInstalled, "background off")
	})

	t.Run("the core uninstall strips the background hook — the desync the app re-applies", func(t *testing.T) {
		in := testInstaller(testCLI, fakeHome(t, ".claude"))
		mustInstall(t, in, agentClaudeCode)
		must[Result](t)(in.InstallBackgroundHold(agentClaudeCode, false))
		wantState(t, in.BackgroundHoldState(agentClaudeCode), StateInstalled, "background")

		// Disconnecting the agent strips every lidwake handler, this one included — which is why a
		// reconnect re-applies the background hook when the setting is on.
		mustUninstall(t, in, agentClaudeCode)
		wantState(t, in.State(agentClaudeCode), StateNotInstalled, "core")
		wantState(t, in.BackgroundHoldState(agentClaudeCode), StateNotInstalled, "core uninstall takes the background hook with it")
	})
}
