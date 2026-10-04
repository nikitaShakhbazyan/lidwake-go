package monitor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/nikitaShakhbazyan/lidwake-go/internal/activity"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/settings"
)

// writeStatus writes a Claude Code status file atomically (temp file + rename), as a sweep must
// never see half a file.
func writeStatus(t *testing.T, dir string, pid int, sessionID, status, waitingFor string, updated time.Time) {
	t.Helper()
	body := map[string]any{"pid": pid, "sessionId": sessionID, "status": status, "statusUpdatedAt": updated.UnixMilli()}
	if waitingFor != "" {
		body["waitingFor"] = waitingFor
	}
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(dir, "tmp-"+strconv.Itoa(pid))
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, strconv.Itoa(pid)+".json")); err != nil {
		t.Fatal(err)
	}
}

type sessionFixture struct {
	m       *SessionStatusMonitor
	reg     *fakeRegistry
	clock   *fakeClock
	dir     string
	actions *recorder[[]activity.WaitAction]
	deadMu  sync.Mutex
	dead    map[int]bool
}

func newSessionFixture(t *testing.T) *sessionFixture {
	f := &sessionFixture{reg: &fakeRegistry{}, clock: newFakeClock(t0), dir: t.TempDir(), actions: &recorder[[]activity.WaitAction]{}, dead: map[int]bool{}}
	f.m = &SessionStatusMonitor{
		Assertions: f.reg.snapshot,
		OnActions:  f.actions.add,
		Dir:        f.dir,
		ProcessAlive: func(pid int) bool {
			f.deadMu.Lock()
			defer f.deadMu.Unlock()
			return !f.dead[pid]
		},
		Now: f.clock.Now,
		Log: quiet(),
	}
	return f
}

func (f *sessionFixture) policy(p settings.WaitingPolicy, graceMinutes int) {
	s := settings.Defaults()
	s.AgentWaitingPolicy = p
	s.AgentWaitingGraceMinutes = graceMinutes
	f.m.ApplySettings(s)
}

func (f *sessionFixture) sweep() []activity.WaitAction {
	n := f.actions.len()
	f.m.sweep()
	if all := f.actions.all(); len(all) > n {
		return all[n]
	}
	return nil
}

func TestSessionStatusMonitor(t *testing.T) {
	t.Run("grace parks a waiting session's hold", func(t *testing.T) {
		f := newSessionFixture(t)
		f.reg.set(hookAssertion("claude-code:s1", 100, t0))
		writeStatus(t, f.dir, 500, "s1", "waiting", "approve Bash", t0)
		got := f.sweep()
		want := []activity.WaitAction{
			activity.SetWaitingFor{Key: "claude-code:s1", Label: "approve Bash"},
			activity.Park{Key: "claude-code:s1", ExpiresAt: t0.Add(10 * time.Minute)},
		}
		if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("actions = %#v", got)
		}
		if !f.m.HasParked() {
			t.Error("nothing parked")
		}
		if again := f.sweep(); again != nil {
			t.Errorf("a second sweep repeated actions: %#v", again)
		}
	})

	t.Run("the grace period comes from settings", func(t *testing.T) {
		f := newSessionFixture(t)
		f.policy(settings.WaitGrace, 3)
		f.reg.set(hookAssertion("claude-code:s1", 100, t0))
		writeStatus(t, f.dir, 500, "s1", "waiting", "", t0)
		got := f.sweep()
		if len(got) != 2 || got[1] != (activity.Park{Key: "claude-code:s1", ExpiresAt: t0.Add(3 * time.Minute)}) {
			t.Fatalf("actions = %#v", got)
		}
		if got[0] != (activity.SetWaitingFor{Key: "claude-code:s1", Label: "input needed"}) {
			t.Errorf("mark = %#v", got[0])
		}
	})

	t.Run("sleep releases a waiting session's hold", func(t *testing.T) {
		f := newSessionFixture(t)
		f.policy(settings.WaitSleep, 10)
		f.reg.set(hookAssertion("claude-code:s1", 100, t0))
		writeStatus(t, f.dir, 500, "s1", "waiting", "approve Bash", t0)
		got := f.sweep()
		if len(got) != 2 || got[1] != (activity.ReleaseWaiting{Key: "claude-code:s1"}) {
			t.Fatalf("actions = %#v", got)
		}
	})

	t.Run("keep awake only marks the hold", func(t *testing.T) {
		f := newSessionFixture(t)
		f.policy(settings.WaitKeepAwake, 10)
		f.reg.set(hookAssertion("claude-code:s1", 100, t0))
		writeStatus(t, f.dir, 500, "s1", "waiting", "approve Bash", t0)
		got := f.sweep()
		if len(got) != 1 || got[0] != (activity.SetWaitingFor{Key: "claude-code:s1", Label: "approve Bash"}) {
			t.Fatalf("actions = %#v", got)
		}
		if f.m.HasParked() {
			t.Error("parked under keepAwake")
		}
	})

	t.Run("a missing directory reads as no sessions", func(t *testing.T) {
		f := newSessionFixture(t)
		f.m.Dir = filepath.Join(f.dir, "absent")
		f.reg.set(hookAssertion("claude-code:s1", 100, t0))
		if got := f.sweep(); got != nil {
			t.Fatalf("actions = %#v", got)
		}
		if f.actions.len() != 0 {
			t.Error("OnActions called for an empty sweep")
		}
	})

	t.Run("a crashed session's leftover file is ignored", func(t *testing.T) {
		f := newSessionFixture(t)
		f.reg.set(hookAssertion("claude-code:s1", 100, t0))
		writeStatus(t, f.dir, 500, "s1", "waiting", "approve Bash", t0)
		f.dead[500] = true
		if got := f.sweep(); got != nil {
			t.Fatalf("actions = %#v", got)
		}
	})

	t.Run("without an assertion source a sweep does nothing", func(t *testing.T) {
		f := newSessionFixture(t)
		f.m.Assertions = nil
		writeStatus(t, f.dir, 500, "s1", "waiting", "", t0)
		f.m.sweep()
		if f.actions.len() != 0 {
			t.Error("acted without a source")
		}
	})

	t.Run("sweeps while blocking or while a wait is parked", func(t *testing.T) {
		f := newSessionFixture(t)
		f.policy(settings.WaitSleep, 10)
		f.m.Interval = time.Millisecond
		hold := hookAssertion("claude-code:s1", 100, t0)
		// The daemon's side: apply the actions to the registry and feed the blocking edge back.
		var reacquired counter
		f.m.OnActions = func(actions []activity.WaitAction) {
			f.actions.add(actions)
			for _, a := range actions {
				switch a := a.(type) {
				case activity.ReleaseWaiting:
					f.reg.set()
					f.m.SetBlocking(false)
				case activity.Reacquire:
					f.reg.set(a.Assertion)
					f.m.SetBlocking(true)
					reacquired.inc()
				}
			}
		}
		writeStatus(t, f.dir, 500, "s1", "busy", "", t0)
		start(t, f.m)
		stays(t, "no sweeps with nothing held or parked", 30*time.Millisecond, func() bool { return f.reg.readCount() == 0 })

		f.reg.set(hold)
		f.m.SetBlocking(true)
		eventually(t, "sweeps while blocking", func() bool { return f.reg.readCount() > 3 })

		writeStatus(t, f.dir, 500, "s1", "waiting", "approve Bash", t0)
		eventually(t, "the waiting hold released and parked", func() bool { return f.m.HasParked() && f.reg.readCount() > 0 && len(f.reg.snapshot()) == 0 })
		n := f.reg.readCount()
		eventually(t, "sweeps continue for the parked wait", func() bool { return f.reg.readCount() > n+3 })

		writeStatus(t, f.dir, 500, "s1", "busy", "", t0.Add(time.Minute))
		eventually(t, "re-acquire once the session turns busy", func() bool { return reacquired.get() == 1 })
		if f.m.HasParked() {
			t.Error("still parked after the re-acquire")
		}

		// The turn ends: the hold goes away, nothing is parked, the sweeps stop.
		f.reg.set()
		f.m.SetBlocking(false)
		eventually(t, "sweeps to stop", func() bool {
			n := f.reg.readCount()
			time.Sleep(5 * time.Millisecond)
			return f.reg.readCount() == n
		})
	})
}

func TestWaitConfigFor(t *testing.T) {
	s := settings.Defaults()
	if got := WaitConfigFor(s); got.Policy != settings.WaitGrace || got.Grace != 10*time.Minute {
		t.Errorf("defaults = %+v", got)
	}
	s.AgentWaitingPolicy = settings.WaitSleep
	s.AgentWaitingGraceMinutes = 45
	if got := WaitConfigFor(s); got.Policy != settings.WaitSleep || got.Grace != 45*time.Minute {
		t.Errorf("got %+v", got)
	}
}
