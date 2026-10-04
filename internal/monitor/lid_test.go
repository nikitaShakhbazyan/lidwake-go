package monitor

import (
	"sync"
	"testing"
	"time"
)

// fakeLid is a settable lid; hasLid false is a desktop Mac.
type fakeLid struct {
	mu     sync.Mutex
	closed bool
	hasLid bool
	reads  int
}

func (f *fakeLid) set(closed bool) {
	f.mu.Lock()
	f.closed = closed
	f.mu.Unlock()
}

func (f *fakeLid) setHasLid(has bool) {
	f.mu.Lock()
	f.hasLid = has
	f.mu.Unlock()
}

func (f *fakeLid) read() (bool, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	return f.closed, f.hasLid
}

func (f *fakeLid) readCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}

func TestLidMonitor(t *testing.T) {
	t.Run("the state at start is the baseline, not a change", func(t *testing.T) {
		lid := &fakeLid{closed: true, hasLid: true}
		var changes recorder[bool]
		m := &LidMonitor{Read: lid.read, Interval: time.Millisecond, OnChange: changes.add, Log: quiet()}
		start(t, m)
		if !m.Closed() {
			t.Fatal("Closed is not current when Start returns")
		}
		eventually(t, "a few polls", func() bool { return lid.readCount() > 5 })
		if n := changes.len(); n != 0 {
			t.Fatalf("got %d changes for an unchanged lid", n)
		}
	})

	t.Run("each change is reported once", func(t *testing.T) {
		lid := &fakeLid{hasLid: true}
		var changes recorder[bool]
		m := &LidMonitor{Read: lid.read, Interval: time.Millisecond, OnChange: changes.add, Log: quiet()}
		start(t, m)
		lid.set(true)
		eventually(t, "close", func() bool { return changes.len() == 1 })
		if !m.Closed() {
			t.Error("Closed not updated")
		}
		n := lid.readCount()
		eventually(t, "more polls", func() bool { return lid.readCount() > n+3 })
		lid.set(false)
		eventually(t, "open", func() bool { return changes.len() == 2 })
		if got := changes.all(); got[0] != true || got[1] != false {
			t.Errorf("changes = %v, want [true false]", got)
		}
	})

	t.Run("a Mac without a lid reads as open", func(t *testing.T) {
		lid := &fakeLid{closed: true, hasLid: false}
		var changes recorder[bool]
		m := &LidMonitor{Read: lid.read, Interval: time.Millisecond, OnChange: changes.add, Log: quiet()}
		start(t, m)
		eventually(t, "a few polls", func() bool { return lid.readCount() > 3 })
		if m.Closed() || changes.len() != 0 {
			t.Errorf("closed=%v changes=%v", m.Closed(), changes.all())
		}
	})

	t.Run("a failed read keeps the last state", func(t *testing.T) {
		lid := &fakeLid{closed: true, hasLid: true}
		var changes recorder[bool]
		m := &LidMonitor{Read: lid.read, Interval: time.Millisecond, OnChange: changes.add, Log: quiet()}
		start(t, m)
		lid.setHasLid(false) // the registry lookup fails for a while
		n := lid.readCount()
		eventually(t, "failed polls", func() bool { return lid.readCount() > n+5 })
		if !m.Closed() || changes.len() != 0 {
			t.Fatalf("closed=%v changes=%v after failed reads", m.Closed(), changes.all())
		}
		lid.setHasLid(true)
		n = lid.readCount()
		eventually(t, "good polls", func() bool { return lid.readCount() > n+5 })
		if !m.Closed() || changes.len() != 0 {
			t.Fatalf("closed=%v changes=%v once reads recover", m.Closed(), changes.all())
		}
		lid.set(false)
		eventually(t, "the real open", func() bool { return changes.len() == 1 })
	})

	t.Run("a failed read at start is open and the first good read is a change", func(t *testing.T) {
		lid := &fakeLid{closed: true, hasLid: false}
		var changes recorder[bool]
		m := &LidMonitor{Read: lid.read, Interval: time.Millisecond, OnChange: changes.add, Log: quiet()}
		start(t, m)
		if m.Closed() {
			t.Fatal("closed after a failed first read")
		}
		lid.setHasLid(true)
		eventually(t, "close", func() bool { return changes.len() == 1 })
		if got := changes.all(); !got[0] || !m.Closed() {
			t.Errorf("changes=%v closed=%v", got, m.Closed())
		}
	})

	t.Run("stop ends polling and start after stop does nothing", func(t *testing.T) {
		lid := &fakeLid{hasLid: true}
		m := &LidMonitor{Read: lid.read, Interval: time.Millisecond, Log: quiet()}
		start(t, m)
		eventually(t, "polls", func() bool { return lid.readCount() > 2 })
		m.Stop()
		n := lid.readCount()
		m.Start(t.Context())
		stays(t, "no polls after Stop", 20*time.Millisecond, func() bool { return lid.readCount() == n })
	})
}
