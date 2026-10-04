// Package monitor holds the daemon's watchers: lid state, wake from sleep, battery, CPU
// temperature, CPU-idle agents, Claude Code waiting sessions, process exits and the display hold.
//
// Every monitor follows the same contract, so the daemon can drive them uniformly:
//
//   - Construct with its NewX function. The zero value works too: nil probes fall back to the
//     package darwin implementation and zero intervals to the defaults. Set the func fields
//     (callbacks, probes, intervals) before Start; they are read without locking afterwards.
//   - Start(ctx) seeds what must be readable at once and launches one goroutine; it never calls
//     a callback itself. Stop ends that goroutine, waits for it (so it waits for a callback in
//     progress) and releases what the monitor holds. Cancelling ctx ends the goroutine too, but
//     only Stop releases resources. Start after Stop does nothing.
//   - Callbacks run on the monitor's own goroutine, one at a time, never while the monitor holds
//     a lock: a callback may call back into the monitor. It must not call Stop, and must not block
//     on a goroutine that is calling Stop.
//   - Setters and getters are safe for concurrent use, before and after Start. A gate change
//     (blocking, lid) takes effect at once: a timer that should run is armed, one that shouldn't
//     is disarmed.
//
// Periodic work is gated wherever it only matters while lidwake keeps the Mac awake, so an idle
// daemon makes no periodic wakeups beyond the lid, wake and battery polls that replace the
// system's event notifications.
package monitor

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// lifecycle starts a monitor's goroutine once and stops it once.
type lifecycle struct {
	mu      sync.Mutex
	started bool
	stopped bool
	stop    chan struct{}
	done    chan struct{}
}

// start runs setup synchronously and then run on a new goroutine. It reports false, running
// neither, when the monitor was already started or stopped.
func (l *lifecycle) start(ctx context.Context, setup func(), run func(ctx context.Context, stop <-chan struct{})) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.started || l.stopped {
		return false
	}
	l.started = true
	if setup != nil {
		setup()
	}
	l.stop = make(chan struct{})
	l.done = make(chan struct{})
	stop, done := l.stop, l.done
	go func() {
		defer close(done)
		run(ctx, stop)
	}()
	return true
}

// halt stops the goroutine and waits for it to return. Safe before start and more than once.
func (l *lifecycle) halt() {
	l.mu.Lock()
	if !l.stopped {
		l.stopped = true
		if l.started {
			close(l.stop)
		}
	}
	started, done := l.started, l.done
	l.mu.Unlock()
	if started {
		<-done
	}
}

// isStopped reports whether halt was called.
func (l *lifecycle) isStopped() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.stopped
}

// ticking describes a monitor loop: onTick every interval while armed reports true, onKick
// whenever a signal arrives on kick (armed is consulted again afterwards), and onEvent for every
// value received on events.
type ticking struct {
	interval time.Duration
	armed    func() bool // nil: always armed
	// onArm runs each time the timer goes from disarmed to armed (at the start too), before the
	// first interval elapses; onDisarm each time it goes the other way.
	onArm    func()
	onDisarm func()
	kick     <-chan struct{}
	onKick   func() // nil: only re-check armed
	events   <-chan int
	onEvent  func(int)
	onTick   func()
}

// runTicking drives t until ctx is done or stop is closed. The timer is re-armed after each tick,
// so a slow tick delays the next one instead of piling them up, and a gate edge that keeps the
// timer armed does not reset it. Go timers run on the monotonic clock, which stops while the Mac
// sleeps, so an interval counts awake time only, as the system's own timers do.
func runTicking(ctx context.Context, stop <-chan struct{}, t ticking) {
	var timer *time.Timer
	var fire <-chan time.Time
	events := t.events
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-stop:
			return
		default:
		}
		on := t.armed == nil || t.armed()
		switch {
		case on && timer == nil:
			if t.onArm != nil {
				t.onArm()
			}
			timer = time.NewTimer(t.interval)
			fire = timer.C
		case !on && timer != nil:
			timer.Stop()
			timer, fire = nil, nil
			if t.onDisarm != nil {
				t.onDisarm()
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-stop:
			return
		case <-t.kick:
			if t.onKick != nil {
				t.onKick()
			}
		case v, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			t.onEvent(v)
		case <-fire:
			t.onTick()
			timer.Reset(t.interval)
		}
	}
}

// kicker is a lazily made, coalescing wake-up channel, usable before Start: signals sent while one
// is pending coalesce, and a signal sent before the loop runs is seen when it starts.
type kicker struct {
	once sync.Once
	ch   chan struct{}
}

func (k *kicker) c() chan struct{} {
	k.once.Do(func() { k.ch = make(chan struct{}, 1) })
	return k.ch
}

// signal wakes the loop without blocking.
func (k *kicker) signal() {
	select {
	case k.c() <- struct{}{}:
	default:
	}
}

func orDefault(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}

func logger(l *slog.Logger, name string) *slog.Logger {
	if l == nil {
		l = slog.Default()
	}
	return l.With("monitor", name)
}

func nowOr(now func() time.Time) time.Time {
	if now == nil {
		return time.Now()
	}
	return now()
}
