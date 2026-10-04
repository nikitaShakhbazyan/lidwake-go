package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ExecutableStaleness is how the daemon notices that an update replaced its binary on disk, so it
// can exit and let launchd relaunch the new image. It must fire on replacement and never
// false-positive: a spurious "replaced" would restart a service that may be keeping the Mac awake.
func TestExecutableStaleness(t *testing.T) {
	makeTempFile := func(t *testing.T, contents string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "staleness")
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	// writeAtomically replaces path through a temp file and a rename, the way an atomic write
	// (and an installer) does: a new inode.
	writeAtomically := func(t *testing.T, path, contents string) {
		t.Helper()
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, path); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("an untouched file is not seen as replaced", func(t *testing.T) {
		s := NewExecutableStaleness(makeTempFile(t, "original"))
		if s.HasBeenReplaced() {
			t.Fatal("untouched file reported as replaced")
		}
		// Repeated checks stay stable.
		if s.HasBeenReplaced() {
			t.Fatal("second check reported a replacement")
		}
	})

	t.Run("overwriting the file with different content is detected as a replacement", func(t *testing.T) {
		path := makeTempFile(t, "v1")
		s := NewExecutableStaleness(path)
		if s.HasBeenReplaced() {
			t.Fatal("reported as replaced before the overwrite")
		}
		writeAtomically(t, path, "a much longer second version")
		if !s.HasBeenReplaced() {
			t.Fatal("overwrite not detected")
		}
	})

	t.Run("replacing the file via a fresh inode is detected", func(t *testing.T) {
		path := makeTempFile(t, "first generation contents")
		s := NewExecutableStaleness(path)
		// Remove and recreate: the way a replaced app bundle swaps the inode.
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		writeAtomically(t, path, "second generation, different length")
		if !s.HasBeenReplaced() {
			t.Fatal("fresh inode not detected")
		}
	})

	t.Run("a nil path never reports a replacement", func(t *testing.T) {
		if NewExecutableStaleness("").HasBeenReplaced() {
			t.Fatal("empty path reported a replacement")
		}
		var none *ExecutableStaleness
		if none.HasBeenReplaced() {
			t.Fatal("nil staleness reported a replacement")
		}
	})

	t.Run("a path that never existed never reports a replacement", func(t *testing.T) {
		if NewExecutableStaleness(filepath.Join(t.TempDir(), "nonexistent", "binary")).HasBeenReplaced() {
			t.Fatal("missing path reported a replacement")
		}
	})

	t.Run("a file deleted after launch fails safe rather than reporting a replacement", func(t *testing.T) {
		path := makeTempFile(t, "present at launch")
		s := NewExecutableStaleness(path)
		if s.HasBeenReplaced() {
			t.Fatal("reported as replaced at launch")
		}
		// Mid-replace a process can see the path gone for a moment; that is not a replacement.
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if s.HasBeenReplaced() {
			t.Fatal("deleted file reported as a replacement")
		}
	})

	t.Run("the no-argument initializer reads the running test binary without crashing", func(t *testing.T) {
		// The running test binary is not being replaced, so it must read as not replaced.
		if RunningExecutableStaleness().HasBeenReplaced() {
			t.Fatal("the running test binary reported as replaced")
		}
	})

	// Go-specific: a same-size, same-inode rewrite still counts, through the mtime.
	t.Run("an in-place rewrite of the same size is detected through the mtime", func(t *testing.T) {
		path := makeTempFile(t, "aaaa")
		s := NewExecutableStaleness(path)
		later := time.Now().Add(time.Hour)
		if err := os.WriteFile(path, []byte("bbbb"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, later, later); err != nil {
			t.Fatal(err)
		}
		if !s.HasBeenReplaced() {
			t.Fatal("in-place rewrite not detected")
		}
	})
}
