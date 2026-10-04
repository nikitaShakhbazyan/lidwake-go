package policy

import (
	"math"
	"time"
)

// The off timer ("turn off in an hour"): when it is due, the daemon pauses itself, so every hold
// is released and the Mac sleeps normally until someone turns lidwake back on.
const (
	OffTimerMinimum = time.Minute
	OffTimerMaximum = 24 * time.Hour
)

// OffTimerPresets are what the dashboard's timer key steps through.
func OffTimerPresets() []time.Duration {
	return []time.Duration{15 * time.Minute, 30 * time.Minute, time.Hour, 2 * time.Hour, 4 * time.Hour}
}

// OffTimerDeadline is the deadline for a request of seconds (the wire TTL) from now, clamped to
// one minute … 24 hours; nil (cancel) for a missing, zero, negative or non-finite request.
func OffTimerDeadline(seconds *float64, now time.Time) *time.Time {
	if seconds == nil || math.IsNaN(*seconds) || math.IsInf(*seconds, 0) || *seconds <= 0 {
		return nil
	}
	s := min(max(*seconds, OffTimerMinimum.Seconds()), OffTimerMaximum.Seconds())
	deadline := now.Add(secondsToDuration(s))
	return &deadline
}

// NextOffTimerPreset steps off → 15m → 30m → 1h → 2h → 4h → off, starting from whatever is left
// now (left nil: the timer is off). A preset is next only when it is more than 30 s past what is
// left, so a timer just set to 15m steps on to 30m rather than to 15m again. ok is false when
// the next step is off.
func NextOffTimerPreset(left *time.Duration) (next time.Duration, ok bool) {
	presets := OffTimerPresets()
	if left == nil {
		return presets[0], true
	}
	for _, p := range presets {
		// p-30s rather than left+30s: a far-future deadline saturates left at the Duration
		// maximum, where adding would wrap around to a negative value.
		if p-30*time.Second > *left {
			return p, true
		}
	}
	return 0, false
}

// OffTimerDue reports whether the timer with this deadline (nil: no timer) has fired by now.
func OffTimerDue(deadline *time.Time, now time.Time) bool {
	return deadline != nil && !now.Before(*deadline)
}

// secondsToDuration converts finite, non-negative seconds to a Duration, saturating instead of
// overflowing.
func secondsToDuration(s float64) time.Duration {
	if math.IsNaN(s) || s <= 0 {
		return 0
	}
	ns := s * float64(time.Second)
	if ns >= math.MaxInt64 {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(ns)
}
