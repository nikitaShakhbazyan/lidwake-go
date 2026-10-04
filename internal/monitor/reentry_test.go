package monitor

import (
	"testing"
	"time"

	"github.com/nikitaShakhbazyan/lidwake-go/internal/activity"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/darwin"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/policy"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/settings"
)

// Callbacks run outside the monitors' locks, so a handler may call back into the monitor that
// called it: the daemon re-reads state and flips gates from inside its handlers. A lock held
// across a callback would deadlock these tests.
func TestCallbacksMayCallBackIn(t *testing.T) {
	t.Run("lid change sees the new state", func(t *testing.T) {
		lid := &fakeLid{hasLid: true}
		var seen recorder[bool]
		m := &LidMonitor{Read: lid.read, Interval: time.Millisecond, Log: quiet()}
		m.OnChange = func(closed bool) { seen.add(m.Closed() == closed) }
		start(t, m)
		lid.set(true)
		eventually(t, "the change", func() bool { return seen.len() == 1 })
		if !seen.all()[0] {
			t.Error("Closed() inside OnChange is not the new state")
		}
	})

	t.Run("battery cutout handler ends blocking", func(t *testing.T) {
		b := &fakeBattery{}
		b.set(5, true)
		var cuts recorder[policy.CutoutCause]
		var lastMatches recorder[bool]
		m := &BatteryMonitor{Read: b.read, Interval: time.Millisecond, Log: quiet()}
		m.OnReading = func(r darwin.Battery) {
			last, ok := m.Last()
			lastMatches.add(ok && last == r)
		}
		m.OnCutout = func(c policy.CutoutCause) {
			cuts.add(c)
			// What the daemon does: release everything, which ends blocking.
			m.SetBlocking(false)
			m.ApplySettings(settings.Defaults())
		}
		m.SetLidClosed(true)
		m.SetBlocking(true)
		start(t, m)
		eventually(t, "the cutout", func() bool { return cuts.len() == 1 })
		n := lastMatches.len()
		eventually(t, "polls continue", func() bool { return lastMatches.len() > n+3 })
		if cuts.len() != 1 {
			t.Errorf("cut out %d times after blocking ended", cuts.len())
		}
		for _, ok := range lastMatches.all() {
			if !ok {
				t.Fatal("Last() inside OnReading is not the reading")
			}
		}
	})

	t.Run("thermal cutout handler ends blocking", func(t *testing.T) {
		s := &fakeSensor{}
		s.set(95, true)
		o := &sensorOpener{sensor: s}
		var cuts counter
		var reads counter
		m := &ThermalMonitor{Open: o.open, Interval: time.Millisecond, Log: quiet()}
		m.OnReading = func(c float64) {
			reads.inc()
			if got, ok := m.Current(); !ok || got != c {
				t.Errorf("Current() inside OnReading = %v %v, want %v", got, ok, c)
			}
		}
		m.OnCutout = func() {
			cuts.inc()
			_, _ = m.ReadNow()
			m.SetBlocking(false)
		}
		m.SetLidClosed(true)
		m.SetBlocking(true)
		start(t, m)
		eventually(t, "the cutout", func() bool { return cuts.get() == 1 })
		n := s.readCount()
		stays(t, "no polls once the handler ended blocking", 20*time.Millisecond, func() bool {
			return s.readCount() <= n+1 && cuts.get() == 1
		})
	})

	t.Run("idle release handler resets baselines and ends blocking", func(t *testing.T) {
		f := newIdleFixture()
		f.m.Interval = time.Millisecond
		f.reg.set(hookAssertion("k", 100, t0))
		f.procs.dead[100] = true
		var batches counter
		f.m.OnRelease = func(rs []activity.Release) {
			batches.inc()
			f.reg.set()
			f.m.ResetBaselines()
			f.m.SetBlocking(false)
			f.m.ApplySettings(settings.Defaults())
		}
		f.m.SetBlocking(true)
		start(t, f.m)
		eventually(t, "the release", func() bool { return batches.get() == 1 })
		eventually(t, "sweeps to stop", func() bool {
			n := f.reg.readCount()
			time.Sleep(5 * time.Millisecond)
			return f.reg.readCount() == n
		})
	})

	t.Run("process exit handler watches again", func(t *testing.T) {
		src := newFakeExits()
		live := &livePIDs{}
		w, _ := newTestWatcher(src, live)
		var exits recorder[int]
		w.OnExit = func(pid int) {
			exits.add(pid)
			if w.Watching(pid) {
				t.Errorf("pid %d still watched inside OnExit", pid)
			}
			w.Watch(pid + 1)
			w.SetBlocking(false)
		}
		start(t, w)
		w.Watch(100)
		src.exit(100)
		eventually(t, "the exit", func() bool { return exits.len() == 1 })
		if !w.Watching(101) {
			t.Error("a Watch from inside OnExit was lost")
		}
	})
}
