package settings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func decode(t *testing.T, js string) Settings {
	t.Helper()
	var s Settings
	if err := json.Unmarshal([]byte(js), &s); err != nil {
		t.Fatalf("decode %s: %v", js, err)
	}
	return s
}

func TestLidwakeSettings(t *testing.T) {
	t.Run("defaults are sane", func(t *testing.T) {
		s := Defaults()
		checks := []struct {
			name string
			ok   bool
		}{
			{"soundOnLidClose", s.SoundOnLidClose},
			{"thermalCutoutEnabled", s.ThermalCutoutEnabled},
			{"thermalThresholdCelsius", s.ThermalThresholdCelsius == 80},
			{"idleReleaseEnabled", s.IdleReleaseEnabled},
			{"idleReleaseSeconds", s.IdleReleaseSeconds == 90},
			{"processSniffingEnabled", s.ProcessSniffingEnabled},
			{"autoAcquireForKnownAgents", !s.AutoAcquireForKnownAgents},
			{"lockOnLidClose", s.LockOnLidClose},
			// The grace default is what makes answering a waiting agent from a phone possible (the
			// Mac must stay reachable) while still letting an unanswered one sleep.
			{"agentWaitingPolicy", s.AgentWaitingPolicy == WaitGrace},
			{"agentWaitingGraceMinutes", s.AgentWaitingGraceMinutes == 10},
			// The background-shell keep-awake is opt-in: it holds the Mac awake for a command with no
			// completion hook, so it must be off unless the user turns it on.
			{"keepAwakeForBackgroundBash", !s.KeepAwakeForBackgroundBash},
			// The pre-sleep cue defaults on with each cause on its own synthesized cue — it only
			// fires with the lid closed (the user is away), which is exactly when it's useful.
			{"sleepSoundEnabled", s.SleepSoundEnabled},
			{"sleepChimeWorkComplete", s.SleepChimeWorkComplete == "default"},
			{"sleepChimeHoldExpired", s.SleepChimeHoldExpired == "default"},
			{"sleepChimeSafetyCutout", s.SleepChimeSafetyCutout == "default"},
			{"sleepChimeUserAction", s.SleepChimeUserAction == "default"},
		}
		for _, c := range checks {
			if !c.ok {
				t.Errorf("default %s is wrong", c.name)
			}
		}
	})

	t.Run("codable roundtrip preserves all fields", func(t *testing.T) {
		original := Defaults()
		original.SoundOnLidClose = false
		original.SoundVolume = 0.25
		original.ThermalThresholdCelsius = 72.5
		original.IdleReleaseSeconds = 120
		original.AutoAcquireForKnownAgents = true
		original.KeepAwakeForBackgroundBash = true
		original.ChimeName = "doot"
		original.SleepSoundEnabled = false
		original.SleepChimeWorkComplete = "Ping"
		original.SleepChimeHoldExpired = "off"
		original.SleepChimeSafetyCutout = "Submarine"
		original.SleepChimeUserAction = "Tink"
		original.LockOnLidClose = false
		original.AgentWaitingPolicy = WaitSleep
		original.AgentWaitingGraceMinutes = 25
		data, err := json.Marshal(original)
		if err != nil {
			t.Fatal(err)
		}
		if got := decode(t, string(data)); got != original {
			t.Fatalf("got %+v\nwant %+v", got, original)
		}
	})

	t.Run("save and load roundtrip via disk", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "nested", "config.json")
		s := Defaults()
		s.ThermalThresholdCelsius = 85
		s.IdleReleaseSeconds = 120
		if err := s.Save(path); err != nil {
			t.Fatal(err)
		}
		if loaded := Load(path); loaded != s {
			t.Fatalf("got %+v\nwant %+v", loaded, s)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		// Pretty-printed with sorted keys, so hand edits and diffs stay readable.
		var keys []string
		for _, line := range strings.Split(string(data), "\n") {
			if k, _, ok := strings.Cut(strings.TrimSpace(line), `":`); ok {
				keys = append(keys, strings.TrimPrefix(k, `"`))
			}
		}
		if !slices.IsSorted(keys) || len(keys) != len(Keys()) {
			t.Fatalf("keys not sorted or incomplete: %v", keys)
		}
		if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
			t.Fatalf("temp file left behind: %v", err)
		}
	})

	t.Run("load from missing file returns defaults", func(t *testing.T) {
		if got := Load(filepath.Join(t.TempDir(), "missing.json")); got != Defaults() {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("load from a corrupt file returns defaults", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte("not json"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := Load(path); got != Defaults() {
			t.Fatalf("got %+v", got)
		}
	})

	// A config written by an older build (missing a newer field) must not fail and reset all
	// settings: each absent field falls back to its default while user-set fields are preserved.
	t.Run("missing new field falls back without losing others", func(t *testing.T) {
		s := decode(t, `{"soundOnLidClose": false, "soundVolume": 0.25, "chimeName": "Tink",
			"thermalCutoutEnabled": false, "thermalThresholdCelsius": 72.5,
			"idleReleaseEnabled": false, "idleReleaseSeconds": 150,
			"processSniffingEnabled": false, "autoAcquireForKnownAgents": true,
			"lockOnLidClose": false}`)
		if s.IdleReleaseSeconds != 150 || s.ThermalThresholdCelsius != 72.5 || s.LockOnLidClose || s.ChimeName != "Tink" {
			t.Errorf("user values lost: %+v", s)
		}
		if s.RequireACPower {
			t.Error("absent requireACPower is not its default")
		}
		if s.KeepAwakeForBackgroundBash {
			t.Error("absent keepAwakeForBackgroundBash is not its opt-in default (off)")
		}
		// Pre-sleep cue fields absent (an older config) → defaults, others intact.
		if !s.SleepSoundEnabled || s.SleepChimeWorkComplete != "default" {
			t.Errorf("sleep cue defaults lost: %+v", s)
		}
	})

	t.Run("empty object decodes to all defaults", func(t *testing.T) {
		if got := decode(t, `{}`); got != Defaults() {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("unknown extra keys are ignored", func(t *testing.T) {
		if got := decode(t, `{"idleReleaseSeconds": 70, "futureSetting": 123}`); got.IdleReleaseSeconds != 70 {
			t.Fatalf("got %d", got.IdleReleaseSeconds)
		}
	})

	t.Run("load from disk missing new field preserves user values", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(`{"idleReleaseSeconds": 220, "thermalThresholdCelsius": 90}`), 0o644); err != nil {
			t.Fatal(err)
		}
		loaded := Load(path)
		if loaded.IdleReleaseSeconds != 220 || loaded.ThermalThresholdCelsius != 90 || !loaded.SafetyCutoutsWithLidOpen {
			t.Fatalf("got %+v", loaded)
		}
	})

	// A config from a build that only knew idleReleaseMinutes migrates to the seconds field (×60).
	t.Run("legacy minutes migrates to seconds", func(t *testing.T) {
		if got := decode(t, `{"idleReleaseMinutes": 3}`); got.IdleReleaseSeconds != 180 {
			t.Errorf("minutes: got %d", got.IdleReleaseSeconds)
		}
		// An explicit seconds field wins over a stale minutes field if both somehow appear.
		if got := decode(t, `{"idleReleaseMinutes": 3, "idleReleaseSeconds": 45}`); got.IdleReleaseSeconds != 45 {
			t.Errorf("both: got %d", got.IdleReleaseSeconds)
		}
		// A mistyped seconds field falls back to the legacy minutes.
		if got := decode(t, `{"idleReleaseMinutes": 2, "idleReleaseSeconds": "45"}`); got.IdleReleaseSeconds != 120 {
			t.Errorf("mistyped seconds: got %d", got.IdleReleaseSeconds)
		}
	})

	t.Run("safety scope defaults: cutouts guard an open lid, AC-only is off", func(t *testing.T) {
		s := Defaults()
		if !s.SafetyCutoutsWithLidOpen || s.RequireACPower {
			t.Fatalf("defaults %+v", s)
		}
		d := decode(t, `{"requireACPower": true, "safetyCutoutsWithLidOpen": false}`)
		if !d.RequireACPower || d.SafetyCutoutsWithLidOpen {
			t.Fatalf("decoded %+v", d)
		}
	})

	t.Run("an unknown waiting policy falls back to grace", func(t *testing.T) {
		if got := decode(t, `{"agentWaitingPolicy": "forever"}`); got.AgentWaitingPolicy != WaitGrace {
			t.Fatalf("got %q", got.AgentWaitingPolicy)
		}
		if got := decode(t, `{"agentWaitingPolicy": "keepAwake"}`); got.AgentWaitingPolicy != WaitKeepAwake {
			t.Fatalf("got %q", got.AgentWaitingPolicy)
		}
	})

	t.Run("keys follow the documented names", func(t *testing.T) {
		keys := Keys()
		want := []string{
			"soundOnLidClose", "soundVolume", "chimeName", "sleepSoundEnabled",
			"sleepChimeWorkComplete", "sleepChimeHoldExpired", "sleepChimeSafetyCutout",
			"sleepChimeUserAction", "lockOnLidClose", "thermalCutoutEnabled",
			"thermalThresholdCelsius", "lowBatteryCutoutEnabled", "lowBatteryThresholdPercent",
			"safetyCutoutsWithLidOpen", "requireACPower", "agentWaitingPolicy",
			"agentWaitingGraceMinutes", "idleReleaseEnabled", "idleReleaseSeconds",
			"processSniffingEnabled", "autoAcquireForKnownAgents", "agentHoldsEnabled",
			"manualHoldMaxHours", "keepAwakeForBackgroundBash",
		}
		if !slices.Equal(keys, want) {
			t.Fatalf("got %v", keys)
		}
	})
}

func TestSettingsHardening(t *testing.T) {
	t.Run("a type-mismatched field costs only that field", func(t *testing.T) {
		s := decode(t, `{"idleReleaseSeconds": "ninety", "thermalThresholdCelsius": 85, "lockOnLidClose": false}`)
		if s.IdleReleaseSeconds != Defaults().IdleReleaseSeconds {
			t.Errorf("the bad field should fall back to its default, got %d", s.IdleReleaseSeconds)
		}
		if s.ThermalThresholdCelsius != 85 || s.LockOnLidClose {
			t.Errorf("good fields should survive: %+v", s)
		}
	})

	// null is a mistyped value too: it must not decode as the type's zero value, which would
	// silently turn the thermal cutout or the screen lock off.
	t.Run("a null field falls back to its default", func(t *testing.T) {
		s := decode(t, `{"thermalCutoutEnabled": null, "lockOnLidClose": null, "thermalThresholdCelsius": null, "idleReleaseSeconds": null, "chimeName": null}`)
		d := Defaults()
		if s != d {
			t.Errorf("got %+v\nwant %+v", s, d)
		}
		if got := decode(t, `{"idleReleaseSeconds": null, "idleReleaseMinutes": 2}`); got.IdleReleaseSeconds != 120 {
			t.Errorf("null seconds should defer to legacy minutes, got %d", got.IdleReleaseSeconds)
		}
		if got := decode(t, `{"idleReleaseMinutes": null}`); got.IdleReleaseSeconds != d.IdleReleaseSeconds {
			t.Errorf("null legacy minutes should keep the default, got %d", got.IdleReleaseSeconds)
		}
	})

	t.Run("out-of-range values are clamped on load", func(t *testing.T) {
		s := decode(t, `{"lowBatteryThresholdPercent": 150, "thermalThresholdCelsius": 0, "idleReleaseSeconds": -5, "manualHoldMaxHours": -1, "soundVolume": 9}`)
		if s.LowBatteryThresholdPercent != 99 {
			t.Errorf("battery threshold %d: 150%% would fire the cutout on every tick", s.LowBatteryThresholdPercent)
		}
		if s.ThermalThresholdCelsius != 50 || s.IdleReleaseSeconds != 30 || s.ManualHoldMaxHours != 0.25 || s.SoundVolume != 1 {
			t.Errorf("not clamped: %+v", s)
		}
	})

	t.Run("upper bounds are clamped too", func(t *testing.T) {
		s := decode(t, `{"lowBatteryThresholdPercent": 0, "thermalThresholdCelsius": 500, "idleReleaseSeconds": 99999, "manualHoldMaxHours": 100, "soundVolume": -1, "agentWaitingGraceMinutes": 0}`)
		if s.LowBatteryThresholdPercent != 1 || s.ThermalThresholdCelsius != 105 || s.IdleReleaseSeconds != 3600 ||
			s.ManualHoldMaxHours != 24 || s.SoundVolume != 0 || s.AgentWaitingGraceMinutes != 1 {
			t.Errorf("not clamped: %+v", s)
		}
	})

	t.Run("a non-object config is an error", func(t *testing.T) {
		var s Settings
		if err := json.Unmarshal([]byte(`[1, 2]`), &s); err == nil {
			t.Fatal("decoded an array")
		}
	})
}

func TestWholeNumberFloatsFillIntegerFields(t *testing.T) {
	var s Settings
	if err := json.Unmarshal([]byte(`{"idleReleaseSeconds": 120.0, "lowBatteryThresholdPercent": 3e1, "agentWaitingGraceMinutes": 2.5}`), &s); err != nil {
		t.Fatal(err)
	}
	if s.IdleReleaseSeconds != 120 || s.LowBatteryThresholdPercent != 30 {
		t.Fatalf("got idle=%d battery=%d", s.IdleReleaseSeconds, s.LowBatteryThresholdPercent)
	}
	if s.AgentWaitingGraceMinutes != Defaults().AgentWaitingGraceMinutes {
		t.Fatalf("a fractional value must fall back to the default, got %d", s.AgentWaitingGraceMinutes)
	}
}
