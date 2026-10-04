package agents

import "encoding/json"

// Hook payload fields. Every Claude-Code-style hook system (Claude Code, Codex, Gemini CLI,
// Cursor) invokes the hook command with a JSON object on stdin. Two identity fields matter:
//
//   - session_id — the parent session/turn id, present on every hook. UserPromptSubmit/Stop key
//     their per-turn hold on it.
//   - agent_id — a sub-agent's own id, present only on SubagentStart/SubagentStop. The parent's
//     session_id is also on those payloads, so a sub-agent hook keyed on session_id would collide
//     with — and, being idempotent by key, wrongly release — the parent's turn hold. Keying the
//     sub-agent hold on agent_id gives it a distinct key that survives the parent Stop and is
//     released only by its own SubagentStop.
//
// Claude Code and Codex both name the field agent_id, so one read covers both.
const (
	// SessionIDField is the parent-session field on every hook payload.
	SessionIDField = "session_id"
	// AgentIDField is the sub-agent-id field on SubagentStart/SubagentStop payloads.
	AgentIDField = "agent_id"
	// ToolInputField nests a tool call's raw input arguments on a PreToolUse payload.
	ToolInputField = "tool_input"
	// RunInBackgroundField is the Bash tool's flag that launches a command in the background.
	RunInBackgroundField = "run_in_background"
)

// SessionID is the parent session_id from a hook payload; ok is false when the bytes aren't a
// JSON object with a non-empty string at that key.
func SessionID(payload []byte) (string, bool) {
	return StringField(SessionIDField, payload)
}

// AgentID is the sub-agent's own agent_id from a SubagentStart/SubagentStop payload; ok is false
// when the bytes aren't a JSON object with a non-empty string at that key (e.g. a non-sub-agent
// hook).
func AgentID(payload []byte) (string, bool) {
	return StringField(AgentIDField, payload)
}

// RunInBackground reports whether a PreToolUse Bash payload carries
// tool_input.run_in_background == true.
//
// tool_input is the raw argument object the model passed the tool; the Bash tool declares
// run_in_background as an optional boolean, so when the model sets it, it lands here. False when
// the field is absent or not a JSON boolean, tool_input isn't a nested object, or the payload
// isn't a JSON object at all — so a foreground Bash call, or any non-Bash tool, reads as "not
// background" and places no hold. Only Claude Code emits this cleanly; Codex models background
// shells as a separate PTY yield with no equivalent pre-tool boolean.
func RunInBackground(payload []byte) bool {
	var obj map[string]json.RawMessage
	if json.Unmarshal(payload, &obj) != nil {
		return false
	}
	var toolInput map[string]json.RawMessage
	if json.Unmarshal(obj[ToolInputField], &toolInput) != nil {
		return false
	}
	var flag bool
	if json.Unmarshal(toolInput[RunInBackgroundField], &flag) != nil {
		return false
	}
	return flag
}

// StringField is the non-empty string value at field in a top-level JSON object. An empty string
// reads as absent — a hook whose expansion came up empty must yield "no id", not the id "".
func StringField(field string, payload []byte) (string, bool) {
	var obj map[string]json.RawMessage
	if json.Unmarshal(payload, &obj) != nil {
		return "", false
	}
	var s string
	if json.Unmarshal(obj[field], &s) != nil || s == "" {
		return "", false
	}
	return s, true
}
