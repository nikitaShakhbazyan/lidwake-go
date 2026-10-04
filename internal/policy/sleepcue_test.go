package policy

import (
	"testing"

	"github.com/nikitaShakhbazyan/lidwake-go/internal/settings"
)

type cueSettings struct {
	disabled                                            bool
	workComplete, holdExpired, safetyCutout, userAction string
}

func (c cueSettings) build() settings.Settings {
	s := settings.Defaults()
	s.SleepSoundEnabled = !c.disabled
	pick := func(v string) string {
		if v == "" {
			return "default"
		}
		return v
	}
	s.SleepChimeWorkComplete = pick(c.workComplete)
	s.SleepChimeHoldExpired = pick(c.holdExpired)
	s.SleepChimeSafetyCutout = pick(c.safetyCutout)
	s.SleepChimeUserAction = pick(c.userAction)
	return s
}

var allReleaseCauses = []ReleaseCause{ReleaseWorkComplete, ReleaseHoldExpired, ReleaseSafetyCutout, ReleaseUserAction}

func TestSleepCueDecider(t *testing.T) {
	silent := SleepCueDecision{}

	t.Run("lid open is silent for every cause", func(t *testing.T) {
		for _, cause := range allReleaseCauses {
			if d := DecideSleepCue(cause, false, cueSettings{}.build()); d != silent || !d.Silent() {
				t.Errorf("cause %s should be silent with the lid open, got %+v", cause, d)
			}
		}
	})

	t.Run("master toggle off is silent for every cause", func(t *testing.T) {
		for _, cause := range allReleaseCauses {
			if d := DecideSleepCue(cause, true, cueSettings{disabled: true}.build()); d != silent {
				t.Errorf("cause %s should be silent when disabled, got %+v", cause, d)
			}
		}
	})

	// A user release against a closed lid is a remote one (SSH force release) — it gets a
	// confirmation cue. At-the-machine releases are covered by the lid-open gate above.
	t.Run("user action with lid closed gets its confirmation cue", func(t *testing.T) {
		want := SleepCueDecision{SoundName: "default", Cue: CueSleepUserAction}
		if d := DecideSleepCue(ReleaseUserAction, true, cueSettings{}.build()); d != want {
			t.Fatalf("got %+v", d)
		}
	})

	t.Run("each cause resolves its own synthesized cue by default", func(t *testing.T) {
		s := cueSettings{}.build()
		want := map[ReleaseCause]Cue{
			ReleaseWorkComplete: CueSleepWorkComplete,
			ReleaseHoldExpired:  CueSleepHoldExpired,
			ReleaseSafetyCutout: CueSleepSafetyCutout,
			ReleaseUserAction:   CueSleepUserAction,
		}
		for _, cause := range allReleaseCauses {
			if d := DecideSleepCue(cause, true, s); d != (SleepCueDecision{SoundName: "default", Cue: want[cause]}) {
				t.Errorf("cause %s: got %+v", cause, d)
			}
		}
	})

	t.Run("per-cause off silences only that cause", func(t *testing.T) {
		s := cueSettings{workComplete: "off"}.build()
		if d := DecideSleepCue(ReleaseWorkComplete, true, s); d != silent {
			t.Errorf("work complete: got %+v", d)
		}
		if d := DecideSleepCue(ReleaseHoldExpired, true, s); d != (SleepCueDecision{SoundName: "default", Cue: CueSleepHoldExpired}) {
			t.Errorf("hold expired: got %+v", d)
		}
	})

	// A system-sound pick passes the name through and carries no synth cue.
	t.Run("system sound passes through without a synth cue", func(t *testing.T) {
		s := cueSettings{safetyCutout: "Submarine"}.build()
		if d := DecideSleepCue(ReleaseSafetyCutout, true, s); d != (SleepCueDecision{SoundName: "Submarine"}) {
			t.Fatalf("got %+v", d)
		}
	})

	t.Run("an empty sound name is silent", func(t *testing.T) {
		s := cueSettings{}.build()
		s.SleepChimeWorkComplete = ""
		if d := DecideSleepCue(ReleaseWorkComplete, true, s); d != silent {
			t.Fatalf("got %+v", d)
		}
	})

	t.Run("idle batch with any completion-like reason reads as work complete", func(t *testing.T) {
		for _, batch := range [][]string{
			{"cpuIdle"},
			{"deadProcess"},
			// A finished agent is the headline over a co-released expired hold.
			{"ttlExpired", "cpuIdle"},
			{"maxAgeBackstop", "deadProcess"},
		} {
			if got := ReleaseCauseForIdleBatch(batch); got != ReleaseWorkComplete {
				t.Errorf("%v: got %s", batch, got)
			}
		}
	})

	t.Run("idle batch of pure expiries reads as hold expired", func(t *testing.T) {
		for _, batch := range [][]string{{"ttlExpired"}, {"maxAgeBackstop"}, {"ttlExpired", "maxAgeBackstop"}} {
			if got := ReleaseCauseForIdleBatch(batch); got != ReleaseHoldExpired {
				t.Errorf("%v: got %s", batch, got)
			}
		}
	})

	// Degenerate but safe: an empty batch defaults to the happy-path cue.
	t.Run("empty idle batch defaults to work complete", func(t *testing.T) {
		if got := ReleaseCauseForIdleBatch([]string{}); got != ReleaseWorkComplete {
			t.Fatalf("got %s", got)
		}
	})

	t.Run("idle batch accepts any string-based reason type", func(t *testing.T) {
		type reason string
		if got := ReleaseCauseForIdleBatch([]reason{"ttlExpired", "cpuIdle"}); got != ReleaseWorkComplete {
			t.Fatalf("got %s", got)
		}
	})
}
