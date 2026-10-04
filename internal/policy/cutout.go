package policy

import (
	"slices"
	"strings"
)

// CutoutCause is a safety cutout that can latch. The values are what model.Status.ActiveCutouts
// reports.
type CutoutCause string

const (
	CutoutThermal    CutoutCause = "thermal"
	CutoutLowBattery CutoutCause = "lowBattery"
	// CutoutOnBattery: requireACPower is on and the Mac was unplugged.
	CutoutOnBattery CutoutCause = "onBattery"
)

// AllCutoutCauses lists every cause, sorted by name.
func AllCutoutCauses() []CutoutCause {
	return []CutoutCause{CutoutLowBattery, CutoutOnBattery, CutoutThermal}
}

const (
	// ThermalHysteresisCelsius is how far below the threshold the temperature must fall before
	// the thermal latch clears.
	ThermalHysteresisCelsius = 5.0
	// BatteryHysteresisPercent is how far above the threshold the charge must rise before the
	// low-battery latch clears.
	BatteryHysteresisPercent = 5
)

// CutoutLatch latches a fired safety cutout until its hazard has actually receded.
//
// A cutout releases every assertion — but the agent that was pinning the Mac is usually still
// running, and its very next hook event (or the sniff sweep) would re-acquire within seconds.
// The monitors re-seed a reading the moment blocking resumes, so without a latch the system
// oscillates: acquire → cutout → release → re-acquire…, each cycle burning more charge below
// the threshold meant to protect it, or re-heating the Mac the cutout just saved. While
// latched, the daemon rejects acquires.
//
// Clearing requires the hazard to recede with margin (hysteresis). When the cutouts only guard a
// closed lid (ClearsOnLidOpen), opening the lid clears them too: a present user can re-close it
// to re-arm protection deliberately. The AC-only cutout (CutoutOnBattery) clears only on AC power.
//
// Use NewCutoutLatch: the zero value has ClearsOnLidOpen off. The latch is a value; copies are
// independent.
type CutoutLatch struct {
	// ClearsOnLidOpen is false when the cutouts also run with the lid open — then the lid says
	// nothing about safety.
	ClearsOnLidOpen bool
	active          uint8
}

// NewCutoutLatch returns an empty latch that clears on lid open.
func NewCutoutLatch() CutoutLatch { return CutoutLatch{ClearsOnLidOpen: true} }

func causeBit(c CutoutCause) uint8 {
	switch c {
	case CutoutThermal:
		return 1
	case CutoutLowBattery:
		return 2
	case CutoutOnBattery:
		return 4
	}
	return 0
}

// IsLatched reports whether any cause is active.
func (l CutoutLatch) IsLatched() bool { return l.active != 0 }

// Has reports whether cause is active.
func (l CutoutLatch) Has(c CutoutCause) bool {
	bit := causeBit(c)
	return bit != 0 && l.active&bit != 0
}

// Active lists the active causes, sorted by name.
func (l CutoutLatch) Active() []CutoutCause {
	var out []CutoutCause
	for _, c := range AllCutoutCauses() {
		if l.Has(c) {
			out = append(out, c)
		}
	}
	return out
}

// Trip latches cause. Unknown causes are ignored.
func (l *CutoutLatch) Trip(c CutoutCause) { l.active |= causeBit(c) }

func (l *CutoutLatch) clear(c CutoutCause) bool {
	if !l.Has(c) {
		return false
	}
	l.active &^= causeBit(c)
	return true
}

// DropDisabled drops the causes whose safety net the user switched off: a latch for a disabled
// cutout would otherwise refuse acquires until a hazard nobody is watching for recedes. It
// returns the dropped causes, sorted by name.
func (l *CutoutLatch) DropDisabled(thermal, lowBattery, acOnly bool) []CutoutCause {
	var dropped []CutoutCause
	enabled := map[CutoutCause]bool{CutoutThermal: thermal, CutoutLowBattery: lowBattery, CutoutOnBattery: acOnly}
	for _, c := range AllCutoutCauses() {
		if !enabled[c] && l.clear(c) {
			dropped = append(dropped, c)
		}
	}
	return dropped
}

// Conditions are the readings the latch is re-evaluated against. A nil reading is unknown.
type Conditions struct {
	TemperatureCelsius      *float64
	ThermalThresholdCelsius float64
	BatteryPercent          *int
	OnBattery               *bool
	BatteryThresholdPercent int
	LidClosed               bool
}

// Update re-evaluates the latch against current conditions and returns the causes that cleared,
// sorted by name. Unknown readings keep a latch held — a cutout must not clear on missing data.
func (l *CutoutLatch) Update(c Conditions) []CutoutCause {
	var cleared []CutoutCause
	lidOpened := l.ClearsOnLidOpen && !c.LidClosed
	if l.Has(CutoutThermal) {
		cooled := c.TemperatureCelsius != nil &&
			*c.TemperatureCelsius <= c.ThermalThresholdCelsius-ThermalHysteresisCelsius
		if lidOpened || cooled {
			l.clear(CutoutThermal)
			cleared = append(cleared, CutoutThermal)
		}
	}
	onAC := c.OnBattery != nil && !*c.OnBattery
	if l.Has(CutoutLowBattery) {
		charged := c.BatteryPercent != nil &&
			*c.BatteryPercent >= c.BatteryThresholdPercent+BatteryHysteresisPercent
		if lidOpened || onAC || charged {
			l.clear(CutoutLowBattery)
			cleared = append(cleared, CutoutLowBattery)
		}
	}
	if l.Has(CutoutOnBattery) && onAC {
		l.clear(CutoutOnBattery)
		cleared = append(cleared, CutoutOnBattery)
	}
	slices.Sort(cleared)
	return cleared
}

// RejectionMessage is the user-facing explanation for a rejected acquire, "" when not latched.
func (l CutoutLatch) RejectionMessage() string {
	if !l.IsLatched() {
		return ""
	}
	orLid := ""
	if l.ClearsOnLidOpen {
		orLid = " or the lid opens"
	}
	var hazards []string
	if l.Has(CutoutThermal) {
		hazards = append(hazards, "overheating")
	}
	if l.Has(CutoutLowBattery) {
		hazards = append(hazards, "low battery")
	}
	if l.Has(CutoutOnBattery) {
		hazards = append(hazards, "running on battery with AC-only mode on")
	}
	switch {
	case len(hazards) > 1:
		return "Safety cutouts are active (" + strings.Join(hazards, ", ") +
			") — acquires are paused until conditions recover" + orLid + "."
	case l.Has(CutoutThermal):
		return "The thermal cutout fired — acquires are paused until the Mac cools down" + orLid + "."
	case l.Has(CutoutLowBattery):
		return "The low-battery cutout fired — acquires are paused until charging resumes" + orLid + "."
	}
	return "AC-only mode is on and the Mac is on battery — acquires are paused until it is plugged in."
}

// ThermalCutout decides whether the thermal cutout should fire.
//
// The cutout force-releases all assertions so a bag-bound, lid-closed Mac can't cook itself. It
// fires only while lidwake is actively keeping the Mac awake with the lid closed — lid-open
// thermals are macOS's problem, and with zero assertions there is nothing to cut out. (With
// safetyCutoutsWithLidOpen the daemon passes LidClosed true whatever the lid does.)
type ThermalCutout struct {
	TemperatureCelsius float64
	ThresholdCelsius   float64
	Enabled            bool
	LidClosed          bool
	Blocking           bool
}

// ShouldFire reports whether the cutout fires: all gates open and the temperature at or over the
// threshold.
func (t ThermalCutout) ShouldFire() bool {
	if !t.Enabled || !t.LidClosed || !t.Blocking {
		return false
	}
	return t.TemperatureCelsius >= t.ThresholdCelsius
}

// LowBatteryCutout decides whether the low-battery cutout should fire.
//
// The sibling of ThermalCutout: where thermal protects a kept-awake, lid-closed Mac from cooking
// itself, this protects it from silently draining to a hard shutdown in a bag. It fires only on
// battery power while lidwake is actively keeping the Mac awake with the lid closed — on AC
// there is no drain risk, and with zero assertions there is nothing to cut out. On crossing, the
// daemon force-releases all assertions so normal low-power sleep can take over before the charge
// is gone.
type LowBatteryCutout struct {
	BatteryPercent   int
	ThresholdPercent int
	OnBattery        bool
	Enabled          bool
	LidClosed        bool
	Blocking         bool
}

// ShouldFire reports whether the cutout fires: all gates open and the charge at or under the
// threshold.
func (b LowBatteryCutout) ShouldFire() bool {
	if !b.Enabled || !b.OnBattery || !b.LidClosed || !b.Blocking {
		return false
	}
	return b.BatteryPercent <= b.ThresholdPercent
}

// ShouldCutoutOnBattery is AC-only mode: any battery power at all ends the block, lid open or
// closed.
func ShouldCutoutOnBattery(onBattery, requireACPower, blocking bool) bool {
	return requireACPower && onBattery && blocking
}
