package policy

import (
	"math"
	"regexp"
	"testing"
	"time"
)

func TestManualHold(t *testing.T) {
	t.Run("clampTTL defaults to one hour when no duration is given", func(t *testing.T) {
		if got := ClampHoldTTL(nil, 4); got != 3600 {
			t.Fatalf("got %v", got)
		}
	})
	t.Run("clampTTL caps an over-long request", func(t *testing.T) {
		if got := ClampHoldTTL(f64(10*3600), 4); got != 4*3600 {
			t.Fatalf("got %v", got)
		}
	})
	t.Run("clampTTL passes a within-cap request through", func(t *testing.T) {
		if got := ClampHoldTTL(f64(1800), 4); got != 1800 {
			t.Fatalf("got %v", got)
		}
	})
	t.Run("clampTTL floors at one second", func(t *testing.T) {
		if got := ClampHoldTTL(f64(0), 4); got != 1 {
			t.Errorf("0: got %v", got)
		}
		if got := ClampHoldTTL(f64(-50), 4); got != 1 {
			t.Errorf("-50: got %v", got)
		}
	})
	t.Run("clampTTL treats non-finite requests safely", func(t *testing.T) {
		if got := ClampHoldTTL(f64(math.NaN()), 4); got != 3600 {
			t.Errorf("NaN: got %v, want the default", got)
		}
		if got := ClampHoldTTL(f64(math.Inf(1)), 4); got != 4*3600 {
			t.Errorf("+Inf: got %v, want the cap", got)
		}
		if got := ClampHoldTTL(f64(math.Inf(-1)), 4); got != 1 {
			t.Errorf("-Inf: got %v, want the floor", got)
		}
	})

	t.Run("newKey is namespaced and recognized", func(t *testing.T) {
		key := NewHoldKey()
		if !regexp.MustCompile(`^hold:[0-9a-f]{8}$`).MatchString(key) {
			t.Fatalf("key %q", key)
		}
		if !IsHoldKey(key) {
			t.Fatal("not recognized")
		}
		if IsHoldKey("claude-code:abc123") {
			t.Fatal("hook key recognized as a hold")
		}
	})
	t.Run("newKey is unique across calls", func(t *testing.T) {
		if NewHoldKey() == NewHoldKey() {
			t.Fatal("collision")
		}
	})

	sessionCases := []struct {
		name            string
		tool, sessionID string
		want            string
	}{
		{"sessionKey prefixes a bare session id", "opencode", "ses_xyz", "opencode:ses_xyz"},
		{"sessionKey passes a hold id through verbatim", "manual", "hold:ab12cd34", "hold:ab12cd34"},
		// `status --json` prints the full <tool>:<session> key; release fed that form back must
		// target the same assertion, not a double-prefixed <tool>:<tool>:<session>.
		{"sessionKey passes an already-prefixed key through verbatim", "opencode", "opencode:ses_xyz", "opencode:ses_xyz"},
		{"sessionKey passes an already-prefixed key through verbatim", "claude-code", "claude-code:f9b4e284", "claude-code:f9b4e284"},
		// Only THIS tool's prefix is a passthrough — a session id that merely contains a colon (or
		// another tool's key under a mismatched --tool) still gets the standard derivation.
		{"sessionKey still prefixes a foreign tool prefix", "cursor", "opencode:ses_xyz", "cursor:opencode:ses_xyz"},
		// With no --tool, a colon-bearing id is a pasted full key, not a session id.
		{"sessionKey passes a prefixed key through when no tool is named", UnknownTool, "opencode:repro-25", "opencode:repro-25"},
		{"sessionKey passes a prefixed key through when no tool is named", UnknownTool, "pi:/Users/u/.pi/agent/sessions/s.jsonl", "pi:/Users/u/.pi/agent/sessions/s.jsonl"},
		{"sessionKey still prefixes a bare id when no tool is named", UnknownTool, "repro-25", "unknown:repro-25"},
	}
	for _, c := range sessionCases {
		t.Run(c.name, func(t *testing.T) {
			if got := SessionKey(c.tool, c.sessionID); got != c.want {
				t.Fatalf("SessionKey(%q, %q) = %q, want %q", c.tool, c.sessionID, got, c.want)
			}
		})
	}
	t.Run("sessionKey derivation is idempotent", func(t *testing.T) {
		once := SessionKey("opencode", "ses_xyz")
		if again := SessionKey("opencode", once); again != once {
			t.Fatalf("%q → %q", once, again)
		}
	})

	acquired := time.Unix(1_000_000, 0)
	t.Run("clampExpiry leaves a TTL-less assertion untouched", func(t *testing.T) {
		// Per-turn and sub-agent hooks carry no TTL; the idle policy governs them, not a deadline.
		if got := ClampExpiry(nil, acquired, 4); got != nil {
			t.Fatalf("got %v", *got)
		}
	})
	t.Run("clampExpiry caps an over-long expiry to the max-hold", func(t *testing.T) {
		// The background-shell hook requests the 24 h ceiling; the daemon must bring it down to the
		// user's live cap so a background task can't pin the Mac past it.
		requested := acquired.Add(24 * time.Hour)
		got := ClampExpiry(&requested, acquired, 4)
		if got == nil || !got.Equal(acquired.Add(4*time.Hour)) {
			t.Fatalf("got %v", got)
		}
	})
	t.Run("clampExpiry passes a within-cap expiry through", func(t *testing.T) {
		requested := acquired.Add(30 * time.Minute)
		got := ClampExpiry(&requested, acquired, 4)
		if got == nil || !got.Equal(requested) {
			t.Fatalf("got %v", got)
		}
		if got == &requested {
			t.Fatal("returned the caller's pointer")
		}
	})
	t.Run("clampExpiry keeps a positive floor for a zero-hour cap", func(t *testing.T) {
		// A degenerate cap must still yield a future expiry, never acquiredAt or earlier.
		requested := acquired.Add(10 * time.Hour)
		got := ClampExpiry(&requested, acquired, 0)
		if got == nil || !got.Equal(acquired.Add(time.Second)) {
			t.Fatalf("got %v", got)
		}
	})
	// Settings clamp the cap to 0.25–24 h, but the clamp must hold on its own: a NaN cap counts
	// as the one-second floor and an infinite one saturates instead of overflowing the expiry.
	t.Run("clampExpiry tolerates a non-finite cap", func(t *testing.T) {
		requested := acquired.Add(10 * time.Hour)
		if got := ClampExpiry(&requested, acquired, math.NaN()); got == nil || !got.Equal(acquired.Add(time.Second)) {
			t.Errorf("NaN cap: got %v", got)
		}
		if got := ClampExpiry(&requested, acquired, math.Inf(1)); got == nil || !got.Equal(requested) {
			t.Errorf("+Inf cap: got %v", got)
		}
		if got := ClampHoldTTL(f64(1800), math.NaN()); got != 1 {
			t.Errorf("NaN cap TTL: got %v", got)
		}
	})
}

func TestDurationParser(t *testing.T) {
	ok := func(t *testing.T, in string, want float64) {
		t.Helper()
		got, valid := ParseDurationSeconds(in)
		if !valid || got != want {
			t.Errorf("%q: got %v (ok=%v), want %v", in, got, valid, want)
		}
	}
	bad := func(t *testing.T, in string) {
		t.Helper()
		if got, valid := ParseDurationSeconds(in); valid {
			t.Errorf("%q: got %v, want rejection", in, got)
		}
	}
	t.Run("bare number is seconds", func(t *testing.T) {
		ok(t, "90", 90)
		ok(t, "1.5", 1.5)
	})
	t.Run("single units", func(t *testing.T) {
		ok(t, "30s", 30)
		ok(t, "45m", 2700)
		ok(t, "2h", 7200)
		ok(t, "1d", 86400)
		ok(t, "1.5h", 5400)
	})
	t.Run("compound durations sum", func(t *testing.T) {
		ok(t, "1h30m", 5400)
		ok(t, "2h15m30s", 8130)
	})
	t.Run("case-insensitive and whitespace-tolerant", func(t *testing.T) {
		ok(t, " 2H ", 7200)
		ok(t, "\t90M\t", 5400)
	})
	t.Run("garbage and ambiguous trailing digits are rejected", func(t *testing.T) {
		bad(t, "")
		bad(t, "soon")
		bad(t, "1h30")
		bad(t, "5x")
		bad(t, "h")
		bad(t, "1..5h")
		bad(t, "-5")
		bad(t, "1h-5m")
	})
}
