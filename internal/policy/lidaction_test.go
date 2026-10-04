package policy

import "testing"

func TestLidActionDecider(t *testing.T) {
	t.Run("blocking with both settings on chimes locks and tracks", func(t *testing.T) {
		want := LidCloseDecision{ShouldChime: true, ShouldLock: true, ShouldBeginAwayTracking: true}
		if got := DecideLidClose(true, true, true); got != want {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("lock setting off still chimes and tracks but does not lock", func(t *testing.T) {
		want := LidCloseDecision{ShouldChime: true, ShouldLock: false, ShouldBeginAwayTracking: true}
		if got := DecideLidClose(true, false, true); got != want {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("sound setting off still locks and tracks but does not chime", func(t *testing.T) {
		want := LidCloseDecision{ShouldChime: false, ShouldLock: true, ShouldBeginAwayTracking: true}
		if got := DecideLidClose(true, true, false); got != want {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("both settings off only tracks", func(t *testing.T) {
		want := LidCloseDecision{ShouldBeginAwayTracking: true}
		if got := DecideLidClose(true, false, false); got != want {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("Not blocking → do nothing, regardless of settings", func(t *testing.T) {
		for _, lock := range []bool{true, false} {
			for _, sound := range []bool{true, false} {
				if got := DecideLidClose(false, lock, sound); got != (LidCloseDecision{}) {
					t.Errorf("lock=%v sound=%v: got %+v", lock, sound, got)
				}
			}
		}
	})
}
