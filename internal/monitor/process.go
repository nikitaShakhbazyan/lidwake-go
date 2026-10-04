package monitor

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/nikitaShakhbazyan/lidwake-go/internal/darwin"
)

// DefaultLivenessInterval is how often the process watcher checks its watched PIDs by hand while
// blocking.
const DefaultLivenessInterval = 30 * time.Second

// ExitSource reports process exits. *darwin.ExitWatcher implements it. Watch on a PID that already
// exited must report it on Exits without blocking the caller.
type ExitSource interface {
	Watch(pid int) error
	Exits() <-chan int
	Close() error
}

// newDarwinExitSource starts a kqueue exit watcher (never a typed nil).
func newDarwinExitSource() (ExitSource, error) {
	w, err := darwin.NewExitWatcher()
	if err != nil {
		return nil, err
	}
	return w, nil
}

// ProcessWatcher reports when an agent process exits, so its assertions are released even when the
// agent dies without firing its end hook (a crash, a killed terminal, Codex's missing Stop).
//
// Exits arrive from kqueue (NOTE_EXIT). A liveness sweep (kill(pid, 0)) backs it up while blocking:
// it catches a PID whose registration failed, or every PID if the kqueue could not be created, so
// a dead agent never pins sleep until the age backstop. Each watched PID is reported once; it can
// be watched again afterwards (a recycled PID is a new process).
type ProcessWatcher struct {
	// NewSource creates the exit source at Start. Nil: darwin.NewExitWatcher. When it fails, the
	// liveness sweep alone reports exits.
	NewSource func() (ExitSource, error)
	// ProcessAlive is the liveness sweep's probe. Nil: darwin.ProcessAlive.
	ProcessAlive func(pid int) bool
	// Interval between liveness sweeps while blocking. Zero: DefaultLivenessInterval.
	Interval time.Duration
	// OnExit is called once for each watched PID that exited.
	OnExit func(pid int)
	Log    *slog.Logger

	life lifecycle
	kick kicker

	mu       sync.Mutex
	watched  map[int]bool
	source   ExitSource
	blocking bool
}

// NewProcessWatcher returns a watcher on the real kqueue.
func NewProcessWatcher() *ProcessWatcher {
	return &ProcessWatcher{
		NewSource:    newDarwinExitSource,
		ProcessAlive: darwin.ProcessAlive,
		Interval:     DefaultLivenessInterval,
	}
}

// Start creates the exit source, arms every PID watched so far and starts delivering exits.
func (w *ProcessWatcher) Start(ctx context.Context) {
	var exits <-chan int
	var pending []int
	var src ExitSource
	started := w.life.start(ctx,
		func() {
			newSource := w.NewSource
			if newSource == nil {
				newSource = newDarwinExitSource
			}
			s, err := newSource()
			if err != nil || s == nil {
				logger(w.Log, "process").Error("cannot watch process exits with kqueue; relying on the liveness sweep", "err", err)
			} else {
				src, exits = s, s.Exits()
			}
			w.mu.Lock()
			w.source = src
			for pid := range w.watched {
				pending = append(pending, pid)
			}
			w.mu.Unlock()
		},
		func(ctx context.Context, stop <-chan struct{}) {
			runTicking(ctx, stop, ticking{
				interval: orDefault(w.Interval, DefaultLivenessInterval),
				armed:    w.sweepNeeded,
				kick:     w.kick.c(),
				events:   exits,
				onEvent:  w.exited,
				onTick:   w.sweep,
			})
		})
	if started && src != nil {
		for _, pid := range pending {
			w.arm(src, pid)
		}
	}
}

// Stop stops delivering exits and closes the exit source. Watch does nothing afterwards.
func (w *ProcessWatcher) Stop() {
	w.life.halt()
	w.mu.Lock()
	src := w.source
	w.source = nil
	w.mu.Unlock()
	if src != nil {
		_ = src.Close()
	}
}

// Watch reports pid's exit once. Idempotent; pid <= 0 (an agent the CLI could not identify) is
// ignored. Usable before Start: the PID is armed when the watcher starts.
func (w *ProcessWatcher) Watch(pid int) {
	if pid <= 0 || w.life.isStopped() {
		return
	}
	w.mu.Lock()
	if w.watched[pid] {
		w.mu.Unlock()
		return
	}
	if w.watched == nil {
		w.watched = map[int]bool{}
	}
	w.watched[pid] = true
	src := w.source
	w.mu.Unlock()
	if src != nil {
		w.arm(src, pid)
	}
	w.kick.signal() // the sweep may need arming
}

// SetBlocking gates the liveness sweep: exits only matter while an assertion is held, and a PID
// whose assertion was released by key can live on for hours without needing a timer.
func (w *ProcessWatcher) SetBlocking(blocking bool) {
	w.mu.Lock()
	changed := w.blocking != blocking
	w.blocking = blocking
	w.mu.Unlock()
	if changed {
		w.kick.signal()
	}
}

// Watching reports whether pid is watched and has not been reported yet.
func (w *ProcessWatcher) Watching(pid int) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.watched[pid]
}

func (w *ProcessWatcher) arm(src ExitSource, pid int) {
	if err := src.Watch(pid); err != nil {
		// The liveness sweep still covers it.
		logger(w.Log, "process").Debug("cannot arm the exit watch", "pid", pid, "err", err)
	}
}

func (w *ProcessWatcher) sweepNeeded() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.blocking && len(w.watched) > 0
}

// exited reports pid once, if it is still watched: kqueue and the liveness sweep can both see the
// same exit.
func (w *ProcessWatcher) exited(pid int) {
	w.mu.Lock()
	if !w.watched[pid] {
		w.mu.Unlock()
		return
	}
	delete(w.watched, pid)
	w.mu.Unlock()
	logger(w.Log, "process").Info("watched process exited", "pid", pid)
	if w.OnExit != nil {
		w.OnExit(pid)
	}
}

// sweep reports every watched PID that no longer exists.
func (w *ProcessWatcher) sweep() {
	alive := w.ProcessAlive
	if alive == nil {
		alive = darwin.ProcessAlive
	}
	w.mu.Lock()
	pids := make([]int, 0, len(w.watched))
	for pid := range w.watched {
		pids = append(pids, pid)
	}
	w.mu.Unlock()
	for _, pid := range pids {
		if !alive(pid) {
			w.exited(pid)
		}
	}
}
