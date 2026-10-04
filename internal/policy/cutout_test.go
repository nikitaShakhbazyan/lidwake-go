package policy

import (
	"slices"
	"strings"
	"testing"
)

func f64(v float64) *float64 { return &v }
func intp(v int) *int        { return &v }
func boolp(v bool) *bool     { return &v }

func conditions(temp *float64, battery *int, onBattery *bool, lidClosed bool) Conditions {
	return Conditions{
		TemperatureCelsius:      temp,
		ThermalThresholdCelsius: 80,
		BatteryPercent:          battery,
		OnBattery:               onBattery,
		BatteryThresholdPercent: 20,
		LidClosed:               lidClosed,
	}
}

func causes(c ...CutoutCause) []CutoutCause { return c }

// The latch prevents the acquire → cutout → release → re-acquire oscillation: a cutout's
// rejection of new acquires must hold until the hazard genuinely recedes (with hysteresis) or
// the lid opens.
func TestCutoutLatch(t *testing.T) {
	t.Run("tripping latches and produces a rejection message", func(t *testing.T) {
		latch := NewCutoutLatch()
		if latch.IsLatched() || latch.RejectionMessage() != "" {
			t.Fatalf("fresh latch: latched=%v message=%q", latch.IsLatched(), latch.RejectionMessage())
		}
		latch.Trip(CutoutThermal)
		if !latch.IsLatched() || latch.RejectionMessage() == "" {
			t.Fatalf("tripped latch: latched=%v message=%q", latch.IsLatched(), latch.RejectionMessage())
		}
	})

	t.Run("thermal latch holds at the threshold and clears only past hysteresis", func(t *testing.T) {
		latch := NewCutoutLatch()
		latch.Trip(CutoutThermal)
		// Just under the threshold is not enough — that's the oscillation the latch exists for.
		latch.Update(conditions(f64(79), nil, nil, true))
		if !latch.IsLatched() {
			t.Fatal("cleared just under the threshold")
		}
		// At threshold − hysteresis it clears.
		cleared := latch.Update(conditions(f64(75), nil, nil, true))
		if !slices.Equal(cleared, causes(CutoutThermal)) || latch.IsLatched() {
			t.Fatalf("cleared=%v latched=%v", cleared, latch.IsLatched())
		}
	})

	t.Run("battery latch clears on AC or with charge margin, not on a missing reading", func(t *testing.T) {
		latch := NewCutoutLatch()
		latch.Trip(CutoutLowBattery)
		// Unknown readings keep the latch — a cutout must not clear on missing data.
		latch.Update(conditions(nil, nil, nil, true))
		if !latch.IsLatched() {
			t.Fatal("cleared on missing data")
		}
		// Still on battery, barely above threshold — not enough margin.
		latch.Update(conditions(nil, intp(22), boolp(true), true))
		if !latch.IsLatched() {
			t.Fatal("cleared without margin")
		}
		// Plugged in clears immediately.
		cleared := latch.Update(conditions(nil, intp(22), boolp(false), true))
		if !slices.Equal(cleared, causes(CutoutLowBattery)) {
			t.Fatalf("cleared=%v", cleared)
		}
	})

	t.Run("battery latch clears once charged past the hysteresis", func(t *testing.T) {
		latch := NewCutoutLatch()
		latch.Trip(CutoutLowBattery)
		latch.Update(conditions(nil, intp(24), boolp(true), true))
		if !latch.IsLatched() {
			t.Fatal("cleared below threshold + hysteresis")
		}
		cleared := latch.Update(conditions(nil, intp(25), boolp(true), true))
		if !slices.Equal(cleared, causes(CutoutLowBattery)) {
			t.Fatalf("cleared=%v", cleared)
		}
	})

	t.Run("opening the lid clears every cause", func(t *testing.T) {
		latch := NewCutoutLatch()
		latch.Trip(CutoutThermal)
		latch.Trip(CutoutLowBattery)
		cleared := latch.Update(conditions(nil, nil, nil, false))
		if !slices.Equal(cleared, causes(CutoutLowBattery, CutoutThermal)) || latch.IsLatched() {
			t.Fatalf("cleared=%v latched=%v", cleared, latch.IsLatched())
		}
	})

	t.Run("causes clear independently", func(t *testing.T) {
		latch := NewCutoutLatch()
		latch.Trip(CutoutThermal)
		latch.Trip(CutoutLowBattery)
		// Cool but still draining: thermal clears, battery stays.
		cleared := latch.Update(conditions(f64(60), intp(15), boolp(true), true))
		if !slices.Equal(cleared, causes(CutoutThermal)) {
			t.Fatalf("cleared=%v", cleared)
		}
		if !slices.Equal(latch.Active(), causes(CutoutLowBattery)) {
			t.Fatalf("active=%v", latch.Active())
		}
	})

	// Lid-open scope, AC-only mode and disabled cutouts.
	t.Run("with lid-open cutouts, opening the lid clears nothing", func(t *testing.T) {
		latch := NewCutoutLatch()
		latch.ClearsOnLidOpen = false
		latch.Trip(CutoutThermal)
		latch.Trip(CutoutLowBattery)
		cleared := latch.Update(conditions(f64(79), intp(15), boolp(true), false))
		if len(cleared) != 0 {
			t.Fatalf("cleared=%v", cleared)
		}
		if msg := latch.RejectionMessage(); msg == "" || strings.Contains(msg, "lid") {
			t.Fatalf("message %q: want one that does not mention the lid", msg)
		}
	})

	t.Run("the AC-only latch clears on AC power alone", func(t *testing.T) {
		latch := NewCutoutLatch()
		latch.Trip(CutoutOnBattery)
		// A full battery or an open lid is not AC power.
		latch.Update(conditions(nil, intp(100), boolp(true), false))
		if !latch.IsLatched() {
			t.Fatal("cleared on battery with the lid open")
		}
		latch.Update(conditions(nil, nil, nil, true))
		if !latch.IsLatched() {
			t.Fatal("cleared on missing data")
		}
		cleared := latch.Update(conditions(nil, intp(100), boolp(false), true))
		if !slices.Equal(cleared, causes(CutoutOnBattery)) {
			t.Fatalf("cleared=%v", cleared)
		}
	})

	t.Run("switching a cutout off drops its latch", func(t *testing.T) {
		latch := NewCutoutLatch()
		latch.Trip(CutoutThermal)
		latch.Trip(CutoutOnBattery)
		dropped := latch.DropDisabled(true, true, false)
		if !slices.Equal(dropped, causes(CutoutOnBattery)) {
			t.Fatalf("dropped=%v", dropped)
		}
		if !slices.Equal(latch.Active(), causes(CutoutThermal)) {
			t.Fatalf("active=%v", latch.Active())
		}
	})

	t.Run("rejection messages name the hazard", func(t *testing.T) {
		cases := []struct {
			trip     []CutoutCause
			clearsOn bool
			want     string
		}{
			{causes(CutoutThermal), true, "The thermal cutout fired — acquires are paused until the Mac cools down or the lid opens."},
			{causes(CutoutLowBattery), false, "The low-battery cutout fired — acquires are paused until charging resumes."},
			{causes(CutoutOnBattery), true, "AC-only mode is on and the Mac is on battery — acquires are paused until it is plugged in."},
			{causes(CutoutThermal, CutoutLowBattery, CutoutOnBattery), true, "Safety cutouts are active (overheating, low battery, running on battery with AC-only mode on) — acquires are paused until conditions recover or the lid opens."},
		}
		for _, c := range cases {
			latch := NewCutoutLatch()
			latch.ClearsOnLidOpen = c.clearsOn
			for _, cause := range c.trip {
				latch.Trip(cause)
			}
			if got := latch.RejectionMessage(); got != c.want {
				t.Errorf("trip %v: got %q, want %q", c.trip, got, c.want)
			}
		}
	})

	t.Run("a copied latch is independent", func(t *testing.T) {
		a := NewCutoutLatch()
		b := a
		b.Trip(CutoutThermal)
		if a.IsLatched() {
			t.Fatal("tripping a copy latched the original")
		}
	})
}

func TestLowBatteryCutout(t *testing.T) {
	gates := func(percent int, onBattery, enabled, lidClosed, blocking bool) bool {
		return LowBatteryCutout{
			BatteryPercent: percent, ThresholdPercent: 20, OnBattery: onBattery,
			Enabled: enabled, LidClosed: lidClosed, Blocking: blocking,
		}.ShouldFire()
	}
	t.Run("fires on battery, lid closed, blocking, at/under threshold", func(t *testing.T) {
		if !gates(15, true, true, true, true) {
			t.Fatal("did not fire")
		}
	})
	t.Run("battery exactly at threshold fires", func(t *testing.T) {
		if !gates(20, true, true, true, true) {
			t.Fatal("did not fire")
		}
	})
	t.Run("above threshold does not fire", func(t *testing.T) {
		if gates(21, true, true, true, true) {
			t.Fatal("fired")
		}
	})
	t.Run("each gate individually closed suppresses the cutout even at empty battery", func(t *testing.T) {
		if gates(1, false, true, true, true) {
			t.Error("fired on AC")
		}
		if gates(1, true, false, true, true) {
			t.Error("fired while disabled")
		}
		if gates(1, true, true, false, true) {
			t.Error("fired with the lid open")
		}
		if gates(1, true, true, true, false) {
			t.Error("fired while not blocking")
		}
	})
	t.Run("AC-only fires on any battery power while blocking, lid open or not", func(t *testing.T) {
		if !ShouldCutoutOnBattery(true, true, true) {
			t.Error("did not fire")
		}
		if ShouldCutoutOnBattery(false, true, true) {
			t.Error("fired on AC")
		}
		if ShouldCutoutOnBattery(true, false, true) {
			t.Error("fired with AC-only off")
		}
		if ShouldCutoutOnBattery(true, true, false) {
			t.Error("fired while not blocking")
		}
	})
}

func TestThermalCutout(t *testing.T) {
	gates := func(temp float64, enabled, lidClosed, blocking bool) bool {
		return ThermalCutout{
			TemperatureCelsius: temp, ThresholdCelsius: 80,
			Enabled: enabled, LidClosed: lidClosed, Blocking: blocking,
		}.ShouldFire()
	}
	t.Run("fires when enabled, lid closed, blocking, and at/over threshold", func(t *testing.T) {
		if !gates(85, true, true, true) {
			t.Fatal("did not fire")
		}
	})
	t.Run("temperature exactly at threshold fires", func(t *testing.T) {
		if !gates(80, true, true, true) {
			t.Fatal("did not fire")
		}
	})
	t.Run("below threshold does not fire", func(t *testing.T) {
		if gates(79.9, true, true, true) {
			t.Fatal("fired")
		}
	})
	t.Run("each gate individually closed suppresses the cutout even when scorching", func(t *testing.T) {
		if gates(99, false, true, true) {
			t.Error("fired while disabled")
		}
		if gates(99, true, false, true) {
			t.Error("fired with the lid open")
		}
		if gates(99, true, true, false) {
			t.Error("fired while not blocking")
		}
	})
}
