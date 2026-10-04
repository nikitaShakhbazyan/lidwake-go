package monitor

import (
	"sync"
	"testing"
	"time"

	"github.com/nikitaShakhbazyan/lidwake-go/internal/darwin"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/policy"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/settings"
)

// fakeBattery is a settable battery; absent is a desktop Mac.
type fakeBattery struct {
	mu      sync.Mutex
	b       darwin.Battery
	absent  bool
	readsMu sync.Mutex
	reads   int
}

func (f *fakeBattery) set(percent int, onBattery bool) {
	f.mu.Lock()
	f.b = darwin.Battery{Percent: percent, OnBattery: onBattery}
	f.mu.Unlock()
}

func (f *fakeBattery) read() (darwin.Battery, bool) {
	f.readsMu.Lock()
	f.reads++
	f.readsMu.Unlock()
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.b, !f.absent
}

func (f *fakeBattery) readCount() int {
	f.readsMu.Lock()
	defer f.readsMu.Unlock()
	return f.reads
}

// newTestBattery returns a monitor whose poll never fires on its own within a test: every
// evaluation below comes from Start or a gate change.
func newTestBattery(b *fakeBattery) (*BatteryMonitor, *recorder[policy.CutoutCause], *recorder[darwin.Battery]) {
	var cuts recorder[policy.CutoutCause]
	var readings recorder[darwin.Battery]
	m := &BatteryMonitor{Read: b.read, Interval: time.Hour, OnCutout: cuts.add, OnReading: readings.add, Log: quiet()}
	return m, &cuts, &readings
}

func TestBatteryMonitor(t *testing.T) {
	t.Run("start seeds the last reading and evaluates at once", func(t *testing.T) {
		b := &fakeBattery{}
		b.set(57, true)
		m, cuts, readings := newTestBattery(b)
		start(t, m)
		if r, ok := m.Last(); !ok || r.Percent != 57 || !r.OnBattery {
			t.Fatalf("Last = %+v %v right after Start", r, ok)
		}
		eventually(t, "the first evaluation", func() bool { return readings.len() == 1 })
		if cuts.len() != 0 {
			t.Error("cut out while not blocking")
		}
	})

	t.Run("low battery cuts out on battery with the lid closed while blocking", func(t *testing.T) {
		b := &fakeBattery{}
		b.set(20, true) // at the default threshold
		m, cuts, readings := newTestBattery(b)
		start(t, m)
		eventually(t, "the first evaluation", func() bool { return readings.len() == 1 })
		m.SetLidClosed(true)
		eventually(t, "lid edge read", func() bool { return readings.len() == 2 })
		if cuts.len() != 0 {
			t.Fatal("cut out while not blocking")
		}
		m.SetBlocking(true)
		eventually(t, "cutout", func() bool { return cuts.len() == 1 })
		if got := cuts.all()[0]; got != policy.CutoutLowBattery {
			t.Errorf("cause = %q", got)
		}
	})

	t.Run("closing the lid while already low on battery cuts out at once", func(t *testing.T) {
		b := &fakeBattery{}
		b.set(10, true)
		m, cuts, _ := newTestBattery(b)
		m.SetBlocking(true) // before Start: picked up when the loop runs
		start(t, m)
		stays(t, "no cutout with the lid open", 20*time.Millisecond, func() bool { return cuts.len() == 0 })
		m.SetLidClosed(true)
		eventually(t, "cutout", func() bool { return cuts.len() == 1 })
	})

	t.Run("no low-battery cutout above the threshold, on AC or when disabled", func(t *testing.T) {
		for name, tc := range map[string]struct {
			percent   int
			onBattery bool
			enabled   bool
		}{
			"above the threshold": {21, true, true},
			"on AC":               {5, false, true},
			"disabled":            {5, true, false},
		} {
			t.Run(name, func(t *testing.T) {
				b := &fakeBattery{}
				b.set(tc.percent, tc.onBattery)
				m, cuts, readings := newTestBattery(b)
				s := settings.Defaults()
				s.LowBatteryCutoutEnabled = tc.enabled
				m.ApplySettings(s)
				m.SetLidClosed(true)
				m.SetBlocking(true)
				start(t, m)
				eventually(t, "evaluation", func() bool { return readings.len() >= 1 })
				m.SetBlocking(false)
				m.SetBlocking(true)
				eventually(t, "more evaluations", func() bool { return readings.len() >= 2 })
				if cuts.len() != 0 {
					t.Errorf("cut out: %v", cuts.all())
				}
			})
		}
	})

	t.Run("the threshold comes from settings", func(t *testing.T) {
		b := &fakeBattery{}
		b.set(30, true)
		m, cuts, _ := newTestBattery(b)
		s := settings.Defaults()
		s.LowBatteryThresholdPercent = 30
		m.ApplySettings(s)
		m.SetLidClosed(true)
		m.SetBlocking(true)
		start(t, m)
		eventually(t, "cutout", func() bool { return cuts.len() == 1 })
	})

	t.Run("AC-only mode cuts out on battery with the lid open, ahead of low battery", func(t *testing.T) {
		b := &fakeBattery{}
		b.set(5, true)
		m, cuts, readings := newTestBattery(b)
		m.SetLidClosed(true)
		m.SetBlocking(true)
		start(t, m)
		eventually(t, "low-battery cutout", func() bool { return cuts.len() == 1 })
		m.SetLidClosed(false)
		eventually(t, "lid edge read", func() bool { return readings.len() == 2 })
		s := settings.Defaults()
		s.RequireACPower = true
		n := readings.len()
		m.ApplySettings(s) // turning AC-only on reads at once
		eventually(t, "AC-only cutout", func() bool { return cuts.len() == 2 })
		if got := cuts.all(); got[1] != policy.CutoutOnBattery {
			t.Errorf("causes = %v", got)
		}
		if readings.len() != n+1 {
			t.Errorf("readings = %d, want one more", readings.len())
		}
		// Plugged in: nothing fires.
		b.set(5, false)
		m.SetBlocking(false)
		m.SetBlocking(true)
		eventually(t, "re-read", func() bool { return readings.len() >= n+2 })
		stays(t, "no cutout on AC", 10*time.Millisecond, func() bool { return cuts.len() == 2 })
	})

	t.Run("a threshold change does not read", func(t *testing.T) {
		b := &fakeBattery{}
		m, _, readings := newTestBattery(b)
		start(t, m)
		eventually(t, "the first evaluation", func() bool { return readings.len() == 1 })
		s := settings.Defaults()
		s.LowBatteryThresholdPercent = 50
		m.ApplySettings(s)
		stays(t, "no read", 20*time.Millisecond, func() bool { return readings.len() == 1 })
	})

	t.Run("polls on the interval while idle", func(t *testing.T) {
		b := &fakeBattery{}
		b.set(80, false)
		m, _, readings := newTestBattery(b)
		m.Interval = time.Millisecond
		start(t, m)
		eventually(t, "polls", func() bool { return readings.len() > 5 })
	})

	t.Run("a Mac without a battery has no reading and no callbacks", func(t *testing.T) {
		b := &fakeBattery{absent: true}
		m, cuts, readings := newTestBattery(b)
		m.Interval = time.Millisecond
		m.SetBlocking(true)
		m.SetLidClosed(true)
		start(t, m)
		eventually(t, "polls", func() bool { return b.readCount() > 5 })
		if _, ok := m.Last(); ok || readings.len() != 0 || cuts.len() != 0 {
			t.Errorf("reading=%v readings=%d cuts=%d", ok, readings.len(), cuts.len())
		}
	})

	t.Run("stop ends polling", func(t *testing.T) {
		b := &fakeBattery{}
		m, _, _ := newTestBattery(b)
		m.Interval = time.Millisecond
		start(t, m)
		eventually(t, "polls", func() bool { return b.readCount() > 2 })
		m.Stop()
		n := b.readCount()
		m.SetBlocking(true)
		stays(t, "no reads after Stop", 20*time.Millisecond, func() bool { return b.readCount() == n })
	})
}
