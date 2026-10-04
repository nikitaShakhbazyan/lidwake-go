// Package settings is config.json: the user's settings, with resilient decoding and clamping.
package settings

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
)

// WaitingPolicy is what happens to an agent's hold while the agent waits for the user.
type WaitingPolicy string

const (
	// WaitKeepAwake keeps the Mac awake for as long as the agent waits.
	WaitKeepAwake WaitingPolicy = "keepAwake"
	// WaitGrace keeps it awake for AgentWaitingGraceMinutes, then lets it sleep.
	WaitGrace WaitingPolicy = "grace"
	// WaitSleep lets the Mac sleep as soon as the agent starts waiting.
	WaitSleep WaitingPolicy = "sleep"
)

// Settings are the user's preferences. JSON keys are the names `lidwake config` shows.
type Settings struct {
	SoundOnLidClose bool    `json:"soundOnLidClose"`
	SoundVolume     float64 `json:"soundVolume"`
	ChimeName       string  `json:"chimeName"`

	// SleepSoundEnabled plays a cue right before a closed-lid Mac goes back to sleep. Per cause:
	// "default" is the synthesized cue, "off" silences it, anything else names a system sound.
	SleepSoundEnabled      bool   `json:"sleepSoundEnabled"`
	SleepChimeWorkComplete string `json:"sleepChimeWorkComplete"`
	SleepChimeHoldExpired  string `json:"sleepChimeHoldExpired"`
	SleepChimeSafetyCutout string `json:"sleepChimeSafetyCutout"`
	SleepChimeUserAction   string `json:"sleepChimeUserAction"`

	// LockOnLidClose locks the screen when the lid closes over a working agent.
	LockOnLidClose bool `json:"lockOnLidClose"`

	ThermalCutoutEnabled       bool    `json:"thermalCutoutEnabled"`
	ThermalThresholdCelsius    float64 `json:"thermalThresholdCelsius"`
	LowBatteryCutoutEnabled    bool    `json:"lowBatteryCutoutEnabled"`
	LowBatteryThresholdPercent int     `json:"lowBatteryThresholdPercent"`
	// SafetyCutoutsWithLidOpen runs both cutouts whatever the lid does: SleepDisabled blocks the
	// kernel's own emergency sleep with the lid open too.
	SafetyCutoutsWithLidOpen bool `json:"safetyCutoutsWithLidOpen"`
	// RequireACPower keeps the Mac awake on AC power only.
	RequireACPower bool `json:"requireACPower"`

	AgentWaitingPolicy       WaitingPolicy `json:"agentWaitingPolicy"`
	AgentWaitingGraceMinutes int           `json:"agentWaitingGraceMinutes"`

	IdleReleaseEnabled bool `json:"idleReleaseEnabled"`
	// IdleReleaseSeconds releases a hook/sniffed hold once its agent's process tree has been
	// CPU-idle this long (the catch for an interrupted turn that fired no end hook).
	IdleReleaseSeconds int `json:"idleReleaseSeconds"`

	ProcessSniffingEnabled    bool `json:"processSniffingEnabled"`
	AutoAcquireForKnownAgents bool `json:"autoAcquireForKnownAgents"`

	// AgentHoldsEnabled allows `lidwake hold`, `lidwake run` and the MCP tool.
	AgentHoldsEnabled  bool    `json:"agentHoldsEnabled"`
	ManualHoldMaxHours float64 `json:"manualHoldMaxHours"`

	// KeepAwakeForBackgroundBash (opt-in, Claude Code): keep awake while a run_in_background
	// command an agent started keeps running. TTL-bounded by ManualHoldMaxHours.
	KeepAwakeForBackgroundBash bool `json:"keepAwakeForBackgroundBash"`
}

// Defaults are the settings of a fresh install.
func Defaults() Settings {
	return Settings{
		SoundOnLidClose:            true,
		SoundVolume:                0.5,
		ChimeName:                  "default",
		SleepSoundEnabled:          true,
		SleepChimeWorkComplete:     "default",
		SleepChimeHoldExpired:      "default",
		SleepChimeSafetyCutout:     "default",
		SleepChimeUserAction:       "default",
		LockOnLidClose:             true,
		ThermalCutoutEnabled:       true,
		ThermalThresholdCelsius:    80,
		LowBatteryCutoutEnabled:    true,
		LowBatteryThresholdPercent: 20,
		SafetyCutoutsWithLidOpen:   true,
		RequireACPower:             false,
		AgentWaitingPolicy:         WaitGrace,
		AgentWaitingGraceMinutes:   10,
		IdleReleaseEnabled:         true,
		IdleReleaseSeconds:         90,
		ProcessSniffingEnabled:     true,
		AutoAcquireForKnownAgents:  false,
		AgentHoldsEnabled:          true,
		ManualHoldMaxHours:         4,
		KeepAwakeForBackgroundBash: false,
	}
}

// UnmarshalJSON decodes field by field: a missing or mistyped key (a newer build's setting, a
// hand edit like "idleReleaseSeconds": "90") falls back to its default instead of discarding the
// whole file. The result is clamped to supported ranges.
func (s *Settings) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	// null counts as absent, like a missing key: decoding it would yield the zero value and, for
	// instance, silently switch the thermal cutout off.
	for key, msg := range raw {
		if bytes.Equal(bytes.TrimSpace(msg), []byte("null")) {
			delete(raw, key)
		}
	}
	*s = Defaults()
	v := reflect.ValueOf(s).Elem()
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		key := jsonKey(t.Field(i))
		msg, ok := raw[key]
		if !ok {
			continue
		}
		field := reflect.New(t.Field(i).Type)
		if err := json.Unmarshal(msg, field.Interface()); err == nil {
			v.Field(i).Set(field.Elem())
		} else if n, ok := wholeNumber(msg); ok && t.Field(i).Type.Kind() == reflect.Int {
			v.Field(i).SetInt(n) // 30.0 or 1e2 in an integer field, as hand edits produce
		}
	}
	if _, ok := raw["idleReleaseSeconds"]; !ok || !validInt(raw["idleReleaseSeconds"]) {
		var minutes int
		if msg, ok := raw["idleReleaseMinutes"]; ok && json.Unmarshal(msg, &minutes) == nil {
			s.IdleReleaseSeconds = minutes * 60
		}
	}
	switch s.AgentWaitingPolicy {
	case WaitKeepAwake, WaitGrace, WaitSleep:
	default:
		s.AgentWaitingPolicy = WaitGrace
	}
	s.Clamp()
	return nil
}

// wholeNumber accepts a JSON number with no fractional part that fits an int32.
func wholeNumber(msg json.RawMessage) (int64, bool) {
	var f float64
	if json.Unmarshal(msg, &f) != nil || f != math.Trunc(f) || math.Abs(f) > math.MaxInt32 {
		return 0, false
	}
	return int64(f), true
}

func validInt(msg json.RawMessage) bool {
	var n int
	return json.Unmarshal(msg, &n) == nil
}

// Clamp keeps numeric fields where the policies stay sane: config.json is hand-editable, and a
// battery threshold of 150 would cut out on every tick while a thermal threshold of 0 never stops.
func (s *Settings) Clamp() {
	d := Defaults()
	if math.IsNaN(s.SoundVolume) || math.IsInf(s.SoundVolume, 0) {
		s.SoundVolume = d.SoundVolume
	}
	if math.IsNaN(s.ThermalThresholdCelsius) || math.IsInf(s.ThermalThresholdCelsius, 0) {
		s.ThermalThresholdCelsius = d.ThermalThresholdCelsius
	}
	if math.IsNaN(s.ManualHoldMaxHours) || math.IsInf(s.ManualHoldMaxHours, 0) {
		s.ManualHoldMaxHours = d.ManualHoldMaxHours
	}
	s.SoundVolume = clampF(s.SoundVolume, 0, 1)
	s.ThermalThresholdCelsius = clampF(s.ThermalThresholdCelsius, 50, 105)
	s.LowBatteryThresholdPercent = clampI(s.LowBatteryThresholdPercent, 1, 99)
	s.IdleReleaseSeconds = clampI(s.IdleReleaseSeconds, 30, 3600)
	s.ManualHoldMaxHours = clampF(s.ManualHoldMaxHours, 0.25, 24)
	s.AgentWaitingGraceMinutes = clampI(s.AgentWaitingGraceMinutes, 1, 120)
}

func clampF(v, lo, hi float64) float64 { return math.Min(math.Max(v, lo), hi) }

func clampI(v, lo, hi int) int { return min(max(v, lo), hi) }

// Load reads config.json at path; a missing or unreadable file yields Defaults().
func Load(path string) Settings {
	data, err := os.ReadFile(path)
	if err != nil {
		return Defaults()
	}
	var s Settings
	if json.Unmarshal(data, &s) != nil {
		return Defaults()
	}
	return s
}

// Save writes s to path atomically, pretty-printed with sorted keys.
func (s Settings) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var m map[string]any
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}
	out, err := json.MarshalIndent(m, "", "  ") // maps marshal with sorted keys
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(out, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Keys lists the JSON keys in declaration order.
func Keys() []string {
	t := reflect.TypeOf(Settings{})
	keys := make([]string, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		keys = append(keys, jsonKey(t.Field(i)))
	}
	return keys
}

func jsonKey(f reflect.StructField) string {
	return strings.Split(f.Tag.Get("json"), ",")[0]
}
