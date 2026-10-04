package hooks

import (
	"os"
	"path/filepath"
	"testing"
)

// TestFilePluginGo covers the single-file plugin integrations (OpenCode, Pi) beyond the ported
// cases: the exact generated sources, escaping, and the file lifecycle.
func TestFilePluginGo(t *testing.T) {
	piPath := func(home string) string { return filepath.Join(home, ".pi", "agent", "extensions", "lidwake.ts") }
	openCodePath := func(home string) string {
		return filepath.Join(home, ".config", "opencode", "plugins", "lidwake.ts")
	}

	t.Run("pi extension content is exact", func(t *testing.T) {
		want := `import { execFileSync } from "node:child_process"

function run(args) {
  try { execFileSync("/usr/local/bin/lidwake", args, { stdio: "ignore" }) } catch (_) {}
}

export default function (pi) {
  const id = (ctx) => ctx?.sessionManager?.getSessionFile?.() ?? String(process.pid)
  pi.on("agent_start", async (_event, ctx) =>
    run(["acquire", id(ctx), "--tool", "pi", "--pid", String(process.pid), "--ttl", "14400"]))
  pi.on("agent_settled", async (_event, ctx) => run(["release", id(ctx), "--tool", "pi"]))
  pi.on("session_shutdown", async (_event, ctx) => run(["release", id(ctx), "--tool", "pi"]))
}`
		if got := piExtensionTS(testCLI); got != want {
			t.Errorf("extension:\n%s\nwant:\n%s", got, want)
		}
	})

	t.Run("pi literal escapes backslashes and line separators but not slashes or html", func(t *testing.T) {
		for in, want := range map[string]string{
			`/a/b<c>&d/lidwake`:  `"/a/b<c>&d/lidwake"`,
			`/a\b/lidwake`:       `"/a\\b/lidwake"`,
			"/a\u2028b/lidwake":  `"/a\u2028b/lidwake"`,
			`/it's "x"/lidwake`:  `"/it's \"x\"/lidwake"`,
			"/tab\there/lidwake": `"/tab\there/lidwake"`,
		} {
			if got := jsStringLiteral(in); got != want {
				t.Errorf("jsStringLiteral(%q) = %s, want %s", in, got, want)
			}
		}
	})

	t.Run("opencode plugin content is exact", func(t *testing.T) {
		want := "export const Lidwake = async ({ $ }) => {\n" +
			"  return {\n" +
			"    event: async ({ event }) => {\n" +
			"      if (event.type === \"session.created\") await $`\"/usr/local/bin/lidwake\" acquire ${event.properties.info.id} --tool opencode`\n" +
			"    }\n" +
			"  }\n" +
			"}"
		if got := openCodePluginTS(testCLI); got != want {
			t.Errorf("plugin:\n%s\nwant:\n%s", got, want)
		}
	})

	t.Run("opencode plugin escapes template and shell metacharacters", func(t *testing.T) {
		got := openCodePluginTS("/a\\b`c$d\"e/lidwake")
		wantContains(t, got, "$`\"/a\\\\b\\`c\\$d\\\"e/lidwake\" acquire", "escaped path")
	})

	t.Run("install diff says create or rewrite", func(t *testing.T) {
		home := fakeHome(t, ".pi")
		in := testInstaller(testCLI, home)
		first := mustInstall(t, in, agentPi)
		if first.Summary != "wrote Pi extension" || first.Diff != "+ "+piPath(home) {
			t.Errorf("first install = %+v", first)
		}
		if again := mustInstall(t, in, agentPi); again.Summary != "already installed" || again.Diff != unchangedDiff {
			t.Errorf("reinstall = %+v", again)
		}
		writeFile(t, piPath(home), "// lidwake, edited by hand\n")
		wantState(t, in.State(agentPi), StateModifiedExternally, "edited")
		repaired := mustInstall(t, in, agentPi)
		if repaired.Diff != "~ "+piPath(home)+" (rewritten to canonical content)" {
			t.Errorf("repair diff = %q", repaired.Diff)
		}
		wantState(t, in.State(agentPi), StateInstalled, "repaired")
	})

	t.Run("state ignores surrounding whitespace and foreign files", func(t *testing.T) {
		home := fakeHome(t, ".pi", ".pi/agent/extensions")
		in := testInstaller(testCLI, home)
		writeFile(t, piPath(home), "// someone else's plugin\n")
		wantState(t, in.State(agentPi), StateNotInstalled, "a file without lidwake isn't ours")
		writeFile(t, piPath(home), "\n"+piExtensionTS(testCLI)+"\n\n")
		wantState(t, in.State(agentPi), StateInstalled, "whitespace-only difference")
	})

	t.Run("uninstall removes only our file", func(t *testing.T) {
		home := fakeHome(t)
		ctx := testContext(testCLI, home)
		must[Result](t)(openCodeIntegration{}.install(ctx, false))
		other := filepath.Join(home, ".config", "opencode", "plugins", "other.ts")
		writeFile(t, other, "export const Other = 1")

		removed := must[Result](t)(openCodeIntegration{}.uninstall(ctx, false))
		if removed.Summary != "removed plugin file" || removed.Diff != "- "+openCodePath(home) {
			t.Errorf("uninstall = %+v", removed)
		}
		if fileExists(openCodePath(home)) {
			t.Error("our plugin must be gone")
		}
		if !fileExists(other) {
			t.Error("the user's other plugin and the shared directory must survive")
		}
		if got := must[Result](t)(openCodeIntegration{}.uninstall(ctx, false)); got != nothingRemoved() {
			t.Errorf("second uninstall = %+v", got)
		}
		wantState(t, openCodeIntegration{}.state(ctx), StateNotInstalled, "after uninstall")
	})

	t.Run("dry run touches nothing", func(t *testing.T) {
		home := fakeHome(t)
		ctx := testContext(testCLI, home)
		result := must[Result](t)(openCodeIntegration{}.install(ctx, true))
		if result.Summary != "wrote OpenCode plugin" || result.Diff != "+ "+openCodePath(home) {
			t.Errorf("dry install = %+v", result)
		}
		if fileExists(filepath.Join(home, ".config")) {
			t.Error("a dry-run install must not create anything")
		}
		must[Result](t)(openCodeIntegration{}.install(ctx, false))
		must[Result](t)(openCodeIntegration{}.uninstall(ctx, true))
		if !fileExists(openCodePath(home)) {
			t.Error("a dry-run uninstall must not remove the plugin")
		}
	})

	t.Run("a symlinked plugin file is written through", func(t *testing.T) {
		home := fakeHome(t, ".pi", ".pi/agent/extensions", "dotfiles")
		target := filepath.Join(home, "dotfiles", "pi-lidwake.ts")
		writeFile(t, target, "// old lidwake\n")
		if err := os.Symlink(target, piPath(home)); err != nil {
			t.Fatal(err)
		}
		mustInstall(t, testInstaller(testCLI, home), agentPi)
		fi, err := os.Lstat(piPath(home))
		if err != nil || fi.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("the symlink must survive: %v %v", fi, err)
		}
		if readFile(t, target) != piExtensionTS(testCLI) {
			t.Error("the write must land in the link's target")
		}
	})
}
