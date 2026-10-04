package hooks

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestHermesGo covers the Hermes integration beyond the ported cases: the exact YAML block, the
// no-op paths, refusal on an unreadable allowlist, and what the Go port does differently.
func TestHermesGo(t *testing.T) {
	cfgPath := func(home string) string { return filepath.Join(home, ".hermes", "config.yaml") }
	allowPath := func(home string) string { return filepath.Join(home, ".hermes", "shell-hooks-allowlist.json") }
	const block = "hooks:\n" +
		"  # >>> lidwake (managed)\n" +
		"  on_session_start:\n" +
		"    - command: \"'/usr/local/bin/lidwake' acquire --tool hermes\"\n" +
		"  pre_gateway_dispatch:\n" +
		"    - command: \"'/usr/local/bin/lidwake' acquire --tool hermes\"\n" +
		"  on_session_end:\n" +
		"    - command: \"'/usr/local/bin/lidwake' release --tool hermes\"\n" +
		"  # <<< lidwake"

	t.Run("the block replaces the default empty hooks map exactly", func(t *testing.T) {
		home := fakeHome(t, ".hermes")
		writeFile(t, cfgPath(home), "model:\n  default: x\nhooks: {}\nhooks_auto_accept: false\n")
		in := testInstaller(testCLI, home)
		result := mustInstall(t, in, agentHermes)
		if result.Summary != "wired Hermes on_session_start/on_session_end shell hooks" {
			t.Errorf("summary = %q", result.Summary)
		}
		wantDiff := "~ " + cfgPath(home) + ": set hooks.on_session_start/on_session_end\n~ " +
			allowPath(home) + ": approve acquire/release"
		if result.Diff != wantDiff {
			t.Errorf("diff = %q, want %q", result.Diff, wantDiff)
		}
		if got, want := readFile(t, cfgPath(home)), "model:\n  default: x\n"+block+"\nhooks_auto_accept: false\n"; got != want {
			t.Errorf("config:\n%s\nwant:\n%s", got, want)
		}
		approvals := objects(t, readJSONFile(t, allowPath(home))["approvals"], "approvals")
		want := []map[string]any{
			{"event": "on_session_start", "command": "'/usr/local/bin/lidwake' acquire --tool hermes"},
			{"event": "pre_gateway_dispatch", "command": "'/usr/local/bin/lidwake' acquire --tool hermes"},
			{"event": "on_session_end", "command": "'/usr/local/bin/lidwake' release --tool hermes"},
		}
		if len(approvals) != len(want) {
			t.Fatalf("approvals = %v", approvals)
		}
		for i := range want {
			if approvals[i]["event"] != want[i]["event"] || approvals[i]["command"] != want[i]["command"] {
				t.Errorf("approval %d = %v, want %v", i, approvals[i], want[i])
			}
		}
	})

	t.Run("a missing config gets an appended block", func(t *testing.T) {
		home := fakeHome(t, ".hermes")
		in := testInstaller(testCLI, home)
		result := mustInstall(t, in, agentHermes)
		wantContains(t, result.Diff, "+ "+cfgPath(home)+": hooks block\n", "diff")
		if got := readFile(t, cfgPath(home)); got != block+"\n" {
			t.Errorf("config = %q", got)
		}
		wantState(t, in.State(agentHermes), StateInstalled, "fresh")
	})

	t.Run("a config without a hooks map gets the block appended after its content", func(t *testing.T) {
		home := fakeHome(t, ".hermes")
		writeFile(t, cfgPath(home), "model: x\n")
		mustInstall(t, testInstaller(testCLI, home), agentHermes)
		if got := readFile(t, cfgPath(home)); got != "model: x\n\n"+block+"\n" {
			t.Errorf("config = %q", got)
		}
	})

	t.Run("a populated hooks map is left for manual setup", func(t *testing.T) {
		home := fakeHome(t, ".hermes")
		original := "hooks:\n  on_x:\n    - command: \"echo\"\n"
		writeFile(t, cfgPath(home), original)
		in := testInstaller(testCLI, home)
		result := mustInstall(t, in, agentHermes)
		if !strings.HasPrefix(result.Summary, "Hermes config already has a hooks: section") || result.Diff != unchangedDiff {
			t.Errorf("result = %+v", result)
		}
		if readFile(t, cfgPath(home)) != original {
			t.Error("a populated hooks map must not be touched")
		}
		if fileExists(allowPath(home)) {
			t.Error("no approvals without hooks")
		}
		wantState(t, in.State(agentHermes), StateNotInstalled, "manual")
	})

	t.Run("reinstall is a no-op", func(t *testing.T) {
		home := fakeHome(t, ".hermes")
		writeFile(t, cfgPath(home), "hooks: {}\n")
		in := testInstaller(testCLI, home)
		mustInstall(t, in, agentHermes)
		cfg, allow := readFile(t, cfgPath(home)), readFile(t, allowPath(home))
		again := mustInstall(t, in, agentHermes)
		if again.Summary != "already installed" || again.Diff != unchangedDiff {
			t.Errorf("reinstall = %+v", again)
		}
		if readFile(t, cfgPath(home)) != cfg || readFile(t, allowPath(home)) != allow {
			t.Error("a no-op reinstall must not rewrite either file")
		}
	})

	t.Run("a wiped allowlist is topped up without touching the yaml", func(t *testing.T) {
		home := fakeHome(t, ".hermes")
		writeFile(t, cfgPath(home), "hooks: {}\n")
		in := testInstaller(testCLI, home)
		mustInstall(t, in, agentHermes)
		cfg := readFile(t, cfgPath(home))
		if err := os.Remove(allowPath(home)); err != nil {
			t.Fatal(err)
		}
		wantState(t, in.State(agentHermes), StateModifiedExternally, "unapproved hooks are skipped by Hermes")

		result := mustInstall(t, in, agentHermes)
		if result.Summary != "already installed" || result.Diff != "~ "+allowPath(home)+": approve acquire/release" {
			t.Errorf("top-up = %+v", result)
		}
		if readFile(t, cfgPath(home)) != cfg {
			t.Error("the yaml must not change")
		}
		wantState(t, in.State(agentHermes), StateInstalled, "topped up")
	})

	t.Run("a deleted release line is repaired on reinstall", func(t *testing.T) {
		home := fakeHome(t, ".hermes")
		writeFile(t, cfgPath(home), "hooks: {}\n")
		in := testInstaller(testCLI, home)
		mustInstall(t, in, agentHermes)
		cfg := readFile(t, cfgPath(home))
		cfg = strings.Replace(cfg, "  on_session_end:\n    - command: \"'/usr/local/bin/lidwake' release --tool hermes\"\n", "", 1)
		writeFile(t, cfgPath(home), cfg)
		wantState(t, in.State(agentHermes), StateModifiedExternally, "no release")

		mustInstall(t, in, agentHermes)
		wantState(t, in.State(agentHermes), StateInstalled, "repaired")
		if got := readFile(t, cfgPath(home)); got != block+"\n" {
			t.Errorf("config = %q", got)
		}
	})

	t.Run("uninstall with nothing installed reports nothing to remove", func(t *testing.T) {
		home := fakeHome(t, ".hermes")
		writeFile(t, cfgPath(home), "hooks: {}\n")
		writeJSONFile(t, map[string]any{"approvals": []any{map[string]any{"event": "on_x", "command": "echo"}}}, allowPath(home))
		allow := readFile(t, allowPath(home))
		if got := mustUninstall(t, testInstaller(testCLI, home), agentHermes); got != nothingRemoved() {
			t.Errorf("uninstall = %+v", got)
		}
		if readFile(t, allowPath(home)) != allow {
			t.Error("an allowlist without our approvals must stay byte-identical")
		}
		if got := mustUninstall(t, testInstaller(testCLI, fakeHome(t, ".hermes")), agentHermes); got != nothingRemoved() {
			t.Errorf("uninstall without any file = %+v", got)
		}
	})

	t.Run("uninstall summary and diff", func(t *testing.T) {
		home := fakeHome(t, ".hermes")
		writeFile(t, cfgPath(home), "hooks: {}\n")
		in := testInstaller(testCLI, home)
		mustInstall(t, in, agentHermes)
		removed := mustUninstall(t, in, agentHermes)
		want := "~ " + cfgPath(home) + ": removed lidwake hooks\n~ " + allowPath(home) + ": revoke lidwake approvals"
		if removed.Summary != "removed Hermes hooks" || removed.Diff != want {
			t.Errorf("uninstall = %+v", removed)
		}
		if got := readFile(t, cfgPath(home)); got != "hooks: {}\n" {
			t.Errorf("config = %q", got)
		}
	})

	t.Run("an unreadable allowlist refuses install before the yaml is touched", func(t *testing.T) {
		home := fakeHome(t, ".hermes")
		writeFile(t, cfgPath(home), "hooks: {}\n")
		writeFile(t, allowPath(home), "{ not json")
		in := testInstaller(testCLI, home)
		wantState(t, in.State(agentHermes), StateConfigUnreadable, "broken allowlist")
		if _, err := in.Install(agentHermes, false); !IsSkip(err, SkipConfigUnreadable) {
			t.Errorf("install err = %v", err)
		}
		if readFile(t, cfgPath(home)) != "hooks: {}\n" || readFile(t, allowPath(home)) != "{ not json" {
			t.Error("a refused install must not write")
		}
		if _, err := in.Uninstall(agentHermes, false); !IsSkip(err, SkipConfigUnreadable) {
			t.Errorf("uninstall err = %v", err)
		}
	})

	t.Run("dry run touches nothing", func(t *testing.T) {
		home := fakeHome(t, ".hermes")
		writeFile(t, cfgPath(home), "hooks: {}\n")
		in := testInstaller(testCLI, home)
		dry := must[Result](t)(in.Install(agentHermes, true))
		if readFile(t, cfgPath(home)) != "hooks: {}\n" || fileExists(allowPath(home)) {
			t.Error("a dry-run install must not write")
		}
		applied := mustInstall(t, in, agentHermes)
		if dry != applied {
			t.Errorf("dry %+v != applied %+v", dry, applied)
		}
		cfg, allow := readFile(t, cfgPath(home)), readFile(t, allowPath(home))
		must[Result](t)(in.Uninstall(agentHermes, true))
		if readFile(t, cfgPath(home)) != cfg || readFile(t, allowPath(home)) != allow {
			t.Error("a dry-run uninstall must not write")
		}
	})

	t.Run("a deleted end marker keeps the yaml after the block", func(t *testing.T) {
		home := fakeHome(t, ".hermes")
		writeFile(t, cfgPath(home), "hooks: {}\nhooks_auto_accept: false\n")
		in := testInstaller(testCLI, home)
		mustInstall(t, in, agentHermes)
		cfg := strings.Replace(readFile(t, cfgPath(home)), "  # <<< lidwake\n", "", 1)
		writeFile(t, cfgPath(home), cfg)

		mustUninstall(t, in, agentHermes)
		if got := readFile(t, cfgPath(home)); got != "hooks: {}\nhooks_auto_accept: false\n" {
			t.Errorf("config = %q", got)
		}
	})

	t.Run("a hooks line whose children follow a blank line is kept", func(t *testing.T) {
		in := "hooks:\n" +
			"  # >>> lidwake (managed)\n" +
			"  on_session_start:\n" +
			"    - command: \"'/x/lidwake' acquire --tool hermes\"\n" +
			"  # <<< lidwake\n" +
			"\n" +
			"  on_x:\n" +
			"    - command: \"echo\"\n"
		want := "hooks:\n\n  on_x:\n    - command: \"echo\"\n"
		if got := hermesRemoveManagedBlock(in); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("lidwake commands outside our block read as drift", func(t *testing.T) {
		home := fakeHome(t, ".hermes")
		writeFile(t, cfgPath(home), "hooks:\n  on_session_start:\n    - command: \"'/old/lidwake' acquire --tool hermes\"\n")
		wantState(t, testInstaller(testCLI, home).State(agentHermes), StateModifiedExternally, "foreign-wired")
		writeFile(t, cfgPath(home), "hooks:\n  # - command: \"'/old/lidwake' acquire --tool hermes\"\n")
		wantState(t, testInstaller(testCLI, home).State(agentHermes), StateNotInstalled, "commented out")
	})

	t.Run("cli path quoting", func(t *testing.T) {
		if got := hermesCommand("acquire", "/it's/lidwake"); got != `"/it's/lidwake" acquire --tool hermes` {
			t.Errorf("single quote: %q", got)
		}
		if got := hermesCommand("release", "/My Apps/lidwake"); got != `'/My Apps/lidwake' release --tool hermes` {
			t.Errorf("space: %q", got)
		}

		// A double quote or backslash is escaped inside the YAML scalar, while the allowlist holds
		// the decoded command Hermes matches against.
		home := fakeHome(t, ".hermes")
		cli := `/a "b"\c/lidwake`
		in := testInstaller(cli, home)
		mustInstall(t, in, agentHermes)
		wantContains(t, readFile(t, cfgPath(home)), `- command: "'/a \"b\"\\c/lidwake' acquire --tool hermes"`, "yaml")
		approvals := objects(t, readJSONFile(t, allowPath(home))["approvals"], "approvals")
		if approvals[0]["command"] != `'/a "b"\c/lidwake' acquire --tool hermes` {
			t.Errorf("approval = %v", approvals[0]["command"])
		}
		wantState(t, in.State(agentHermes), StateInstalled, "escaped path")
		mustUninstall(t, in, agentHermes)
		wantState(t, in.State(agentHermes), StateNotInstalled, "after uninstall")
	})

	t.Run("approvals that aren't objects are kept", func(t *testing.T) {
		home := fakeHome(t, ".hermes")
		writeFile(t, cfgPath(home), "hooks: {}\n")
		writeJSONFile(t, map[string]any{"approvals": []any{"legacy", map[string]any{"event": "on_x", "command": "echo"}}}, allowPath(home))
		in := testInstaller(testCLI, home)
		mustInstall(t, in, agentHermes)
		approvals, _ := readJSONFile(t, allowPath(home))["approvals"].([]any)
		if len(approvals) != 5 || approvals[0] != "legacy" {
			t.Errorf("after install = %v", approvals)
		}
		mustUninstall(t, in, agentHermes)
		approvals, _ = readJSONFile(t, allowPath(home))["approvals"].([]any)
		if len(approvals) != 2 || approvals[0] != "legacy" {
			t.Errorf("after uninstall = %v", approvals)
		}
	})

	t.Run("a stale approval is pruned while the user's stay", func(t *testing.T) {
		before := map[string]any{"approvals": []any{
			map[string]any{"event": "on_session_end", "command": "'/old/lidwake' release --tool hermes"},
			map[string]any{"event": "on_session_end", "command": "/usr/bin/say done"},
			map[string]any{"event": "on_session_end", "command": "'/old/lidwake' release --tool codex"},
		}}
		after := hermesReconciled(before, hermesApprovals(testCLI))
		var cmds []string
		for _, e := range after["approvals"].([]any) {
			cmds = append(cmds, e.(map[string]any)["command"].(string))
		}
		want := []string{
			"/usr/bin/say done",
			"'/old/lidwake' release --tool codex",
			"'/usr/local/bin/lidwake' acquire --tool hermes",
			"'/usr/local/bin/lidwake' acquire --tool hermes",
			"'/usr/local/bin/lidwake' release --tool hermes",
		}
		if !slices.Equal(cmds, want) {
			t.Errorf("approvals = %v, want %v", cmds, want)
		}
		if len(before["approvals"].([]any)) != 3 {
			t.Error("reconciling must not mutate the document it read")
		}
	})
}
