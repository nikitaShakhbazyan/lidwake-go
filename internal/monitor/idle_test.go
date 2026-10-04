package monitor

import (
	"errors"
	"maps"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/nikitaShakhbazyan/lidwake-go/internal/activity"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/agents"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/model"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/settings"
)

// fakeProcs is a process table: liveness, parent→children and cumulative CPU per PID.
type fakeProcs struct {
	mu        sync.Mutex
	dead      map[int]bool
	children  map[int][]int
	cpu       map[int]time.Duration
	wake      map[int]bool
	treeReads int
	wakeReads int
}

func newFakeProcs() *fakeProcs {
	return &fakeProcs{dead: map[int]bool{}, children: map[int][]int{}, cpu: map[int]time.Duration{}, wake: map[int]bool{}}
}

func (f *fakeProcs) setCPU(pid int, d time.Duration) {
	f.mu.Lock()
	f.cpu[pid] = d
	f.mu.Unlock()
}

func (f *fakeProcs) addCPU(pid int, d time.Duration) {
	f.mu.Lock()
	f.cpu[pid] += d
	f.mu.Unlock()
}

func (f *fakeProcs) resolver() *agents.Resolver {
	return &agents.Resolver{
		ProcessAlive: func(pid int) bool {
			f.mu.Lock()
			defer f.mu.Unlock()
			return !f.dead[pid]
		},
		ProcessTree: func() (map[int][]int, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.treeReads++
			return maps.Clone(f.children), nil
		},
		CPUTime: func(pid int) (time.Duration, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			d, ok := f.cpu[pid]
			if !ok {
				return 0, errors.New("no such process")
			}
			return d, nil
		},
	}
}

func (f *fakeProcs) wakePIDs() map[int]bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.wakeReads++
	return maps.Clone(f.wake)
}

func (f *fakeProcs) reads() (tree, wake int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.treeReads, f.wakeReads
}

// fakeRegistry is a settable assertion snapshot.
type fakeRegistry struct {
	mu    sync.Mutex
	as    []model.Assertion
	reads int
}

func (r *fakeRegistry) set(as ...model.Assertion) {
	r.mu.Lock()
	r.as = as
	r.mu.Unlock()
}

func (r *fakeRegistry) snapshot() []model.Assertion {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reads++
	return slices.Clone(r.as)
}

func (r *fakeRegistry) readCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reads
}

var t0 = time.Date(2026, 10, 4, 10, 0, 0, 0, time.UTC)

func hookAssertion(key string, pid int, at time.Time) model.Assertion {
	return model.New(key, "claude-code", "", pid, "claude", at, nil, model.OriginHook)
}

type idleFixture struct {
	m        *IdleMonitor
	procs    *fakeProcs
	reg      *fakeRegistry
	clock    *fakeClock
	released *recorder[[]activity.Release]
}

func newIdleFixture() *idleFixture {
	f := &idleFixture{procs: newFakeProcs(), reg: &fakeRegistry{}, clock: newFakeClock(t0), released: &recorder[[]activity.Release]{}}
	f.m = &IdleMonitor{
		Assertions: f.reg.snapshot,
		OnRelease:  f.released.add,
		Procs:      f.procs.resolver(),
		WakePIDs:   f.procs.wakePIDs,
		Now:        f.clock.Now,
		Log:        quiet(),
	}
	return f
}

// sweepAt advances the clock by d and sweeps once, returning that sweep's releases.
func (f *idleFixture) sweepAt(d time.Duration) []activity.Release {
	f.clock.Advance(d)
	n := f.released.len()
	f.m.sweep()
	if all := f.released.all(); len(all) > n {
		return all[n]
	}
	return nil
}

func TestIdleMonitor(t *testing.T) {
	t.Run("releases the assertion of a dead process", func(t *testing.T) {
		f := newIdleFixture()
		f.reg.set(hookAssertion("k", 100, t0))
		f.procs.dead[100] = true
		got := f.sweepAt(0)
		if len(got) != 1 || got[0] != (activity.Release{Key: "k", Reason: activity.ReasonDeadProcess}) {
			t.Fatalf("releases = %v", got)
		}
	})

	t.Run("releases once the tree stays CPU-idle past the window", func(t *testing.T) {
		f := newIdleFixture()
		f.reg.set(hookAssertion("k", 100, t0))
		f.procs.setCPU(100, time.Second)
		if got := f.sweepAt(0); got != nil {
			t.Fatalf("seed sweep released %v", got)
		}
		if got := f.sweepAt(30 * time.Second); got != nil {
			t.Fatalf("released after 30 s: %v", got)
		}
		got := f.sweepAt(61 * time.Second)
		if len(got) != 1 || got[0].Reason != activity.ReasonCPUIdle {
			t.Fatalf("releases after 91 s idle = %v", got)
		}
	})

	t.Run("a busy child keeps the tree active", func(t *testing.T) {
		f := newIdleFixture()
		f.reg.set(hookAssertion("k", 100, t0))
		f.procs.children[100] = []int{200}
		f.procs.setCPU(100, time.Second)
		f.procs.setCPU(200, time.Second)
		f.sweepAt(0)
		for range 6 {
			f.procs.addCPU(200, 2*time.Second) // ~6.7% of a core
			if got := f.sweepAt(30 * time.Second); got != nil {
				t.Fatalf("released a tree whose child is busy: %v", got)
			}
		}
	})

	t.Run("a wake assertion held by a descendant keeps an idle tree active", func(t *testing.T) {
		f := newIdleFixture()
		f.reg.set(hookAssertion("k", 100, t0))
		f.procs.children[100] = []int{200}
		f.procs.children[200] = []int{300}
		f.procs.setCPU(100, time.Second)
		f.procs.wake[300] = true // the agent's caffeinate grandchild
		f.sweepAt(0)
		for range 6 {
			if got := f.sweepAt(30 * time.Second); got != nil {
				t.Fatalf("released while a descendant holds a wake assertion: %v", got)
			}
		}
	})

	t.Run("a wake assertion outside the tree does not count", func(t *testing.T) {
		f := newIdleFixture()
		f.reg.set(hookAssertion("k", 100, t0))
		f.procs.children[100] = []int{200}
		f.procs.children[1] = []int{100, 999}
		f.procs.setCPU(100, time.Second)
		f.procs.wake[999] = true
		f.sweepAt(0)
		f.sweepAt(30 * time.Second)
		if got := f.sweepAt(61 * time.Second); len(got) != 1 || got[0].Reason != activity.ReasonCPUIdle {
			t.Fatalf("releases = %v", got)
		}
	})

	t.Run("one process-table snapshot and one wake read per sweep", func(t *testing.T) {
		f := newIdleFixture()
		f.reg.set(hookAssertion("a", 100, t0), hookAssertion("b", 200, t0), hookAssertion("c", 300, t0))
		for _, pid := range []int{100, 200, 300} {
			f.procs.setCPU(pid, time.Second)
		}
		f.sweepAt(0)
		if tree, wake := f.procs.reads(); tree != 1 || wake != 0 {
			t.Fatalf("seed sweep: %d snapshots, %d wake reads; want 1 and 0", tree, wake)
		}
		f.sweepAt(30 * time.Second)
		if tree, wake := f.procs.reads(); tree != 2 || wake != 1 {
			t.Fatalf("after two sweeps: %d snapshots, %d wake reads; want 2 and 1", tree, wake)
		}
	})

	t.Run("manual holds read no process table", func(t *testing.T) {
		f := newIdleFixture()
		f.reg.set(model.New("hold:1", "manual", "", 100, "manual", t0, nil, model.OriginManual))
		f.procs.setCPU(100, time.Second)
		for range 3 {
			f.sweepAt(60 * time.Second)
		}
		if tree, wake := f.procs.reads(); tree != 0 || wake != 0 {
			t.Errorf("%d snapshots, %d wake reads for a manual hold", tree, wake)
		}
	})

	t.Run("one release per key with the first reason", func(t *testing.T) {
		f := newIdleFixture()
		a := hookAssertion("k", 100, t0)
		exp := t0.Add(time.Minute)
		a.ExpiresAt = &exp
		f.reg.set(a)
		f.procs.setCPU(100, time.Second)
		f.sweepAt(0)
		got := f.sweepAt(2 * time.Minute) // idle past the window and past its TTL
		if len(got) != 1 || got[0] != (activity.Release{Key: "k", Reason: activity.ReasonCPUIdle}) {
			t.Fatalf("releases = %v", got)
		}
	})

	t.Run("manual holds are exempt from CPU idle but not from their TTL", func(t *testing.T) {
		f := newIdleFixture()
		ttl := 10 * time.Minute
		f.reg.set(model.New("hold:1", "manual", "", 100, "manual", t0, &ttl, model.OriginManual))
		f.procs.setCPU(100, time.Second)
		for range 5 {
			if got := f.sweepAt(time.Minute); got != nil {
				t.Fatalf("released a manual hold: %v", got)
			}
		}
		got := f.sweepAt(6 * time.Minute)
		if len(got) != 1 || got[0].Reason != activity.ReasonTTLExpired {
			t.Fatalf("releases = %v", got)
		}
	})

	t.Run("with idle release off only the safety rules apply", func(t *testing.T) {
		f := newIdleFixture()
		s := settings.Defaults()
		s.IdleReleaseEnabled = false
		f.m.ApplySettings(s)
		f.reg.set(hookAssertion("idle", 100, t0), hookAssertion("dead", 200, t0))
		f.procs.setCPU(100, time.Second)
		f.procs.dead[200] = true
		f.sweepAt(0)
		got := f.sweepAt(5 * time.Minute)
		if len(got) != 1 || got[0] != (activity.Release{Key: "dead", Reason: activity.ReasonDeadProcess}) {
			t.Fatalf("releases = %v", got)
		}
	})

	t.Run("the idle window comes from settings", func(t *testing.T) {
		f := newIdleFixture()
		s := settings.Defaults()
		s.IdleReleaseSeconds = 300
		f.m.ApplySettings(s)
		f.reg.set(hookAssertion("k", 100, t0))
		f.procs.setCPU(100, time.Second)
		f.sweepAt(0)
		if got := f.sweepAt(200 * time.Second); got != nil {
			t.Fatalf("released inside a 300 s window: %v", got)
		}
		if got := f.sweepAt(101 * time.Second); len(got) != 1 {
			t.Fatalf("not released past the window: %v", got)
		}
	})

	t.Run("the age backstop releases a leaked assertion", func(t *testing.T) {
		f := newIdleFixture()
		f.reg.set(hookAssertion("leak", 0, t0.Add(-25*time.Hour)))
		got := f.sweepAt(0)
		if len(got) != 1 || got[0].Reason != activity.ReasonMaxAgeBackstop {
			t.Fatalf("releases = %v", got)
		}
	})

	t.Run("resetting baselines re-seeds the next sweep", func(t *testing.T) {
		f := newIdleFixture()
		f.reg.set(hookAssertion("k", 100, t0))
		f.procs.setCPU(100, time.Second)
		f.sweepAt(0)
		f.m.ResetBaselines() // the Mac slept through the next 200 s
		if got := f.sweepAt(200 * time.Second); got != nil {
			t.Fatalf("released across a sleep: %v", got)
		}
		if got := f.sweepAt(30 * time.Second); got != nil {
			t.Fatalf("released 30 s after re-seeding: %v", got)
		}
	})

	t.Run("bookkeeping is dropped for pids no longer backing an assertion", func(t *testing.T) {
		f := newIdleFixture()
		a := hookAssertion("k", 100, t0)
		f.reg.set(a)
		f.procs.setCPU(100, time.Second)
		f.sweepAt(0)
		f.reg.set()
		f.sweepAt(10 * time.Second)
		f.reg.set(a) // back, with the same acquisition time
		if got := f.sweepAt(190 * time.Second); got != nil {
			t.Fatalf("released against a forgotten baseline: %v", got)
		}
	})

	t.Run("without an assertion source a sweep does nothing", func(t *testing.T) {
		f := newIdleFixture()
		f.m.Assertions = nil
		f.m.sweep()
		if tree, wake := f.procs.reads(); tree != 0 || wake != 0 || f.released.len() != 0 {
			t.Error("a sweep without a source did work")
		}
	})

	t.Run("sweeps only while blocking and forgets baselines when blocking ends", func(t *testing.T) {
		f := newIdleFixture()
		f.m.Interval = time.Millisecond
		f.reg.set(hookAssertion("k", 100, t0))
		f.procs.setCPU(100, time.Second)
		start(t, f.m)
		stays(t, "no sweeps while idle", 30*time.Millisecond, func() bool { return f.reg.readCount() == 0 })
		f.m.SetBlocking(true)
		eventually(t, "sweeps", func() bool { return f.reg.readCount() > 3 })
		f.m.SetBlocking(false)
		eventually(t, "sweeps to stop", func() bool {
			n := f.reg.readCount()
			time.Sleep(5 * time.Millisecond)
			return f.reg.readCount() == n
		})
		f.clock.Advance(200 * time.Second) // the baseline would now read as long idle
		n := f.reg.readCount()
		f.m.SetBlocking(true)
		eventually(t, "sweeps again", func() bool { return f.reg.readCount() > n+3 })
		if f.released.len() != 0 {
			t.Fatalf("released against a baseline from before the gap: %v", f.released.all())
		}
	})
}

func TestIdleConfigFor(t *testing.T) {
	s := settings.Defaults()
	s.IdleReleaseEnabled = false
	s.IdleReleaseSeconds = 120
	cfg := IdleConfigFor(s)
	def := activity.DefaultIdleConfig()
	if cfg.Enabled || cfg.IdleThreshold != 2*time.Minute || cfg.CPURateThreshold != def.CPURateThreshold || cfg.MaxAssertionAge != def.MaxAssertionAge {
		t.Errorf("cfg = %+v", cfg)
	}
	if got := IdleConfigFor(settings.Defaults()); got != def {
		t.Errorf("defaults = %+v, want %+v", got, def)
	}
}

// The wake detector ticks on its own timer, so after a sleep the idle timer can fire before the
// daemon's wake handler resets the baselines. The timed sweep notices the sleep itself.
func TestIdleMonitorSleepBetweenSweeps(t *testing.T) {
	t.Run("a timed sweep after a sleep re-seeds instead of releasing a working agent", func(t *testing.T) {
		f := newIdleFixture()
		f.m.Interval = time.Millisecond
		f.reg.set(hookAssertion("k", 100, t0))
		f.procs.setCPU(100, time.Second)
		f.m.SetBlocking(true)
		start(t, f.m)
		eventually(t, "sweeps", func() bool { return f.reg.readCount() > 3 })
		f.clock.Advance(time.Hour) // the Mac slept; no wake handler has run
		n := f.reg.readCount()
		eventually(t, "sweeps after the sleep", func() bool { return f.reg.readCount() > n+3 })
		if f.released.len() != 0 {
			t.Fatalf("released across a sleep: %v", f.released.all())
		}
	})

	t.Run("timed sweeps an interval apart still release past the window", func(t *testing.T) {
		f := newIdleFixture()
		f.m.Interval = 30 * time.Second
		f.reg.set(hookAssertion("k", 100, t0))
		f.procs.setCPU(100, time.Second)
		f.m.timedSweep()
		for range 3 {
			f.clock.Advance(30 * time.Second)
			f.m.timedSweep()
		}
		if f.released.len() != 0 {
			t.Fatalf("released at 90 s: %v", f.released.all())
		}
		f.clock.Advance(31 * time.Second) // late, but within two intervals
		f.m.timedSweep()
		if got := f.released.all(); len(got) != 1 || got[0][0].Reason != activity.ReasonCPUIdle {
			t.Fatalf("releases = %v", got)
		}
	})

	t.Run("a gap of more than two intervals re-seeds", func(t *testing.T) {
		f := newIdleFixture()
		f.m.Interval = 30 * time.Second
		f.reg.set(hookAssertion("k", 100, t0))
		f.procs.setCPU(100, time.Second)
		f.m.timedSweep()
		f.clock.Advance(61 * time.Second)
		f.m.timedSweep()
		f.clock.Advance(30 * time.Second) // 91 s after the first sample, 30 s after the re-seed
		f.m.timedSweep()
		if f.released.len() != 0 {
			t.Fatalf("released against the pre-gap baseline: %v", f.released.all())
		}
		for range 3 {
			f.clock.Advance(30 * time.Second)
			f.m.timedSweep()
		}
		if f.released.len() != 1 {
			t.Fatalf("not released 120 s after the re-seed: %v", f.released.all())
		}
	})
}
