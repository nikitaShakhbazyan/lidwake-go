package monitor

import (
	"errors"
	"os/exec"
	"sync"
	"testing"
	"time"
)

// fakeExits is an exit source the test fires by hand.
type fakeExits struct {
	mu     sync.Mutex
	ch     chan int
	armed  []int
	fail   map[int]bool
	closed bool
}

func newFakeExits() *fakeExits { return &fakeExits{ch: make(chan int, 16), fail: map[int]bool{}} }

func (f *fakeExits) Watch(pid int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return errors.New("closed")
	}
	f.armed = append(f.armed, pid)
	if f.fail[pid] {
		return errors.New("kevent failed")
	}
	return nil
}

func (f *fakeExits) Exits() <-chan int { return f.ch }

func (f *fakeExits) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.closed = true
		close(f.ch)
	}
	return nil
}

func (f *fakeExits) exit(pid int) { f.ch <- pid }

func (f *fakeExits) armedPIDs() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.armed...)
}

func (f *fakeExits) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

// livePIDs is a settable liveness probe that counts its calls.
type livePIDs struct {
	mu    sync.Mutex
	dead  map[int]bool
	calls int
}

func (l *livePIDs) alive(pid int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	return !l.dead[pid]
}

func (l *livePIDs) kill(pid int) {
	l.mu.Lock()
	if l.dead == nil {
		l.dead = map[int]bool{}
	}
	l.dead[pid] = true
	l.mu.Unlock()
}

func (l *livePIDs) callCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls
}

func newTestWatcher(src *fakeExits, live *livePIDs) (*ProcessWatcher, *recorder[int]) {
	var exits recorder[int]
	w := &ProcessWatcher{
		NewSource:    func() (ExitSource, error) { return src, nil },
		ProcessAlive: live.alive,
		Interval:     time.Millisecond,
		OnExit:       exits.add,
		Log:          quiet(),
	}
	return w, &exits
}

func TestProcessWatcher(t *testing.T) {
	t.Run("reports a watched pid's exit once", func(t *testing.T) {
		src := newFakeExits()
		w, exits := newTestWatcher(src, &livePIDs{})
		start(t, w)
		w.Watch(100)
		if got := src.armedPIDs(); len(got) != 1 || got[0] != 100 {
			t.Fatalf("armed = %v", got)
		}
		src.exit(100)
		eventually(t, "the exit", func() bool { return exits.len() == 1 })
		src.exit(100) // seen again (e.g. by both kqueue and the sweep)
		src.exit(300) // never watched
		stays(t, "no duplicate or foreign exits", 20*time.Millisecond, func() bool { return exits.len() == 1 })
		if w.Watching(100) {
			t.Error("still watching an exited pid")
		}
	})

	t.Run("pids watched before start are armed at start", func(t *testing.T) {
		src := newFakeExits()
		w, exits := newTestWatcher(src, &livePIDs{})
		w.Watch(100)
		w.Watch(200)
		start(t, w)
		if got := src.armedPIDs(); len(got) != 2 {
			t.Fatalf("armed = %v", got)
		}
		src.exit(200)
		eventually(t, "the exit", func() bool { return exits.len() == 1 && exits.all()[0] == 200 })
	})

	t.Run("pid zero and below are not watched, and watching twice arms once", func(t *testing.T) {
		src := newFakeExits()
		w, _ := newTestWatcher(src, &livePIDs{})
		start(t, w)
		w.Watch(0)
		w.Watch(-1)
		w.Watch(100)
		w.Watch(100)
		if got := src.armedPIDs(); len(got) != 1 || got[0] != 100 {
			t.Errorf("armed = %v", got)
		}
		if w.Watching(0) || w.Watching(-1) {
			t.Error("watching an unresolved pid")
		}
	})

	t.Run("a pid can be watched again after its exit", func(t *testing.T) {
		src := newFakeExits()
		w, exits := newTestWatcher(src, &livePIDs{})
		start(t, w)
		w.Watch(100)
		src.exit(100)
		eventually(t, "the first exit", func() bool { return exits.len() == 1 })
		w.Watch(100) // a recycled pid is a new process
		src.exit(100)
		eventually(t, "the second exit", func() bool { return exits.len() == 2 })
	})

	t.Run("the liveness sweep reports dead pids while blocking", func(t *testing.T) {
		live := &livePIDs{}
		var exits recorder[int]
		w := &ProcessWatcher{
			NewSource:    func() (ExitSource, error) { return nil, errors.New("kqueue: too many open files") },
			ProcessAlive: live.alive,
			Interval:     time.Millisecond,
			OnExit:       exits.add,
			Log:          quiet(),
		}
		start(t, w)
		w.Watch(100)
		w.Watch(200)
		w.SetBlocking(true)
		eventually(t, "sweeps", func() bool { return live.callCount() > 4 })
		live.kill(100)
		eventually(t, "the exit", func() bool { return exits.len() == 1 })
		if got := exits.all()[0]; got != 100 {
			t.Errorf("exited = %d", got)
		}
		if !w.Watching(200) || w.Watching(100) {
			t.Error("watch set wrong after the sweep")
		}
	})

	t.Run("no liveness sweep while not blocking or with nothing watched", func(t *testing.T) {
		live := &livePIDs{}
		w, _ := newTestWatcher(newFakeExits(), live)
		start(t, w)
		w.SetBlocking(true)
		stays(t, "no sweep with nothing watched", 20*time.Millisecond, func() bool { return live.callCount() == 0 })
		w.SetBlocking(false)
		w.Watch(100)
		stays(t, "no sweep while not blocking", 20*time.Millisecond, func() bool { return live.callCount() == 0 })
		w.SetBlocking(true)
		eventually(t, "sweeps once blocking", func() bool { return live.callCount() > 0 })
	})

	t.Run("a pid whose watch could not be armed is caught by the sweep", func(t *testing.T) {
		src := newFakeExits()
		src.fail[100] = true
		live := &livePIDs{}
		w, exits := newTestWatcher(src, live)
		start(t, w)
		w.Watch(100)
		w.SetBlocking(true)
		live.kill(100)
		eventually(t, "the exit", func() bool { return exits.len() == 1 })
	})

	t.Run("stop closes the source and later watches do nothing", func(t *testing.T) {
		src := newFakeExits()
		w, _ := newTestWatcher(src, &livePIDs{})
		w.Start(t.Context())
		w.Stop()
		if !src.isClosed() {
			t.Error("source left open")
		}
		w.Watch(100)
		if w.Watching(100) {
			t.Error("watching after Stop")
		}
		w.Stop() // twice is fine
	})
}

// The real kqueue source, against a child process this test starts and kills itself.
func TestProcessWatcherWithKqueue(t *testing.T) {
	cmd := exec.Command("/bin/sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	var exits recorder[int]
	w := NewProcessWatcher()
	w.OnExit = exits.add
	w.Log = quiet()
	start(t, w)
	w.Watch(pid)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	// Not reaped yet on purpose: a zombie still answers kill(pid, 0), so only NOTE_EXIT sees it.
	eventually(t, "the kqueue exit", func() bool { return exits.len() == 1 })
	if got := exits.all()[0]; got != pid {
		t.Errorf("exited = %d, want %d", got, pid)
	}
}
