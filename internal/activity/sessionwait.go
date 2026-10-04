package activity

import (
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/nikitaShakhbazyan/lidwake-go/internal/model"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/settings"
)

// claudeCodeKeyPrefix is the registry key prefix of a per-turn Claude Code hold
// (`claude-code:<session id>`).
const claudeCodeKeyPrefix = "claude-code:"

// defaultWaitingLabel marks a wait whose status file names no dialog.
const defaultWaitingLabel = "input needed"

// WaitConfig is the user's waiting policy.
type WaitConfig struct {
	Policy settings.WaitingPolicy
	// Grace is how long WaitGrace keeps a waiting session's hold.
	Grace time.Duration
}

// WaitAction is one decision of SessionWaitEvaluator for the daemon to apply to the registry:
// SetWaitingFor, Park, ReleaseWaiting, Restore or Reacquire.
type WaitAction interface {
	waitAction()
}

// SetWaitingFor stamps (Label non-empty) or clears (Label "") a hold's WaitingFor mark. UI only;
// every policy emits these.
type SetWaitingFor struct {
	Key   string
	Label string
}

// Park arms the grace TTL on a live hold (WaitGrace). ExpiresAt is already the earlier of the
// grace deadline and any existing expiry.
type Park struct {
	Key       string
	ExpiresAt time.Time
}

// ReleaseWaiting releases the hold now (WaitSleep).
type ReleaseWaiting struct {
	Key string
}

// Restore puts a live hold's pre-park expiry back (nil: no expiry) — its wait resolved.
type Restore struct {
	Key    string
	Expiry *time.Time
}

// Reacquire re-acquires a hold that was fully released while its session waited (the grace TTL
// ran out, or WaitSleep): the session is busy again and the resumed turn needs protection.
type Reacquire struct {
	Assertion model.Assertion
}

func (SetWaitingFor) waitAction()  {}
func (Park) waitAction()           {}
func (ReleaseWaiting) waitAction() {}
func (Restore) waitAction()        {}
func (Reacquire) waitAction()      {}

type parkedHold struct {
	assertion      model.Assertion
	originalExpiry *time.Time
}

// SessionWaitEvaluator decides what happens to Claude Code session holds while their sessions
// wait for the user. A question, a plan approval or a permission prompt fires no end hook, so
// without this the hold stands until the CPU-idle sweep — the better part of an hour of blocked
// sleep.
//
// The waiting signal is ClaudeSessionStatus (the session's own status file); the policy is the
// user's WaitingPolicy. The evaluator holds the cross-sweep bookkeeping (which holds are parked,
// and what their expiry was before); the session-status monitor supplies the file reads, process
// probes and timer, and the daemon applies the returned actions.
//
// A parked hold is one this evaluator acted on for a wait that has not resolved: under WaitGrace
// it carries a grace TTL (and may since have been released by that TTL); under WaitSleep it was
// released outright. Parking is remembered so the moment the session turns busy again — answered
// at the keyboard, from a phone, or auto-continued by a timeout — the hold is restored or
// re-acquired, protecting the resumed turn that no hook announces.
//
// Not safe for concurrent use: the session-status monitor owns it.
type SessionWaitEvaluator struct {
	// parked: holds acted on for a still-unresolved wait, by assertion key.
	parked map[string]parkedHold
	// waitingMarks: keys currently marked as waiting in the registry, with the label last set, so
	// marks are emitted on change and not on every sweep.
	waitingMarks map[string]string
}

// NewSessionWaitEvaluator returns an evaluator with nothing parked.
func NewSessionWaitEvaluator() *SessionWaitEvaluator {
	return &SessionWaitEvaluator{parked: map[string]parkedHold{}, waitingMarks: map[string]string{}}
}

func (e *SessionWaitEvaluator) init() {
	if e.parked == nil {
		e.parked = map[string]parkedHold{}
	}
	if e.waitingMarks == nil {
		e.waitingMarks = map[string]string{}
	}
}

// Clone returns an independent copy of the evaluator's state.
func (e *SessionWaitEvaluator) Clone() *SessionWaitEvaluator {
	c := NewSessionWaitEvaluator()
	maps.Copy(c.parked, e.parked)
	maps.Copy(c.waitingMarks, e.waitingMarks)
	return c
}

// HasParked reports whether any parked wait is outstanding. The monitor keeps sweeping while true
// even with nothing blocking: a released hold's session must still be seen turning busy.
func (e *SessionWaitEvaluator) HasParked() bool {
	return len(e.parked) > 0
}

// Evaluate runs one sweep. assertions is the live registry snapshot, statuses every parsed status
// file. Holds whose key has no matching session (sub-agent ids, background-shell holds, other
// agents, manual holds) are left strictly alone.
func (e *SessionWaitEvaluator) Evaluate(assertions []model.Assertion, statuses []ClaudeSessionStatus, now time.Time, cfg WaitConfig, pidAlive func(pid int) bool) []WaitAction {
	e.init()
	now = now.Round(0) // wall clock: a park deadline is persisted and must survive sleep
	var actions []WaitAction
	byKey := make(map[string]bool, len(assertions))
	for _, a := range assertions {
		byKey[a.Key] = true
	}

	for _, a := range assertions {
		sessionID, ok := sessionIDOfKey(a.Key)
		if !ok || !isHookOrigin(a.Origin) {
			continue
		}
		status, ok := BestSessionStatus(sessionID, statuses, pidAlive)
		if !ok {
			// No live status (file gone, custom config dir, pid dead): stand down from anything
			// this evaluator did and leave the hold to the normal nets.
			if entry, ok := e.parked[a.Key]; ok {
				delete(e.parked, a.Key)
				actions = append(actions, Restore{Key: a.Key, Expiry: copyTime(entry.originalExpiry)})
			}
			actions = e.clearMark(a.Key, actions)
			continue
		}

		switch status.Activity {
		case ActivityWaiting:
			label := status.WaitingFor
			if label == "" {
				label = defaultWaitingLabel
			}
			actions = e.setMark(a.Key, label, actions)
			if _, already := e.parked[a.Key]; already {
				break
			}
			switch cfg.Policy {
			case settings.WaitGrace:
				graceExpiry := now.Add(cfg.Grace)
				if a.ExpiresAt != nil && a.ExpiresAt.Before(graceExpiry) {
					graceExpiry = *a.ExpiresAt
				}
				e.parked[a.Key] = parkedHold{assertion: a, originalExpiry: copyTime(a.ExpiresAt)}
				actions = append(actions, Park{Key: a.Key, ExpiresAt: graceExpiry})
			case settings.WaitSleep:
				e.parked[a.Key] = parkedHold{assertion: a, originalExpiry: copyTime(a.ExpiresAt)}
				actions = append(actions, ReleaseWaiting{Key: a.Key})
			default: // WaitKeepAwake: only the mark
			}
		default: // busy or idle
			// The wait resolved (busy), or the turn ended entirely (idle — the Stop hook releases
			// the hold on its own). Either way, put the expiry back and stand down.
			actions = e.clearMark(a.Key, actions)
			if entry, ok := e.parked[a.Key]; ok {
				delete(e.parked, a.Key)
				actions = append(actions, Restore{Key: a.Key, Expiry: copyTime(entry.originalExpiry)})
			}
		}
	}

	// Parked holds that no longer exist: released by WaitSleep, or the grace TTL ran out and the
	// idle sweep collected it. Wait for the session to move. Sorted so the actions are
	// deterministic.
	for _, key := range slices.Sorted(maps.Keys(e.parked)) {
		if byKey[key] {
			continue
		}
		entry := e.parked[key]
		sessionID, ok := sessionIDOfKey(key)
		var status ClaudeSessionStatus
		if ok {
			status, ok = BestSessionStatus(sessionID, statuses, pidAlive)
		}
		if !ok {
			delete(e.parked, key) // the session (or its file) is gone: nothing to resume
			continue
		}
		switch status.Activity {
		case ActivityWaiting:
			// Still parked; the Mac may sleep meanwhile.
		case ActivityBusy:
			// The turn resumed with no hook to announce it: re-acquire the remembered hold, with
			// its pre-park expiry and no stale waiting mark.
			delete(e.parked, key)
			a := entry.assertion
			a.ExpiresAt = copyTime(entry.originalExpiry)
			a.WaitingFor = ""
			a.LastActivityAt = now
			actions = append(actions, Reacquire{Assertion: a})
		default: // idle: the turn is over, nothing to protect
			delete(e.parked, key)
		}
	}

	// Marks for keys that vanished from the registry need no clearing action — the mark lived on
	// the assertion and died with it. Just forget the bookkeeping.
	for key := range e.waitingMarks {
		if !byKey[key] {
			delete(e.waitingMarks, key)
		}
	}
	return actions
}

// sessionIDOfKey is the session id a per-turn Claude Code hold is keyed on; ok is false for every
// other key shape. (Sub-agent and background-shell holds share the prefix, but their suffixes
// never match a status file's sessionId, so they fall out at the status lookup.)
func sessionIDOfKey(key string) (string, bool) {
	suffix, ok := strings.CutPrefix(key, claudeCodeKeyPrefix)
	if !ok || suffix == "" {
		return "", false
	}
	return suffix, true
}

// isHookOrigin treats a missing origin as a hook, the default for an assertion.
func isHookOrigin(o model.Origin) bool {
	return o == model.OriginHook || o == ""
}

func (e *SessionWaitEvaluator) setMark(key, label string, actions []WaitAction) []WaitAction {
	if cur, ok := e.waitingMarks[key]; ok && cur == label {
		return actions
	}
	e.waitingMarks[key] = label
	return append(actions, SetWaitingFor{Key: key, Label: label})
}

func (e *SessionWaitEvaluator) clearMark(key string, actions []WaitAction) []WaitAction {
	if _, ok := e.waitingMarks[key]; !ok {
		return actions
	}
	delete(e.waitingMarks, key)
	return append(actions, SetWaitingFor{Key: key})
}

func copyTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	c := *t
	return &c
}
