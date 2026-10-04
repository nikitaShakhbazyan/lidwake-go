package monitor

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/nikitaShakhbazyan/lidwake-go/internal/activity"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/agents"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/darwin"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/model"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/settings"
)

// DefaultIdleInterval is the idle sweep's period: two samples this close make a meaningful CPU
// rate, and it bounds the latency of noticing an interrupted turn to roughly the idle threshold
// rather than a multiple of it.
const DefaultIdleInterval = 30 * time.Second

// IdleMonitor releases assertions whose owning process is dead, whose process tree has been
// CPU-idle for the configured window, whose TTL expired, or which outlived the age backstop.
//
// The decisions are activity.IdleReleaseEvaluator's; this monitor supplies the process probes and
// the timer. The sweep runs only while blocking: with nothing held there is nothing to release,
// and a 30 s timer would spin the CPU back up exactly when the Mac would otherwise be asleep.
type IdleMonitor struct {
	// Assertions returns the live assertions (the registry snapshot). Nil: sweeps do nothing.
	Assertions func() []model.Assertion
	// OnRelease receives each sweep's releases, one per key with the first reason the evaluator
	// gave, so the daemon can pick the pre-sleep cue (a TTL expiry sounds different from a finished
	// agent).
	OnRelease func([]activity.Release)
	// Procs reads the process table: liveness, one child-map snapshot per sweep and CPU times.
	// Nil: agents.System().
	Procs *agents.Resolver
	// WakePIDs returns the PIDs holding a system-sleep assertion. Nil:
	// darwin.PIDsPreventingSystemSleep.
	WakePIDs func() map[int]bool
	// Now is the wall clock. Nil: time.Now.
	Now func() time.Time
	// Interval between sweeps while blocking. Zero: DefaultIdleInterval.
	Interval time.Duration
	Log      *slog.Logger

	life lifecycle
	kick kicker

	mu       sync.Mutex
	cfg      *activity.IdleConfig // nil: from settings.Defaults()
	blocking bool

	// evalMu guards eval: the sweep and ResetBaselines (on wake) run on different goroutines.
	evalMu sync.Mutex
	eval   activity.IdleReleaseEvaluator

	// lastTimed is the wall clock of the previous timer-driven sweep, zero right after the timer
	// is armed. Only the loop goroutine touches it.
	lastTimed time.Time
}

// IdleConfigFor builds the evaluator's configuration from the user's settings: the idle switch and
// window come from settings, the CPU-rate line and the 24 h age backstop are the defaults.
func IdleConfigFor(s settings.Settings) activity.IdleConfig {
	cfg := activity.DefaultIdleConfig()
	cfg.Enabled = s.IdleReleaseEnabled
	cfg.IdleThreshold = time.Duration(s.IdleReleaseSeconds) * time.Second
	return cfg
}

// NewIdleMonitor returns a monitor probing the real process table, configured with the default
// settings.
func NewIdleMonitor() *IdleMonitor {
	return &IdleMonitor{
		Procs:    agents.System(),
		WakePIDs: darwin.PIDsPreventingSystemSleep,
		Now:      time.Now,
		Interval: DefaultIdleInterval,
	}
}

// Start starts the loop. Sweeps begin only once SetBlocking(true), the first one an interval
// later.
func (m *IdleMonitor) Start(ctx context.Context) {
	m.life.start(ctx, nil, func(ctx context.Context, stop <-chan struct{}) {
		runTicking(ctx, stop, ticking{
			interval: orDefault(m.Interval, DefaultIdleInterval),
			armed:    m.isBlocking,
			onArm:    func() { m.lastTimed = time.Time{} },
			// Bookkeeping frozen across a stopped stretch would poison the next blocking period: the
			// first rate would span the whole gap (diluted toward zero) and a stale last-active time
			// would read a brand-new turn as long idle.
			onDisarm: m.ResetBaselines,
			kick:     m.kick.c(),
			onTick:   m.timedSweep,
		})
	})
}

// Stop ends the sweeps.
func (m *IdleMonitor) Stop() { m.life.halt() }

// ApplySettings takes the idle-release switch and window.
func (m *IdleMonitor) ApplySettings(s settings.Settings) {
	cfg := IdleConfigFor(s)
	m.mu.Lock()
	m.cfg = &cfg
	m.mu.Unlock()
}

// SetBlocking arms or disarms the sweep.
func (m *IdleMonitor) SetBlocking(blocking bool) {
	m.mu.Lock()
	changed := m.blocking != blocking
	m.blocking = blocking
	m.mu.Unlock()
	if changed {
		m.kick.signal()
	}
}

// ResetBaselines drops the CPU baselines so the next sweep re-seeds. Call it on system wake: the
// wall clock advanced through the sleep, so a pre-sleep sample would produce a gap-spanning rate
// and an instantly expired idle clock for an agent that was mid-work.
func (m *IdleMonitor) ResetBaselines() {
	m.evalMu.Lock()
	m.eval.Forget(nil)
	m.evalMu.Unlock()
}

func (m *IdleMonitor) isBlocking() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.blocking
}

func (m *IdleMonitor) config() activity.IdleConfig {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cfg == nil {
		return IdleConfigFor(settings.Defaults())
	}
	return *m.cfg
}

// timedSweep is a sweep from the timer. The timer runs on the monotonic clock, which stops while
// the Mac sleeps, so timed sweeps come one interval of awake time apart, and a wall-clock gap of
// more than two intervals means the Mac slept in between. The baselines are then dropped first, as
// on wake: the daemon's wake handler does the same, but the wake detector ticks on its own timer
// and can report the wake only after this sweep, which would read a mid-work agent's pre-sleep
// sample as long idle.
func (m *IdleMonitor) timedSweep() {
	now := nowOr(m.Now).Round(0)
	if prev := m.lastTimed; !prev.IsZero() && now.Sub(prev) > 2*orDefault(m.Interval, DefaultIdleInterval) {
		logger(m.Log, "idle").Info("the Mac slept since the last sweep; re-seeding the CPU baselines")
		m.ResetBaselines()
	}
	m.lastTimed = now
	m.sweepAt(now)
}

// sweep is sweepAt the current time.
func (m *IdleMonitor) sweep() { m.sweepAt(nowOr(m.Now)) }

// sweepAt evaluates every live assertion once at now and hands the releases to OnRelease.
func (m *IdleMonitor) sweepAt(now time.Time) {
	if m.Assertions == nil {
		return
	}
	assertions := m.Assertions()
	procs := m.Procs
	if procs == nil {
		procs = agents.System()
	}
	wakePIDs := m.WakePIDs
	if wakePIDs == nil {
		wakePIDs = darwin.PIDsPreventingSystemSleep
	}

	// One process-table snapshot per sweep, shared by every tree walk (the CPU sum and the wake
	// assertion check), and one system-wide read of the wake-asserting PIDs, scoped per assertion
	// to the owning agent's tree (globally it would always be true). Both are taken on first use,
	// so a sweep that needs neither (manual holds only, first samples) reads neither.
	var childMap map[int][]int
	tree := func() map[int][]int {
		if childMap == nil {
			childMap = map[int][]int{}
			if procs.ProcessTree != nil {
				childMap = procs.ChildMap()
			}
		}
		return childMap
	}
	var wake map[int]bool
	wakeRead := false
	probes := activity.IdleProbes{
		PIDAlive: procs.ProcessAlive,
		TreeHoldsWakeAssertion: func(pid int) bool {
			if !wakeRead {
				wake, wakeRead = wakePIDs(), true
			}
			return treeContains(pid, wake, tree)
		},
	}
	if procs.CPUTime != nil {
		probes.CPUTime = func(pid int) (time.Duration, bool) {
			return procs.TreeCPUTime(pid, tree())
		}
	}

	live := map[int]bool{}
	for _, a := range assertions {
		if a.PID > 0 {
			live[a.PID] = true
		}
	}
	m.evalMu.Lock()
	releases := m.eval.Evaluate(assertions, now, m.config(), probes)
	// Prune bookkeeping for PIDs that no longer back a live assertion: it bounds the per-PID maps
	// on this always-on daemon and stops a recycled PID inheriting a vanished process's baseline.
	m.eval.Forget(live)
	m.evalMu.Unlock()
	if len(releases) == 0 {
		return
	}

	log := logger(m.Log, "idle")
	for _, r := range releases {
		if r.Reason == activity.ReasonMaxAgeBackstop {
			log.Warn("backstop-releasing an assertion past the max-age cap (likely a leaked session with an unresolved pid and a missed end hook)", "key", r.Key)
		}
	}
	unique := dedupeReleases(releases)
	log.Info("idle-releasing assertions", "count", len(unique))
	if m.OnRelease != nil {
		m.OnRelease(unique)
	}
}

// dedupeReleases keeps one release per key, with the first reason reported for it: an assertion
// can match several rules (CPU-idle and TTL-expired).
func dedupeReleases(releases []activity.Release) []activity.Release {
	seen := make(map[string]bool, len(releases))
	out := make([]activity.Release, 0, len(releases))
	for _, r := range releases {
		if seen[r.Key] {
			continue
		}
		seen[r.Key] = true
		out = append(out, r)
	}
	return out
}

// treeContains reports whether root or any descendant is in pids. It walks the same tree as the
// CPU sum, so a wake assertion held by a child counts for the agent: Claude Code's `caffeinate`
// is a child of `claude`, not `claude` itself. Nothing asserting (the common case) skips the walk.
func treeContains(root int, pids map[int]bool, tree func() map[int][]int) bool {
	if len(pids) == 0 {
		return false
	}
	if pids[root] {
		return true
	}
	children := tree()
	seen := map[int]bool{root: true}
	stack := append([]int(nil), children[root]...)
	for len(stack) > 0 {
		pid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[pid] {
			continue
		}
		seen[pid] = true
		if pids[pid] {
			return true
		}
		stack = append(stack, children[pid]...)
	}
	return false
}
