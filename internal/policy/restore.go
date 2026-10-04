package policy

import (
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nikitaShakhbazyan/lidwake-go/internal/model"
)

// RestoreOutcome splits the assertions restored from state.json.
type RestoreOutcome struct {
	Kept    []model.Assertion
	Dropped []model.Assertion
}

// PartitionRestored validates assertions restored from state.json at daemon start.
//
// The state file survives reboots and crashes, but the processes behind its assertions don't:
// after a reboot every stored PID is stale and densely recycled — a stored PID can name a live,
// busy system process, which the CPU-idle sweep then reads as "agent working" and pins
// disablesleep for up to the 24-hour backstop (the bag-cook scenario the cutouts exist to
// prevent). So a restored assertion is kept only when it was acquired during the current boot
// AND its PID still resolves to a plausibly-matching executable. An assertion without a PID
// (PID ≤ 0) has nothing to validate and is left to the idle sweep and the backstop.
//
// bootTime is the kernel boot time; the zero time skips the reboot check (fail-open: the PID
// checks still apply). pathOf returns the executable path of a live PID and an error when the
// process is gone — darwin.ProcessPath fits.
func PartitionRestored(restored []model.Assertion, bootTime time.Time, pathOf func(pid int) (string, error)) RestoreOutcome {
	var out RestoreOutcome
	for _, a := range restored {
		if !bootTime.IsZero() && a.AcquiredAt.Before(bootTime) {
			out.Dropped = append(out.Dropped, a)
			continue
		}
		if a.PID > 0 {
			path, err := pathOf(a.PID)
			if err != nil || path == "" || !executablePlausiblyMatches(a.ProcessName, path) {
				out.Dropped = append(out.Dropped, a)
				continue
			}
		}
		out.Kept = append(out.Kept, a)
	}
	return out
}

// executablePlausiblyMatches reports whether a live executable path plausibly belongs to the
// process the assertion was stored for. Exact comparison is impossible: the stored ProcessName
// may be a tool label ("claude-code") while the binary is `claude` — or even a versioned basename
// like `2.1.156` under a `claude/versions/` directory. So the check is containment between the
// stored name and each path component, in either direction, ignoring trivially short components.
// A false positive merely defers to the CPU-idle sweep; a recycled PID pointing at an unrelated
// binary (mdworker, WindowServer) is dropped.
func executablePlausiblyMatches(storedName, currentPath string) bool {
	stored := strings.ToLower(storedName)
	if utf8.RuneCountInString(stored) < 3 {
		return true
	}
	for _, component := range strings.Split(currentPath, "/") {
		c := strings.ToLower(component)
		if utf8.RuneCountInString(c) < 3 {
			continue
		}
		if strings.Contains(stored, c) || strings.Contains(c, stored) {
			return true
		}
	}
	return false
}
