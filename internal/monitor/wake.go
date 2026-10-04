package monitor

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// DefaultWakePeriod is the wake detector's tick. A wake is noticed at most this long after it
// happens; the daemon's 60 s reconcile covers anything slower anyway.
const DefaultWakePeriod = 10 * time.Second

// WakeDetector reports that the Mac woke from sleep.
//
// The helper's clamshell-disable bit can be reset by the kernel across a sleep/wake cycle, a dark
// display may need relighting for a display hold, and CPU baselines taken before the sleep would
// read a mid-work agent as long idle — so the daemon needs to hear about every wake.
//
// Detected without IOKit power notifications: Go timers run on the monotonic clock, which stops
// while a Mac sleeps, so a ticker keeps its period in awake time while the wall clock runs on. A
// wall-clock gap between two ticks far beyond the period therefore means the Mac slept in between.
// A large forward change of the system clock reads as a wake too; every wake action is idempotent,
// so that costs nothing. A sleep shorter than the threshold goes unnoticed, which the reconcile
// covers.
type WakeDetector struct {
	// Period between ticks. Zero: DefaultWakePeriod.
	Period time.Duration
	// Threshold is the wall-clock gap between ticks that means the Mac slept. Zero, or not above
	// Period: three periods.
	Threshold time.Duration
	// Now is the wall clock. Nil: time.Now.
	Now func() time.Time
	// OnWake is called once per detected wake.
	OnWake func()
	Log    *slog.Logger

	life lifecycle
	mu   sync.Mutex
	last time.Time // wall clock of the previous tick, without a monotonic reading
}

// NewWakeDetector returns a detector with the default period.
func NewWakeDetector() *WakeDetector {
	return &WakeDetector{Period: DefaultWakePeriod, Now: time.Now}
}

// Start takes the first reading and starts ticking.
func (d *WakeDetector) Start(ctx context.Context) {
	d.life.start(ctx,
		func() { d.observe(nowOr(d.Now)) },
		func(ctx context.Context, stop <-chan struct{}) {
			runTicking(ctx, stop, ticking{interval: d.period(), onTick: d.tick})
		})
}

// Stop ends ticking.
func (d *WakeDetector) Stop() { d.life.halt() }

func (d *WakeDetector) period() time.Duration { return orDefault(d.Period, DefaultWakePeriod) }

func (d *WakeDetector) threshold() time.Duration {
	if p := d.period(); d.Threshold <= p {
		return 3 * p
	}
	return d.Threshold
}

func (d *WakeDetector) tick() {
	if !d.observe(nowOr(d.Now)) {
		return
	}
	logger(d.Log, "wake").Info("system woke from sleep")
	if d.OnWake != nil {
		d.OnWake()
		// The next tick comes a period after the handler returns, and the handler can be slow (a
		// helper round-trip). Measured from before it, a handler slower than the threshold minus
		// the period would read as another sleep, and every wake would fire the next one.
		d.rebase(nowOr(d.Now))
	}
}

// rebase makes t the previous tick without checking the gap.
func (d *WakeDetector) rebase(t time.Time) {
	d.mu.Lock()
	d.last = t.Round(0)
	d.mu.Unlock()
}

// observe records a tick at t and reports whether the wall-clock gap since the previous tick means
// the Mac slept. Round(0) drops the monotonic reading, which would otherwise decide Sub and hide
// the sleep. A clock that went backwards is not a wake.
func (d *WakeDetector) observe(t time.Time) bool {
	t = t.Round(0)
	d.mu.Lock()
	defer d.mu.Unlock()
	prev := d.last
	d.last = t
	if prev.IsZero() {
		return false
	}
	return t.Sub(prev) > d.threshold()
}
