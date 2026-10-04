package monitor

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// waitLimit bounds every wait for something a monitor goroutine should do soon.
const waitLimit = 5 * time.Second

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// eventually fails the test unless cond becomes true within waitLimit.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitLimit)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// stays fails the test if cond turns false within d.
func stays(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if !cond() {
			t.Fatalf("%s did not hold", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// recorder collects callback values from a monitor goroutine.
type recorder[T any] struct {
	mu   sync.Mutex
	vals []T
}

func (r *recorder[T]) add(v T) {
	r.mu.Lock()
	r.vals = append(r.vals, v)
	r.mu.Unlock()
}

func (r *recorder[T]) all() []T {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]T(nil), r.vals...)
}

func (r *recorder[T]) len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.vals)
}

// counter counts calls, safely across goroutines.
type counter struct {
	mu sync.Mutex
	n  int
}

func (c *counter) inc() {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
}

func (c *counter) get() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// stoppable is what every goroutine-backed monitor offers.
type stoppable interface {
	Start(ctx context.Context)
	Stop()
}

// start starts m and stops it when the test ends.
func start(t *testing.T, m stoppable) {
	t.Helper()
	m.Start(context.Background())
	t.Cleanup(m.Stop)
}

// fakeClock is a settable wall clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock(t time.Time) *fakeClock { return &fakeClock{t: t} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func TestRunTicking(t *testing.T) {
	t.Run("stops on ctx cancel and on stop", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			runTicking(ctx, make(chan struct{}), ticking{interval: time.Hour, onTick: func() {}})
			close(done)
		}()
		cancel()
		select {
		case <-done:
		case <-time.After(waitLimit):
			t.Fatal("did not stop on cancel")
		}
		stop := make(chan struct{})
		done = make(chan struct{})
		go func() {
			runTicking(context.Background(), stop, ticking{interval: time.Hour, onTick: func() {}})
			close(done)
		}()
		close(stop)
		select {
		case <-done:
		case <-time.After(waitLimit):
			t.Fatal("did not stop on stop")
		}
	})

	t.Run("arms and disarms with the gate", func(t *testing.T) {
		var mu sync.Mutex
		on := false
		var arms, disarms, ticks counter
		var k kicker
		stop := make(chan struct{})
		done := make(chan struct{})
		go func() {
			defer close(done)
			runTicking(context.Background(), stop, ticking{
				interval: time.Millisecond,
				armed:    func() bool { mu.Lock(); defer mu.Unlock(); return on },
				onArm:    arms.inc,
				onDisarm: disarms.inc,
				kick:     k.c(),
				onTick:   ticks.inc,
			})
		}()
		defer func() { close(stop); <-done }()
		stays(t, "no ticks while disarmed", 30*time.Millisecond, func() bool { return ticks.get() == 0 && arms.get() == 0 })
		mu.Lock()
		on = true
		mu.Unlock()
		k.signal()
		eventually(t, "ticks once armed", func() bool { return ticks.get() >= 3 })
		if arms.get() != 1 {
			t.Errorf("armed %d times, want once across many ticks", arms.get())
		}
		mu.Lock()
		on = false
		mu.Unlock()
		k.signal()
		eventually(t, "disarm", func() bool { return disarms.get() == 1 })
		n := ticks.get()
		stays(t, "no ticks after disarm", 30*time.Millisecond, func() bool { return ticks.get() == n })
	})

	t.Run("delivers events until the channel closes", func(t *testing.T) {
		events := make(chan int)
		var got recorder[int]
		stop := make(chan struct{})
		done := make(chan struct{})
		go func() {
			defer close(done)
			runTicking(context.Background(), stop, ticking{interval: time.Hour, events: events, onEvent: got.add, onTick: func() {}})
		}()
		events <- 1
		events <- 2
		close(events)
		eventually(t, "events", func() bool { return got.len() == 2 })
		close(stop)
		<-done
	})
}

// The constructors wire the real system calls and the default intervals. Nothing here calls them.
func TestConstructorsWireDefaults(t *testing.T) {
	if m := NewLidMonitor(); m.Read == nil || m.Interval != DefaultLidInterval {
		t.Error("lid")
	}
	if d := NewWakeDetector(); d.Now == nil || d.Period != DefaultWakePeriod || d.threshold() != 3*DefaultWakePeriod {
		t.Error("wake")
	}
	if m := NewBatteryMonitor(); m.Read == nil || m.Interval != DefaultBatteryInterval {
		t.Error("battery")
	}
	if m := NewThermalMonitor(); m.Open == nil || m.Interval != DefaultThermalInterval {
		t.Error("thermal")
	}
	if m := NewIdleMonitor(); m.Procs == nil || m.Procs.ProcessTree == nil || m.Procs.CPUTime == nil || m.WakePIDs == nil || m.Now == nil || m.Interval != DefaultIdleInterval {
		t.Error("idle")
	}
	if m := NewSessionStatusMonitor(); m.Dir == "" || m.ProcessAlive == nil || m.Now == nil || m.Interval != DefaultSessionInterval {
		t.Error("session")
	}
	if w := NewProcessWatcher(); w.NewSource == nil || w.ProcessAlive == nil || w.Interval != DefaultLivenessInterval {
		t.Error("process")
	}
	if h := NewDisplayHold(); h.Create == nil || h.DeclareUserActivity == nil || h.OnlyDisplayIsClosedBuiltIn == nil {
		t.Error("display")
	}
}
