package activity

import (
	"fmt"
	"testing"
	"time"

	"github.com/nikitaShakhbazyan/lidwake-go/internal/model"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/settings"
)

var waitNow = time.Unix(1_000_000, 0)

func sessionAssertion(sessionID string, expiresAt *time.Time) model.Assertion {
	a := model.New("claude-code:"+sessionID, "claude-code", "", 500, "claude", waitNow.Add(-300*time.Second), nil, model.OriginHook)
	a.ExpiresAt = expiresAt
	return a
}

func status(sessionID string, activity SessionActivity, waitingFor string) ClaudeSessionStatus {
	return ClaudeSessionStatus{PID: 500, SessionID: sessionID, Activity: activity, WaitingFor: waitingFor, StatusUpdatedAt: waitNow}
}

type waitCall struct {
	policy settings.WaitingPolicy
	grace  time.Duration
}

func evaluate(e *SessionWaitEvaluator, assertions []model.Assertion, statuses []ClaudeSessionStatus, opts ...func(*waitCall)) []WaitAction {
	c := waitCall{policy: settings.WaitGrace, grace: 600 * time.Second}
	for _, o := range opts {
		o(&c)
	}
	return e.Evaluate(assertions, statuses, waitNow, WaitConfig{Policy: c.policy, Grace: c.grace}, func(int) bool { return true })
}

func policy(p settings.WaitingPolicy) func(*waitCall) { return func(c *waitCall) { c.policy = p } }

func timePtr(t time.Time) *time.Time { return &t }

// actionEqual compares actions by value, with times compared as instants.
func actionEqual(a, b WaitAction) bool {
	switch x := a.(type) {
	case SetWaitingFor:
		y, ok := b.(SetWaitingFor)
		return ok && x == y
	case Park:
		y, ok := b.(Park)
		return ok && x.Key == y.Key && x.ExpiresAt.Equal(y.ExpiresAt)
	case ReleaseWaiting:
		y, ok := b.(ReleaseWaiting)
		return ok && x == y
	case Restore:
		y, ok := b.(Restore)
		return ok && x.Key == y.Key && timePtrEqual(x.Expiry, y.Expiry)
	case Reacquire:
		y, ok := b.(Reacquire)
		return ok && x.Assertion.Key == y.Assertion.Key && timePtrEqual(x.Assertion.ExpiresAt, y.Assertion.ExpiresAt) &&
			x.Assertion.WaitingFor == y.Assertion.WaitingFor
	}
	return false
}

func timePtrEqual(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

func expectActions(t *testing.T, got []WaitAction, want ...WaitAction) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("actions = %s, want %s", fmtActions(got), fmtActions(want))
	}
	for i := range got {
		if !actionEqual(got[i], want[i]) {
			t.Fatalf("actions = %s, want %s", fmtActions(got), fmtActions(want))
		}
	}
}

func expectContains(t *testing.T, got []WaitAction, want WaitAction) {
	t.Helper()
	for _, a := range got {
		if actionEqual(a, want) {
			return
		}
	}
	t.Fatalf("actions %s do not contain %s", fmtActions(got), fmtActions([]WaitAction{want}))
}

func fmtActions(as []WaitAction) string {
	s := "["
	for i, a := range as {
		if i > 0 {
			s += ", "
		}
		s += fmt.Sprintf("%T%+v", a, a)
	}
	return s + "]"
}

func TestSessionWaitEvaluator(t *testing.T) {
	t.Run("grace parks a waiting hold and restores it on busy", func(t *testing.T) {
		e := NewSessionWaitEvaluator()
		a := sessionAssertion("s1", nil)

		parked := evaluate(e, []model.Assertion{a}, []ClaudeSessionStatus{status("s1", ActivityWaiting, "approve Bash")})
		expectActions(t, parked,
			SetWaitingFor{Key: a.Key, Label: "approve Bash"},
			Park{Key: a.Key, ExpiresAt: waitNow.Add(600 * time.Second)},
		)
		if !e.HasParked() {
			t.Fatal("expected a parked wait")
		}

		// Still waiting: no repeated actions.
		stillParked := e.Clone()
		expectActions(t, evaluate(stillParked, []model.Assertion{a}, []ClaudeSessionStatus{status("s1", ActivityWaiting, "approve Bash")}))

		// Answered (keyboard, phone, or timeout — all just flip the file to busy).
		restored := evaluate(e, []model.Assertion{a}, []ClaudeSessionStatus{status("s1", ActivityBusy, "")})
		expectActions(t, restored,
			SetWaitingFor{Key: a.Key},
			Restore{Key: a.Key, Expiry: nil},
		)
		if e.HasParked() {
			t.Fatal("expected nothing parked")
		}
	})

	// A hold that already carries a shorter TTL must never be extended by the grace window.
	t.Run("grace never extends an existing shorter expiry", func(t *testing.T) {
		e := NewSessionWaitEvaluator()
		soon := waitNow.Add(120 * time.Second)
		a := sessionAssertion("s1", timePtr(soon))

		actions := evaluate(e, []model.Assertion{a}, []ClaudeSessionStatus{status("s1", ActivityWaiting, "")})
		expectContains(t, actions, Park{Key: a.Key, ExpiresAt: soon})

		// And the restore puts the original expiry back, not nil.
		restored := evaluate(e, []model.Assertion{a}, []ClaudeSessionStatus{status("s1", ActivityBusy, "")})
		expectContains(t, restored, Restore{Key: a.Key, Expiry: timePtr(soon)})
	})

	// The grace TTL ran out and the idle sweep released the hold; the session is answered later
	// (the Mac slept, woke, and the user replied) — the resumed turn gets its hold back.
	t.Run("grace reacquires after the released hold's session turns busy", func(t *testing.T) {
		e := NewSessionWaitEvaluator()
		a := sessionAssertion("s1", nil)
		evaluate(e, []model.Assertion{a}, []ClaudeSessionStatus{status("s1", ActivityWaiting, "")})

		// Hold gone (TTL expiry), still waiting: nothing to do, but the entry stays parked.
		expectActions(t, evaluate(e, nil, []ClaudeSessionStatus{status("s1", ActivityWaiting, "")}))
		if !e.HasParked() {
			t.Fatal("expected the entry to stay parked")
		}

		resumed := evaluate(e, nil, []ClaudeSessionStatus{status("s1", ActivityBusy, "")})
		if len(resumed) != 1 {
			t.Fatalf("expected a single reacquire, got %s", fmtActions(resumed))
		}
		r, ok := resumed[0].(Reacquire)
		if !ok {
			t.Fatalf("expected a single reacquire, got %s", fmtActions(resumed))
		}
		if r.Assertion.Key != a.Key || r.Assertion.ExpiresAt != nil || r.Assertion.WaitingFor != "" {
			t.Fatalf("reacquired = %+v", r.Assertion)
		}
		if !r.Assertion.LastActivityAt.Equal(waitNow) {
			t.Fatalf("reacquired lastActivityAt = %v, want %v", r.Assertion.LastActivityAt, waitNow)
		}
		if e.HasParked() {
			t.Fatal("expected nothing parked")
		}
	})

	t.Run("sleep releases immediately and reacquires on busy", func(t *testing.T) {
		e := NewSessionWaitEvaluator()
		a := sessionAssertion("s1", nil)

		actions := evaluate(e, []model.Assertion{a}, []ClaudeSessionStatus{status("s1", ActivityWaiting, "input needed")}, policy(settings.WaitSleep))
		expectActions(t, actions,
			SetWaitingFor{Key: a.Key, Label: "input needed"},
			ReleaseWaiting{Key: a.Key},
		)

		resumed := evaluate(e, nil, []ClaudeSessionStatus{status("s1", ActivityBusy, "")}, policy(settings.WaitSleep))
		if len(resumed) == 0 {
			t.Fatal("expected reacquire, got nothing")
		}
		if _, ok := resumed[0].(Reacquire); !ok {
			t.Fatalf("expected reacquire, got %s", fmtActions(resumed))
		}
	})

	// keepAwake still marks the wait ("waiting for you") but never touches the hold.
	t.Run("keepAwake only marks", func(t *testing.T) {
		e := NewSessionWaitEvaluator()
		a := sessionAssertion("s1", nil)

		actions := evaluate(e, []model.Assertion{a}, []ClaudeSessionStatus{status("s1", ActivityWaiting, "")}, policy(settings.WaitKeepAwake))
		expectActions(t, actions, SetWaitingFor{Key: a.Key, Label: "input needed"})
		if e.HasParked() {
			t.Fatal("expected nothing parked")
		}

		cleared := evaluate(e, []model.Assertion{a}, []ClaudeSessionStatus{status("s1", ActivityBusy, "")}, policy(settings.WaitKeepAwake))
		expectActions(t, cleared, SetWaitingFor{Key: a.Key})
	})

	// waiting → idle (the user's answer ended the whole turn): restore, never re-acquire.
	t.Run("idle resolves a parked wait without reacquiring", func(t *testing.T) {
		e := NewSessionWaitEvaluator()
		a := sessionAssertion("s1", nil)
		evaluate(e, []model.Assertion{a}, []ClaudeSessionStatus{status("s1", ActivityWaiting, "")})

		live := evaluate(e, []model.Assertion{a}, []ClaudeSessionStatus{status("s1", ActivityIdle, "")})
		expectContains(t, live, Restore{Key: a.Key, Expiry: nil})

		// Same but the hold was already gone (sleep policy / TTL): the entry just drops.
		e2 := NewSessionWaitEvaluator()
		evaluate(e2, []model.Assertion{a}, []ClaudeSessionStatus{status("s1", ActivityWaiting, "")}, policy(settings.WaitSleep))
		expectActions(t, evaluate(e2, nil, []ClaudeSessionStatus{status("s1", ActivityIdle, "")}, policy(settings.WaitSleep)))
		if e2.HasParked() {
			t.Fatal("expected nothing parked")
		}
	})

	// A dead session (pid gone, or its file swept) can never trigger a re-acquire — the normal
	// nets own that hold's fate, and this evaluator stands down.
	t.Run("dead or vanished sessions drop parked state", func(t *testing.T) {
		e := NewSessionWaitEvaluator()
		a := sessionAssertion("s1", nil)
		evaluate(e, []model.Assertion{a}, []ClaudeSessionStatus{status("s1", ActivityWaiting, "")}, policy(settings.WaitSleep))

		expectActions(t, evaluate(e, nil, nil))
		if e.HasParked() {
			t.Fatal("expected nothing parked")
		}

		// Dead pid variant: the status exists but its process does not.
		e2 := NewSessionWaitEvaluator()
		evaluate(e2, []model.Assertion{a}, []ClaudeSessionStatus{status("s1", ActivityWaiting, "")}, policy(settings.WaitSleep))
		actions := e2.Evaluate(nil, []ClaudeSessionStatus{status("s1", ActivityBusy, "")}, waitNow,
			WaitConfig{Policy: settings.WaitSleep, Grace: 600 * time.Second}, func(int) bool { return false })
		expectActions(t, actions)
		if e2.HasParked() {
			t.Fatal("expected nothing parked")
		}
	})

	// A parked hold whose session file vanishes while the hold is still live: restore and stand
	// down rather than leaving a grace TTL armed with nothing to ever clear it.
	t.Run("vanished status restores a live parked hold", func(t *testing.T) {
		e := NewSessionWaitEvaluator()
		a := sessionAssertion("s1", nil)
		evaluate(e, []model.Assertion{a}, []ClaudeSessionStatus{status("s1", ActivityWaiting, "")})

		actions := evaluate(e, []model.Assertion{a}, nil)
		expectContains(t, actions, Restore{Key: a.Key, Expiry: nil})
		expectContains(t, actions, SetWaitingFor{Key: a.Key})
		if e.HasParked() {
			t.Fatal("expected nothing parked")
		}
	})

	// Everything that is not a per-turn Claude Code session hold is out of scope: manual holds,
	// other agents, sub-agent / background-shell keys with no matching session file.
	t.Run("only claude code session holds are touched", func(t *testing.T) {
		e := NewSessionWaitEvaluator()
		base := sessionAssertion("s1", nil)
		ttl := 3600 * time.Second
		manual := model.New(base.Key, base.Tool, "", base.PID, base.ProcessName, base.AcquiredAt, &ttl, model.OriginManual)
		cursor := model.New("cursor:s2", "cursor", "", 600, "cursor", waitNow, nil, model.OriginHook)
		subagent := sessionAssertion("agent-abc123", nil)
		bgShell := sessionAssertion("bg-39750244", nil)

		actions := evaluate(e,
			[]model.Assertion{manual, cursor, subagent, bgShell},
			[]ClaudeSessionStatus{status("s1", ActivityWaiting, ""), status("s2", ActivityWaiting, "")},
		)
		expectActions(t, actions)
		if e.HasParked() {
			t.Fatal("expected nothing parked")
		}
	})

	// The waiting label follows the dialog: a permission prompt replacing a question re-labels
	// the same wait without re-parking it.
	t.Run("label changes re-mark without re-parking", func(t *testing.T) {
		e := NewSessionWaitEvaluator()
		a := sessionAssertion("s1", nil)
		evaluate(e, []model.Assertion{a}, []ClaudeSessionStatus{status("s1", ActivityWaiting, "input needed")})

		relabeled := evaluate(e, []model.Assertion{a}, []ClaudeSessionStatus{status("s1", ActivityWaiting, "approve Bash")})
		expectActions(t, relabeled, SetWaitingFor{Key: a.Key, Label: "approve Bash"})
	})
}

// Go-specific behavior.
func TestSessionWaitEvaluatorGo(t *testing.T) {
	t.Run("zero-value evaluator is usable", func(t *testing.T) {
		var e SessionWaitEvaluator
		a := sessionAssertion("s1", nil)
		actions := evaluate(&e, []model.Assertion{a}, []ClaudeSessionStatus{status("s1", ActivityWaiting, "")})
		if len(actions) != 2 || !e.HasParked() {
			t.Fatalf("actions = %s", fmtActions(actions))
		}
	})

	t.Run("clone is independent of the original", func(t *testing.T) {
		e := NewSessionWaitEvaluator()
		a := sessionAssertion("s1", nil)
		evaluate(e, []model.Assertion{a}, []ClaudeSessionStatus{status("s1", ActivityWaiting, "")})
		c := e.Clone()
		evaluate(c, []model.Assertion{a}, []ClaudeSessionStatus{status("s1", ActivityBusy, "")})
		if c.HasParked() || !e.HasParked() {
			t.Fatalf("clone parked=%v original parked=%v", c.HasParked(), e.HasParked())
		}
	})

	t.Run("reacquires of several parked holds come out in key order", func(t *testing.T) {
		e := NewSessionWaitEvaluator()
		keys := []string{"s3", "s1", "s2"}
		var as []model.Assertion
		var waiting, busy []ClaudeSessionStatus
		for _, k := range keys {
			as = append(as, sessionAssertion(k, nil))
			waiting = append(waiting, status(k, ActivityWaiting, ""))
			busy = append(busy, status(k, ActivityBusy, ""))
		}
		evaluate(e, as, waiting, policy(settings.WaitSleep))
		got := evaluate(e, nil, busy, policy(settings.WaitSleep))
		var order []string
		for _, a := range got {
			order = append(order, a.(Reacquire).Assertion.Key)
		}
		if fmt.Sprint(order) != "[claude-code:s1 claude-code:s2 claude-code:s3]" {
			t.Fatalf("order = %v", order)
		}
	})

	t.Run("a missing origin counts as a hook hold", func(t *testing.T) {
		e := NewSessionWaitEvaluator()
		a := sessionAssertion("s1", nil)
		a.Origin = ""
		actions := evaluate(e, []model.Assertion{a}, []ClaudeSessionStatus{status("s1", ActivityWaiting, "")}, policy(settings.WaitSleep))
		expectContains(t, actions, ReleaseWaiting{Key: a.Key})
	})

	t.Run("a bare prefix key is not a session hold", func(t *testing.T) {
		e := NewSessionWaitEvaluator()
		a := sessionAssertion("", nil)
		actions := evaluate(e, []model.Assertion{a}, []ClaudeSessionStatus{status("", ActivityWaiting, "")})
		expectActions(t, actions)
	})
}
