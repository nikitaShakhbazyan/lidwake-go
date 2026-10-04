package helper

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/nikitaShakhbazyan/lidwake-go/internal/darwin"
)

// The helper owns the two real mechanisms behind policy.SleepBlockPolicy, where the compose,
// idempotence and crash-recovery logic lives:
//
//  1. Idle system sleep: a standard, reference-counted IOPMAssertion
//     (PreventUserIdleSystemSleep). The kernel releases it if the helper dies, and it shows in
//     `pmset -g assertions`, so the user can see lidwake is active. By its own header it does not
//     survive a lid close.
//  2. Clamshell (lid-closed) sleep: the global SleepDisabled setting, applied with
//     `pmset -a disablesleep`. The in-process IOKit routes either fail as root or report success
//     while the Mac still sleeps on lid close; pmset is Apple's tested implementation and runs
//     only when the block flips. disablesleep is not cleared when the helper dies and can reset
//     across a sleep/wake cycle, so the policy restores it at start and on release, and the
//     daemon re-applies the block on wake.

// idleAssertionName is shown in `pmset -g assertions`; ASCII only, pmset renders anything else
// as "?".
const idleAssertionName = "lidwake: agent active"

// idleAssertion holds at most one idle-sleep assertion: Acquire while held is a no-op, which
// the policy relies on when the daemon re-asserts the block after a wake. Not synchronized: the
// policy calls it under its own lock.
type idleAssertion struct {
	create  func() (release func(), err error)
	release func()
	log     *slog.Logger
}

func newIdleAssertion(log *slog.Logger) *idleAssertion {
	return &idleAssertion{
		create: func() (func(), error) {
			a, err := darwin.CreateAssertion(darwin.PreventUserIdleSystemSleep, idleAssertionName)
			if err != nil {
				return nil, err
			}
			return a.Release, nil
		},
		log: log,
	}
}

func (a *idleAssertion) IsHeld() bool { return a.release != nil }

func (a *idleAssertion) Acquire() {
	if a.release != nil {
		return
	}
	release, err := a.create()
	if err != nil {
		a.log.Error("creating the idle-sleep assertion failed", "err", err)
		return
	}
	a.release = release
	a.log.Info("idle-sleep assertion created")
}

func (a *idleAssertion) Release() {
	if a.release == nil {
		return
	}
	a.release()
	a.release = nil
	a.log.Info("idle-sleep assertion released")
}

// pmsetClamshell is the clamshell block through the SleepDisabled setting. SetDisabled fails
// when pmset exits non-zero or hangs past its watchdog; that error reaches the daemon on block
// and is logged on unblock.
type pmsetClamshell struct{}

func (pmsetClamshell) IsDisabled() bool { return darwin.SleepDisabled() }

func (pmsetClamshell) SetDisabled(disabled bool) error { return darwin.SetSleepDisabled(disabled) }

// originalFile stores the pre-block SleepDisabled value as "1" or "0" (paths.OriginalSleepSetting),
// under a root-only directory so no user process can plant a value for the helper to "restore".
// Anything else in the file, or no file, reads as nothing saved.
type originalFile struct {
	path string
	log  *slog.Logger
}

func (f originalFile) Load() (disabled bool, ok bool) {
	data, err := os.ReadFile(f.path)
	if err != nil {
		return false, false
	}
	switch strings.TrimSpace(string(data)) {
	case "1":
		return true, true
	case "0":
		return false, true
	default:
		return false, false
	}
}

// Save writes the value atomically and durably: it is the crash-recovery record, read back after
// a crash or a reboot.
func (f originalFile) Save(disabled bool) error {
	dir := filepath.Dir(f.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		return fmt.Errorf("chmod %s: %w", dir, err)
	}
	value := "0"
	if disabled {
		value = "1"
	}
	return writeFileAtomic(f.path, []byte(value))
}

func (f originalFile) Clear() {
	if err := os.Remove(f.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		f.logger().Warn("removing the saved sleep setting failed", "path", f.path, "err", err)
	}
}

func (f originalFile) logger() *slog.Logger {
	if f.log == nil {
		return slog.Default()
	}
	return f.log
}

// writeFileAtomic replaces path with data (mode 0644) through a synced temporary file and a
// rename, so a crash leaves either the old value or the new one, never a torn file.
func writeFileAtomic(path string, data []byte) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	defer func() {
		if err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err = tmp.Chmod(0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err = tmp.Sync(); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err = os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
