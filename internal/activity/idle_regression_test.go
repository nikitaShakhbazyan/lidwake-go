package activity

import (
	"testing"
	"time"

	"github.com/nikitaShakhbazyan/lidwake-go/internal/model"
)

// Regressions for the cross-sweep bookkeeping: the evaluator's per-PID state outlives an
// assertion (polling stops when nothing is held, then resumes), and stale baselines must never
// release a freshly acquired assertion mid-turn.

func codexAssertion(acquiredAt time.Time, opts ...assertionOpt) model.Assertion {
	a := model.New("k", "codex", "", 100, "codex", acquiredAt, nil, model.OriginHook)
	for _, o := range opts {
		o(&a)
	}
	return a
}

func TestIdleReleaseEvaluatorBookkeeping(t *testing.T) {
	// A new assertion on a PID with stale bookkeeping (the previous one was released ten minutes
	// ago, polling stopped, the agent starts a new turn) must not be released on its first sweep —
	// neither by the gap-diluted rate nor by the stale idle clock.
	t.Run("a re-acquire on a pid with stale bookkeeping is not released on its first sweep", func(t *testing.T) {
		e := NewIdleReleaseEvaluator()
		first := codexAssertion(t0, withKey("codex:turn1"))

		// Turn 1: seed, go idle, get released at +91s.
		e.Evaluate([]model.Assertion{first}, t0, cfg(), probes(true, cpuConst(10)))
		released := e.Evaluate([]model.Assertion{first}, at(91), cfg(), probes(true, cpuConst(10)))
		expectReasons(t, released, ReasonCPUIdle)

		// Ten minutes later a new turn re-acquires on the same pid. The tree did 5 CPU-seconds of
		// real work, but spread over the 600s gap that is a 0.008 rate — diluted below the line.
		// The fresh acquiredAt must win over the stale clock.
		gapEnd := at(91 + 600)
		second := codexAssertion(gapEnd.Add(-30*time.Second), withKey("codex:turn2"))
		out := e.Evaluate([]model.Assertion{second}, gapEnd, cfg(), probes(true, cpuConst(15)))
		if len(out) != 0 {
			t.Fatalf("a brand-new turn must never be cpuIdle-released on its first sweep: %v", out)
		}

		// The re-seeded baseline measures rate from here: a busy next sweep stays held.
		busy := e.Evaluate([]model.Assertion{second}, gapEnd.Add(30*time.Second), cfg(), probes(true, cpuConst(17)))
		expectReasons(t, busy)
	})

	// LastActivityAt (advanced by the registry on every duplicate acquire) floors the idle clock:
	// a hook re-acquire is an explicit activity signal.
	t.Run("a refreshed lastActivityAt resets the idle clock", func(t *testing.T) {
		e := NewIdleReleaseEvaluator()
		a := codexAssertion(t0)

		e.Evaluate([]model.Assertion{a}, t0, cfg(), probes(true, cpuConst(1))) // seed
		e.Evaluate([]model.Assertion{a}, at(30), cfg(), probes(true, cpuConst(1)))

		// The hook fires a re-acquire at +60s (registry stamps lastActivityAt); CPU stays flat.
		a.LastActivityAt = at(60)
		held := e.Evaluate([]model.Assertion{a}, at(120), cfg(), probes(true, cpuConst(1)))
		if len(held) != 0 {
			t.Fatalf("60s after the activity stamp is inside the 90s window: %v", held)
		}

		released := e.Evaluate([]model.Assertion{a}, at(60+91), cfg(), probes(true, cpuConst(1)))
		expectReasons(t, released, ReasonCPUIdle)
	})

	// MARK: Boundary pins

	t.Run("idle duration exactly at the threshold does not release", func(t *testing.T) {
		e := NewIdleReleaseEvaluator()
		a := codexAssertion(t0)
		e.Evaluate([]model.Assertion{a}, t0, cfg(), probes(true, cpuConst(1)))
		atThreshold := e.Evaluate([]model.Assertion{a}, at(90), cfg(), probes(true, cpuConst(1)))
		if len(atThreshold) != 0 {
			t.Fatalf("the rule is strictly past the window: %v", atThreshold)
		}
	})

	t.Run("cpu rate exactly at the threshold counts as active", func(t *testing.T) {
		e := NewIdleReleaseEvaluator()
		a := codexAssertion(t0)
		// Binary-exact values so the boundary is genuinely exact: threshold 1/32, and 0.9375
		// (15/16) CPU-seconds per 30s divides to exactly 1/32.
		exact := IdleConfig{Enabled: true, IdleThreshold: 90 * time.Second, CPURateThreshold: 0.031_25, MaxAssertionAge: 1000 * time.Hour}
		e.Evaluate([]model.Assertion{a}, t0, exact, probes(true, cpuConst(1)))
		for step := 1; step <= 10; step++ {
			cpu := 1.0 + float64(step)*0.937_5
			out := e.Evaluate([]model.Assertion{a}, at(float64(step)*30), exact, probes(true, cpuConst(cpu)))
			expectReasons(t, out)
		}
	})

	t.Run("ttl expiring exactly now does not release yet", func(t *testing.T) {
		e := NewIdleReleaseEvaluator()
		a := codexAssertion(at(-5), withTTL(5))
		out := e.Evaluate([]model.Assertion{a}, t0, cfg(), probes(true, cpuNone))
		if len(out) != 0 {
			t.Fatalf("expiry is strictly past the deadline: %v", out)
		}
	})

	t.Run("age exactly at the backstop does not release yet", func(t *testing.T) {
		e := NewIdleReleaseEvaluator()
		a := codexAssertion(at(-24*3600), withPID(-1))
		c := IdleConfig{Enabled: true, IdleThreshold: 90 * time.Second, CPURateThreshold: DefaultCPURateThreshold, MaxAssertionAge: 24 * time.Hour}
		out := e.Evaluate([]model.Assertion{a}, t0, c, probes(true, cpuNone))
		expectReasons(t, out)
	})

	// MARK: Degenerate clocks and probes

	t.Run("a zero or negative sample gap neither crashes nor releases spuriously", func(t *testing.T) {
		e := NewIdleReleaseEvaluator()
		a := codexAssertion(t0)
		e.Evaluate([]model.Assertion{a}, t0, cfg(), probes(true, cpuConst(1)))
		// Same instant again (dt == 0): rate forced to 0, inside the window → nothing.
		same := e.Evaluate([]model.Assertion{a}, t0, cfg(), probes(true, cpuConst(1)))
		expectReasons(t, same)
		// Clock stepped backwards: idle duration goes negative → nothing.
		backwards := e.Evaluate([]model.Assertion{a}, at(-60), cfg(), probes(true, cpuConst(1)))
		expectReasons(t, backwards)
	})

	t.Run("decreasing tree cpu (a busy child exited) reads as idle and eventually releases", func(t *testing.T) {
		e := NewIdleReleaseEvaluator()
		a := codexAssertion(t0)
		e.Evaluate([]model.Assertion{a}, t0, cfg(), probes(true, cpuConst(100)))
		// Child exits: the cumulative tree total drops. A negative rate is below the threshold.
		e.Evaluate([]model.Assertion{a}, at(30), cfg(), probes(true, cpuConst(40)))
		out := e.Evaluate([]model.Assertion{a}, at(30+91), cfg(), probes(true, cpuConst(40)))
		expectReasons(t, out, ReasonCPUIdle)
	})

	t.Run("a cpu probe outage suspends the idle policy without releasing", func(t *testing.T) {
		e := NewIdleReleaseEvaluator()
		a := codexAssertion(t0)
		e.Evaluate([]model.Assertion{a}, t0, cfg(), probes(true, cpuConst(1)))
		// Probe fails for several sweeps — nothing released, no crash.
		for step := 1; step <= 5; step++ {
			out := e.Evaluate([]model.Assertion{a}, at(float64(step)*30), cfg(), probes(true, cpuNone))
			expectReasons(t, out)
		}
		// Probe recovers; the retained baseline spans the outage. Flat CPU across it means the
		// tree really was idle the whole time — releasing here is correct.
		recovered := e.Evaluate([]model.Assertion{a}, at(180), cfg(), probes(true, cpuConst(1)))
		expectReasons(t, recovered, ReasonCPUIdle)
	})

	t.Run("two assertions sharing one pid evaluate sanely in a single sweep", func(t *testing.T) {
		e := NewIdleReleaseEvaluator()
		a := codexAssertion(t0, withKey("codex:a"))
		b := codexAssertion(t0, withKey("codex:b"))
		e.Evaluate([]model.Assertion{a, b}, t0, cfg(), probes(true, cpuConst(1)))
		out := e.Evaluate([]model.Assertion{a, b}, at(91), cfg(), probes(true, cpuConst(1)))
		keys := map[string]bool{}
		for _, r := range out {
			keys[r.Key] = true
		}
		if len(out) != 2 || !keys["codex:a"] || !keys["codex:b"] {
			t.Fatalf("both idle assertions release; neither corrupts the other's bookkeeping: %v", out)
		}
	})

	t.Run("forget keeping live pids retains their baselines", func(t *testing.T) {
		e := NewIdleReleaseEvaluator()
		a := codexAssertion(t0)
		e.Evaluate([]model.Assertion{a}, t0, cfg(), probes(true, cpuConst(1)))
		e.Forget(map[int]bool{100: true})
		// The baseline survived: the next sweep computes a rate (rather than re-seeding) and
		// releases the long-idle tree.
		out := e.Evaluate([]model.Assertion{a}, at(91), cfg(), probes(true, cpuConst(1)))
		expectReasons(t, out, ReasonCPUIdle)
	})
}
