package policy

import (
	"errors"
	"testing"
	"time"

	"github.com/nikitaShakhbazyan/lidwake-go/internal/model"
)

func restoredAssertion(pid int, acquiredAt time.Time) model.Assertion {
	return model.New("claude-code:abc", "claude-code", "", pid, "claude-code", acquiredAt, nil, model.OriginHook)
}

func livePath(path string) func(int) (string, error) {
	return func(int) (string, error) { return path, nil }
}

func deadPID(int) (string, error) { return "", errors.New("no such process") }

// Restored state must never re-pin sleep on behalf of processes that no longer exist: after a
// reboot every stored PID is recycled, and a stale assertion whose PID lands on a busy system
// process would hold disablesleep for up to the 24 h backstop.
func TestRestoreFilter(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)

	t.Run("assertions acquired before boot are dropped even if their pid is live", func(t *testing.T) {
		boot := now
		stale := restoredAssertion(500, boot.Add(-time.Hour))
		out := PartitionRestored([]model.Assertion{stale}, boot, livePath("/usr/local/bin/claude"))
		if len(out.Kept) != 0 || len(out.Dropped) != 1 {
			t.Fatalf("kept=%d dropped=%d", len(out.Kept), len(out.Dropped))
		}
	})

	t.Run("a recycled pid pointing at an unrelated binary is dropped", func(t *testing.T) {
		out := PartitionRestored([]model.Assertion{restoredAssertion(500, now)}, now.Add(-100*time.Second), livePath("/usr/libexec/mdworker"))
		if len(out.Kept) != 0 {
			t.Fatalf("kept=%v", out.Kept)
		}
	})

	t.Run("a dead pid is dropped", func(t *testing.T) {
		out := PartitionRestored([]model.Assertion{restoredAssertion(500, now)}, now.Add(-100*time.Second), deadPID)
		if len(out.Kept) != 0 || len(out.Dropped) != 1 {
			t.Fatalf("kept=%d dropped=%d", len(out.Kept), len(out.Dropped))
		}
		// A lookup that succeeds with no path has nothing to match against: the process is gone.
		out = PartitionRestored([]model.Assertion{restoredAssertion(500, now)}, now.Add(-100*time.Second), livePath(""))
		if len(out.Kept) != 0 || len(out.Dropped) != 1 {
			t.Fatalf("empty path: kept=%d dropped=%d", len(out.Kept), len(out.Dropped))
		}
	})

	t.Run("a live matching assertion from this boot is kept", func(t *testing.T) {
		out := PartitionRestored([]model.Assertion{restoredAssertion(500, now)}, now.Add(-100*time.Second), livePath("/usr/local/bin/claude"))
		if len(out.Kept) != 1 {
			t.Fatalf("kept=%d", len(out.Kept))
		}
	})

	t.Run("a pid-less sentinel assertion from this boot is kept", func(t *testing.T) {
		out := PartitionRestored([]model.Assertion{restoredAssertion(-1, now)}, now.Add(-100*time.Second), deadPID)
		if len(out.Kept) != 1 {
			t.Fatal("no pid to validate — kept; the idle sweep and backstop govern it")
		}
	})

	t.Run("nil boot time skips only the reboot check", func(t *testing.T) {
		a := restoredAssertion(500, time.Unix(0, 0))
		if out := PartitionRestored([]model.Assertion{a}, time.Time{}, livePath("/usr/local/bin/claude")); len(out.Kept) != 1 {
			t.Fatalf("kept=%d, want 1", len(out.Kept))
		}
		if out := PartitionRestored([]model.Assertion{a}, time.Time{}, livePath("/usr/libexec/mdworker")); len(out.Kept) != 0 {
			t.Fatalf("kept=%d, want 0", len(out.Kept))
		}
	})

	t.Run("name matching tolerates tool labels and versioned install paths", func(t *testing.T) {
		cases := []struct {
			stored, path string
			want         bool
		}{
			// The versioned basename doesn't match, but the claude path component does.
			{"claude-code", "/Users/u/.local/share/claude/versions/2.1.156", true},
			{"codex", "/opt/homebrew/bin/codex", true},
			{"claude-code", "/System/Library/CoreServices/WindowServer", false},
			// Trivially short components must not create accidental matches.
			{"claude-code", "/bin/sh", false},
			// Matching ignores case; a too-short stored name can't be checked and is kept.
			{"Codex", "/opt/homebrew/bin/CODEX", true},
			{"sh", "/usr/libexec/mdworker", true},
		}
		for _, c := range cases {
			if got := executablePlausiblyMatches(c.stored, c.path); got != c.want {
				t.Errorf("(%q, %q) = %v, want %v", c.stored, c.path, got, c.want)
			}
		}
	})

	t.Run("system boot time is available and in the past", func(t *testing.T) {
		t.Skip("kern.boottime is read by darwin.BootTime and tested in internal/darwin; the filter takes it as a parameter")
	})

	t.Run("partition keeps the input order", func(t *testing.T) {
		in := []model.Assertion{
			restoredAssertion(-1, now),
			restoredAssertion(500, now.Add(-2*time.Hour)),
			restoredAssertion(0, now.Add(time.Second)),
		}
		in[0].Key, in[1].Key, in[2].Key = "a", "b", "c"
		out := PartitionRestored(in, now.Add(-time.Hour), deadPID)
		if len(out.Kept) != 2 || out.Kept[0].Key != "a" || out.Kept[1].Key != "c" {
			t.Fatalf("kept=%v", out.Kept)
		}
		if len(out.Dropped) != 1 || out.Dropped[0].Key != "b" {
			t.Fatalf("dropped=%v", out.Dropped)
		}
	})
}
