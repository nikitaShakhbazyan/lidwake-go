package hooks

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testCLI = "/usr/local/bin/lidwake"

// fakeHome is a temp home with the given subdirectories created, so the matching agents read as
// detected.
func fakeHome(t *testing.T, dirs ...string) string {
	t.Helper()
	home := t.TempDir()
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(home, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

// testInstaller is fully hermetic: no real /Applications, no real PATH.
func testInstaller(cliPath, home string) *Installer {
	return &Installer{
		CLIPath:         cliPath,
		Home:            home,
		ApplicationsDir: filepath.Join(home, "Applications"),
		SearchPath:      []string{},
	}
}

func testContext(cliPath, home string) hookContext {
	return testInstaller(cliPath, home).context()
}

func readJSONFile(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return m
}

func writeJSONFile(t *testing.T, obj any, path string) {
	t.Helper()
	data, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, string(data))
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func fileExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// hooksIn returns the "hooks" object of the JSON file at path.
func hooksIn(t *testing.T, path string) map[string]any {
	t.Helper()
	return object(t, readJSONFile(t, path)["hooks"], "hooks")
}

func object(t *testing.T, v any, what string) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%s: want object, got %T", what, v)
	}
	return m
}

// objects requires v to be an array of objects.
func objects(t *testing.T, v any, what string) []map[string]any {
	t.Helper()
	arr, ok := v.([]any)
	if !ok {
		t.Fatalf("%s: want array, got %T (%v)", what, v, v)
	}
	out := make([]map[string]any, len(arr))
	for i, e := range arr {
		out[i] = object(t, e, what)
	}
	return out
}

// firstInnerCommand is the command of the first handler of the first group under event.
func firstInnerCommand(t *testing.T, hooks map[string]any, event string) string {
	t.Helper()
	groups := objects(t, hooks[event], event)
	if len(groups) == 0 {
		t.Fatalf("%s: no groups", event)
	}
	inner := objects(t, groups[0]["hooks"], event+" inner hooks")
	if len(inner) == 0 {
		t.Fatalf("%s: no handlers", event)
	}
	cmd, ok := inner[0]["command"].(string)
	if !ok {
		t.Fatalf("%s: handler has no command", event)
	}
	return cmd
}

func must[T any](t *testing.T) func(T, error) T {
	return func(v T, err error) T {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
}

func mustInstall(t *testing.T, in *Installer, agent string) Result {
	t.Helper()
	return must[Result](t)(in.Install(agent, false))
}

func mustUninstall(t *testing.T, in *Installer, agent string) Result {
	t.Helper()
	return must[Result](t)(in.Uninstall(agent, false))
}

// requireSkip requires err to be a *SkipError.
func requireSkip(t *testing.T, err error) *SkipError {
	t.Helper()
	var s *SkipError
	if !errors.As(err, &s) {
		t.Fatalf("want *SkipError, got %v", err)
	}
	return s
}

func wantState(t *testing.T, got, want InstallState, msg string) {
	t.Helper()
	if got != want {
		t.Errorf("%s: state = %s, want %s", msg, got, want)
	}
}

func wantContains(t *testing.T, s, sub, msg string) {
	t.Helper()
	if !strings.Contains(s, sub) {
		t.Errorf("%s: %q does not contain %q", msg, s, sub)
	}
}

func wantNotContains(t *testing.T, s, sub, msg string) {
	t.Helper()
	if strings.Contains(s, sub) {
		t.Errorf("%s: %q contains %q", msg, s, sub)
	}
}

// wantSkip requires err to be a *SkipError with the given reason.
func wantSkip(t *testing.T, err error, reason SkipReason) {
	t.Helper()
	if s := requireSkip(t, err); s.Reason != reason {
		t.Errorf("skip reason = %v, want %v (%v)", s.Reason, reason, err)
	}
}
