package store

import (
	"bufio"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nikitaShakhbazyan/lidwake-go/internal/model"
)

func readLines(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return lines
}

func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

func TestEventLogAppendsJSONLines(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "support")
	l := NewEventLog(dir)
	defer l.Close()
	if l.Path != filepath.Join(dir, "events.log") || l.RotatedPath() != filepath.Join(dir, "events.log.1") {
		t.Fatalf("paths = %s, %s", l.Path, l.RotatedPath())
	}
	at := time.Date(2026, 10, 4, 10, 15, 30, 999_000_000, time.FixedZone("GST", 4*3600))
	l.Now = fixedClock(at)

	if _, _, ok := l.Last(); ok {
		t.Fatal("a last event before any append")
	}
	l.Append(model.EventAcquired)
	l.Append(model.EventLidClosed)

	lines := readLines(t, l.Path)
	if len(lines) != 2 {
		t.Fatalf("lines = %q", lines)
	}
	var entry map[string]string
	if err := json.Unmarshal([]byte(lines[1]), &entry); err != nil {
		t.Fatal(err)
	}
	// The same two fields as before: an ISO 8601 UTC time to the second, and the event name.
	if len(entry) != 2 || entry["event"] != "lidClosed" || entry["t"] != "2026-10-04T06:15:30Z" {
		t.Errorf("entry = %v", entry)
	}
	ev, last, ok := l.Last()
	if !ok || ev != model.EventLidClosed || !last.Equal(at) {
		t.Errorf("last = %q %v %v", ev, last, ok)
	}

	info, err := os.Stat(l.Path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("events.log mode = %o, want 600", perm)
	}
}

func TestEventLogRotates(t *testing.T) {
	dir := t.TempDir()
	l := NewEventLog(dir)
	defer l.Close()
	l.Now = fixedClock(time.Date(2026, 10, 4, 10, 0, 0, 0, time.UTC))
	line := len(`{"t":"2026-10-04T10:00:00Z","event":"acquired"}`) + 1
	l.MaxBytes = int64(3 * line) // rotation once a fourth line is written

	for range 3 {
		l.Append(model.EventAcquired)
	}
	if _, err := os.Stat(l.RotatedPath()); !os.IsNotExist(err) {
		t.Fatal("rotated at the threshold, want only past it")
	}
	l.Append(model.EventReleased)
	if got := readLines(t, l.RotatedPath()); len(got) != 4 || !strings.Contains(got[3], "released") {
		t.Fatalf("events.log.1 = %q", got)
	}
	if _, err := os.Stat(l.Path); !os.IsNotExist(err) {
		t.Fatal("events.log still present right after rotation")
	}

	// The next append starts a fresh file; a second rotation replaces events.log.1.
	l.Append(model.EventIdleRelease)
	if got := readLines(t, l.Path); len(got) != 1 || !strings.Contains(got[0], "idleRelease") {
		t.Fatalf("events.log = %q", got)
	}
	l.Append(model.EventLidOpened)
	l.Append(model.EventLidOpened) // 51 + 49 + 49 bytes: past the 144-byte threshold
	got := readLines(t, l.RotatedPath())
	if len(got) != 3 || !strings.Contains(got[0], "idleRelease") {
		t.Fatalf("events.log.1 after the second rotation = %q", got)
	}
	if ev, _, _ := l.Last(); ev != model.EventLidOpened {
		t.Errorf("last = %q", ev)
	}
}

func TestEventLogCountsExistingContent(t *testing.T) {
	dir := t.TempDir()
	l := NewEventLog(dir)
	l.MaxBytes = 100
	if err := os.WriteFile(l.Path, []byte(strings.Repeat("x", 99)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	l.Append(model.EventAcquired)
	if _, err := os.Stat(l.RotatedPath()); err != nil {
		t.Fatalf("a log already at the threshold was not rotated: %v", err)
	}
	l.Close()

	// Close, then append again: the file is reopened and its size picked up again.
	l.Append(model.EventReleased)
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	l.Append(model.EventReleased)
	if got := readLines(t, l.Path); len(got) != 2 {
		t.Fatalf("lines after reopen = %q", got)
	}
}

func TestEventLogWriteFailureKeepsLastEvent(t *testing.T) {
	dir := t.TempDir()
	l := NewEventLog(dir)
	l.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := os.Mkdir(l.Path, 0o700); err != nil { // a directory where the log should be
		t.Fatal(err)
	}
	l.Append(model.EventThermalCutout)
	if ev, _, ok := l.Last(); !ok || ev != model.EventThermalCutout {
		t.Errorf("last = %q %v", ev, ok)
	}
	if err := os.Remove(l.Path); err != nil {
		t.Fatal(err)
	}
	// The next append retries from a clean open.
	l.Append(model.EventLowBatteryCutout)
	defer l.Close()
	if got := readLines(t, l.Path); len(got) != 1 || !strings.Contains(got[0], "lowBatteryCutout") {
		t.Errorf("lines = %q", got)
	}
}

func TestEventLogConcurrentAppends(t *testing.T) {
	l := NewEventLog(t.TempDir())
	defer l.Close()
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				l.Append(model.EventAcquired)
				l.Last()
			}
		}()
	}
	wg.Wait()
	lines := readLines(t, l.Path)
	if len(lines) != 400 {
		t.Fatalf("got %d lines, want 400", len(lines))
	}
	for _, line := range lines {
		var entry map[string]string
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("torn line %q: %v", line, err)
		}
	}
}

// Concurrent appends: the lines, their times and Last agree on one order.
func TestEventLogConcurrentAppendsStayInTimeOrder(t *testing.T) {
	l := NewEventLog(t.TempDir())
	defer l.Close()
	var mu sync.Mutex
	clock := time.Date(2026, 10, 4, 10, 0, 0, 0, time.UTC)
	l.Now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		clock = clock.Add(time.Second)
		return clock
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				l.Append(model.EventReleased)
			}
		}()
	}
	wg.Wait()
	var prev time.Time
	for _, line := range readLines(t, l.Path) {
		var entry map[string]string
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatal(err)
		}
		at, err := time.Parse(time.RFC3339, entry["t"])
		if err != nil {
			t.Fatal(err)
		}
		if !at.After(prev) {
			t.Fatalf("line at %v written after %v", at, prev)
		}
		prev = at
	}
	if _, last, ok := l.Last(); !ok || !last.Equal(prev) {
		t.Errorf("last = %v, want the last line's %v", last, prev)
	}
}
