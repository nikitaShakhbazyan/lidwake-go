// Package model holds the data types shared by the daemon, the CLI and the dashboard.
package model

import (
	"encoding/json"
	"time"

	"github.com/nikitaShakhbazyan/lidwake-go/internal/settings"
)

// Origin says how an assertion came to exist and governs its lifecycle.
type Origin string

const (
	// OriginHook: acquired by an agent's hook. Subject to the full idle policy.
	OriginHook Origin = "hook"
	// OriginManual: an explicit `lidwake hold` / `lidwake run` — reasoned, TTL-bounded and exempt
	// from the CPU-idle rule.
	OriginManual Origin = "manual"
	// OriginSniffed: auto-acquired by the daemon's process-sniffing sweep.
	OriginSniffed Origin = "sniffed"
)

// Assertion is one reason to keep the Mac awake. Keys are unique; the Mac is blocked while at
// least one assertion exists.
type Assertion struct {
	Key            string     `json:"key"`
	Tool           string     `json:"tool"`
	Reason         string     `json:"reason,omitempty"`
	PID            int        `json:"pid"`
	ProcessName    string     `json:"processName"`
	AcquiredAt     time.Time  `json:"acquiredAt"`
	LastActivityAt time.Time  `json:"lastActivityAt"`
	ExpiresAt      *time.Time `json:"expiresAt,omitempty"`
	Origin         Origin     `json:"origin"`
	// HoldsDisplay also keeps the display awake (screen-reading agents). Sticky per key.
	HoldsDisplay bool `json:"holdsDisplay,omitempty"`
	// WaitingFor is what the owning agent waits on when it stopped mid-turn for the user.
	WaitingFor string `json:"waitingFor,omitempty"`
}

// New builds an assertion acquired at `at`, with an optional TTL.
func New(key, tool, reason string, pid int, processName string, at time.Time, ttl *time.Duration, origin Origin) Assertion {
	a := Assertion{
		Key: key, Tool: tool, Reason: reason, PID: pid, ProcessName: processName,
		AcquiredAt: at, LastActivityAt: at, Origin: origin,
	}
	if ttl != nil {
		exp := at.Add(*ttl)
		a.ExpiresAt = &exp
	}
	if a.Origin == "" {
		a.Origin = OriginHook
	}
	return a
}

// UnmarshalJSON defaults a missing origin to OriginHook: a hold that doesn't say how it was made
// is treated as a live agent's, under the full idle policy.
func (a *Assertion) UnmarshalJSON(data []byte) error {
	type plain Assertion
	var p plain
	if err := json.Unmarshal(data, &p); err != nil {
		return err
	}
	if p.Origin == "" {
		p.Origin = OriginHook
	}
	*a = Assertion(p)
	return nil
}

// Event is a line in the event log and the status's "last event".
type Event string

const (
	EventAcquired         Event = "acquired"
	EventReleased         Event = "released"
	EventThermalCutout    Event = "thermalCutout"
	EventLowBatteryCutout Event = "lowBatteryCutout"
	EventACPowerCutout    Event = "acPowerCutout"
	EventIdleRelease      Event = "idleRelease"
	EventLidClosed        Event = "lidClosed"
	EventLidOpened        Event = "lidOpened"
	EventPaused           Event = "paused"
	EventResumed          Event = "resumed"
	EventOffTimer         Event = "offTimer"
)

// Status is what the daemon reports to `lidwake status` and `lidwake stats`.
type Status struct {
	Paused          bool        `json:"paused"`
	Blocking        bool        `json:"blocking"`
	Assertions      []Assertion `json:"assertions"`
	LidClosed       bool        `json:"lidClosed"`
	HelperConnected bool        `json:"helperConnected"`
	// SleepDisabled is the kernel's flag: whether a closed lid is actually ignored right now.
	SleepDisabled  bool     `json:"sleepDisabled"`
	CPUTemperature *float64 `json:"cpuTemperature,omitempty"`
	BatteryPercent *int     `json:"batteryPercent,omitempty"`
	OnBattery      *bool    `json:"onBattery,omitempty"`
	// ThermalState is NSProcessInfo.thermalState: 0 nominal, 1 fair, 2 serious, 3 critical.
	ThermalState int `json:"thermalState"`
	// ActiveCutouts are latched safety cutouts: "thermal", "lowBattery", "onBattery".
	ActiveCutouts []string   `json:"activeCutouts"`
	OffAt         *time.Time `json:"offAt,omitempty"`
	LastEvent     Event      `json:"lastEvent,omitempty"`
	LastEventAt   *time.Time `json:"lastEventAt,omitempty"`
	// Warnings are degraded-protection notices: a block that couldn't be applied, an unreadable
	// temperature, an active cutout.
	Warnings    []string          `json:"warnings"`
	AwaySummary *AwaySummary      `json:"awaySummary,omitempty"`
	Settings    settings.Settings `json:"settings"`
	Version     string            `json:"version"`
}

// FinishedAgent is one agent's line in the "while the lid was closed" summary.
type FinishedAgent struct {
	Key         string        `json:"key"`
	Tool        string        `json:"tool"`
	DisplayName string        `json:"displayName"`
	Duration    time.Duration `json:"duration"`
}

// AwaySummary describes the last closed-lid period that had at least one assertion.
type AwaySummary struct {
	ClosedAt         time.Time       `json:"closedAt"`
	OpenedAt         time.Time       `json:"openedAt"`
	Finished         []FinishedAgent `json:"finished"`
	StillActive      []FinishedAgent `json:"stillActive"`
	PeakTemperature  *float64        `json:"peakTemperature,omitempty"`
	ThermalCutout    bool            `json:"thermalCutout"`
	LowBatteryCutout bool            `json:"lowBatteryCutout"`
}

// PersistedState is state.json: what the daemon restores after a restart.
type PersistedState struct {
	Assertions []Assertion `json:"assertions"`
	Paused     bool        `json:"paused"`
	OffAt      *time.Time  `json:"offAt,omitempty"`
}
