package darwin

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"syscall"
)

// wakeIdent is the EVFILT_USER event Close triggers to unblock the kevent loop, which otherwise
// waits with no timeout (no periodic wakeups while nothing exits).
const wakeIdent = 1

var errWatcherClosed = errors.New("darwin: exit watcher closed")

type exitWatcherState struct {
	mu      sync.Mutex
	watched map[int]bool
	closed  bool
	done    chan struct{}
	wg      sync.WaitGroup // the kevent loop and in-flight immediate deliveries
}

func newExitWatcher() (*ExitWatcher, error) {
	kq, err := syscall.Kqueue()
	if err != nil {
		return nil, fmt.Errorf("darwin: kqueue: %w", err)
	}
	syscall.CloseOnExec(kq)
	wake := syscall.Kevent_t{Ident: wakeIdent, Filter: syscall.EVFILT_USER, Flags: syscall.EV_ADD | syscall.EV_CLEAR}
	if err := keventChange(kq, wake); err != nil {
		syscall.Close(kq)
		return nil, fmt.Errorf("darwin: kevent EVFILT_USER: %w", err)
	}
	w := &ExitWatcher{kq: kq, exits: make(chan int, 64)}
	w.state.watched = map[int]bool{}
	w.state.done = make(chan struct{})
	w.state.wg.Add(1)
	go w.loop()
	return w, nil
}

// keventChange applies one change without collecting events, retrying an interrupted call.
func keventChange(kq int, ev syscall.Kevent_t) error {
	for {
		_, err := syscall.Kevent(kq, []syscall.Kevent_t{ev}, nil, nil)
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}

func (w *ExitWatcher) watch(pid int) error {
	if pid <= 0 || pid > math.MaxInt32 {
		return fmt.Errorf("darwin: watch pid %d: invalid pid", pid)
	}
	s := &w.state
	// Held across the registration so Close can't close (and the fd number be reused) mid-call.
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errWatcherClosed
	}
	if s.watched[pid] {
		return nil
	}
	ev := syscall.Kevent_t{
		Ident:  uint64(pid),
		Filter: syscall.EVFILT_PROC,
		Flags:  syscall.EV_ADD | syscall.EV_ENABLE | syscall.EV_ONESHOT,
		Fflags: syscall.NOTE_EXIT,
	}
	if err := keventChange(w.kq, ev); err != nil {
		// ESRCH: the process exited before it could be armed, so NOTE_EXIT will never come —
		// report it now. It is not left in the watched set, or a later process reusing the
		// pid could never be watched.
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			w.deliver(pid)
		}()
		return nil
	}
	s.watched[pid] = true
	return nil
}

// deliver hands an exit to the consumer, giving up once the watcher is closed.
func (w *ExitWatcher) deliver(pid int) {
	select {
	case w.exits <- pid:
	case <-w.state.done:
	}
}

func (w *ExitWatcher) loop() {
	s := &w.state
	defer s.wg.Done()
	events := make([]syscall.Kevent_t, 16)
	for {
		n, err := syscall.Kevent(w.kq, nil, events, nil)
		if err != nil {
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			slog.Error("exit watcher stopped: kevent failed", "err", err)
			return
		}
		for _, ev := range events[:n] {
			switch {
			case ev.Filter == syscall.EVFILT_USER:
				return
			case ev.Filter == syscall.EVFILT_PROC && ev.Fflags&syscall.NOTE_EXIT != 0:
				pid := int(ev.Ident)
				s.mu.Lock()
				delete(s.watched, pid)
				s.mu.Unlock()
				w.deliver(pid)
			}
		}
	}
}

func (w *ExitWatcher) close() error {
	if w == nil {
		return nil
	}
	s := &w.state
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	close(s.done)
	err := keventChange(w.kq, syscall.Kevent_t{Ident: wakeIdent, Filter: syscall.EVFILT_USER, Fflags: syscall.NOTE_TRIGGER})
	s.mu.Unlock()
	if err != nil {
		// The loop can't be woken; leave the kqueue open rather than close it under a
		// blocked kevent.
		return fmt.Errorf("darwin: wake exit watcher: %w", err)
	}
	s.wg.Wait()
	close(w.exits)
	if err := syscall.Close(w.kq); err != nil {
		return fmt.Errorf("darwin: close kqueue: %w", err)
	}
	return nil
}
