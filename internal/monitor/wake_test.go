package monitor

import (
	"testing"
	"time"
)

func TestWakeDetector(t *testing.T) {
	base := time.Date(2026, 10, 4, 10, 0, 0, 0, time.UTC)

	t.Run("a wall-clock gap far beyond the period is a wake", func(t *testing.T) {
		d := &WakeDetector{Period: 10 * time.Second}
		if d.observe(base) {
			t.Fatal("the first tick is a wake")
		}
		if d.observe(base.Add(11 * time.Second)) {
			t.Error("a late tick is a wake")
		}
		if d.observe(base.Add(11*time.Second + 30*time.Second)) {
			t.Error("exactly the threshold is a wake")
		}
		if !d.observe(base.Add(41*time.Second + 2*time.Hour)) {
			t.Error("two hours asleep is not a wake")
		}
		if d.observe(base.Add(41*time.Second + 2*time.Hour + 10*time.Second)) {
			t.Error("the tick after a wake is a wake")
		}
	})

	t.Run("a clock that went backwards is not a wake", func(t *testing.T) {
		d := &WakeDetector{Period: time.Second}
		d.observe(base)
		if d.observe(base.Add(-time.Hour)) {
			t.Error("backwards jump read as a wake")
		}
		if d.observe(base.Add(-time.Hour + time.Second)) {
			t.Error("a normal tick after the jump read as a wake")
		}
	})

	t.Run("threshold defaults to three periods", func(t *testing.T) {
		for _, tc := range []struct {
			period, threshold, want time.Duration
		}{
			{0, 0, 30 * time.Second},
			{time.Second, 0, 3 * time.Second},
			{time.Second, time.Second, 3 * time.Second},
			{time.Second, 10 * time.Second, 10 * time.Second},
		} {
			d := &WakeDetector{Period: tc.period, Threshold: tc.threshold}
			if got := d.threshold(); got != tc.want {
				t.Errorf("period %v threshold %v: got %v, want %v", tc.period, tc.threshold, got, tc.want)
			}
		}
	})

	t.Run("reports each wake once from the loop", func(t *testing.T) {
		clock := newFakeClock(base)
		var ticks counter
		now := func() time.Time { ticks.inc(); return clock.Now() }
		var wakes counter
		d := &WakeDetector{Period: time.Millisecond, Now: now, OnWake: wakes.inc, Log: quiet()}
		start(t, d)
		eventually(t, "ticks", func() bool { return ticks.get() > 3 })
		if wakes.get() != 0 {
			t.Fatal("woke without a gap")
		}
		clock.Advance(time.Hour)
		eventually(t, "wake", func() bool { return wakes.get() == 1 })
		n := ticks.get()
		eventually(t, "more ticks", func() bool { return ticks.get() > n+3 })
		if wakes.get() != 1 {
			t.Errorf("woke %d times for one sleep", wakes.get())
		}
	})
}

func TestWakeDetectorSlowHandler(t *testing.T) {
	t.Run("a slow wake handler does not read as another wake", func(t *testing.T) {
		clock := newFakeClock(time.Date(2026, 10, 4, 10, 0, 0, 0, time.UTC))
		var ticks counter
		now := func() time.Time { ticks.inc(); return clock.Now() }
		var wakes counter
		d := &WakeDetector{Period: time.Millisecond, Now: now, Log: quiet()}
		// The handler takes far longer than the threshold (a wedged helper round-trip).
		d.OnWake = func() { wakes.inc(); clock.Advance(time.Hour) }
		start(t, d)
		eventually(t, "ticks", func() bool { return ticks.get() > 3 })
		clock.Advance(2 * time.Hour)
		eventually(t, "wake", func() bool { return wakes.get() >= 1 })
		n := ticks.get()
		eventually(t, "more ticks", func() bool { return ticks.get() > n+5 })
		if got := wakes.get(); got != 1 {
			t.Errorf("woke %d times for one sleep: the handler's own duration read as a sleep", got)
		}
	})

	t.Run("stop ends ticking", func(t *testing.T) {
		var ticks counter
		now := func() time.Time { ticks.inc(); return time.Now() }
		d := &WakeDetector{Period: time.Millisecond, Now: now, Log: quiet()}
		start(t, d)
		eventually(t, "ticks", func() bool { return ticks.get() > 2 })
		d.Stop()
		n := ticks.get()
		stays(t, "no ticks after Stop", 20*time.Millisecond, func() bool { return ticks.get() == n })
	})
}
