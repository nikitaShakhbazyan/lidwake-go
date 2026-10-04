package hooks

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// filePlugin is the integration for agents that auto-discover plugins or extensions from a
// directory (OpenCode, Pi). lidwake owns exactly one file there: install writes it, uninstall
// removes only that file (never the shared directory, which may hold the user's other plugins),
// and the state compares the file on disk with the canonical content, so the two never drift.
type filePlugin struct {
	root     string
	fileName string
	// content is the canonical file content, also used to detect external modification.
	content        string
	installSummary string
}

func (p filePlugin) path() string { return filepath.Join(p.root, p.fileName) }

func (p filePlugin) install(dryRun bool) (Result, error) {
	path := p.path()
	existing, err := os.ReadFile(path)
	if err == nil && string(existing) == p.content {
		return Result{Summary: "already installed", Diff: unchangedDiff}, nil
	}
	if !dryRun {
		if err := os.MkdirAll(p.root, 0o755); err != nil {
			return Result{}, fmt.Errorf("create %s: %w", p.root, err)
		}
		if err := writeString(p.content, path); err != nil {
			return Result{}, err
		}
	}
	// The file is wholly ours, so overwriting a modified copy is intended — but the diff must say
	// so rather than pretend a fresh create.
	diff := "+ " + path
	if !errors.Is(err, fs.ErrNotExist) {
		diff = "~ " + path + " (rewritten to canonical content)"
	}
	return Result{Summary: p.installSummary, Diff: diff}, nil
}

func (p filePlugin) uninstall(dryRun bool) (Result, error) {
	path := p.path()
	if !exists(path) {
		return nothingRemoved(), nil
	}
	if !dryRun {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return Result{}, fmt.Errorf("remove %s: %w", path, err)
		}
	}
	return Result{Summary: "removed plugin file", Diff: "- " + path}, nil
}

func (p filePlugin) state() InstallState {
	data, err := os.ReadFile(p.path())
	if err != nil || !strings.Contains(string(data), "lidwake") {
		return StateNotInstalled
	}
	if strings.TrimSpace(string(data)) == strings.TrimSpace(p.content) {
		return StateInstalled
	}
	return StateModifiedExternally
}
