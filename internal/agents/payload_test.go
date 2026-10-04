package agents

import "testing"

// The pure field extraction behind Stdin.SessionID/AgentID: which field each reads, and the
// empty-string rejection, against crafted payloads.
func TestHookPayload(t *testing.T) {
	b := func(s string) []byte { return []byte(s) }

	t.Run("sessionID reads the session_id field", func(t *testing.T) {
		if got, ok := SessionID(b(`{"session_id":"sess-123"}`)); !ok || got != "sess-123" {
			t.Fatalf("got %q, %v", got, ok)
		}
	})

	t.Run("sessionID ignores an agent_id-only payload", func(t *testing.T) {
		// A payload carrying only agent_id proves the fields are read independently.
		if got, ok := SessionID(b(`{"agent_id":"agt-1"}`)); ok {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("agentID reads the agent_id field", func(t *testing.T) {
		// The SubagentStart/SubagentStop shape: the parent's session_id is present, but AgentID
		// must read the sub-agent's own agent_id.
		payload := `{"hook_event_name":"SubagentStart","session_id":"parent-sess","agent_id":"sub-agent-9","agent_type":"general"}`
		if got, ok := AgentID(b(payload)); !ok || got != "sub-agent-9" {
			t.Fatalf("got %q, %v", got, ok)
		}
	})

	t.Run("agentID is distinct from the parent session_id in the same payload", func(t *testing.T) {
		// Both fields present (as SubagentStart/Stop always send): the two pull apart exactly —
		// this separation keeps the sub-agent hold from clobbering the parent's.
		payload := b(`{"session_id":"parent","agent_id":"child"}`)
		if got, ok := AgentID(payload); !ok || got != "child" {
			t.Fatalf("agent: got %q, %v", got, ok)
		}
		if got, ok := SessionID(payload); !ok || got != "parent" {
			t.Fatalf("session: got %q, %v", got, ok)
		}
	})

	t.Run("agentID is nil when agent_id is absent", func(t *testing.T) {
		// A non-sub-agent payload (UserPromptSubmit) has session_id but no agent_id.
		if got, ok := AgentID(b(`{"session_id":"only-session"}`)); ok {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("an empty field value reads as absent", func(t *testing.T) {
		// A hook whose expansion came up empty must yield "no id", not the id "".
		if _, ok := AgentID(b(`{"agent_id":""}`)); ok {
			t.Fatal("empty agent_id accepted")
		}
		if _, ok := SessionID(b(`{"session_id":""}`)); ok {
			t.Fatal("empty session_id accepted")
		}
	})

	t.Run("non-object and non-string values are nil", func(t *testing.T) {
		for _, p := range []string{"not json", "[1,2,3]", `{"agent_id":42}`, "", "null", `{"agent_id":null}`, `"agent_id"`} {
			if got, ok := AgentID(b(p)); ok {
				t.Errorf("AgentID(%q) = %q", p, got)
			}
		}
		if _, ok := AgentID(nil); ok {
			t.Error("AgentID(nil) accepted")
		}
	})

	t.Run("runInBackground is true for a backgrounded Bash call", func(t *testing.T) {
		// The real PreToolUse shape: tool_name + the raw tool_input carrying run_in_background.
		payload := `{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"npm run build","run_in_background":true}}`
		if !RunInBackground(b(payload)) {
			t.Fatal("false")
		}
	})

	t.Run("runInBackground is false for a foreground Bash call", func(t *testing.T) {
		if RunInBackground(b(`{"tool_name":"Bash","tool_input":{"command":"ls","run_in_background":false}}`)) {
			t.Fatal("true")
		}
	})

	t.Run("runInBackground is false when the flag is absent", func(t *testing.T) {
		// Claude Code omits run_in_background when the model doesn't set it — must read as "not
		// background", not fail or default to true.
		if RunInBackground(b(`{"tool_name":"Bash","tool_input":{"command":"ls"}}`)) {
			t.Fatal("true")
		}
	})

	t.Run("runInBackground is false when tool_input is missing or not an object", func(t *testing.T) {
		// A non-Bash tool, or a malformed payload with no nested tool_input, places no hold.
		for _, p := range []string{
			`{"tool_name":"Read"}`,
			`{"tool_input":"oops"}`,
			`{"tool_input":{"run_in_background":"true"}}`,
			`{"tool_input":{"run_in_background":1}}`,
			`{"tool_input":null}`,
			`{"tool_input":{"run_in_background":null}}`,
			"not json",
			"",
		} {
			if RunInBackground(b(p)) {
				t.Errorf("RunInBackground(%q) = true", p)
			}
		}
		if RunInBackground(nil) {
			t.Error("RunInBackground(nil) = true")
		}
	})
}
