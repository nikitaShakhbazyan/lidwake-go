package hooks

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// shellWrapper is the integration for agents with no hook system (Aider, Cline). It writes a
// standalone wrapper script (~/.local/bin/<tool>-lidwake) that brackets the real tool with
// acquire/release, and an alias in the user's shell rc files so the wrapper runs in place of the
// bare tool.
//
// rc files are the most valuable files this package touches, so every change is line-scoped and
// recognizer-based: only lines that are provably ours (our markers, our alias) are ever removed.
// In particular a damaged block — the user deleted the end marker — must never make everything
// after the start marker look like ours; that would truncate their rc file.
type shellWrapper struct {
	tool string // the --tool value, e.g. "aider"
	ctx  hookContext
}

// scriptPath is the standalone wrapper script, e.g. ~/.local/bin/aider-lidwake.
func (w shellWrapper) scriptPath() string {
	return w.ctx.homePath(".local", "bin", w.tool+"-lidwake")
}

// rcPaths are the shell rc files that receive the alias. Only existing files are modified; when
// neither exists, .zshrc (the macOS default shell) is created.
func (w shellWrapper) rcPaths() []string {
	return []string{w.ctx.homePath(".zshrc"), w.ctx.homePath(".bashrc")}
}

func (w shellWrapper) marker() string    { return "# lidwake-" + w.tool }
func (w shellWrapper) endMarker() string { return "# end-lidwake-" + w.tool }

// aliasLine points the bare tool name at the wrapper. The single-quoted path survives a home
// directory with spaces; an embedded single quote is escaped the POSIX way.
func (w shellWrapper) aliasLine() string {
	return "alias " + w.tool + "='" + strings.ReplaceAll(w.scriptPath(), "'", `'\''`) + "'"
}

func (w shellWrapper) rcBlock() string {
	return w.marker() + "\n" + w.aliasLine() + "\n" + w.endMarker()
}

// script is the wrapper: acquire → run the real tool → release. release carries the same --tool as
// acquire, so it targets the <tool>:$$ hold acquire placed. Inside the script the alias doesn't
// apply (non-interactive shell), so the bare name runs the real tool.
func (w shellWrapper) script() string {
	cli := w.ctx.quotedCLI()
	return "#!/usr/bin/env bash\n" +
		cli + " acquire $$ --tool " + w.tool + "\n" +
		w.tool + " \"$@\"\n" +
		"status=$?\n" +
		cli + " release $$ --tool " + w.tool + "\n" +
		"exit $status"
}

func (w shellWrapper) install(dryRun bool) (Result, error) {
	var diff strings.Builder
	changed := false

	var targets []string
	for _, rc := range w.rcPaths() {
		if exists(rc) {
			targets = append(targets, rc)
		}
	}
	if len(targets) == 0 {
		targets = w.rcPaths()[:1]
	}
	for _, rc := range targets {
		current, err := readText(rc)
		if err != nil {
			return Result{}, err
		}
		if strings.Contains(current, w.marker()) && strings.Contains(current, w.aliasLine()) {
			continue
		}
		// A drifted block (edited alias, stale wrapper path) is repaired by removing our lines and
		// appending a fresh block.
		updated := current
		if strings.Contains(current, w.marker()) {
			updated = w.removeOurLines(current)
		}
		updated += "\n" + w.rcBlock() + "\n"
		if !dryRun {
			if err := writeString(updated, rc); err != nil {
				return Result{}, err
			}
		}
		fmt.Fprintf(&diff, "+ %s: %s\n", rc, w.aliasLine())
		changed = true
	}

	// Covers both a missing script and one whose embedded CLI path drifted (the binary moved); the
	// script is wholly ours, so rewriting it is always safe.
	script := w.script()
	if existing, err := os.ReadFile(w.scriptPath()); err != nil || string(existing) != script {
		fmt.Fprintf(&diff, "+ %s (wrapper script)\n", w.scriptPath())
		if !dryRun {
			if err := w.writeScript(script); err != nil {
				return Result{}, err
			}
		}
		changed = true
	}

	if !changed {
		return Result{Summary: "already installed", Diff: unchangedDiff}, nil
	}
	return Result{Summary: "installed " + w.tool + "-lidwake wrapper + alias", Diff: diff.String()}, nil
}

func (w shellWrapper) writeScript(script string) error {
	path := w.scriptPath()
	if err := ensureParentDir(path); err != nil {
		return err
	}
	if err := writeString(script, path); err != nil {
		return err
	}
	if err := os.Chmod(path, 0o755); err != nil {
		return fmt.Errorf("make %s executable: %w", path, err)
	}
	return nil
}

func (w shellWrapper) uninstall(dryRun bool) (Result, error) {
	var diff strings.Builder
	for _, rc := range w.rcPaths() {
		data, err := os.ReadFile(rc)
		if err != nil {
			continue
		}
		current := string(data)
		updated := w.removeOurLines(current)
		if updated == current {
			continue
		}
		fmt.Fprintf(&diff, "- %s: removed alias block\n", rc)
		if !dryRun {
			if err := writeString(updated, rc); err != nil {
				return Result{}, err
			}
		}
	}

	if path := w.scriptPath(); exists(path) {
		fmt.Fprintf(&diff, "- %s\n", path)
		if !dryRun {
			if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return Result{}, fmt.Errorf("remove %s: %w", path, err)
			}
		}
	}

	if diff.Len() == 0 {
		return nothingRemoved(), nil
	}
	return Result{Summary: "removed " + w.tool + " wrapper + alias", Diff: diff.String()}, nil
}

func (w shellWrapper) state() InstallState {
	script, scriptErr := os.ReadFile(w.scriptPath())
	var withMarker []string
	for _, rc := range w.rcPaths() {
		if exists(rc) && rcContains(rc, w.marker()) {
			withMarker = append(withMarker, rc)
		}
	}
	if len(withMarker) == 0 && scriptErr != nil {
		return StateNotInstalled
	}
	// The script must match what install writes (a drifted embedded CLI path silently invokes a
	// dead binary), at least one rc file must carry the alias, and every rc file with our marker
	// must still have the alias intact.
	if scriptErr != nil || string(script) != w.script() || len(withMarker) == 0 {
		return StateModifiedExternally
	}
	for _, rc := range withMarker {
		if !rcContains(rc, w.aliasLine()) {
			return StateModifiedExternally
		}
	}
	return StateInstalled
}

// removeOurLines removes only lines that are provably ours: the markers, our alias (current or
// with a stale wrapper path), and the legacy single-line marker. User content always survives —
// even inside a damaged block whose end marker was deleted.
func (w shellWrapper) removeOurLines(content string) string {
	lines := strings.Split(content, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, w.marker()), strings.HasPrefix(trimmed, w.endMarker()):
			continue
		case strings.HasPrefix(trimmed, "# lidwake-") && strings.Contains(trimmed, w.tool):
			// Legacy single-line marker (no end marker).
			continue
		case strings.HasPrefix(trimmed, "alias "+w.tool+"=") && strings.Contains(trimmed, "-lidwake"):
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

func rcContains(path, needle string) bool {
	data, err := os.ReadFile(path)
	return err == nil && strings.Contains(string(data), needle)
}
