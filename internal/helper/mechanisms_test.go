package helper

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nikitaShakhbazyan/lidwake-go/internal/policy"
)

func TestIdleAssertion(t *testing.T) {
	newFake := func(fail *error) (*idleAssertion, *int, *int) {
		var created, released int
		a := &idleAssertion{
			create: func() (func(), error) {
				if *fail != nil {
					return nil, *fail
				}
				created++
				return func() { released++ }, nil
			},
			log: discard,
		}
		return a, &created, &released
	}

	t.Run("acquire is idempotent", func(t *testing.T) {
		var fail error
		a, created, _ := newFake(&fail)
		a.Acquire()
		a.Acquire()
		if !a.IsHeld() || *created != 1 {
			t.Fatalf("held=%v created=%d, want one assertion", a.IsHeld(), *created)
		}
	})

	t.Run("release is idempotent and safe before any acquire", func(t *testing.T) {
		var fail error
		a, _, released := newFake(&fail)
		a.Release()
		a.Acquire()
		a.Release()
		a.Release()
		if a.IsHeld() || *released != 1 {
			t.Fatalf("held=%v released=%d, want released once", a.IsHeld(), *released)
		}
	})

	t.Run("a failed create leaves nothing held and the next acquire retries", func(t *testing.T) {
		fail := errors.New("kIOReturnError")
		a, created, _ := newFake(&fail)
		a.Acquire()
		if a.IsHeld() {
			t.Fatal("held after a failed create")
		}
		fail = nil
		a.Acquire()
		if !a.IsHeld() || *created != 1 {
			t.Fatal("the retry did not create the assertion")
		}
	})
}

func TestOriginalFile(t *testing.T) {
	newFile := func(t *testing.T) originalFile {
		return originalFile{path: filepath.Join(t.TempDir(), "db", "sleep-disabled-before"), log: discard}
	}

	t.Run("nothing saved reads as not saved", func(t *testing.T) {
		if _, ok := newFile(t).Load(); ok {
			t.Fatal("a missing file read as saved")
		}
	})

	t.Run("round trips both values as 1 and 0", func(t *testing.T) {
		for _, value := range []bool{true, false} {
			f := newFile(t)
			if err := f.Save(value); err != nil {
				t.Fatal(err)
			}
			got, ok := f.Load()
			if !ok || got != value {
				t.Fatalf("Load after Save(%v) = (%v, %v)", value, got, ok)
			}
			data, _ := os.ReadFile(f.path)
			want := map[bool]string{true: "1", false: "0"}[value]
			if string(data) != want {
				t.Fatalf("file holds %q, want %q", data, want)
			}
		}
	})

	t.Run("save overwrites an earlier value", func(t *testing.T) {
		f := newFile(t)
		_ = f.Save(true)
		if err := f.Save(false); err != nil {
			t.Fatal(err)
		}
		if got, ok := f.Load(); !ok || got {
			t.Fatalf("Load = (%v, %v), want (false, true)", got, ok)
		}
	})

	t.Run("save creates the parent directory 0755 and the file 0644, leaving no temporary files", func(t *testing.T) {
		f := newFile(t)
		if err := f.Save(true); err != nil {
			t.Fatal(err)
		}
		dir, err := os.Stat(filepath.Dir(f.path))
		if err != nil {
			t.Fatal(err)
		}
		if dir.Mode().Perm() != 0o755 {
			t.Fatalf("directory mode = %v, want 0755", dir.Mode().Perm())
		}
		file, err := os.Stat(f.path)
		if err != nil {
			t.Fatal(err)
		}
		if file.Mode().Perm() != 0o644 {
			t.Fatalf("file mode = %v, want 0644", file.Mode().Perm())
		}
		entries, _ := os.ReadDir(filepath.Dir(f.path))
		if len(entries) != 1 {
			t.Fatalf("directory holds %d entries, want only the value file", len(entries))
		}
	})

	t.Run("garbage reads as not saved", func(t *testing.T) {
		for _, garbage := range []string{"", "yes", "true", "2", "10", "01", "-1", "1 0", "\x00"} {
			f := newFile(t)
			_ = os.MkdirAll(filepath.Dir(f.path), 0o755)
			if err := os.WriteFile(f.path, []byte(garbage), 0o644); err != nil {
				t.Fatal(err)
			}
			if got, ok := f.Load(); ok {
				t.Errorf("%q read as saved %v", garbage, got)
			}
		}
	})

	t.Run("surrounding whitespace is ignored", func(t *testing.T) {
		for content, want := range map[string]bool{"1\n": true, " 0 ": false, "\t1\r\n": true} {
			f := newFile(t)
			_ = os.MkdirAll(filepath.Dir(f.path), 0o755)
			_ = os.WriteFile(f.path, []byte(content), 0o644)
			if got, ok := f.Load(); !ok || got != want {
				t.Errorf("%q = (%v, %v), want (%v, true)", content, got, ok, want)
			}
		}
	})

	t.Run("clear removes the value and is safe to repeat", func(t *testing.T) {
		f := newFile(t)
		_ = f.Save(true)
		f.Clear()
		if _, ok := f.Load(); ok {
			t.Fatal("still saved after Clear")
		}
		if _, err := os.Stat(f.path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("file still present: %v", err)
		}
		f.Clear()
	})

	t.Run("save fails when the directory cannot be created", func(t *testing.T) {
		blocker := filepath.Join(t.TempDir(), "file")
		_ = os.WriteFile(blocker, nil, 0o644)
		f := originalFile{path: filepath.Join(blocker, "db", "v"), log: discard}
		if err := f.Save(true); err == nil {
			t.Fatal("Save succeeded under a regular file")
		}
	})
}

// TestPolicyRestoreOnStart covers crash and boot recovery with the real file store: the
// LaunchDaemon runs at load, and building the policy puts back what a previous instance saved.
func TestPolicyRestoreOnStart(t *testing.T) {
	t.Run("a value saved by a previous instance is restored and cleared", func(t *testing.T) {
		for _, saved := range []bool{false, true} {
			f := originalFile{path: filepath.Join(t.TempDir(), "sleep-disabled-before"), log: discard}
			_ = f.Save(saved)
			clamshell := &fakeClamshell{disabled: true} // left blocked by a crashed helper
			p := policy.NewSleepBlockPolicy(&fakeIdle{}, clamshell, f)
			if clamshell.IsDisabled() != saved {
				t.Fatalf("disablesleep = %v after start, want the saved %v", clamshell.IsDisabled(), saved)
			}
			if _, ok := f.Load(); ok {
				t.Fatal("the saved value survived a successful restore")
			}
			if p.Blocked() {
				t.Fatal("a fresh policy reports blocked")
			}
		}
	})

	t.Run("nothing saved leaves disablesleep alone", func(t *testing.T) {
		f := originalFile{path: filepath.Join(t.TempDir(), "sleep-disabled-before"), log: discard}
		clamshell := &fakeClamshell{disabled: true}
		policy.NewSleepBlockPolicy(&fakeIdle{}, clamshell, f)
		if clamshell.callCount() != 0 || !clamshell.IsDisabled() {
			t.Fatal("start touched disablesleep with nothing saved")
		}
	})

	t.Run("garbage in the file leaves disablesleep alone", func(t *testing.T) {
		f := originalFile{path: filepath.Join(t.TempDir(), "sleep-disabled-before"), log: discard}
		_ = os.WriteFile(f.path, []byte("maybe"), 0o644)
		clamshell := &fakeClamshell{disabled: true}
		policy.NewSleepBlockPolicy(&fakeIdle{}, clamshell, f)
		if clamshell.callCount() != 0 {
			t.Fatal("start acted on a garbage value")
		}
	})

	t.Run("a failed restore keeps the saved value for the next start", func(t *testing.T) {
		f := originalFile{path: filepath.Join(t.TempDir(), "sleep-disabled-before"), log: discard}
		_ = f.Save(false)
		clamshell := &fakeClamshell{disabled: true, failOn: map[bool]error{false: errors.New("pmset timed out")}}
		policy.NewSleepBlockPolicy(&fakeIdle{}, clamshell, f)
		if saved, ok := f.Load(); !ok || saved {
			t.Fatalf("saved value = (%v, %v), want (false, true) kept for a retry", saved, ok)
		}
	})

	t.Run("a block after a restart saves the then-current value", func(t *testing.T) {
		f := originalFile{path: filepath.Join(t.TempDir(), "sleep-disabled-before"), log: discard}
		_ = f.Save(false)
		clamshell := &fakeClamshell{disabled: true}
		p := policy.NewSleepBlockPolicy(&fakeIdle{}, clamshell, f)
		if err := p.Set(true); err != nil {
			t.Fatal(err)
		}
		if saved, ok := f.Load(); !ok || saved {
			t.Fatalf("saved = (%v, %v), want the restored false", saved, ok)
		}
		_ = p.Set(false)
		if clamshell.IsDisabled() {
			t.Fatal("release did not put back sleep allowed")
		}
	})
}

func TestRunRequiresRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root would start the real helper")
	}
	err := Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "must run as root") {
		t.Fatalf("Run as a normal user = %v, want a must-run-as-root error", err)
	}
}
