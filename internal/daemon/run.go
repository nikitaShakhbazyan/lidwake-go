package daemon

import (
	"context"
	"errors"
	"log/slog"
	"os"

	"github.com/nikitaShakhbazyan/lidwake-go/internal/darwin"
)

// Run starts the daemon and blocks until ctx is cancelled or a fatal error occurs.
// CONTRACT: `lidwake daemon` calls exactly this, on the main goroutine, which main.go locks to the
// main OS thread.
//
// Run keeps that goroutine as the daemon's main-thread executor: the screen lock (macOS wants it on
// the main thread) is posted here and run in order, while Serve runs on another goroutine. It logs
// to stderr, which launchd sends to the daemon log. After stopping, idle, to adopt an updated
// binary it returns nil, and launchd (KeepAlive) relaunches the daemon from the new image.
func Run(ctx context.Context) error {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	slog.SetDefault(log)
	log.Info("daemon process starting", "uid", os.Getuid(), "pid", os.Getpid())

	main := newMainThread()
	d := New(Config{
		// Recorded now, at launch, so it is the binary this process runs.
		Executable: RunningExecutableStaleness(),
		LockScreen: func() {
			if !main.post(func() {
				if err := darwin.LockScreen(); err != nil {
					log.Error("screen lock failed", "err", err)
				}
			}) {
				log.Warn("screen lock skipped: one is already queued")
			}
		},
		Log: log,
	})
	err := main.run(func() error { return d.Serve(ctx) })
	if errors.Is(err, ErrExecutableReplaced) {
		return nil
	}
	return err
}

// mainThread runs posted work on the goroutine that calls run — the main goroutine, pinned to the
// main OS thread.
type mainThread struct {
	tasks chan func()
}

func newMainThread() *mainThread {
	return &mainThread{tasks: make(chan func(), 4)}
}

// post queues f for the main thread without blocking; false when the queue is full.
func (m *mainThread) post(f func()) bool {
	select {
	case m.tasks <- f:
		return true
	default:
		return false
	}
}

// run calls serve on a new goroutine and executes posted work on the calling goroutine until
// serve returns. Work still queued then is dropped.
func (m *mainThread) run(serve func() error) error {
	errc := make(chan error, 1)
	go func() { errc <- serve() }()
	for {
		select {
		case f := <-m.tasks:
			f()
		case err := <-errc:
			return err
		}
	}
}
