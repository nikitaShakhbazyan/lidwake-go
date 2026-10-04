package helper

import (
	"os"
	"path/filepath"
	"testing"
)

// The replacement check must fire on a replaced binary and never false-positive: a spurious
// "replaced" would restart a helper that may be keeping the Mac awake.
func TestExecutableStaleness(t *testing.T) {
	tempFile := func(t *testing.T, contents string) string {
		path := filepath.Join(t.TempDir(), "binary")
		if err := os.WriteFile(path, []byte(contents), 0o755); err != nil {
			t.Fatal(err)
		}
		return path
	}

	t.Run("an untouched file is not seen as replaced", func(t *testing.T) {
		replaced := replacementCheck(tempFile(t, "original"))
		if replaced() || replaced() {
			t.Fatal("an untouched file reads as replaced")
		}
	})

	t.Run("overwriting the file with different content is detected as a replacement", func(t *testing.T) {
		path := tempFile(t, "v1")
		replaced := replacementCheck(path)
		if replaced() {
			t.Fatal("replaced before any change")
		}
		// In place: same inode, different size.
		if err := os.WriteFile(path, []byte("a much longer second version"), 0o755); err != nil {
			t.Fatal(err)
		}
		if !replaced() {
			t.Fatal("an in-place overwrite went unnoticed")
		}
	})

	t.Run("replacing the file via a fresh inode is detected", func(t *testing.T) {
		path := tempFile(t, "first generation contents")
		replaced := replacementCheck(path)
		// An atomic install renames a new file over the old one, swapping the inode — even with
		// identical size.
		next := path + ".new"
		if err := os.WriteFile(next, []byte("first generation contents"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(next, path); err != nil {
			t.Fatal(err)
		}
		if !replaced() {
			t.Fatal("a rename over the binary went unnoticed")
		}
	})

	t.Run("a nil path never reports a replacement", func(t *testing.T) {
		if replacementCheck("")() {
			t.Fatal("an empty path reads as replaced")
		}
	})

	t.Run("a path that never existed never reports a replacement", func(t *testing.T) {
		if replacementCheck(filepath.Join(t.TempDir(), "missing", "binary"))() {
			t.Fatal("a missing path reads as replaced")
		}
	})

	t.Run("a file deleted after launch fails safe rather than reporting a replacement", func(t *testing.T) {
		path := tempFile(t, "present at launch")
		replaced := replacementCheck(path)
		// Mid-replace the path can momentarily be gone.
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if replaced() {
			t.Fatal("a vanished file reads as replaced")
		}
	})

	t.Run("the no-argument initializer reads the running test binary without crashing", func(t *testing.T) {
		exe := runningExecutable()
		if exe == "" || !filepath.IsAbs(exe) {
			t.Fatalf("runningExecutable() = %q, want an absolute path", exe)
		}
		if replacementCheck(exe)() {
			t.Fatal("the running test binary reads as replaced")
		}
	})
}
