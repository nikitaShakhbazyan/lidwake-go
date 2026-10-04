package darwin

import "sync"

// Exported helpers beyond the original contract in api.go.

// DeviceCapabilities is what the host Mac physically has, so laptop-only features (sound and lock
// on lid close, the low-battery cutout, the "while the lid was closed" summary) can be hidden on a
// desktop. The core keep-awake works on any Mac: idle sleep is driven by input inactivity, not
// CPU load, so a desktop running an agent task sleeps and stalls it just like a laptop.
type DeviceCapabilities struct {
	HasLid     bool
	HasBattery bool
}

// IsDesktop reports a desktop Mac: nothing to close, nothing to drain. Both signals must be
// absent — a laptop always has a battery, so a transiently missed lid probe can't mislabel it.
func (c DeviceCapabilities) IsDesktop() bool { return !c.HasLid && !c.HasBattery }

// capabilityProbe keeps each capability once a probe has found it. Hardware does not change
// within a run, but a probe can miss: the power-source list is unreadable or not yet populated
// while powerd restarts, and the root-domain lookup can fail. So only a positive answer is final;
// a negative one is probed again on the next call rather than cached for the life of the process.
type capabilityProbe struct {
	mu         sync.Mutex
	hasLid     func() bool
	hasBattery func() bool
	found      DeviceCapabilities
}

func (p *capabilityProbe) get() DeviceCapabilities {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.found.HasLid {
		p.found.HasLid = p.hasLid()
	}
	if !p.found.HasBattery {
		p.found.HasBattery = p.hasBattery()
	}
	return p.found
}

var capabilities = &capabilityProbe{
	// A lid exists iff IOPMrootDomain publishes AppleClamshellState (the property LidClosed
	// reads); a battery iff a power source of type InternalBattery is present.
	hasLid: func() bool {
		_, ok := lidClosed()
		return ok
	},
	hasBattery: hasInternalBattery,
}

// Capabilities probes the host. A capability once found is kept; one not found yet is probed
// again on every call, so a transient miss heals on the next call.
func Capabilities() DeviceCapabilities { return capabilities.get() }

// OnlyDisplayIsClosedBuiltIn reports that the Mac's only display is a lid-closed built-in panel:
// a display-class hold is meaningless then (nothing relights a closed lid) and its agent is
// blind. External and virtual displays count as real targets. False when the display list can't
// be read.
func OnlyDisplayIsClosedBuiltIn(lidClosed bool) bool {
	if !lidClosed {
		return false
	}
	external, ok := externalDisplayCount()
	return ok && external == 0
}

// ChildMap maps each parent PID to its direct children, from a single KERN_PROC_ALL snapshot.
// Process-tree walks (CPU time of an agent and its children, a child's wake assertion) take one
// snapshot per sweep instead of one per visited process. Best effort: a process spawned during
// the snapshot may be missing until the next one.
func ChildMap() (map[int][]int, error) { return childMap() }
