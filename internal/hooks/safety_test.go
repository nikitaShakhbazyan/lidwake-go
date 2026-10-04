package hooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestInstallerSafety: configs owned by other programs must never be destroyed (unparseable files,
// symlinked dotfiles), and only entries that are provably ours may be rewritten or removed.
func TestInstallerSafety(t *testing.T) {
	// ---- Unparseable configs are never overwritten ----

	t.Run("install refuses a config with comments and leaves it untouched", func(t *testing.T) {
		home := fakeHome(t, ".claude")
		path := filepath.Join(home, ".claude", "settings.json")
		jsonc := "{\n  // my permission allowlist — do not lose this\n  \"permissions\": {\"allow\": [\"Bash(npm:*)\"]}\n}"
		writeFile(t, path, jsonc)

		in := testInstaller(testCLI, home)
		_, err := in.Install(agentClaudeCode, false)
		wantSkip(t, err, SkipConfigUnreadable)
		if readFile(t, path) != jsonc {
			t.Error("an unparseable config must be left byte-identical")
		}
		wantState(t, in.State(agentClaudeCode), StateConfigUnreadable, "jsonc")
	})

	t.Run("uninstall refuses an unparseable config rather than reporting nothing to remove", func(t *testing.T) {
		home := fakeHome(t, ".claude")
		writeFile(t, filepath.Join(home, ".claude", "settings.json"), "{ not json")
		_, err := testInstaller(testCLI, home).Uninstall(agentClaudeCode, false)
		wantSkip(t, err, SkipConfigUnreadable)
	})

	t.Run("install refuses an array-rooted config", func(t *testing.T) {
		home := fakeHome(t, ".cursor")
		path := filepath.Join(home, ".cursor", "hooks.json")
		writeFile(t, path, "[1, 2, 3]")
		_, err := testInstaller(testCLI, home).Install(agentCursor, false)
		wantSkip(t, err, SkipConfigUnreadable)
		if readFile(t, path) != "[1, 2, 3]" {
			t.Error("array-rooted config must be left untouched")
		}
	})

	t.Run("mcp install refuses an unparseable claude json", func(t *testing.T) {
		home := fakeHome(t, ".claude")
		// ~/.claude.json holds Claude Code's whole state — the worst file to wipe.
		writeFile(t, filepath.Join(home, ".claude.json"), `{ "oauthAccount": `)
		in := testInstaller(testCLI, home)
		_, err := in.InstallMCP(agentClaudeCode, false)
		wantSkip(t, err, SkipConfigUnreadable)
		wantState(t, in.MCPState(agentClaudeCode), StateConfigUnreadable, "mcp")
	})

	t.Run("write refuses when the config changed between read and write", func(t *testing.T) {
		home := fakeHome(t)
		path := filepath.Join(home, "config.json")
		writeFile(t, path, `{"a": 1}`)

		stale := map[string]any{"a": json.Number("2")}
		err := writeJSON(map[string]any{"a": 3}, path, stale)
		if s := requireSkip(t, err); s.Reason != SkipConcurrentModification {
			t.Errorf("reason = %v", s.Reason)
		}
		if readJSONFile(t, path)["a"] != float64(1) {
			t.Error("a conflicting write must not land")
		}
	})

	// ---- Symlinked configs (stow/chezmoi) survive writes ----

	t.Run("install through a symlinked config keeps the symlink and its permissions", func(t *testing.T) {
		home := fakeHome(t, ".claude", "dotfiles")
		target := filepath.Join(home, "dotfiles", "claude-settings.json")
		link := filepath.Join(home, ".claude", "settings.json")
		writeFile(t, target, `{"permissions": {"allow": []}}`)
		if err := os.Chmod(target, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}

		mustInstall(t, testInstaller(testCLI, home), agentClaudeCode)

		fi, err := os.Lstat(link)
		if err != nil || fi.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("the symlink must survive the write: %v %v", fi, err)
		}
		targetDict := readJSONFile(t, target)
		if targetDict["hooks"] == nil {
			t.Error("the write must land in the symlink's target")
		}
		if targetDict["permissions"] == nil {
			t.Error("user content must survive")
		}
		tfi, err := os.Stat(target)
		if err != nil {
			t.Fatal(err)
		}
		if tfi.Mode().Perm() != 0o600 {
			t.Errorf("original permissions must be preserved: %v", tfi.Mode().Perm())
		}
	})

	// ---- The shell wrapper never truncates rc files ----

	t.Run("uninstall with a deleted end marker preserves everything after the block", func(t *testing.T) {
		home := fakeHome(t)
		zshrc := filepath.Join(home, ".zshrc")
		userTail := "export PATH=$HOME/bin:$PATH\nsource ~/.fzf.zsh\nalias gs='git status'"
		writeFile(t, zshrc, "# my prelude\n# lidwake-aider\nalias aider='"+home+"/.local/bin/aider-lidwake'\n"+userTail)

		must[Result](t)(aiderIntegration{}.uninstall(testContext(testCLI, home), false))

		after := readFile(t, zshrc)
		wantContains(t, after, userTail, "user content after a damaged block must survive")
		wantContains(t, after, "# my prelude", "prelude")
		wantNotContains(t, after, "lidwake", "our lines must be gone")
	})

	t.Run("install repairs a wrapper script whose cli path drifted", func(t *testing.T) {
		home := fakeHome(t)
		writeFile(t, filepath.Join(home, ".zshrc"), "")
		writeFile(t, filepath.Join(home, ".bashrc"), "")

		must[Result](t)(aiderIntegration{}.install(testContext(testCLI, home), false))

		// The binary moved: a fresh context with the new path must see drift and repair it.
		moved := testContext("/Applications/lidwake.app/Contents/Helpers/lidwake", home)
		wantState(t, aiderIntegration{}.state(moved), StateModifiedExternally, "drifted")
		must[Result](t)(aiderIntegration{}.install(moved, false))
		wantState(t, aiderIntegration{}.state(moved), StateInstalled, "repaired")
		script := readFile(t, filepath.Join(home, ".local", "bin", "aider-lidwake"))
		wantContains(t, script, "/Applications/lidwake.app", "new path")
		wantNotContains(t, script, "/usr/local/bin/lidwake ", "old path")
	})

	t.Run("install touches only rc files that exist", func(t *testing.T) {
		home := fakeHome(t)
		writeFile(t, filepath.Join(home, ".zshrc"), "")

		must[Result](t)(aiderIntegration{}.install(testContext(testCLI, home), false))

		if fileExists(filepath.Join(home, ".bashrc")) {
			t.Error("a missing rc file must not be created")
		}
		wantState(t, testInstaller(testCLI, home).State(agentAider), StateInstalled,
			"one rc file with the alias is a complete install")
	})

	// ---- Hermes line-scoped YAML surgery ----

	t.Run("hermes install never rewrites a nested or lookalike hooks map", func(t *testing.T) {
		home := fakeHome(t, ".hermes")
		cfg := filepath.Join(home, ".hermes", "config.yaml")
		// No top-level hooks map; a nested empty one and a lookalike key that must both survive.
		writeFile(t, cfg, "agents:\n  sub:\n    hooks: {}\npython_hooks: {}\n")

		mustInstall(t, testInstaller(testCLI, home), agentHermes)

		after := readFile(t, cfg)
		wantContains(t, after, "    hooks: {}", "the nested empty hooks map must survive verbatim")
		wantContains(t, after, "python_hooks: {}", "a lookalike key must survive verbatim")
		wantContains(t, after, "on_session_start", "our block must be appended as a new top-level hooks map")
	})

	t.Run("hermes install repairs a stale cli path in place", func(t *testing.T) {
		home := fakeHome(t, ".hermes")
		cfg := filepath.Join(home, ".hermes", "config.yaml")
		writeFile(t, cfg, "model:\n  default: x\nhooks: {}\n")

		mustInstall(t, testInstaller("/old/place/lidwake", home), agentHermes)

		in := testInstaller("/Applications/lidwake.app/Contents/Helpers/lidwake", home)
		wantState(t, in.State(agentHermes), StateModifiedExternally,
			"a dead CLI path must not read as notInstalled or installed")

		mustInstall(t, in, agentHermes)
		wantState(t, in.State(agentHermes), StateInstalled, "repaired")
		after := readFile(t, cfg)
		wantNotContains(t, after, "/old/place/lidwake", "the stale block must be gone")
		if n := strings.Count(after, "on_session_start"); n != 1 {
			t.Errorf("exactly one acquire hook, got %d:\n%s", n, after)
		}

		// The allowlist must hold approvals only for the current commands.
		approvals := objects(t, readJSONFile(t, filepath.Join(home, ".hermes", "shell-hooks-allowlist.json"))["approvals"], "approvals")
		if len(approvals) != 3 {
			t.Errorf("approvals = %v, want 3", approvals)
		}
		for _, a := range approvals {
			if cmd, _ := a["command"].(string); strings.Contains(cmd, "/old/place") {
				t.Errorf("stale approval survived: %q", cmd)
			}
		}
	})

	t.Run("hermes allowlist round-trips foreign keys and user approvals", func(t *testing.T) {
		home := fakeHome(t, ".hermes")
		writeFile(t, filepath.Join(home, ".hermes", "config.yaml"), "hooks: {}\n")
		allowPath := filepath.Join(home, ".hermes", "shell-hooks-allowlist.json")
		writeJSONFile(t, map[string]any{
			"version":   2,
			"approvals": []any{map[string]any{"event": "on_session_end", "command": "/usr/bin/say done"}},
		}, allowPath)

		in := testInstaller(testCLI, home)
		mustInstall(t, in, agentHermes)
		mustUninstall(t, in, agentHermes)

		after := readJSONFile(t, allowPath)
		if after["version"] != float64(2) {
			t.Errorf("foreign top-level keys must survive install + uninstall: %v", after)
		}
		approvals := objects(t, after["approvals"], "approvals")
		found := false
		for _, a := range approvals {
			cmd, _ := a["command"].(string)
			found = found || cmd == "/usr/bin/say done"
			if strings.Contains(cmd, "lidwake") {
				t.Errorf("our approval survived uninstall: %q", cmd)
			}
		}
		if !found {
			t.Error("user approvals must survive")
		}
	})

	// ---- Ownership: only provably-ours entries are rewritten or removed ----

	t.Run("a user hook merely containing the word lidwake is never ours", func(t *testing.T) {
		home := fakeHome(t, ".claude")
		path := filepath.Join(home, ".claude", "settings.json")
		userCommand := "~/bin/lidwake-notify.sh"
		writeJSONFile(t, map[string]any{
			"hooks": map[string]any{
				"UserPromptSubmit": []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": userCommand}}}},
			},
		}, path)

		in := testInstaller(testCLI, home)
		mustInstall(t, in, agentClaudeCode)
		if n := len(objects(t, hooksIn(t, path)["UserPromptSubmit"], "UPS")); n != 2 {
			t.Errorf("the user's hook must be preserved alongside ours: %d", n)
		}

		mustUninstall(t, in, agentClaudeCode)
		start := objects(t, hooksIn(t, path)["UserPromptSubmit"], "UPS")
		if len(start) != 1 {
			t.Fatalf("groups = %d", len(start))
		}
		if got := firstInnerCommand(t, hooksIn(t, path), "UserPromptSubmit"); got != userCommand {
			t.Errorf("uninstall must remove only our entry: survivor %q", got)
		}
	})

	t.Run("repair and uninstall preserve a user handler sharing our group", func(t *testing.T) {
		home := fakeHome(t, ".claude")
		path := filepath.Join(home, ".claude", "settings.json")
		in := testInstaller(testCLI, home)
		mustInstall(t, in, agentClaudeCode)

		// The user adds their own handler inside our group's inner hooks array.
		dict := readJSONFile(t, path)
		group := objects(t, object(t, dict["hooks"], "hooks")["UserPromptSubmit"], "UPS")[0]
		group["hooks"] = append(group["hooks"].([]any), map[string]any{"type": "command", "command": "echo sibling"})
		writeJSONFile(t, dict, path)

		innerAfter := func() []map[string]any {
			return objects(t, objects(t, hooksIn(t, path)["UserPromptSubmit"], "UPS")[0]["hooks"], "inner")
		}

		// Reinstall (repair) must not clobber the sibling.
		mustInstall(t, in, agentClaudeCode)
		found := false
		for _, h := range innerAfter() {
			if h["command"] == "echo sibling" {
				found = true
			}
		}
		if !found {
			t.Error("repair must replace only our handler")
		}

		// Uninstall must remove only our handler, keeping the group for the sibling.
		mustUninstall(t, in, agentClaudeCode)
		inner := innerAfter()
		if len(inner) != 1 || inner[0]["command"] != "echo sibling" {
			t.Errorf("inner after uninstall = %v", inner)
		}
	})

	t.Run("leftover release hooks after a deleted acquire read as drift not notInstalled", func(t *testing.T) {
		home := fakeHome(t, ".claude")
		path := filepath.Join(home, ".claude", "settings.json")
		in := testInstaller(testCLI, home)
		mustInstall(t, in, agentClaudeCode)

		// Delete only the acquire entry; our Stop + Notification releases remain live.
		dict := readJSONFile(t, path)
		delete(object(t, dict["hooks"], "hooks"), "UserPromptSubmit")
		writeJSONFile(t, dict, path)

		wantState(t, in.State(agentClaudeCode), StateModifiedExternally,
			"live lidwake entries must never be invisible as notInstalled")
	})

	t.Run("cursor uninstall leaves no empty event arrays behind", func(t *testing.T) {
		home := fakeHome(t, ".cursor")
		in := testInstaller(testCLI, home)
		mustInstall(t, in, agentCursor)
		mustUninstall(t, in, agentCursor)

		hooks, _ := readJSONFile(t, filepath.Join(home, ".cursor", "hooks.json"))["hooks"].(map[string]any)
		if _, ok := hooks["beforeSubmitPrompt"]; ok {
			t.Error("emptied event keys must be dropped")
		}
		if _, ok := hooks["stop"]; ok {
			t.Error("emptied event keys must be dropped")
		}
	})

	// ---- Generated-code escaping ----

	t.Run("shell quoting survives quotes and metacharacters in the cli path", func(t *testing.T) {
		plain := hookContext{cliPath: "/Applications/lidwake.app/Contents/Helpers/lidwake"}
		if plain.quotedCLI() != plain.cliPath {
			t.Error("a plain path needs no quoting")
		}
		spaced := hookContext{cliPath: "/Apps/My Tools/lidwake"}
		if got := spaced.quotedCLI(); got != "'/Apps/My Tools/lidwake'" {
			t.Errorf("spaced = %q", got)
		}
		quoted := hookContext{cliPath: "/Apps/it's here/lidwake"}
		if got := quoted.quotedCLI(); got != `'/Apps/it'\''s here/lidwake'` {
			t.Errorf("quoted = %q", got)
		}
	})

	t.Run("pi extension escapes quotes in the cli path", func(t *testing.T) {
		home := fakeHome(t, ".pi")
		must[Result](t)(piIntegration{}.install(testContext(`/Apps/say "hi"/lidwake`, home), false))

		ts := readFile(t, filepath.Join(home, ".pi", "agent", "extensions", "lidwake.ts"))
		wantContains(t, ts, `\"hi\"`, "quotes in the path must be escaped in the generated literal")
		wantNotContains(t, ts, `execFileSync("/Apps/say "hi"`, "an unescaped quote would break the literal")
	})
}

// TestInstallerSafetyGo covers safety behavior specific to the Go port.
func TestInstallerSafetyGo(t *testing.T) {
	t.Run("numbers and unrelated content round-trip exactly", func(t *testing.T) {
		home := fakeHome(t, ".claude")
		path := filepath.Join(home, ".claude", "settings.json")
		writeFile(t, path, `{"big": 12345678901234567890, "ratio": 1.50, "tiny": 1e-7, "html": "<a&b>", "url": "https://x/y"}`)

		in := testInstaller(testCLI, home)
		mustInstall(t, in, agentClaudeCode)
		mustUninstall(t, in, agentClaudeCode)

		got := readFile(t, path)
		for _, want := range []string{`12345678901234567890`, `1.50`, `1e-7`, `"<a&b>"`, `"https://x/y"`} {
			wantContains(t, got, want, "user value survives verbatim")
		}
	})

	t.Run("non-object array elements are kept, never dropped", func(t *testing.T) {
		home := fakeHome(t, ".claude")
		path := filepath.Join(home, ".claude", "settings.json")
		writeFile(t, path, `{"hooks": {"UserPromptSubmit": ["odd-entry", {"hooks": [{"type": "command", "command": "echo mine"}, 42]}]}}`)

		in := testInstaller(testCLI, home)
		mustInstall(t, in, agentClaudeCode)
		wantState(t, in.State(agentClaudeCode), StateInstalled, "mixed array")
		got := readFile(t, path)
		wantContains(t, got, `"odd-entry"`, "after install")
		wantContains(t, got, `42`, "after install")

		mustUninstall(t, in, agentClaudeCode)
		got = readFile(t, path)
		wantContains(t, got, `"odd-entry"`, "after uninstall")
		wantContains(t, got, `"echo mine"`, "after uninstall")
		wantNotContains(t, got, "lidwake acquire", "after uninstall")
	})

	t.Run("a no-op uninstall leaves the file byte-identical", func(t *testing.T) {
		home := fakeHome(t, ".claude")
		path := filepath.Join(home, ".claude", "settings.json")
		original := "{\"hooks\":{\"Stop\":[{\"hooks\":[{\"type\":\"command\",\"command\":\"echo hi\"}]}]},  \"z\":1}\n"
		writeFile(t, path, original)
		result := mustUninstall(t, testInstaller(testCLI, home), agentClaudeCode)
		if result.Diff != unchangedDiff {
			t.Errorf("diff = %q", result.Diff)
		}
		if readFile(t, path) != original {
			t.Error("nothing to change must mean nothing written")
		}
	})

	t.Run("a dangling dotfile symlink is written through to its target", func(t *testing.T) {
		home := fakeHome(t, ".claude", "dotfiles")
		target := filepath.Join(home, "dotfiles", "settings.json")
		link := filepath.Join(home, ".claude", "settings.json")
		if err := os.Symlink("../dotfiles/settings.json", link); err != nil {
			t.Fatal(err)
		}
		mustInstall(t, testInstaller(testCLI, home), agentClaudeCode)
		fi, err := os.Lstat(link)
		if err != nil || fi.Mode()&os.ModeSymlink == 0 {
			t.Fatal("the link must survive")
		}
		if readJSONFile(t, target)["hooks"] == nil {
			t.Error("the target must be created")
		}
	})

	t.Run("invalid utf-8 and trailing garbage are unreadable", func(t *testing.T) {
		home := fakeHome(t, ".claude")
		path := filepath.Join(home, ".claude", "settings.json")
		in := testInstaller(testCLI, home)
		for _, content := range []string{"{\"a\": \"\xff\"}", `{"a": 1} {"b": 2}`, `{"a": 1}]`, "", "null"} {
			writeFile(t, path, content)
			if _, err := in.Install(agentClaudeCode, false); !IsSkip(err, SkipConfigUnreadable) {
				t.Errorf("%q: err = %v", content, err)
			}
			if readFile(t, path) != content {
				t.Errorf("%q: file modified", content)
			}
			wantState(t, in.State(agentClaudeCode), StateConfigUnreadable, content)
		}
	})

	t.Run("a byte order mark is tolerated", func(t *testing.T) {
		home := fakeHome(t, ".claude")
		path := filepath.Join(home, ".claude", "settings.json")
		writeFile(t, path, "\xef\xbb\xbf{\"model\": \"opus\"}")
		in := testInstaller(testCLI, home)
		mustInstall(t, in, agentClaudeCode)
		wantState(t, in.State(agentClaudeCode), StateInstalled, "bom")
	})

	t.Run("write refuses a file created or deleted since the read", func(t *testing.T) {
		home := fakeHome(t)
		path := filepath.Join(home, "config.json")
		writeFile(t, path, `{}`)
		if err := writeJSON(map[string]any{"a": 1}, path, nil); !IsSkip(err, SkipConcurrentModification) {
			t.Errorf("created since read: err = %v", err)
		}
		if err := writeJSON(map[string]any{"a": 1}, filepath.Join(home, "gone.json"), map[string]any{}); !IsSkip(err, SkipConcurrentModification) {
			t.Errorf("deleted since read: err = %v", err)
		}
		if err := writeJSON(map[string]any{"a": 1}, path, map[string]any{}); err != nil {
			t.Errorf("unchanged since read: err = %v", err)
		}
	})

	t.Run("a new config gets the default mode and no temp file is left", func(t *testing.T) {
		home := fakeHome(t, ".claude")
		mustInstall(t, testInstaller(testCLI, home), agentClaudeCode)
		fi, err := os.Stat(filepath.Join(home, ".claude", "settings.json"))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o644 {
			t.Errorf("mode = %v", fi.Mode().Perm())
		}
		entries, _ := os.ReadDir(filepath.Join(home, ".claude"))
		for _, e := range entries {
			if strings.Contains(e.Name(), ".lidwake-") {
				t.Errorf("temp file left: %s", e.Name())
			}
		}
	})

	t.Run("skip error messages", func(t *testing.T) {
		for _, tc := range []struct {
			err  *SkipError
			want string
		}{
			{&SkipError{Reason: SkipNotInstalled}, "The agent isn't installed on this system."},
			{&SkipError{Reason: SkipUnsupported, Detail: "MCP not supported for codex"}, "MCP not supported for codex"},
			{&SkipError{Reason: SkipConfigUnreadable, Detail: "/p"}, "/p exists but isn't valid JSON — fix or remove it, then retry."},
			{&SkipError{Reason: SkipConcurrentModification, Detail: "/p"}, "/p changed while updating it (the agent may be running) — retry."},
		} {
			if got := tc.err.Error(); got != tc.want {
				t.Errorf("Error() = %q, want %q", got, tc.want)
			}
		}
	})

	t.Run("command ownership needs the cli, a verb and --tool", func(t *testing.T) {
		for cmd, want := range map[string]bool{
			"/usr/local/bin/lidwake acquire --tool codex":  true,
			"lidwake release $X --tool claude-code":        true,
			"lidwake hold --for 2h --tool x":               true,
			"~/bin/lidwake-notify.sh":                      false,
			"lidwake acquire":                              false,
			"/usr/bin/other acquire --tool codex":          false,
			"lidwake status --tool codex":                  false,
			"":                                             false,
			"'/Apps/My Tools/lidwake' acquire --tool pi":   true,
			"/usr/local/bin/lidwake acquire --tool cursor": true,
		} {
			if got := commandInvokesLidwakeCLI(cmd); got != want {
				t.Errorf("commandInvokesLidwakeCLI(%q) = %v, want %v", cmd, got, want)
			}
		}
	})
}
