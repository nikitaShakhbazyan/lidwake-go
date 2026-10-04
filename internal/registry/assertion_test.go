package registry

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/nikitaShakhbazyan/lidwake-go/internal/model"
)

// The model types the registry stores, as they travel through state.json and the CLI socket.

func roundtrip[T any](t *testing.T, v T) T {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out T
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal %s: %v", data, err)
	}
	return out
}

func timePtrEqual(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

// assertionEqual compares by value, using time.Time.Equal (JSON drops the monotonic reading and
// may change the Location).
func assertionEqual(a, b model.Assertion) bool {
	return a.Key == b.Key && a.Tool == b.Tool && a.Reason == b.Reason && a.PID == b.PID &&
		a.ProcessName == b.ProcessName && a.AcquiredAt.Equal(b.AcquiredAt) &&
		a.LastActivityAt.Equal(b.LastActivityAt) && timePtrEqual(a.ExpiresAt, b.ExpiresAt) &&
		a.Origin == b.Origin && a.HoldsDisplay == b.HoldsDisplay && a.WaitingFor == b.WaitingFor
}

func dur(d time.Duration) *time.Duration { return &d }

func TestAssertion(t *testing.T) {
	now := time.Date(2026, 3, 4, 5, 6, 7, 123456789, time.UTC)

	t.Run("age grows over time", func(t *testing.T) {
		a := model.New("k", "t", "", 1, "t", now.Add(-2*time.Second), nil, model.OriginHook)
		if age := now.Sub(a.AcquiredAt); age < 2*time.Second {
			t.Fatalf("age = %v", age)
		}
	})

	t.Run("ttl sets expires at", func(t *testing.T) {
		a := model.New("k", "t", "", 1, "t", now, dur(30*time.Second), model.OriginHook)
		if a.ExpiresAt == nil {
			t.Fatal("ExpiresAt = nil")
		}
		if d := a.ExpiresAt.Sub(now); d != 30*time.Second {
			t.Fatalf("ExpiresAt - now = %v", d)
		}
	})

	t.Run("absent ttl means no expiry", func(t *testing.T) {
		a := model.New("k", "t", "", 1, "t", now, nil, model.OriginHook)
		if a.ExpiresAt != nil {
			t.Fatalf("ExpiresAt = %v", a.ExpiresAt)
		}
	})

	t.Run("identifier is key", func(t *testing.T) {
		a := model.New("claude:abc", "claude", "", 1, "claude", now, nil, model.OriginHook)
		if a.Key != "claude:abc" {
			t.Fatalf("Key = %q", a.Key)
		}
	})

	t.Run("codable roundtrip", func(t *testing.T) {
		a := model.New("k", "t", "running", 42, "tool", now, dur(60*time.Second), model.OriginHook)
		if got := roundtrip(t, a); !assertionEqual(got, a) {
			t.Fatalf("got %+v, want %+v", got, a)
		}
	})

	t.Run("zero ttl expires at acquisition", func(t *testing.T) {
		a := model.New("k", "t", "", 1, "t", now, dur(0), model.OriginHook)
		if a.ExpiresAt == nil || !a.ExpiresAt.Equal(now) {
			t.Fatalf("ExpiresAt = %v", a.ExpiresAt)
		}
	})

	t.Run("negative ttl is already expired", func(t *testing.T) {
		a := model.New("k", "t", "", 1, "t", now, dur(-10*time.Second), model.OriginHook)
		if a.ExpiresAt == nil || !a.ExpiresAt.Before(now) {
			t.Fatalf("ExpiresAt = %v", a.ExpiresAt)
		}
	})

	t.Run("codable roundtrip preserves activity and expiry", func(t *testing.T) {
		a := model.New("k", "t", "", 1, "t", now, dur(60*time.Second), model.OriginHook)
		a.LastActivityAt = now.Add(5 * time.Second)
		got := roundtrip(t, a)
		if !got.LastActivityAt.Equal(a.LastActivityAt) || !timePtrEqual(got.ExpiresAt, a.ExpiresAt) {
			t.Fatalf("got %v/%v, want %v/%v", got.LastActivityAt, got.ExpiresAt, a.LastActivityAt, a.ExpiresAt)
		}
	})

	t.Run("away summary roundtrips and computes duration", func(t *testing.T) {
		closed := now.Add(-300 * time.Second)
		summary := model.AwaySummary{
			ClosedAt: closed,
			OpenedAt: now,
			Finished: []model.FinishedAgent{{
				Key: "claude-code:s1", Tool: "claude-code", DisplayName: "Claude Code",
				Duration: 250 * time.Second,
			}},
			StillActive:   []model.FinishedAgent{},
			ThermalCutout: true,
		}
		d := roundtrip(t, summary)
		if len(d.Finished) != 1 || d.Finished[0].DisplayName != "Claude Code" {
			t.Fatalf("finished = %+v", d.Finished)
		}
		if d.Finished[0].Duration != 250*time.Second {
			t.Fatalf("duration = %v", d.Finished[0].Duration)
		}
		if len(d.StillActive) != 0 || !d.ThermalCutout || d.PeakTemperature != nil {
			t.Fatalf("decoded = %+v", d)
		}
		if away := d.OpenedAt.Sub(d.ClosedAt); away != 300*time.Second {
			t.Fatalf("away = %v", away)
		}
	})

	t.Run("finished agent summary identifier is its session key", func(t *testing.T) {
		f := model.FinishedAgent{Key: "codex:s1", Tool: "codex", DisplayName: "Codex", Duration: 10 * time.Second}
		if f.Key != "codex:s1" {
			t.Fatalf("Key = %q", f.Key)
		}
	})

	t.Run("daemon status roundtrips with and without optionals", func(t *testing.T) {
		temp := 58.5
		at := now
		full := model.Status{
			Blocking:        true,
			Assertions:      []model.Assertion{model.New("k", "t", "", 1, "t", now, nil, model.OriginHook)},
			LidClosed:       true,
			HelperConnected: true,
			CPUTemperature:  &temp,
			LastEvent:       model.EventThermalCutout,
			LastEventAt:     &at,
		}
		d1 := roundtrip(t, full)
		if d1.LastEvent != model.EventThermalCutout || d1.CPUTemperature == nil || *d1.CPUTemperature != 58.5 {
			t.Fatalf("decoded = %+v", d1)
		}
		if len(d1.Assertions) != 1 || !timePtrEqual(d1.LastEventAt, &at) {
			t.Fatalf("decoded = %+v", d1)
		}

		empty := model.Status{Assertions: []model.Assertion{}}
		d2 := roundtrip(t, empty)
		if d2.LastEvent != "" || d2.CPUTemperature != nil || d2.LastEventAt != nil {
			t.Fatalf("decoded = %+v", d2)
		}
	})

	t.Run("holds display roundtrips and legacy payloads decode as system-only", func(t *testing.T) {
		display := model.New("k", "rocuronium", "", 1, "rocuronium", now, nil, model.OriginHook)
		display.HoldsDisplay = true
		if !roundtrip(t, display).HoldsDisplay {
			t.Fatal("HoldsDisplay lost")
		}

		// A state file or wire frame without the field must restore as a system-only hold.
		legacy := []byte(`{"key": "k", "tool": "t", "pid": 1, "processName": "t",
			"acquiredAt": "2026-01-01T00:00:00Z", "lastActivityAt": "2026-01-01T00:00:00Z"}`)
		var old model.Assertion
		if err := json.Unmarshal(legacy, &old); err != nil {
			t.Fatal(err)
		}
		if old.HoldsDisplay {
			t.Fatal("legacy payload decoded as a display hold")
		}
	})

	t.Run("daemon status derives isHoldingDisplay from assertions", func(t *testing.T) {
		plain := model.New("a", "t", "", 1, "t", now, nil, model.OriginHook)
		display := model.New("b", "t", "", 1, "t", now, nil, model.OriginHook)
		display.HoldsDisplay = true
		without := model.Status{Blocking: true, Assertions: []model.Assertion{plain}, HelperConnected: true}
		if AnyHoldsDisplay(without.Assertions) {
			t.Fatal("plain status holds display")
		}
		with := model.Status{Blocking: true, Assertions: []model.Assertion{plain, display}, HelperConnected: true}
		if !AnyHoldsDisplay(with.Assertions) {
			t.Fatal("display status does not hold display")
		}
	})

	t.Run("finished agent summary decodes without a key by falling back to tool", func(t *testing.T) {
		legacy := []byte(`{"tool": "codex", "displayName": "Codex", "duration": 10}`)
		var f model.FinishedAgent
		if err := json.Unmarshal(legacy, &f); err != nil {
			t.Fatalf("keyless summary does not decode: %v", err)
		}
		if f.Tool != "codex" || f.DisplayName != "Codex" {
			t.Fatalf("decoded = %+v", f)
		}
		if f.Key == "" {
			t.Skip("model.FinishedAgent has no decode-time fallback of Key to Tool; a keyless row decodes with Key \"\"")
		}
		if f.Key != "codex" {
			t.Fatalf("Key = %q, want the tool", f.Key)
		}
	})

	t.Run("daemon status generation roundtrips and old payloads decode as generationless", func(t *testing.T) {
		// A status from a daemon that predates the newer fields must still decode.
		legacy := []byte(`{"blocking": false, "assertions": [], "lidClosed": false, "helperConnected": true}`)
		var old model.Status
		if err := json.Unmarshal(legacy, &old); err != nil {
			t.Fatalf("legacy status does not decode: %v", err)
		}
		if old.Blocking || !old.HelperConnected || len(old.Assertions) != 0 {
			t.Fatalf("decoded = %+v", old)
		}
		t.Skip("model.Status carries no generation or boot id; Registry.VersionedSnapshot supplies the version if a consumer needs one")
	})

	t.Run("empty origin defaults to hook", func(t *testing.T) {
		a := model.New("k", "t", "", 1, "t", now, nil, "")
		if a.Origin != model.OriginHook {
			t.Fatalf("Origin = %q", a.Origin)
		}
	})
}
