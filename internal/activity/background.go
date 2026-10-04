package activity

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"
)

// Background-shell hold (opt-in, Claude Code). Agents and sub-agents bracket their work with a
// hook pair; what remains is a shell command an agent launches with `run_in_background: true`.
// Such a command keeps running after the turn's Stop, and neither the parent nor any sub-agent
// fires a completion hook for it — there is only the PreToolUse that starts it. Two consequences:
//
//   - TTL-bounded, not idle-released. With no end hook the hold can only end on a deadline, and
//     the CPU-idle net will not do it either: while the agent serves other turns its process tree
//     is not idle. The TTL is the requested one (the installed hook passes `--ttl <ceiling>`), else
//     BackgroundDefaultTTL, which the daemon clamps to the live manualHoldMaxHours — so the
//     effective TTL tracks the user's configured cap rather than a value baked in at install time.
//   - Per-invocation key. Each background command holds independently (`bg-<id>` in the session
//     slot of `<tool>:<key>`), so overlapping background tasks cannot release each other's hold.
//
// Claude Code only: Codex models background shells as a PTY yield with no single pre-tool boolean
// to key on.

// BackgroundKeyInfix namespaces a background-shell hold within the session slot of its key, so it
// reads `<tool>:bg-<id>` and never collides with a per-turn `<tool>:<session_id>`.
const BackgroundKeyInfix = "bg-"

// BackgroundDefaultTTL is requested when the hook carries no explicit TTL: the CLI's 24 h
// absolute ceiling, so the daemon's live manualHoldMaxHours clamp is what governs the duration.
const BackgroundDefaultTTL = 24 * time.Hour

// BackgroundPlan is a background-shell hold to place.
type BackgroundPlan struct {
	// Key is the registry key, `<tool>:bg-<id>`, unique per invocation.
	Key string
	// TTL is the requested TTL; the daemon clamps it down to manualHoldMaxHours.
	TTL time.Duration
}

// PlanBackgroundHold is the decision for `acquire --if-background`, given the hook's PreToolUse
// stdin payload. ok is false when the tool call is not run_in_background — the common path (every
// foreground Bash call, every non-Bash PreToolUse), where the CLI places no hold and exits 0.
// Otherwise the plan carries a per-invocation `bg-` key built from uniqueID (production passes
// FreshBackgroundID, tests a fixed one) and requestedTTL, or BackgroundDefaultTTL when nil.
func PlanBackgroundHold(payload []byte, tool string, requestedTTL *time.Duration, uniqueID string) (BackgroundPlan, bool) {
	if !runInBackground(payload) {
		return BackgroundPlan{}, false
	}
	ttl := BackgroundDefaultTTL
	if requestedTTL != nil {
		ttl = *requestedTTL
	}
	return BackgroundPlan{Key: hookSessionKey(tool, BackgroundKeyInfix+uniqueID), TTL: ttl}, true
}

// FreshBackgroundID is a fresh per-invocation id: 8 lowercase hex characters, as brief as a
// manual hold key.
func FreshBackgroundID() string {
	var b [4]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails
	return hex.EncodeToString(b[:])
}

// runInBackground reports whether a PreToolUse payload carries `tool_input.run_in_background ==
// true`. False when the field is absent or not a boolean, tool_input is not an object, or the
// payload is not a JSON object — so a foreground Bash call or any other tool places no hold.
func runInBackground(payload []byte) bool {
	var obj, toolInput map[string]json.RawMessage
	if json.Unmarshal(payload, &obj) != nil || json.Unmarshal(obj["tool_input"], &toolInput) != nil || toolInput == nil {
		return false
	}
	var flag bool
	return json.Unmarshal(toolInput["run_in_background"], &flag) == nil && flag
}

// hookSessionKey derives the registry key of a hook hold exactly as the CLI's acquire and release
// do (`<tool>:<id>`, with the same passthrough of ids that already are full keys), so a
// background-shell hold lands in the same key space as every other hook hold.
func hookSessionKey(tool, sessionID string) string {
	const holdKeyPrefix, unknownTool = "hold:", "unknown"
	if strings.HasPrefix(sessionID, holdKeyPrefix) || strings.HasPrefix(sessionID, tool+":") {
		return sessionID
	}
	if tool == unknownTool && strings.Contains(sessionID, ":") {
		return sessionID
	}
	return tool + ":" + sessionID
}
