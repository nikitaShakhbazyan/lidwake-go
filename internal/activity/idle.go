// Package activity decides when an agent's hold has stopped meaning "work is happening": the
// CPU-idle and safety release rules, the Claude Code waiting-for-the-user policy (from Claude
// Code's own session status files) and the background-shell hold plan.
//
// Everything here is pure policy over injected probes and times, so it is testable without live
// processes, real status files or wall-clock waits.
package activity

import (
	"time"

	"github.com/nikitaShakhbazyan/lidwake-go/internal/model"
)

// DefaultCPURateThreshold is the CPU usage rate (fraction of one core) below which a process tree
// counts as idle. An interrupted `claude` TUI idles around 1%; real work (model streaming, a busy
// tool child) runs far higher, so 3% cleanly separates the two.
const DefaultCPURateThreshold = 0.03

// IdleConfig tunes IdleReleaseEvaluator. Start from DefaultIdleConfig: the zero value has a CPU
// threshold of 0, which reads every tree as active.
type IdleConfig struct {
	// Enabled is the user-tunable CPU-idle policy. When false only the safety rules (backstop,
	// dead PID, TTL) apply — those are correctness, not preference, so nothing can pin sleep
	// forever.
	Enabled bool
	// IdleThreshold releases a hold once its process tree has stayed below CPURateThreshold this
	// long.
	IdleThreshold time.Duration
	// CPURateThreshold is the usage rate (0.03 = 3% of one core) below which the tree counts as
	// idle. Measured against the whole tree so a long tool call (a busy build child) keeps it above
	// the line even while the agent process itself waits.
	CPURateThreshold float64
	// MaxAssertionAge is the hard backstop: any assertion older than this is released regardless
	// of PID or Enabled. Zero disables it.
	MaxAssertionAge time.Duration
}

// DefaultIdleConfig is the policy of a fresh install: enabled, 90 s idle window, 3% rate line,
// 24 h backstop.
func DefaultIdleConfig() IdleConfig {
	return IdleConfig{
		Enabled:          true,
		IdleThreshold:    90 * time.Second,
		CPURateThreshold: DefaultCPURateThreshold,
		MaxAssertionAge:  24 * time.Hour,
	}
}

// ReleaseReason says why an assertion is released by the idle sweep.
type ReleaseReason string

const (
	// ReasonMaxAgeBackstop: a leaked session, too old (unresolved PID plus a missed end hook).
	ReasonMaxAgeBackstop ReleaseReason = "maxAgeBackstop"
	// ReasonDeadProcess: the owning process is gone.
	ReasonDeadProcess ReleaseReason = "deadProcess"
	// ReasonCPUIdle: the owning process tree has been CPU-idle past the threshold.
	ReasonCPUIdle ReleaseReason = "cpuIdle"
	// ReasonTTLExpired: the assertion's TTL elapsed.
	ReasonTTLExpired ReleaseReason = "ttlExpired"
)

// Release is one assertion to release and why.
type Release struct {
	Key    string
	Reason ReleaseReason
}

// IdleProbes are the process probes IdleReleaseEvaluator.Evaluate consults.
type IdleProbes struct {
	// PIDAlive is the liveness probe (darwin.ProcessAlive in production). Nil reads as alive.
	PIDAlive func(pid int) bool
	// CPUTime is the cumulative user+system CPU time of the PID's whole process tree; ok is false
	// when unavailable. The tree (not just the agent process) so a long tool call — where the agent
	// waits while a busy child does the work — still reads as active. Nil reads as unavailable.
	CPUTime func(pid int) (cpu time.Duration, ok bool)
	// TreeHoldsWakeAssertion reports whether the PID's process tree currently holds a system-sleep
	// assertion (an agent's own `caffeinate` child). When true the tree counts as active even with
	// idle CPU: this keeps a hold alive through server-side thinking, where the local process is
	// near-idle but the agent has declared it is working. Nil reads as false, which keeps the pure
	// CPU-rate behavior.
	TreeHoldsWakeAssertion func(pid int) bool
}

// IdleReleaseEvaluator decides which held assertions the idle monitor releases on a sweep. It
// holds the cross-sweep CPU bookkeeping the "CPU-idle for N seconds" rule needs. Not safe for
// concurrent use: the idle monitor owns it.
type IdleReleaseEvaluator struct {
	// Per PID: the last cumulative CPU reading, when it was sampled (two readings make a rate),
	// and the last time the tree was observed active (rate at or above the threshold). Idle
	// duration is measured from lastActiveAt.
	lastCPUTime  map[int]time.Duration
	lastSampleAt map[int]time.Time
	lastActiveAt map[int]time.Time
}

// NewIdleReleaseEvaluator returns an evaluator with no bookkeeping.
func NewIdleReleaseEvaluator() *IdleReleaseEvaluator {
	return &IdleReleaseEvaluator{
		lastCPUTime:  map[int]time.Duration{},
		lastSampleAt: map[int]time.Time{},
		lastActiveAt: map[int]time.Time{},
	}
}

func (e *IdleReleaseEvaluator) init() {
	if e.lastCPUTime == nil {
		e.lastCPUTime = map[int]time.Duration{}
		e.lastSampleAt = map[int]time.Time{}
		e.lastActiveAt = map[int]time.Time{}
	}
}

// Evaluate returns the assertions to release, each tagged with why. One assertion can match more
// than one rule (CPU-idle and TTL-expired); callers release by key and treat release as
// idempotent.
//
// Times are compared by wall clock: the monotonic clock stops while a Mac sleeps, and a TTL or the
// age backstop must count the time asleep.
func (e *IdleReleaseEvaluator) Evaluate(assertions []model.Assertion, now time.Time, cfg IdleConfig, p IdleProbes) []Release {
	e.init()
	now = now.Round(0)
	alive := p.PIDAlive
	if alive == nil {
		alive = func(int) bool { return true }
	}
	cpuTime := p.CPUTime
	if cpuTime == nil {
		cpuTime = func(int) (time.Duration, bool) { return 0, false }
	}
	holdsWake := p.TreeHoldsWakeAssertion
	if holdsWake == nil {
		holdsWake = func(int) bool { return false }
	}

	var releases []Release
	for _, a := range assertions {
		// Safety backstop: a too-old assertion is a leak, whatever Enabled or the PID say.
		if cfg.MaxAssertionAge > 0 && now.Sub(a.AcquiredAt) > cfg.MaxAssertionAge {
			releases = append(releases, Release{Key: a.Key, Reason: ReasonMaxAgeBackstop})
			continue
		}
		if a.PID > 0 && !alive(a.PID) {
			releases = append(releases, Release{Key: a.Key, Reason: ReasonDeadProcess})
			continue
		}
		// CPU-rate idle check, only when enabled. Manual holds are exempt: an explicit
		// `lidwake hold` for a background job has no activity to measure and is governed by its
		// TTL. The dead-process rule above still applies to a PID-bound hold, so it releases the
		// moment the watched job exits.
		//
		// The first sighting of a PID only seeds the baseline (and marks it active, so a freshly
		// seen process is never released on the same sweep). Each later sweep recomputes the
		// rate: at or above the threshold the tree is working and lastActiveAt is stamped; below
		// it, the hold releases once the tree has been continuously idle for IdleThreshold. A rate,
		// not an absolute change, because an idle `claude` TUI still burns ~1% CPU — an
		// absolute-delta rule reads that as active forever.
		if cfg.Enabled && a.Origin != model.OriginManual && a.PID > 0 {
			if cpu, ok := cpuTime(a.PID); ok {
				if e.idleExpired(a, cpu, now, cfg, holdsWake) {
					releases = append(releases, Release{Key: a.Key, Reason: ReasonCPUIdle})
				}
			}
		}
		if a.ExpiresAt != nil && a.ExpiresAt.Before(now) {
			releases = append(releases, Release{Key: a.Key, Reason: ReasonTTLExpired})
		}
	}
	return releases
}

// idleExpired records a CPU sample for a's PID and reports whether its tree has been idle past
// the threshold.
func (e *IdleReleaseEvaluator) idleExpired(a model.Assertion, cpu time.Duration, now time.Time, cfg IdleConfig, holdsWake func(int) bool) bool {
	pid := a.PID
	// A baseline that predates this assertion is stale: polling stops whenever nothing is held, so
	// a re-acquire on the same PID (a new turn after a quiet stretch, a resumed session) would
	// otherwise compute its first rate across the whole gap — diluted toward zero — and read a
	// brand-new turn as long idle. Re-seed instead.
	if prevAt, ok := e.lastSampleAt[pid]; ok && prevAt.Before(a.AcquiredAt) {
		delete(e.lastCPUTime, pid)
		delete(e.lastSampleAt, pid)
		delete(e.lastActiveAt, pid)
	}
	prev, havePrev := e.lastCPUTime[pid]
	prevAt, havePrevAt := e.lastSampleAt[pid]
	if !havePrev || !havePrevAt {
		e.lastCPUTime[pid] = cpu
		e.lastSampleAt[pid] = now
		e.lastActiveAt[pid] = now
		return false
	}
	dt := now.Sub(prevAt)
	rate := 0.0
	if dt > 0 {
		rate = float64(cpu-prev) / float64(dt)
	}
	e.lastCPUTime[pid] = cpu
	e.lastSampleAt[pid] = now
	// Active if the tree burns CPU or holds its own wake assertion: an explicit "I'm working"
	// signal that holds through server-side thinking while local CPU is near idle. (Connection
	// presence is deliberately not used — a pooled keep-alive to the API stays established across
	// idle turns and would pin the hold forever.)
	if rate >= cfg.CPURateThreshold || holdsWake(pid) {
		e.lastActiveAt[pid] = now
		return false
	}
	// The idle clock is floored at acquisition and at the assertion's own activity stamp: a hook
	// re-acquire is an explicit "the agent just did something", as authoritative as a CPU sample.
	idleSince := latest(e.lastActiveAt[pid], latest(a.AcquiredAt, a.LastActivityAt))
	return now.Sub(idleSince) > cfg.IdleThreshold
}

// Forget drops the bookkeeping of every PID not in livePIDs, so the per-PID maps cannot grow
// unbounded on a long-running daemon and a recycled PID starts from a fresh baseline instead of
// inheriting a vanished process's CPU total (which could release or pin the new owner wrongly).
// Call once per sweep with the PIDs still backing a live assertion.
func (e *IdleReleaseEvaluator) Forget(livePIDs map[int]bool) {
	e.init()
	for pid := range e.lastSampleAt {
		if !livePIDs[pid] {
			delete(e.lastSampleAt, pid)
		}
	}
	for pid := range e.lastCPUTime {
		if !livePIDs[pid] {
			delete(e.lastCPUTime, pid)
		}
	}
	for pid := range e.lastActiveAt {
		if !livePIDs[pid] {
			delete(e.lastActiveAt, pid)
		}
	}
}

// latest is the later of a and b by wall clock. Assertion times can carry monotonic readings, and
// the monotonic clock stops while a Mac sleeps, so it must not decide the order here either.
func latest(a, b time.Time) time.Time {
	a, b = a.Round(0), b.Round(0)
	if a.After(b) {
		return a
	}
	return b
}
