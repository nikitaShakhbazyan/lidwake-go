package daemon

import "time"

// gatedTicker runs fn every interval while armed. The periodic work it drives only matters while
// lidwake keeps the Mac awake (or while a cutout latch is held), so it is disarmed the rest of the
// time: no timer while idle means no wakeups exactly when the Mac would otherwise be asleep. Go
// tickers run on the monotonic clock, which stops while the Mac sleeps, so an interval counts awake
// time only.
//
// set is called under the daemon's mutex and never blocks; fn runs on the ticker's own goroutine
// and re-checks its gates, since a tick can race a disarm.
type gatedTicker struct {
	stop chan struct{}
}

// set arms (on) or disarms the ticker. Arming an armed ticker keeps its phase.
func (t *gatedTicker) set(on bool, interval time.Duration, fn func()) {
	switch {
	case on && t.stop == nil:
		stop := make(chan struct{})
		t.stop = stop
		go func() {
			tick := time.NewTicker(interval)
			defer tick.Stop()
			for {
				select {
				case <-stop:
					return
				case <-tick.C:
					select {
					case <-stop:
						return
					default:
					}
					fn()
				}
			}
		}()
	case !on && t.stop != nil:
		close(t.stop)
		t.stop = nil
	}
}
