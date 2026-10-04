package policy

// LidCloseDecision is what the daemon does the instant the lid closes.
//
// The single gate is "are we keeping the Mac awake for an agent?" — with zero assertions a lid
// close is an ordinary sleep and lidwake does nothing. While blocking, the chime and the explicit
// screen lock are each independently toggleable, but away-tracking always begins so the lid-open
// summary can be assembled.
type LidCloseDecision struct {
	// ShouldChime plays the lid-close chime (the screen is off, so it's the only close-time
	// feedback).
	ShouldChime bool
	// ShouldLock explicitly locks the screen — works even while an idle-lock-prevention assertion
	// is held.
	ShouldLock bool
	// ShouldBeginAwayTracking snapshots the held assertions and starts tracking for the "while
	// the lid was closed" summary.
	ShouldBeginAwayTracking bool
}

// DecideLidClose decides the lid-close actions from whether at least one assertion is held and
// the lockOnLidClose / soundOnLidClose settings. Not blocking: the zero decision (do nothing).
func DecideLidClose(blocking, lockOnLidClose, soundOnLidClose bool) LidCloseDecision {
	if !blocking {
		return LidCloseDecision{}
	}
	return LidCloseDecision{
		ShouldChime:             soundOnLidClose,
		ShouldLock:              lockOnLidClose,
		ShouldBeginAwayTracking: true,
	}
}
