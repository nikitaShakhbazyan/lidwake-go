package agents_test

import (
	"testing"

	"github.com/nikitaShakhbazyan/lidwake-go/internal/agents"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/model"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/policy"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/registry"
)

// The core of the sub-agent keep-awake rule: a sub-agent hold is keyed on the sub-agent's own
// agent_id, giving it a key distinct from the parent turn's `<tool>:<session_id>` hold. A
// foreground sub-agent's start+stop is then net-neutral on the parent hold, and a backgrounded
// sub-agent's hold survives the parent Stop and is released only by its own SubagentStop.
//
// The registry is driven directly (it is the daemon's source of truth for IsBlocking), so these
// exercise the real reference counting. The ids come out of real hook payloads through
// agents.SessionID/AgentID, and the keys are derived with policy.SessionKey exactly as the CLI's
// acquire and release derive them.

// keysFromPayloads derives the parent key from a UserPromptSubmit payload and the sub-agent key
// from a SubagentStart payload that, as real ones do, carries the parent's session_id too.
func keysFromPayloads(t *testing.T, tool, session, agentID string) (parent, sub string) {
	t.Helper()
	prompt := []byte(`{"hook_event_name":"UserPromptSubmit","session_id":"` + session + `"}`)
	start := []byte(`{"hook_event_name":"SubagentStart","session_id":"` + session + `","agent_id":"` + agentID + `"}`)
	sid, ok := agents.SessionID(prompt)
	if !ok {
		t.Fatal("no session_id")
	}
	aid, ok := agents.AgentID(start)
	if !ok {
		t.Fatal("no agent_id")
	}
	return policy.SessionKey(tool, sid), policy.SessionKey(tool, aid)
}

func hookAssertion(key, tool string) model.Assertion {
	return model.Assertion{Key: key, Tool: tool, PID: 0, ProcessName: tool, Origin: model.OriginHook}
}

func TestSubagentHold(t *testing.T) {
	t.Run("a subagent hold is a distinct key from the parent session hold", func(t *testing.T) {
		// SubagentStart/Stop emit the parent's session_id and the sub-agent's own agent_id.
		// Keying on agent_id (not session_id) is what makes the two holds independent.
		parent, sub := keysFromPayloads(t, "claude-code", "sess-1", "agent-A")
		if parent != "claude-code:sess-1" || sub != "claude-code:agent-A" {
			t.Fatalf("parent = %q, sub = %q", parent, sub)
		}
		if parent == sub {
			t.Fatal("sub-agent hold must not collide with the parent turn's hold")
		}
	})

	t.Run("two subagents of the same parent get distinct keys", func(t *testing.T) {
		_, a := keysFromPayloads(t, "codex", "sess-1", "agent-A")
		_, b := keysFromPayloads(t, "codex", "sess-1", "agent-B")
		if a == b {
			t.Fatal("each sub-agent tracks its own hold, released by its own SubagentStop")
		}
	})

	// A foreground sub-agent runs and finishes within the parent turn: SubagentStart → acquire,
	// SubagentStop → release, both before the parent's Stop. Because the sub-agent hold is a
	// distinct key, its start+stop is net-neutral on the parent hold.
	t.Run("a foreground subagent start and stop leaves the parent hold intact", func(t *testing.T) {
		r := registry.New(nil)
		t.Cleanup(r.Close)
		parent, sub := keysFromPayloads(t, "claude-code", "sess-1", "agent-A")

		r.Acquire(hookAssertion(parent, "claude-code"))
		r.Acquire(hookAssertion(sub, "claude-code"))
		if !r.IsBlocking() || r.Count() != 2 {
			t.Fatalf("blocking = %v, count = %d", r.IsBlocking(), r.Count())
		}

		r.Release(sub)
		if !r.IsBlocking() {
			t.Fatal("parent's turn hold must survive the sub-agent's Stop")
		}
		if r.Count() != 1 {
			t.Fatalf("count = %d", r.Count())
		}

		r.Release(parent)
		if r.IsBlocking() {
			t.Fatal("Mac can sleep once the parent turn ends")
		}
	})

	// A backgrounded sub-agent keeps running after the parent turn's Stop. The parent Stop
	// releases the parent key, but the sub-agent hold — a distinct key — remains, so the Mac
	// stays awake until the sub-agent's own SubagentStop.
	t.Run("a backgrounded subagent keeps the Mac awake past the parent Stop", func(t *testing.T) {
		r := registry.New(nil)
		t.Cleanup(r.Close)
		parent, sub := keysFromPayloads(t, "codex", "sess-1", "agent-A")

		r.Acquire(hookAssertion(parent, "codex"))
		r.Acquire(hookAssertion(sub, "codex"))
		if !r.IsBlocking() {
			t.Fatal("not blocking")
		}

		r.Release(parent)
		if !r.IsBlocking() {
			t.Fatal("backgrounded sub-agent must keep the Mac awake past Stop")
		}
		if r.Count() != 1 {
			t.Fatalf("count = %d", r.Count())
		}

		r.Release(sub)
		if r.IsBlocking() {
			t.Fatal("Mac sleeps once the backgrounded sub-agent finishes")
		}
	})
}
