package activity

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/nikitaShakhbazyan/lidwake-go/internal/paths"
)

// SessionActivity is a Claude Code session's activity state.
type SessionActivity string

const (
	ActivityBusy    SessionActivity = "busy"
	ActivityIdle    SessionActivity = "idle"
	ActivityWaiting SessionActivity = "waiting"
)

// ClaudeSessionStatus is one Claude Code session's live activity state, read from its status
// file.
//
// Every top-level Claude Code session maintains `~/.claude/sessions/<pid>.json` with, among other
// fields, `status: "busy" | "idle" | "waiting"` and a `waitingFor` label while a dialog is open
// ("approve Bash", "input needed", …). The status is computed from the session's whole dialog
// stack — questions, plan approvals, permission prompts, elicitations — and rewritten by the same
// code path that resolves a dialog, however it resolves: an answer at the keyboard, an answer from
// a phone over Remote Control, or an auto-continue on timeout. That makes it the one deterministic
// channel for "this agent is waiting on its user", including the prompt kinds that fire no hook.
//
// The format is Claude Code internals (it backs `claude ps`; fields have only accreted), so parsing
// is strictly defensive: anything unrecognized reads as "no status", and every consumer must
// degrade to lidwake's other nets when it does. Sessions started with a non-default
// CLAUDE_CONFIG_DIR write elsewhere and simply are not seen.
//
// The `<pid>.<hash>.key` files in the same directory are messaging-socket auth material: the
// filename filter must never match them, and nothing may read them.
type ClaudeSessionStatus struct {
	PID       int
	SessionID string
	Activity  SessionActivity
	// WaitingFor is what the session waits on while Activity is ActivityWaiting ("approve Bash",
	// "input needed", "dialog open", …); "" when absent, which older builds do even while waiting.
	WaitingFor string
	// StatusUpdatedAt is when Activity last changed, from the file's millisecond-epoch
	// `statusUpdatedAt` (or `updatedAt`); the zero time when absent.
	StatusUpdatedAt time.Time
}

// maxStatusFileSize bounds a status file read. Real files are well under a kilobyte; anything
// this large is not a status file.
const maxStatusFileSize = 1 << 20

// DefaultSessionsDir is where Claude Code writes its status files: under its config home. The
// daemon has no view of a user's shell environment, so a custom CLAUDE_CONFIG_DIR is out of scope.
func DefaultSessionsDir() string {
	return filepath.Join(paths.Home(), ".claude", "sessions")
}

// IsStatusFilename reports whether filename is a session status file: `<pid>.json` with an
// all-digit stem, strictly, anchored like Claude Code's own reader so the `.key` auth files and
// anything else are never touched.
func IsStatusFilename(filename string) bool {
	stem, ok := strings.CutSuffix(filename, ".json")
	if !ok || stem == "" {
		return false
	}
	for i := 0; i < len(stem); i++ {
		if stem[i] < '0' || stem[i] > '9' {
			return false
		}
	}
	return true
}

// ParseSessionStatus parses one status file. ok is false unless the bytes are a JSON object with
// a positive integer `pid`, a non-empty string `sessionId` and a `status` in the known set: an
// unknown status string means a newer Claude Code changed the vocabulary, and guessing would be
// worse than falling back to the CPU nets.
func ParseSessionStatus(data []byte) (ClaudeSessionStatus, bool) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil || obj == nil {
		return ClaudeSessionStatus{}, false
	}
	if _, err := dec.Token(); err != io.EOF {
		return ClaudeSessionStatus{}, false // trailing content: not one JSON document
	}
	pid, ok := intValue(obj["pid"])
	if !ok || pid <= 0 || pid > math.MaxInt32 {
		return ClaudeSessionStatus{}, false
	}
	sessionID, _ := obj["sessionId"].(string)
	if sessionID == "" {
		return ClaudeSessionStatus{}, false
	}
	statusString, _ := obj["status"].(string)
	activity := SessionActivity(statusString)
	switch activity {
	case ActivityBusy, ActivityIdle, ActivityWaiting:
	default:
		return ClaudeSessionStatus{}, false
	}
	waitingFor, _ := obj["waitingFor"].(string)
	s := ClaudeSessionStatus{PID: int(pid), SessionID: sessionID, Activity: activity, WaitingFor: waitingFor}
	millis, ok := floatValue(obj["statusUpdatedAt"])
	if !ok {
		millis, ok = floatValue(obj["updatedAt"])
	}
	if ok {
		s.StatusUpdatedAt = fromMillis(millis)
	}
	return s, true
}

// BestSessionStatus is the authoritative status for sessionID among possibly several files
// claiming it. A crashed session leaves its file behind (cleanup never ran), and `--resume`
// registers the same session id under a new pid — so dead pids are discarded, and among live ones
// the freshest StatusUpdatedAt wins (the first one listed on a tie).
func BestSessionStatus(sessionID string, statuses []ClaudeSessionStatus, pidAlive func(pid int) bool) (ClaudeSessionStatus, bool) {
	var best ClaudeSessionStatus
	found := false
	for _, s := range statuses {
		if s.SessionID != sessionID || (pidAlive != nil && !pidAlive(s.PID)) {
			continue
		}
		if !found || s.StatusUpdatedAt.After(best.StatusUpdatedAt) {
			best, found = s, true
		}
	}
	return best, found
}

// ReadSessionStatuses returns every parseable status file in dir. A missing directory (Claude
// Code absent or never run) reads as no sessions, which disables the waiting policy gracefully.
// Only regular files named `<pid>.json` are opened.
func ReadSessionStatuses(dir string) []ClaudeSessionStatus {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []ClaudeSessionStatus
	for _, entry := range entries {
		if !IsStatusFilename(entry.Name()) {
			continue
		}
		data, ok := readRegularFile(filepath.Join(dir, entry.Name()))
		if !ok {
			continue
		}
		if s, ok := ParseSessionStatus(data); ok {
			out = append(out, s)
		}
	}
	return out
}

// readRegularFile reads a regular file (following a symlink) of at most maxStatusFileSize bytes.
// Anything else — a FIFO that would block the sweep, a directory, an oversized file — is skipped.
// The open is non-blocking and the type is checked on the opened descriptor, so a file swapped
// for a FIFO between listing and reading cannot hang the sweep either.
func readRegularFile(path string) ([]byte, bool) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxStatusFileSize {
		return nil, false
	}
	data, err := io.ReadAll(io.LimitReader(f, maxStatusFileSize+1))
	if err != nil || len(data) > maxStatusFileSize {
		return nil, false
	}
	return data, true
}

// intValue accepts a JSON number with an integral value; strings, booleans and fractions are not
// integers.
func intValue(v any) (int64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	if i, err := n.Int64(); err == nil {
		return i, true
	}
	f, err := n.Float64()
	if err != nil || f != math.Trunc(f) || math.Abs(f) > math.MaxInt64/2 {
		return 0, false
	}
	return int64(f), true
}

func floatValue(v any) (float64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	f, err := n.Float64()
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

// fromMillis converts a millisecond epoch to a time, keeping sub-millisecond fractions.
// Out-of-range values (beyond ±1e15 s) read as absent rather than overflowing.
func fromMillis(ms float64) time.Time {
	if math.Abs(ms) > 1e18 {
		return time.Time{}
	}
	whole := math.Floor(ms)
	frac := math.Round((ms - whole) * 1e6) // nanoseconds below the millisecond
	return time.UnixMilli(int64(whole)).Add(time.Duration(frac))
}
