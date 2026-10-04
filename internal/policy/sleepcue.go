package policy

import "github.com/nikitaShakhbazyan/lidwake-go/internal/settings"

// ReleaseCause is why the daemon stopped keeping the Mac awake — the category of the release
// that took the registry to zero. The daemon records it at each release site and reads it on the
// blocking → idle edge to pick the pre-sleep cue.
type ReleaseCause string

const (
	// ReleaseWorkComplete is the happy path: the agent's end hook released, its process exited, or
	// the CPU-idle sweep decided it was done.
	ReleaseWorkComplete ReleaseCause = "workComplete"
	// ReleaseHoldExpired: a hold's TTL ran out (or the max-age backstop fired) — the work may not
	// be finished.
	ReleaseHoldExpired ReleaseCause = "holdExpired"
	// ReleaseSafetyCutout: a thermal, low-battery or AC-only cutout stopped the work mid-task to
	// protect the machine.
	ReleaseSafetyCutout ReleaseCause = "safetyCutout"
	// ReleaseUserAction: the user did it themselves (force release, pause, off timer). With the lid
	// closed — the only state where any cue plays — that means a remote release, typically over
	// SSH.
	ReleaseUserAction ReleaseCause = "userAction"
)

// Idle-sweep release reasons that mean the agent finished (the idle evaluator's reason values).
const (
	IdleReasonCPUIdle     = "cpuIdle"
	IdleReasonDeadProcess = "deadProcess"
)

// ReleaseCauseForIdleBatch collapses an idle-sweep batch to a single cause. An agent finishing
// (CPU-idle or its process exiting) is the headline over a co-released expired hold, so any
// completion-like reason wins; a batch of pure expiries ("ttlExpired", "maxAgeBackstop") is
// ReleaseHoldExpired. Any string type works, so the idle evaluator keeps its own reason type.
func ReleaseCauseForIdleBatch[R ~string](reasons []R) ReleaseCause {
	if len(reasons) == 0 {
		return ReleaseWorkComplete
	}
	for _, r := range reasons {
		if string(r) == IdleReasonCPUIdle || string(r) == IdleReasonDeadProcess {
			return ReleaseWorkComplete
		}
	}
	return ReleaseHoldExpired
}

// Cue names a synthesized sound; the values match the chime synthesizer's cue names.
type Cue string

const (
	CueLidClose          Cue = "lidClose"
	CueSleepWorkComplete Cue = "sleepWorkComplete"
	CueSleepHoldExpired  Cue = "sleepHoldExpired"
	CueSleepSafetyCutout Cue = "sleepSafetyCutout"
	CueSleepUserAction   Cue = "sleepUserAction"
)

// SleepCueDecision is what to play right before the Mac goes back to sleep. The zero value is
// silence.
type SleepCueDecision struct {
	// SoundName is "" for silence, "default" to play Cue, anything else a system sound name.
	SoundName string
	// Cue is the synthesized cue to render when SoundName is "default", else "".
	Cue Cue
}

// Silent reports whether nothing should play.
func (d SleepCueDecision) Silent() bool { return d.SoundName == "" }

// DecideSleepCue decides whether (and what) to play the instant the last assertion releases —
// the moment before the daemon clears the sleep block and a closed-lid Mac goes back to sleep.
//
//   - Lid open → silent. The user is present and can see the state; the cue exists for the
//     closed-lid, user-away case. This gate also keeps ReleaseUserAction sensible: a release
//     issued at the machine is silent, while one issued remotely (SSH against a closed lid) gets
//     its confirmation cue.
//   - Master toggle off → silent.
//   - Otherwise the cause's configured sound plays: "default" is the synthesized cue for that
//     cause, "off" silences just that cause, anything else names a macOS system sound.
func DecideSleepCue(cause ReleaseCause, lidClosed bool, s settings.Settings) SleepCueDecision {
	if !lidClosed || !s.SleepSoundEnabled {
		return SleepCueDecision{}
	}
	var sound string
	var cue Cue
	switch cause {
	case ReleaseHoldExpired:
		sound, cue = s.SleepChimeHoldExpired, CueSleepHoldExpired
	case ReleaseSafetyCutout:
		sound, cue = s.SleepChimeSafetyCutout, CueSleepSafetyCutout
	case ReleaseUserAction:
		sound, cue = s.SleepChimeUserAction, CueSleepUserAction
	default:
		sound, cue = s.SleepChimeWorkComplete, CueSleepWorkComplete
	}
	if sound == "off" || sound == "" {
		return SleepCueDecision{}
	}
	if sound != "default" {
		cue = ""
	}
	return SleepCueDecision{SoundName: sound, Cue: cue}
}
