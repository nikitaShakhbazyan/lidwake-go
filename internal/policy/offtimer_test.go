package policy

import (
	"math"
	"testing"
	"time"
)

func TestOffTimer(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)

	t.Run("a deadline is clamped to a minute … a day, and zero cancels", func(t *testing.T) {
		cases := []struct {
			seconds *float64
			want    *time.Duration
		}{
			{f64(3600), durp(time.Hour)},
			{f64(5), durp(time.Minute)},
			{f64(99 * 3600), durp(24 * time.Hour)},
			{f64(0), nil},
			{nil, nil},
			{f64(math.Inf(1)), nil},
			{f64(math.NaN()), nil},
			{f64(-60), nil},
		}
		for _, c := range cases {
			got := OffTimerDeadline(c.seconds, now)
			switch {
			case c.want == nil && got != nil:
				t.Errorf("%v: got %v, want nil", fmtF(c.seconds), *got)
			case c.want != nil && (got == nil || !got.Equal(now.Add(*c.want))):
				t.Errorf("%v: got %v, want now+%v", fmtF(c.seconds), got, *c.want)
			}
		}
	})

	t.Run("due once the deadline passes", func(t *testing.T) {
		if OffTimerDue(nil, now) {
			t.Error("no timer is due")
		}
		later := now.Add(time.Second)
		if OffTimerDue(&later, now) {
			t.Error("future deadline is due")
		}
		if !OffTimerDue(&now, now) {
			t.Error("deadline now is not due")
		}
	})

	t.Run("the timer key steps through the presets and back to off", func(t *testing.T) {
		inputs := []*time.Duration{nil, durp(900 * time.Second), durp(3000 * time.Second), durp(14400 * time.Second)}
		want := []*time.Duration{durp(900 * time.Second), durp(1800 * time.Second), durp(3600 * time.Second), nil}
		for i, in := range inputs {
			got, ok := NextOffTimerPreset(in)
			switch {
			case want[i] == nil && ok:
				t.Errorf("step %d: got %v, want off", i, got)
			case want[i] != nil && (!ok || got != *want[i]):
				t.Errorf("step %d: got %v (ok=%v), want %v", i, got, ok, *want[i])
			}
		}
	})

	// What is left can be anything: a deadline that already passed, one right at the 30 s margin
	// of a preset, or a far-future one restored from state.json (time.Until saturates).
	t.Run("the timer key handles overdue, boundary and far-future timers", func(t *testing.T) {
		cases := []struct {
			left time.Duration
			want *time.Duration
		}{
			{-time.Hour, durp(15 * time.Minute)},
			{869 * time.Second, durp(15 * time.Minute)},
			{870 * time.Second, durp(30 * time.Minute)},
			{4 * time.Hour, nil},
			{time.Duration(math.MaxInt64), nil},
		}
		for _, c := range cases {
			got, ok := NextOffTimerPreset(&c.left)
			switch {
			case c.want == nil && ok:
				t.Errorf("left %v: got %v, want off", c.left, got)
			case c.want != nil && (!ok || got != *c.want):
				t.Errorf("left %v: got %v (ok=%v), want %v", c.left, got, ok, *c.want)
			}
		}
	})
}

func durp(d time.Duration) *time.Duration { return &d }

func fmtF(p *float64) any {
	if p == nil {
		return "nil"
	}
	return *p
}
