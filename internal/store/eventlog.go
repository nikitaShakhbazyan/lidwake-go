package store

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/nikitaShakhbazyan/lidwake-go/internal/model"
)

const (
	// EventLogFileName is the event log's name inside the support directory.
	EventLogFileName = "events.log"
	// DefaultMaxEventLogBytes is the size past which the log is rotated to events.log.1.
	DefaultMaxEventLogBytes = 10 << 20
)

// EventLog is events.log: one JSON object per line, {"t": RFC 3339 UTC time to the second,
// "event": model.Event}, appended as the daemon's state changes. Once the file passes MaxBytes it
// is renamed to events.log.1 (replacing the previous one) and a fresh file starts, so the log keeps
// between one and two rotations' worth of history. It also remembers the last event and when it
// happened, for the status.
//
// Logging is best effort: a failed write is logged and dropped, and the next append retries from a
// fresh open. Safe for concurrent use.
type EventLog struct {
	// Path is the log file. NewEventLog puts it in the support directory.
	Path string
	// MaxBytes is the rotation threshold. Zero: DefaultMaxEventLogBytes.
	MaxBytes int64
	// Now is the clock. Nil: time.Now.
	Now func() time.Time
	Log *slog.Logger

	mu     sync.Mutex
	file   *os.File // kept open at end of file between appends, reopened after rotation
	size   int64    // bytes in the file, so rotation needs no stat per append
	last   model.Event
	lastAt time.Time
}

// NewEventLog returns the log at dir/events.log; the daemon passes paths.SupportDir().
func NewEventLog(dir string) *EventLog {
	return &EventLog{Path: filepath.Join(dir, EventLogFileName)}
}

// RotatedPath is where a full log is moved: events.log.1.
func (l *EventLog) RotatedPath() string { return l.Path + ".1" }

type eventLine struct {
	T     string      `json:"t"`
	Event model.Event `json:"event"`
}

// Append records e now: it becomes the last event even when the write fails.
func (l *EventLog) Append(e model.Event) {
	now := time.Now
	if l.Now != nil {
		now = l.Now
	}
	// Stamped under the lock, so the lines, their times and Last agree on the order of
	// concurrent appends.
	l.mu.Lock()
	defer l.mu.Unlock()
	t := now().Round(0)
	l.last, l.lastAt = e, t
	line, err := json.Marshal(eventLine{T: t.UTC().Format(time.RFC3339), Event: e})
	if err != nil {
		return
	}
	line = append(line, '\n')
	if err := l.write(line); err != nil {
		l.logger().Error("cannot append to the event log", "err", err)
		return
	}
	if err := l.rotateIfNeeded(); err != nil {
		l.logger().Error("cannot rotate the event log", "err", err)
	}
}

// Last is the most recently appended event and its time; ok is false before the first append.
func (l *EventLog) Last() (e model.Event, at time.Time, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.last, l.lastAt, l.last != ""
}

// Close closes the log file. A later Append reopens it.
func (l *EventLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closeFile()
}

func (l *EventLog) logger() *slog.Logger {
	if l.Log == nil {
		return slog.Default()
	}
	return l.Log
}

func (l *EventLog) maxBytes() int64 {
	if l.MaxBytes <= 0 {
		return DefaultMaxEventLogBytes
	}
	return l.MaxBytes
}

// open returns the append handle, opening (and creating, owner-only) the file on first use.
// Callers hold l.mu.
func (l *EventLog) open() (*os.File, error) {
	if l.file != nil {
		return l.file, nil
	}
	if err := os.MkdirAll(filepath.Dir(l.Path), 0o700); err != nil {
		return nil, fmt.Errorf("store: create %s: %w", filepath.Dir(l.Path), err)
	}
	f, err := os.OpenFile(l.Path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", l.Path, err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("store: stat %s: %w", l.Path, err)
	}
	l.file, l.size = f, info.Size()
	return f, nil
}

// write appends line. On failure the handle is dropped so the next append retries from a clean
// open. Callers hold l.mu.
func (l *EventLog) write(line []byte) error {
	f, err := l.open()
	if err != nil {
		return err
	}
	n, err := f.Write(line)
	l.size += int64(n)
	if err != nil {
		l.closeFile()
		return fmt.Errorf("store: write %s: %w", l.Path, err)
	}
	return nil
}

// rotateIfNeeded moves a log past MaxBytes to events.log.1. Callers hold l.mu.
func (l *EventLog) rotateIfNeeded() error {
	if l.size <= l.maxBytes() {
		return nil
	}
	l.closeFile()
	rotated := l.RotatedPath()
	if err := os.Remove(rotated); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("store: remove %s: %w", rotated, err)
	}
	if err := os.Rename(l.Path, rotated); err != nil {
		return fmt.Errorf("store: rename %s: %w", l.Path, err)
	}
	l.size = 0
	return nil
}

// closeFile drops the handle. Callers hold l.mu.
func (l *EventLog) closeFile() error {
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}
