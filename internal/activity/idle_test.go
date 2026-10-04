package activity

import (
	"slices"
	"testing"
	"time"

	"github.com/nikitaShakhbazyan/lidwake-go/internal/model"
)

var t0 = time.Unix(1_000_000, 0)

func secs(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

func at(s float64) time.Time { return t0.Add(secs(s)) }

type assertionOpt func(*model.Assertion)

func withKey(k string) assertionOpt { return func(a *model.Assertion) { a.Key = k } }
func withPID(p int) assertionOpt    { return func(a *model.Assertion) { a.PID = p } }
func withTTL(s float64) assertionOpt {
	return func(a *model.Assertion) { e := a.AcquiredAt.Add(secs(s)); a.ExpiresAt = &e }
}
func withOrigin(o model.Origin) assertionOpt {
	return func(a *model.Assertion) { a.Origin = o }
}

func idleAssertion(acquiredAt time.Time, opts ...assertionOpt) model.Assertion {
	a := model.New("k", "claude-code", "", 100, "claude", acquiredAt, nil, model.OriginHook)
	for _, o := range opts {
		o(&a)
	}
	return a
}

func reasons(rs []Release) []ReleaseReason {
	out := []ReleaseReason{}
	for _, r := range rs {
		out = append(out, r.Reason)
	}
	return out
}

func expectReasons(t *testing.T, got []Release, want ...ReleaseReason) {
	t.Helper()
	if want == nil {
		want = []ReleaseReason{}
	}
	if !slices.Equal(reasons(got), want) {
		t.Fatalf("reasons = %v, want %v", reasons(got), want)
	}
}

func pidAlive(v bool) func(int) bool { return func(int) bool { return v } }

func cpuConst(s float64) func(int) (time.Duration, bool) {
	return func(int) (time.Duration, bool) { return secs(s), true }
}

func cpuNone(int) (time.Duration, bool) { return 0, false }

func probes(alive bool, cpu func(int) (time.Duration, bool)) IdleProbes {
	return IdleProbes{PIDAlive: pidAlive(alive), CPUTime: cpu}
}

func probesWake(cpu func(int) (time.Duration, bool), wake bool) IdleProbes {
	return IdleProbes{PIDAlive: pidAlive(true), CPUTime: cpu, TreeHoldsWakeAssertion: func(int) bool { return wake }}
}

// cfg is the default config for the CPU-rate tests: 90 s idle window, 3% rate threshold, a
// backstop far away. CPU values are cumulative seconds, so a rate is the slope between two
// samples (30 s apart ≈ a real sweep).
func cfg() IdleConfig {
	return IdleConfig{Enabled: true, IdleThreshold: 90 * time.Second, CPURateThreshold: 0.03, MaxAssertionAge: 1000 * time.Hour}
}

func TestIdleReleaseEvaluator(t *testing.T) {
	// MARK: Safety backstop

	t.Run("max-age backstop releases an old assertion regardless of PID or enabled", func(t *testing.T) {
		e := NewIdleReleaseEvaluator()
		old := idleAssertion(at(-25*3600), withPID(100))
		c := IdleConfig{Enabled: false, IdleThreshold: 90 * time.Second, CPURateThreshold: DefaultCPURateThreshold, MaxAssertionAge: 24 * time.Hour}
		out := e.Evaluate([]model.Assertion{old}, t0, c, probes(true, cpuConst(1)))
		expectReasons(t, out, ReasonMaxAgeBackstop)
	})

	t.Run("backstop applies even to unresolved (pid <= 0) assertions", func(t *testing.T) {
		e := NewIdleReleaseEvaluator()
		old := idleAssertion(at(-25*3600), withPID(-1))
		out := e.Evaluate([]model.Assertion{old}, t0, DefaultIdleConfig(), probes(true, cpuNone))
		expectReasons(t, out, ReasonMaxAgeBackstop)
	})

	t.Run("a too-young assertion with no resolved PID survives (until the backstop)", func(t *testing.T) {
		e := NewIdleReleaseEvaluator()
		young := idleAssertion(t0, withPID(-1))
		out := e.Evaluate([]model.Assertion{young}, at(60), DefaultIdleConfig(), probes(true, cpuNone))
		expectReasons(t, out)
	})

	// MARK: Dead process

	t.Run("a dead owning process is released", func(t *testing.T) {
		e := NewIdleReleaseEvaluator()
		a := idleAssertion(t0, withPID(100))
		out := e.Evaluate([]model.Assertion{a}, t0, DefaultIdleConfig(), probes(false, cpuConst(1)))
		expectReasons(t, out, ReasonDeadProcess)
	})

	// MARK: CPU-rate idle

	t.Run("first sweep seeds and never releases; idle tree past the window then releases", func(t *testing.T) {
		e := NewIdleReleaseEvaluator()
		a := idleAssertion(t0, withPID(100))
		sweep1 := e.Evaluate([]model.Assertion{a}, t0, cfg(), probes(true, cpuConst(1)))
		expectReasons(t, sweep1) // seeded, not released
		// Flat CPU (rate 0) for >90s → released.
		sweep2 := e.Evaluate([]model.Assertion{a}, at(91), cfg(), probes(true, cpuConst(1)))
		expectReasons(t, sweep2, ReasonCPUIdle)
	})

	// The core of the rule: an idle `claude` TUI still burns ~1% CPU. A slow, steady drift well
	// below the 3% line must read as idle and release.
	t.Run("slow sub-threshold CPU drift still counts as idle and releases", func(t *testing.T) {
		e := NewIdleReleaseEvaluator()
		a := idleAssertion(t0, withPID(100))
		// ~1% rate: +0.3 CPU-seconds every 30s = 0.01/s, well under 0.03.
		cpu := secs(1)
		cpuFn := func(int) (time.Duration, bool) { return cpu, true }
		e.Evaluate([]model.Assertion{a}, t0, cfg(), probes(true, cpuFn))
		var out []Release
		for step := 1; step <= 4; step++ { // 30, 60, 90, 120s
			cpu += 300 * time.Millisecond
			out = e.Evaluate([]model.Assertion{a}, at(float64(step)*30), cfg(), probes(true, cpuFn))
		}
		expectReasons(t, out, ReasonCPUIdle) // by 120s the drift never cleared the rate line
	})

	t.Run("a busy tree (rate above threshold) stamps activity and is never released", func(t *testing.T) {
		e := NewIdleReleaseEvaluator()
		a := idleAssertion(t0, withPID(100))
		// +2.0 CPU-seconds every 30s = 0.067/s, above the 3% line (a busy build child).
		cpu := secs(1)
		cpuFn := func(int) (time.Duration, bool) { return cpu, true }
		e.Evaluate([]model.Assertion{a}, t0, cfg(), probes(true, cpuFn))
		for step := 1; step <= 10; step++ { // 300s of sustained work
			cpu += 2 * time.Second
			out := e.Evaluate([]model.Assertion{a}, at(float64(step)*30), cfg(), probes(true, cpuFn))
			expectReasons(t, out)
		}
	})

	t.Run("a burst of activity resets the idle clock", func(t *testing.T) {
		e := NewIdleReleaseEvaluator()
		a := idleAssertion(t0, withPID(100))
		e.Evaluate([]model.Assertion{a}, t0, cfg(), probes(true, cpuConst(1))) // seed
		// Active burst at +30s (rate 0.033) → resets lastActive to t0+30.
		active := e.Evaluate([]model.Assertion{a}, at(30), cfg(), probes(true, cpuConst(2)))
		expectReasons(t, active)
		// Flat for >90s since the reset → releases.
		idle := e.Evaluate([]model.Assertion{a}, at(30+91), cfg(), probes(true, cpuConst(2)))
		expectReasons(t, idle, ReasonCPUIdle)
	})

	t.Run("enabled=false suppresses CPU-idle but not the safety rules", func(t *testing.T) {
		e := NewIdleReleaseEvaluator()
		a := idleAssertion(t0, withPID(100))
		disabled := IdleConfig{Enabled: false, IdleThreshold: 90 * time.Second, CPURateThreshold: DefaultCPURateThreshold, MaxAssertionAge: 1000 * time.Hour}

		// Flat CPU long past the window, but disabled → no CPU-idle release.
		e.Evaluate([]model.Assertion{a}, t0, disabled, probes(true, cpuConst(1)))
		out := e.Evaluate([]model.Assertion{a}, at(10_000), disabled, probes(true, cpuConst(1)))
		expectReasons(t, out)

		// Same disabled config, but a dead PID still releases (safety, not preference).
		dead := e.Evaluate([]model.Assertion{a}, t0, disabled, probes(false, cpuConst(1)))
		expectReasons(t, dead, ReasonDeadProcess)
	})

	// MARK: Wake-assertion signal (the thinking gap)

	t.Run("an idle tree that still holds a wake assertion is NOT released (thinking)", func(t *testing.T) {
		e := NewIdleReleaseEvaluator()
		a := idleAssertion(t0, withPID(100))
		// Flat CPU (a thinking agent: server-side compute, near-idle client) but the tree holds a
		// wake assertion (its caffeinate child) the whole time → must never release.
		e.Evaluate([]model.Assertion{a}, t0, cfg(), probesWake(cpuConst(1), true))
		for step := 1; step <= 8; step++ { // 240s of idle CPU but asserting
			out := e.Evaluate([]model.Assertion{a}, at(float64(step)*30), cfg(), probesWake(cpuConst(1), true))
			expectReasons(t, out)
		}
	})

	t.Run("once the wake assertion drops, an idle tree releases after the window", func(t *testing.T) {
		e := NewIdleReleaseEvaluator()
		a := idleAssertion(t0, withPID(100))
		// Asserting + idle CPU for a while → held.
		e.Evaluate([]model.Assertion{a}, t0, cfg(), probesWake(cpuConst(1), true))
		held := e.Evaluate([]model.Assertion{a}, at(120), cfg(), probesWake(cpuConst(1), true))
		expectReasons(t, held)
		// Turn ends / interrupt → caffeinate gone, CPU still idle. The idle clock restarts from
		// the last active stamp (t0+120); >90s later → released.
		e.Evaluate([]model.Assertion{a}, at(150), cfg(), probesWake(cpuConst(1), false))
		out := e.Evaluate([]model.Assertion{a}, at(120+91), cfg(), probesWake(cpuConst(1), false))
		expectReasons(t, out, ReasonCPUIdle)
	})

	t.Run("a manual hold ignores the wake-assertion signal (TTL still governs)", func(t *testing.T) {
		e := NewIdleReleaseEvaluator()
		a := idleAssertion(t0, withPID(100), withOrigin(model.OriginManual))
		// Manual holds skip the whole CPU/assertion branch — only TTL/dead-pid apply.
		e.Evaluate([]model.Assertion{a}, t0, cfg(), probesWake(cpuConst(1), true))
		out := e.Evaluate([]model.Assertion{a}, at(10_000), cfg(), probesWake(cpuConst(1), true))
		expectReasons(t, out)
	})

	// MARK: Manual-hold idle exemption

	t.Run("a manual hold is exempt from CPU-idle release", func(t *testing.T) {
		e := NewIdleReleaseEvaluator()
		a := idleAssertion(t0, withPID(100), withOrigin(model.OriginManual))
		e.Evaluate([]model.Assertion{a}, t0, cfg(), probes(true, cpuConst(1)))
		// Flat CPU well past the window — a hook assertion would be CPU-idle released here.
		out := e.Evaluate([]model.Assertion{a}, at(10_000), cfg(), probes(true, cpuConst(1)))
		expectReasons(t, out)
	})

	t.Run("a pid-bound manual hold still releases when its process dies", func(t *testing.T) {
		e := NewIdleReleaseEvaluator()
		a := idleAssertion(t0, withPID(100), withOrigin(model.OriginManual))
		out := e.Evaluate([]model.Assertion{a}, t0, DefaultIdleConfig(), probes(false, cpuConst(1)))
		expectReasons(t, out, ReasonDeadProcess)
	})

	t.Run("a manual hold still expires on TTL", func(t *testing.T) {
		e := NewIdleReleaseEvaluator()
		a := idleAssertion(at(-10), withPID(-1), withTTL(5), withOrigin(model.OriginManual))
		out := e.Evaluate([]model.Assertion{a}, t0, DefaultIdleConfig(), probes(true, cpuNone))
		expectReasons(t, out, ReasonTTLExpired)
	})

	// MARK: PID bookkeeping pruning

	t.Run("forget() gives a recycled PID a fresh baseline instead of inheriting stale idle state", func(t *testing.T) {
		e := NewIdleReleaseEvaluator()
		c := IdleConfig{Enabled: true, IdleThreshold: 90 * time.Second, CPURateThreshold: DefaultCPURateThreshold, MaxAssertionAge: 1000 * time.Hour}
		a := idleAssertion(t0, withPID(100))

		// Seed the baseline, then let it sit idle but not yet past the idle threshold.
		e.Evaluate([]model.Assertion{a}, t0, c, probes(true, cpuConst(1)))
		still := e.Evaluate([]model.Assertion{a}, at(60), c, probes(true, cpuConst(1)))
		expectReasons(t, still)

		// The PID exits and is recycled by a fresh owner. Pruning must drop its stale
		// lastActiveAt, so the new owner is treated as first-seen.
		e.Forget(map[int]bool{})
		fresh := e.Evaluate([]model.Assertion{a}, at(200), c, probes(true, cpuConst(1)))
		if len(fresh) != 0 {
			t.Fatalf("a forgotten PID must re-seed, not release on the vanished process's idle clock: %v", fresh)
		}
	})

	// MARK: TTL

	t.Run("an expired TTL releases regardless of enabled", func(t *testing.T) {
		e := NewIdleReleaseEvaluator()
		a := idleAssertion(at(-10), withPID(100), withTTL(5)) // expired 5s ago
		c := IdleConfig{Enabled: false, IdleThreshold: 90 * time.Second, CPURateThreshold: DefaultCPURateThreshold, MaxAssertionAge: 1000 * time.Hour}
		out := e.Evaluate([]model.Assertion{a}, t0, c, probes(true, cpuNone))
		expectReasons(t, out, ReasonTTLExpired)
	})

	t.Run("a hook hold with no resolved PID still expires on TTL", func(t *testing.T) {
		// A Node-hosted acquire resolved pid=-1, so the dead-process and CPU-idle rules could
		// never evaluate it — the TTL must be the net that still works.
		e := NewIdleReleaseEvaluator()
		a := idleAssertion(at(-10), withPID(-1), withTTL(5), withOrigin(model.OriginHook))
		out := e.Evaluate([]model.Assertion{a}, t0, DefaultIdleConfig(), probes(true, cpuNone))
		expectReasons(t, out, ReasonTTLExpired)
	})
}

// Go-specific behavior.
func TestIdleReleaseEvaluatorGo(t *testing.T) {
	t.Run("default config matches the documented policy", func(t *testing.T) {
		c := DefaultIdleConfig()
		if !c.Enabled || c.IdleThreshold != 90*time.Second || c.CPURateThreshold != 0.03 || c.MaxAssertionAge != 24*time.Hour {
			t.Fatalf("DefaultIdleConfig() = %+v", c)
		}
	})

	t.Run("release reasons keep their wire values", func(t *testing.T) {
		// The sleep-cue cause and the event log read these strings.
		for got, want := range map[ReleaseReason]string{
			ReasonMaxAgeBackstop: "maxAgeBackstop", ReasonDeadProcess: "deadProcess",
			ReasonCPUIdle: "cpuIdle", ReasonTTLExpired: "ttlExpired",
		} {
			if string(got) != want {
				t.Errorf("reason %q, want %q", got, want)
			}
		}
	})

	t.Run("zero-value evaluator and nil probes are usable", func(t *testing.T) {
		var e IdleReleaseEvaluator
		a := idleAssertion(at(-10), withTTL(5))
		out := e.Evaluate([]model.Assertion{a}, t0, cfg(), IdleProbes{})
		expectReasons(t, out, ReasonTTLExpired)
		e.Forget(nil)
	})

	t.Run("cpu-idle and ttl can both match one assertion", func(t *testing.T) {
		e := NewIdleReleaseEvaluator()
		a := idleAssertion(t0, withTTL(60))
		e.Evaluate([]model.Assertion{a}, t0, cfg(), probes(true, cpuConst(1)))
		out := e.Evaluate([]model.Assertion{a}, at(91), cfg(), probes(true, cpuConst(1)))
		expectReasons(t, out, ReasonCPUIdle, ReasonTTLExpired)
	})

	t.Run("a now carrying a monotonic reading is compared by wall clock", func(t *testing.T) {
		e := NewIdleReleaseEvaluator()
		now := time.Now()
		a := idleAssertion(now.Round(0).Add(-10*time.Second), withTTL(5))
		out := e.Evaluate([]model.Assertion{a}, now, cfg(), IdleProbes{})
		expectReasons(t, out, ReasonTTLExpired)
	})
}
