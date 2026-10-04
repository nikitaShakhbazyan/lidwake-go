package activity

import (
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"
)

func TestClaudeSessionStatus(t *testing.T) {
	t.Run("parses a real status file shape", func(t *testing.T) {
		s, ok := ParseSessionStatus([]byte(`
		{"pid": 37388, "sessionId": "5328115a-1c2f", "cwd": "/tmp", "version": "2.1.238",
		 "kind": "interactive", "status": "waiting", "waitingFor": "approve Bash",
		 "statusUpdatedAt": 1787347947387, "updatedAt": 1787347947387}
		`))
		if !ok {
			t.Fatal("expected a status")
		}
		if s.PID != 37_388 || s.SessionID != "5328115a-1c2f" || s.Activity != ActivityWaiting || s.WaitingFor != "approve Bash" {
			t.Fatalf("status = %+v", s)
		}
		if s.StatusUpdatedAt.IsZero() {
			t.Fatal("expected statusUpdatedAt")
		}
		if want := time.UnixMilli(1787347947387); !s.StatusUpdatedAt.Equal(want) {
			t.Fatalf("statusUpdatedAt = %v, want %v", s.StatusUpdatedAt, want)
		}
	})

	t.Run("waitingFor is optional and empty reads as absent", func(t *testing.T) {
		bare, ok := ParseSessionStatus([]byte(`{"pid": 1, "sessionId": "s", "status": "busy"}`))
		if !ok || bare.Activity != ActivityBusy || bare.WaitingFor != "" {
			t.Fatalf("bare = %+v, %v", bare, ok)
		}
		empty, ok := ParseSessionStatus([]byte(`{"pid": 1, "sessionId": "s", "status": "waiting", "waitingFor": ""}`))
		if !ok || empty.WaitingFor != "" {
			t.Fatalf("empty = %+v, %v", empty, ok)
		}
	})

	// The file is Claude Code internals — anything off-shape must read as "no status", never
	// crash or guess. An unknown status string means the vocabulary changed upstream.
	t.Run("rejects malformed and unknown content", func(t *testing.T) {
		for _, in := range []string{
			"not json",
			"[1,2]",
			`{"pid": 0, "sessionId": "s", "status": "busy"}`,
			`{"pid": 5, "sessionId": "", "status": "busy"}`,
			`{"pid": 5, "sessionId": "s"}`,
			`{"pid": 5, "sessionId": "s", "status": "blocked"}`,
			`{"pid": "5", "sessionId": "s", "status": "busy"}`,
		} {
			if s, ok := ParseSessionStatus([]byte(in)); ok {
				t.Errorf("ParseSessionStatus(%q) = %+v, want no status", in, s)
			}
		}
	})

	// Only `<pid>.json` is a status file. The `<pid>.<hash>.key` files beside them are
	// messaging-socket auth material and must never match.
	t.Run("filename filter matches pid json only", func(t *testing.T) {
		if !IsStatusFilename("37388.json") {
			t.Error("37388.json should match")
		}
		for _, name := range []string{"37388.3247fb14.key", "37388.3247fb14.json", "notes.json", ".json", "37388"} {
			if IsStatusFilename(name) {
				t.Errorf("%q should not match", name)
			}
		}
	})

	// A crashed session leaves its file behind, and `--resume` re-registers the same session id
	// under a new pid — dead pids lose, and among live ones the freshest status wins.
	t.Run("best prefers live pids then freshest status", func(t *testing.T) {
		stale := ClaudeSessionStatus{PID: 100, SessionID: "s", Activity: ActivityWaiting, StatusUpdatedAt: time.Unix(1_000, 0)}
		older := ClaudeSessionStatus{PID: 200, SessionID: "s", Activity: ActivityIdle, StatusUpdatedAt: time.Unix(2_000, 0)}
		fresh := ClaudeSessionStatus{PID: 300, SessionID: "s", Activity: ActivityBusy, StatusUpdatedAt: time.Unix(3_000, 0)}
		other := ClaudeSessionStatus{PID: 400, SessionID: "t", Activity: ActivityWaiting, StatusUpdatedAt: time.Unix(9_000, 0)}

		best, ok := BestSessionStatus("s", []ClaudeSessionStatus{stale, older, fresh, other}, func(pid int) bool { return pid != 100 })
		if !ok || best.PID != 300 {
			t.Fatalf("best = %+v, %v", best, ok)
		}
		if _, ok := BestSessionStatus("s", []ClaudeSessionStatus{stale}, func(int) bool { return false }); ok {
			t.Fatal("a dead pid must not win")
		}
		if _, ok := BestSessionStatus("missing", []ClaudeSessionStatus{stale, fresh}, func(int) bool { return true }); ok {
			t.Fatal("an unknown session has no status")
		}
	})
}

// Go-specific behavior.
func TestClaudeSessionStatusGo(t *testing.T) {
	t.Run("filename filter rejects non-ascii digits", func(t *testing.T) {
		for _, name := range []string{"٣٤.json", "½.json", "12a.json", "-1.json", "1.JSON"} {
			if IsStatusFilename(name) {
				t.Errorf("%q should not match", name)
			}
		}
	})

	t.Run("rejects pids out of range, fractions, booleans and trailing content", func(t *testing.T) {
		for _, in := range []string{
			`{"pid": 4294967296, "sessionId": "s", "status": "busy"}`,
			`{"pid": 1.5, "sessionId": "s", "status": "busy"}`,
			`{"pid": true, "sessionId": "s", "status": "busy"}`,
			`{"pid": -3, "sessionId": "s", "status": "busy"}`,
			`{"pid": 5, "sessionId": 7, "status": "busy"}`,
			`{"pid": 5, "sessionId": "s", "status": "busy"} {}`,
			`null`,
			``,
		} {
			if s, ok := ParseSessionStatus([]byte(in)); ok {
				t.Errorf("ParseSessionStatus(%q) = %+v, want no status", in, s)
			}
		}
	})

	t.Run("an integral float pid is accepted", func(t *testing.T) {
		s, ok := ParseSessionStatus([]byte(`{"pid": 5.0, "sessionId": "s", "status": "idle"}`))
		if !ok || s.PID != 5 {
			t.Fatalf("status = %+v, %v", s, ok)
		}
	})

	t.Run("updatedAt is the fallback timestamp and a non-number is absent", func(t *testing.T) {
		s, _ := ParseSessionStatus([]byte(`{"pid": 5, "sessionId": "s", "status": "idle", "statusUpdatedAt": "x", "updatedAt": 2500.5}`))
		if want := time.Unix(2, 500_500_000); !s.StatusUpdatedAt.Equal(want) {
			t.Fatalf("statusUpdatedAt = %v, want %v", s.StatusUpdatedAt, want)
		}
		s, _ = ParseSessionStatus([]byte(`{"pid": 5, "sessionId": "s", "status": "idle", "updatedAt": "x"}`))
		if !s.StatusUpdatedAt.IsZero() {
			t.Fatalf("statusUpdatedAt = %v, want zero", s.StatusUpdatedAt)
		}
		s, _ = ParseSessionStatus([]byte(`{"pid": 5, "sessionId": "s", "status": "idle", "statusUpdatedAt": 1e300}`))
		if !s.StatusUpdatedAt.IsZero() {
			t.Fatalf("statusUpdatedAt = %v, want zero", s.StatusUpdatedAt)
		}
	})

	t.Run("a non-string waitingFor reads as absent", func(t *testing.T) {
		s, ok := ParseSessionStatus([]byte(`{"pid": 5, "sessionId": "s", "status": "waiting", "waitingFor": 3}`))
		if !ok || s.WaitingFor != "" {
			t.Fatalf("status = %+v, %v", s, ok)
		}
	})

	t.Run("best breaks timestamp ties by listing order", func(t *testing.T) {
		a := ClaudeSessionStatus{PID: 1, SessionID: "s", Activity: ActivityBusy}
		b := ClaudeSessionStatus{PID: 2, SessionID: "s", Activity: ActivityIdle}
		best, _ := BestSessionStatus("s", []ClaudeSessionStatus{a, b}, nil)
		if best.PID != 1 {
			t.Fatalf("best = %+v", best)
		}
	})

	t.Run("read statuses filters names, skips bad files and never opens key files", func(t *testing.T) {
		dir := t.TempDir()
		write := func(name, content string, mode os.FileMode) {
			t.Helper()
			if err := os.WriteFile(filepath.Join(dir, name), []byte(content), mode); err != nil {
				t.Fatal(err)
			}
		}
		write("100.json", `{"pid": 100, "sessionId": "a", "status": "busy"}`, 0o600)
		write("200.json", `{"pid": 200, "sessionId": "b", "status": "waiting", "waitingFor": "approve Bash"}`, 0o600)
		write("300.json", `garbage`, 0o600)
		write("notes.json", `{"pid": 400, "sessionId": "c", "status": "busy"}`, 0o600)
		// Valid-looking JSON, so a filter that let the key file through would surface "d".
		write("100.3247fb14.key", `{"pid": 500, "sessionId": "d", "status": "busy"}`, 0o600)
		if err := os.Mkdir(filepath.Join(dir, "600.json"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Mkfifo(filepath.Join(dir, "700.json"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(dir, "100.json"), filepath.Join(dir, "800.json")); err != nil {
			t.Fatal(err)
		}

		got := ReadSessionStatuses(dir)
		var ids []string
		for _, s := range got {
			ids = append(ids, s.SessionID)
		}
		slices.Sort(ids)
		if !slices.Equal(ids, []string{"a", "a", "b"}) {
			t.Fatalf("session ids = %v", ids)
		}
	})

	t.Run("a missing directory reads as no sessions", func(t *testing.T) {
		if got := ReadSessionStatuses(filepath.Join(t.TempDir(), "nope")); len(got) != 0 {
			t.Fatalf("got %v", got)
		}
	})

	t.Run("an oversized file is skipped", func(t *testing.T) {
		dir := t.TempDir()
		big := make([]byte, maxStatusFileSize+10)
		for i := range big {
			big[i] = ' '
		}
		copy(big, `{"pid": 1, "sessionId": "s", "status": "busy"}`)
		if err := os.WriteFile(filepath.Join(dir, "1.json"), big, 0o600); err != nil {
			t.Fatal(err)
		}
		if got := ReadSessionStatuses(dir); len(got) != 0 {
			t.Fatalf("got %v", got)
		}
	})

	t.Run("default directory is under the home config dir", func(t *testing.T) {
		t.Setenv("HOME", "/Users/someone")
		if got := DefaultSessionsDir(); got != "/Users/someone/.claude/sessions" {
			t.Fatalf("DefaultSessionsDir() = %q", got)
		}
	})
}
