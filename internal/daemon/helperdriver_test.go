package daemon

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/nikitaShakhbazyan/lidwake-go/internal/ipc"
)

// startDriver runs a driver against fake until the test ends.
func startDriver(t *testing.T, fake *fakeHelper, j *journal) *helperDriver {
	t.Helper()
	h := newHelperDriver(fake, quiet())
	h.retryBase = 5 * time.Millisecond
	h.retryMax = 20 * time.Millisecond
	h.beforeUnblock = func(context.Context) { j.add("cue") }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		fake.release()
		<-done
	})
	return h
}

func TestHelperDriver(t *testing.T) {
	t.Run("applies requests in order and the first unblock has no cue", func(t *testing.T) {
		j := &journal{}
		fake := newFakeHelper(j)
		h := startDriver(t, fake, j)
		h.Wait(context.Background(), h.Request(false))
		h.Wait(context.Background(), h.Request(true))
		if got, want := j.all(), []string{"set(false)", "set(true)"}; !slices.Equal(got, want) {
			t.Fatalf("journal = %v, want %v", got, want)
		}
	})

	t.Run("plays the cue before turning a block into an unblock", func(t *testing.T) {
		j := &journal{}
		fake := newFakeHelper(j)
		h := startDriver(t, fake, j)
		h.Wait(context.Background(), h.Request(true))
		h.Wait(context.Background(), h.Request(false))
		h.Wait(context.Background(), h.Request(false)) // already unblocked: no second cue
		if got, want := j.all(), []string{"set(true)", "cue", "set(false)", "set(false)"}; !slices.Equal(got, want) {
			t.Fatalf("journal = %v, want %v", got, want)
		}
	})

	t.Run("latest wins while a call is in flight", func(t *testing.T) {
		j := &journal{}
		fake := newFakeHelper(j)
		h := startDriver(t, fake, j)
		fake.hold()
		h.Request(true)
		eventually(t, "the first call in flight", func() bool { return fake.inFlight() })
		h.Request(false)
		h.Request(true)
		last := h.Request(false)
		last = h.Request(true)
		fake.release()
		h.Wait(context.Background(), last)
		eventually(t, "the burst applied", func() bool { return len(j.all()) >= 2 })
		stays(t, "no stale applies", 30*time.Millisecond, func() bool { return len(j.all()) == 2 })
		if got, want := j.all(), []string{"set(true)", "set(true)"}; !slices.Equal(got, want) {
			t.Fatalf("journal = %v, want %v (the burst collapses to its latest state)", got, want)
		}
	})

	t.Run("an acquire during the cue keeps the block", func(t *testing.T) {
		j := &journal{}
		fake := newFakeHelper(j)
		h := newHelperDriver(fake, quiet())
		cuePlaying := make(chan struct{})
		finishCue := make(chan struct{})
		h.beforeUnblock = func(context.Context) {
			j.add("cue")
			close(cuePlaying)
			<-finishCue
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go h.run(ctx)
		h.Wait(ctx, h.Request(true))
		h.Request(false)
		<-cuePlaying
		gen := h.Request(true)
		close(finishCue)
		h.Wait(ctx, gen)
		if got, want := j.all(), []string{"set(true)", "cue", "set(true)"}; !slices.Equal(got, want) {
			t.Fatalf("journal = %v, want %v", got, want)
		}
	})

	t.Run("a failed apply is recorded and retried until it takes", func(t *testing.T) {
		j := &journal{}
		fake := newFakeHelper(j)
		fake.fail(errors.New("helper unreachable"))
		h := startDriver(t, fake, j)
		h.Wait(context.Background(), h.Request(true))
		if !h.LastApplyFailed() {
			t.Fatal("a failed apply is not recorded")
		}
		eventually(t, "retries", func() bool { return fake.calls() >= 3 })
		fake.fail(nil)
		eventually(t, "the retry that takes", func() bool { return !h.LastApplyFailed() })
		n := fake.calls()
		stays(t, "no retries after success", 50*time.Millisecond, func() bool { return fake.calls() == n })
	})

	t.Run("an unblock is retried when the helper failed it, not when it is unreachable", func(t *testing.T) {
		j := &journal{}
		fake := newFakeHelper(j)
		h := startDriver(t, fake, j)
		h.Wait(context.Background(), h.Request(true))
		fake.fail(ipc.ErrHelperUnreachable)
		h.Wait(context.Background(), h.Request(false))
		if !h.LastApplyFailed() {
			t.Fatal("the failure is not recorded")
		}
		stays(t, "no retry against an unreachable helper", 60*time.Millisecond, func() bool { return fake.calls() == 2 })
		fake.fail(errors.New("pmset failed"))
		h.Wait(context.Background(), h.Request(false))
		eventually(t, "retries of a failed unblock", func() bool { return fake.calls() >= 5 })
		fake.fail(nil)
		eventually(t, "the unblock that takes", func() bool { return !h.LastApplyFailed() })
	})

	t.Run("a helper that reaches a different state counts as failed", func(t *testing.T) {
		j := &journal{}
		fake := newFakeHelper(j)
		fake.mismatch(true)
		h := startDriver(t, fake, j)
		h.Wait(context.Background(), h.Request(true))
		if !h.LastApplyFailed() {
			t.Fatal("a partial block is not recorded as a failure")
		}
		fake.mismatch(false)
		eventually(t, "the retry that takes", func() bool { return !h.LastApplyFailed() })
	})

	t.Run("backoff doubles up to the cap", func(t *testing.T) {
		h := newHelperDriver(newFakeHelper(&journal{}), quiet())
		var got []time.Duration
		for i := range 7 {
			got = append(got, h.backoff(i))
		}
		want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second}
		if !slices.Equal(got, want) {
			t.Fatalf("backoff = %v, want %v", got, want)
		}
	})

	t.Run("asks for the helper version once after the first good apply", func(t *testing.T) {
		j := &journal{}
		fake := newFakeHelper(j)
		h := startDriver(t, fake, j)
		h.Wait(context.Background(), h.Request(false))
		h.Wait(context.Background(), h.Request(true))
		eventually(t, "the version probe", func() bool { return fake.versionCalls() == 1 })
		stays(t, "a single probe", 20*time.Millisecond, func() bool { return fake.versionCalls() == 1 })
	})

	t.Run("stop unblocks after the call in flight and no call follows", func(t *testing.T) {
		j := &journal{}
		fake := newFakeHelper(j)
		h := newHelperDriver(fake, quiet())
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			h.run(ctx)
		}()
		fake.hold()
		h.Request(true)
		eventually(t, "the call in flight", func() bool { return fake.inFlight() })
		cancel()
		stopped := make(chan struct{})
		go func() {
			h.stop(time.Second)
			close(stopped)
		}()
		fake.release()
		<-stopped
		<-done
		h.Request(true)
		stays(t, "no call after stop", 20*time.Millisecond, func() bool { return len(j.all()) == 2 })
		if got, want := j.all(), []string{"set(true)", "set(false)"}; !slices.Equal(got, want) {
			t.Fatalf("journal = %v, want %v", got, want)
		}
	})

	t.Run("stop gives up on a wedged helper after the timeout", func(t *testing.T) {
		fake := newFakeHelper(&journal{})
		fake.hold()
		defer fake.release()
		h := newHelperDriver(fake, quiet())
		start := time.Now()
		h.stop(30 * time.Millisecond)
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("stop waited %v for a wedged helper", elapsed)
		}
	})

	t.Run("wait returns when its context ends", func(t *testing.T) {
		fake := newFakeHelper(&journal{})
		fake.hold()
		defer fake.release()
		h := newHelperDriver(fake, quiet())
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		if h.Wait(ctx, h.Request(true)) {
			t.Fatal("Wait reported an apply that never ran")
		}
	})
}

// fakeHelper is a scriptable HelperClient. Calls are journaled as "set(true)"/"set(false)".
type fakeHelper struct {
	j *journal

	mu         sync.Mutex
	err        error
	wrongState bool
	gate       chan struct{} // non-nil: calls wait for it to close
	flight     bool
	n          int
	versions   int
	states     int
	held       bool
	connected  bool
	sets       []bool
}

func newFakeHelper(j *journal) *fakeHelper { return &fakeHelper{j: j, connected: true} }

func (f *fakeHelper) SetBlocked(blocked bool) (bool, error) {
	f.mu.Lock()
	gate := f.gate
	f.flight = true
	f.mu.Unlock()
	if gate != nil {
		<-gate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.flight = false
	f.n++
	f.sets = append(f.sets, blocked)
	if blocked {
		f.j.add("set(true)")
	} else {
		f.j.add("set(false)")
	}
	if f.err != nil {
		return false, f.err
	}
	if f.wrongState {
		f.held = !blocked
		return !blocked, nil
	}
	f.held = blocked
	return blocked, nil
}

// State is the block the helper holds: the last state a set reached, false after restart.
func (f *fakeHelper) State() (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.states++
	if f.err != nil {
		return false, f.err
	}
	return f.held, nil
}

// restart simulates a helper crash: it comes back without the block.
func (f *fakeHelper) restart() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.held = false
	f.j.add("helper restarted")
}

func (f *fakeHelper) Version() (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.versions++
	return "test", nil
}

func (f *fakeHelper) Connected() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connected
}

// Close waits for a call in flight, as ipc.HelperClient does: both take the client's mutex.
func (f *fakeHelper) Close() {
	f.mu.Lock()
	gate, flight := f.gate, f.flight
	f.mu.Unlock()
	if flight && gate != nil {
		<-gate
	}
}

func (f *fakeHelper) fail(err error) {
	f.mu.Lock()
	f.err = err
	f.mu.Unlock()
}

func (f *fakeHelper) mismatch(on bool) {
	f.mu.Lock()
	f.wrongState = on
	f.mu.Unlock()
}

// hold makes calls wait until release.
func (f *fakeHelper) hold() {
	f.mu.Lock()
	f.gate = make(chan struct{})
	f.mu.Unlock()
}

func (f *fakeHelper) release() {
	f.mu.Lock()
	if f.gate != nil {
		close(f.gate)
		f.gate = nil
	}
	f.mu.Unlock()
}

func (f *fakeHelper) inFlight() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.flight
}

func (f *fakeHelper) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.n
}

func (f *fakeHelper) versionCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.versions
}

// lastSet is the state of the most recent call; ok is false before any.
func (f *fakeHelper) lastSet() (blocked, ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sets) == 0 {
		return false, false
	}
	return f.sets[len(f.sets)-1], true
}

func (f *fakeHelper) allSets() []bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]bool(nil), f.sets...)
}
