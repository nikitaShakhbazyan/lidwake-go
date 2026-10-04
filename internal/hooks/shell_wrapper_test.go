package hooks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestShellWrapperGo covers the shell-wrapper integration (Aider, Cline) beyond the ported cases,
// including what the Go port does differently.
func TestShellWrapperGo(t *testing.T) {
	rc := func(home string) string { return filepath.Join(home, ".zshrc") }
	script := func(home, tool string) string { return filepath.Join(home, ".local", "bin", tool+"-lidwake") }

	t.Run("wrapper script is exact and executable", func(t *testing.T) {
		home := fakeHome(t)
		must[Result](t)(aiderIntegration{}.install(testContext(testCLI, home), false))

		want := "#!/usr/bin/env bash\n" +
			"/usr/local/bin/lidwake acquire $$ --tool aider\n" +
			"aider \"$@\"\n" +
			"status=$?\n" +
			"/usr/local/bin/lidwake release $$ --tool aider\n" +
			"exit $status"
		if got := readFile(t, script(home, "aider")); got != want {
			t.Errorf("script:\n%s\nwant:\n%s", got, want)
		}
		fi, err := os.Stat(script(home, "aider"))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o755 {
			t.Errorf("script mode = %v, want 0755", fi.Mode().Perm())
		}
	})

	t.Run("a spaced cli path is single-quoted in the wrapper", func(t *testing.T) {
		home := fakeHome(t)
		ctx := testContext("/Apps/My $Tools/lidwake", home)
		must[Result](t)(aiderIntegration{}.install(ctx, false))
		wantContains(t, readFile(t, script(home, "aider")), "'/Apps/My $Tools/lidwake' acquire $$ --tool aider", "script")
		wantState(t, aiderIntegration{}.state(ctx), StateInstalled, "quoted path")
	})

	t.Run("rc block layout and its removal", func(t *testing.T) {
		home := fakeHome(t)
		writeFile(t, rc(home), "export A=1\n")
		ctx := testContext(testCLI, home)
		result := must[Result](t)(aiderIntegration{}.install(ctx, false))
		if result.Summary != "installed aider-lidwake wrapper + alias" {
			t.Errorf("summary = %q", result.Summary)
		}
		alias := "alias aider='" + script(home, "aider") + "'"
		want := "export A=1\n\n# lidwake-aider\n" + alias + "\n# end-lidwake-aider\n"
		if got := readFile(t, rc(home)); got != want {
			t.Errorf("zshrc = %q, want %q", got, want)
		}
		wantContains(t, result.Diff, "+ "+rc(home)+": "+alias+"\n", "diff")
		wantContains(t, result.Diff, "+ "+script(home, "aider")+" (wrapper script)\n", "diff")

		removed := must[Result](t)(aiderIntegration{}.uninstall(ctx, false))
		if removed.Summary != "removed aider wrapper + alias" {
			t.Errorf("uninstall summary = %q", removed.Summary)
		}
		if removed.Diff != "- "+rc(home)+": removed alias block\n- "+script(home, "aider")+"\n" {
			t.Errorf("uninstall diff = %q", removed.Diff)
		}
		// Only our three lines go; the separator line install added stays, as before.
		if got := readFile(t, rc(home)); got != "export A=1\n\n" {
			t.Errorf("zshrc after uninstall = %q", got)
		}
	})

	t.Run("reinstall reports already installed", func(t *testing.T) {
		home := fakeHome(t)
		writeFile(t, rc(home), "")
		ctx := testContext(testCLI, home)
		must[Result](t)(aiderIntegration{}.install(ctx, false))
		before := readFile(t, rc(home))
		again := must[Result](t)(aiderIntegration{}.install(ctx, false))
		if again.Summary != "already installed" || again.Diff != unchangedDiff {
			t.Errorf("reinstall = %+v", again)
		}
		if readFile(t, rc(home)) != before {
			t.Error("reinstall must not touch the rc file")
		}
	})

	t.Run("without rc files only zshrc is created", func(t *testing.T) {
		home := fakeHome(t)
		ctx := testContext(testCLI, home)
		must[Result](t)(aiderIntegration{}.install(ctx, false))
		wantContains(t, readFile(t, rc(home)), "# lidwake-aider", "zshrc created")
		if fileExists(filepath.Join(home, ".bashrc")) {
			t.Error("bashrc must not be created")
		}
		wantState(t, aiderIntegration{}.state(ctx), StateInstalled, "zshrc only")
	})

	t.Run("dry run touches nothing", func(t *testing.T) {
		home := fakeHome(t)
		writeFile(t, rc(home), "x\n")
		ctx := testContext(testCLI, home)
		result := must[Result](t)(aiderIntegration{}.install(ctx, true))
		wantContains(t, result.Diff, "(wrapper script)", "dry diff")
		if readFile(t, rc(home)) != "x\n" || fileExists(script(home, "aider")) {
			t.Error("a dry-run install must not write")
		}

		must[Result](t)(aiderIntegration{}.install(ctx, false))
		installed := readFile(t, rc(home))
		dry := must[Result](t)(aiderIntegration{}.uninstall(ctx, true))
		if dry.Summary != "removed aider wrapper + alias" {
			t.Errorf("dry uninstall = %+v", dry)
		}
		if readFile(t, rc(home)) != installed || !fileExists(script(home, "aider")) {
			t.Error("a dry-run uninstall must not write")
		}
	})

	t.Run("uninstall with nothing installed", func(t *testing.T) {
		home := fakeHome(t)
		writeFile(t, rc(home), "x\n")
		if got := must[Result](t)(aiderIntegration{}.uninstall(testContext(testCLI, home), false)); got != nothingRemoved() {
			t.Errorf("uninstall = %+v", got)
		}
		if readFile(t, rc(home)) != "x\n" {
			t.Error("an untouched rc file must stay byte-identical")
		}
	})

	t.Run("a drifted alias is replaced, not duplicated", func(t *testing.T) {
		home := fakeHome(t)
		writeFile(t, rc(home), "# lidwake-aider\nalias aider='/old/home/.local/bin/aider-lidwake'\n# end-lidwake-aider\nexport B=2\n")
		ctx := testContext(testCLI, home)
		wantState(t, aiderIntegration{}.state(ctx), StateModifiedExternally, "stale alias")
		must[Result](t)(aiderIntegration{}.install(ctx, false))
		after := readFile(t, rc(home))
		wantNotContains(t, after, "/old/home", "the stale alias must be gone")
		wantContains(t, after, "export B=2", "user content survives")
		if n := strings.Count(after, "alias aider="); n != 1 {
			t.Errorf("%d alias lines:\n%s", n, after)
		}
		wantState(t, aiderIntegration{}.state(ctx), StateInstalled, "repaired")
	})

	t.Run("a marked rc file missing the alias reads as modified", func(t *testing.T) {
		home := fakeHome(t)
		writeFile(t, rc(home), "")
		writeFile(t, filepath.Join(home, ".bashrc"), "")
		ctx := testContext(testCLI, home)
		must[Result](t)(aiderIntegration{}.install(ctx, false))
		writeFile(t, filepath.Join(home, ".bashrc"), "# lidwake-aider\n# end-lidwake-aider\n")
		wantState(t, aiderIntegration{}.state(ctx), StateModifiedExternally, "alias deleted")

		must[Result](t)(aiderIntegration{}.install(ctx, false))
		wantState(t, aiderIntegration{}.state(ctx), StateInstalled, "repaired")
	})

	t.Run("a script without rc wiring reads as modified", func(t *testing.T) {
		home := fakeHome(t)
		ctx := testContext(testCLI, home)
		must[Result](t)(aiderIntegration{}.install(ctx, false))
		writeFile(t, rc(home), "")
		wantState(t, aiderIntegration{}.state(ctx), StateModifiedExternally, "script only")
		if err := os.Remove(script(home, "aider")); err != nil {
			t.Fatal(err)
		}
		wantState(t, aiderIntegration{}.state(ctx), StateNotInstalled, "nothing left")
	})

	t.Run("aider and cline blocks coexist", func(t *testing.T) {
		home := fakeHome(t)
		writeFile(t, rc(home), "")
		ctx := testContext(testCLI, home)
		must[Result](t)(aiderIntegration{}.install(ctx, false))
		cline := must[Result](t)(clineIntegration{}.install(ctx, false))
		if cline.Summary != "installed cline-lidwake wrapper + alias" {
			t.Errorf("cline summary = %q", cline.Summary)
		}
		wantContains(t, readFile(t, script(home, "cline")), "cline \"$@\"", "cline script runs cline")
		wantContains(t, readFile(t, script(home, "cline")), "release $$ --tool cline", "cline script")

		removed := must[Result](t)(aiderIntegration{}.uninstall(ctx, false))
		wantNotContains(t, removed.Diff, "cline-lidwake", "aider uninstall must not touch cline")
		after := readFile(t, rc(home))
		wantNotContains(t, after, "lidwake-aider", "aider block gone")
		wantNotContains(t, after, "alias aider", "aider alias gone")
		wantContains(t, after, "# lidwake-cline\nalias cline='"+script(home, "cline")+"'\n# end-lidwake-cline", "cline block intact")
		wantState(t, clineIntegration{}.state(ctx), StateInstalled, "cline")
		wantState(t, aiderIntegration{}.state(ctx), StateNotInstalled, "aider")
		if got := must[Result](t)(clineIntegration{}.uninstall(ctx, false)); got.Summary != "removed cline wrapper + alias" {
			t.Errorf("cline uninstall = %+v", got)
		}
	})

	t.Run("a single quote in the home path is escaped in the alias", func(t *testing.T) {
		home := filepath.Join(t.TempDir(), "o'brien")
		if err := os.Mkdir(home, 0o755); err != nil {
			t.Fatal(err)
		}
		ctx := testContext(testCLI, home)
		must[Result](t)(aiderIntegration{}.install(ctx, false))
		wantContains(t, readFile(t, rc(home)), `alias aider='`+strings.ReplaceAll(script(home, "aider"), "'", `'\''`)+`'`, "alias")
		wantState(t, aiderIntegration{}.state(ctx), StateInstalled, "quoted home")
		must[Result](t)(aiderIntegration{}.uninstall(ctx, false))
		wantNotContains(t, readFile(t, rc(home)), "alias aider", "uninstall recognizes the escaped alias")
	})

	t.Run("rc bytes that aren't utf-8 round-trip", func(t *testing.T) {
		home := fakeHome(t)
		original := "# caf\xe9 \xff\nexport C=3\n"
		writeFile(t, rc(home), original)
		ctx := testContext(testCLI, home)
		must[Result](t)(aiderIntegration{}.install(ctx, false))
		if got := readFile(t, rc(home)); !strings.HasPrefix(got, original) {
			t.Errorf("install must keep the user's bytes: %q", got)
		}
		must[Result](t)(aiderIntegration{}.uninstall(ctx, false))
		if got := readFile(t, rc(home)); !strings.HasPrefix(got, original) || strings.Contains(got, "lidwake") {
			t.Errorf("uninstall must keep the user's bytes: %q", got)
		}
	})

	t.Run("an unreadable rc file is refused, never overwritten", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root reads files regardless of mode")
		}
		home := fakeHome(t)
		writeFile(t, rc(home), "secret config\n")
		if err := os.Chmod(rc(home), 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(rc(home), 0o644) })

		if _, err := (aiderIntegration{}).install(testContext(testCLI, home), false); err == nil {
			t.Fatal("install over an unreadable rc file must fail")
		}
		if err := os.Chmod(rc(home), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := readFile(t, rc(home)); got != "secret config\n" {
			t.Errorf("rc file changed: %q", got)
		}
	})

	t.Run("summaries match the CLI wording", func(t *testing.T) {
		home := fakeHome(t)
		in := testInstaller(testCLI, home)
		ctx := testContext(testCLI, home)
		must[Result](t)(clineIntegration{}.install(ctx, false))
		wantState(t, in.State(agentCline), StateInstalled, "cline via installer")
		if got := mustUninstall(t, in, agentCline); got.Summary != "removed cline wrapper + alias" {
			t.Errorf("uninstall = %+v", got)
		}
		if got := mustUninstall(t, in, agentCline); got != nothingRemoved() {
			t.Errorf("second uninstall = %+v", got)
		}
	})
}
