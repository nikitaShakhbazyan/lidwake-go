package activity

import (
	"testing"
	"time"
)

// The pure acquire decision behind `acquire --if-background`: whether to place a hold for a
// PreToolUse payload, and with what key and TTL, exercised against crafted payloads with a fixed
// uniqueID for determinism.

func backgroundPayload() []byte {
	return []byte(`{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"npm run build","run_in_background":true}}`)
}

func durPtr(d time.Duration) *time.Duration { return &d }

func TestBackgroundBashHold(t *testing.T) {
	// MARK: Background → place a hold

	t.Run("a backgrounded command yields a bg-keyed plan", func(t *testing.T) {
		plan, ok := PlanBackgroundHold(backgroundPayload(), "claude-code", nil, "abc123")
		// Keyed <tool>:bg-<id> — namespaced so it never collides with a per-turn <tool>:<session_id>.
		if !ok || plan.Key != "claude-code:bg-abc123" {
			t.Fatalf("plan = %+v, %v", plan, ok)
		}
	})

	t.Run("a plan without an explicit TTL uses the default ceiling", func(t *testing.T) {
		plan, ok := PlanBackgroundHold(backgroundPayload(), "claude-code", nil, "x")
		// The default is the 24h ceiling; the daemon clamps it down to the live max-hold.
		if !ok || plan.TTL != BackgroundDefaultTTL {
			t.Fatalf("plan = %+v, %v", plan, ok)
		}
		if BackgroundDefaultTTL != 24*60*60*time.Second {
			t.Fatalf("BackgroundDefaultTTL = %v", BackgroundDefaultTTL)
		}
	})

	t.Run("an explicit TTL passes through the plan", func(t *testing.T) {
		// The installed hook always carries --ttl; the daemon, not this decision, applies the cap.
		plan, ok := PlanBackgroundHold(backgroundPayload(), "claude-code", durPtr(3600*time.Second), "x")
		if !ok || plan.TTL != 3600*time.Second {
			t.Fatalf("plan = %+v, %v", plan, ok)
		}
	})

	t.Run("each invocation keys on its own id so overlapping tasks are independent", func(t *testing.T) {
		a, _ := PlanBackgroundHold(backgroundPayload(), "claude-code", nil, "a")
		b, _ := PlanBackgroundHold(backgroundPayload(), "claude-code", nil, "b")
		if a.Key == b.Key {
			t.Fatal("two background tasks must not share — and thus can't release — one hold")
		}
	})

	// MARK: Not background → no hold

	t.Run("a foreground command yields no plan", func(t *testing.T) {
		payload := []byte(`{"tool_name":"Bash","tool_input":{"command":"ls","run_in_background":false}}`)
		if plan, ok := PlanBackgroundHold(payload, "claude-code", nil, "x"); ok {
			t.Fatalf("plan = %+v", plan)
		}
	})

	t.Run("an absent flag yields no plan", func(t *testing.T) {
		payload := []byte(`{"tool_name":"Bash","tool_input":{"command":"ls"}}`)
		if plan, ok := PlanBackgroundHold(payload, "claude-code", nil, "x"); ok {
			t.Fatalf("plan = %+v", plan)
		}
	})

	t.Run("a malformed or empty payload yields no plan", func(t *testing.T) {
		if plan, ok := PlanBackgroundHold([]byte("not json"), "claude-code", nil, "x"); ok {
			t.Fatalf("plan = %+v", plan)
		}
		if plan, ok := PlanBackgroundHold(nil, "claude-code", nil, "x"); ok {
			t.Fatalf("plan = %+v", plan)
		}
	})

	// MARK: Fresh id

	t.Run("freshID is short and unique", func(t *testing.T) {
		id := FreshBackgroundID()
		if len(id) != 8 {
			t.Fatalf("id = %q", id)
		}
		if FreshBackgroundID() == FreshBackgroundID() {
			t.Fatal("two fresh ids collided")
		}
	})
}

// Go-specific behavior.
func TestBackgroundBashHoldGo(t *testing.T) {
	t.Run("non-boolean flags and non-object tool_input yield no plan", func(t *testing.T) {
		for _, in := range []string{
			`{"tool_input":{"run_in_background":"true"}}`,
			`{"tool_input":{"run_in_background":1}}`,
			`{"tool_input":{"run_in_background":null}}`,
			`{"tool_input":"run_in_background"}`,
			`{"tool_input":null}`,
			`{"TOOL_INPUT":{"run_in_background":true}}`,
			`[{"tool_input":{"run_in_background":true}}]`,
			`null`,
		} {
			if plan, ok := PlanBackgroundHold([]byte(in), "claude-code", nil, "x"); ok {
				t.Errorf("PlanBackgroundHold(%s) = %+v", in, plan)
			}
		}
	})

	t.Run("fresh ids are lowercase hex", func(t *testing.T) {
		for range 20 {
			for _, c := range FreshBackgroundID() {
				if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
					t.Fatalf("non-hex rune %q", c)
				}
			}
		}
	})

	t.Run("key derivation matches the hook key space", func(t *testing.T) {
		cases := []struct{ tool, id, want string }{
			{"claude-code", "bg-1", "claude-code:bg-1"},
			{"claude-code", "claude-code:bg-1", "claude-code:bg-1"},
			{"claude-code", "hold:abcd", "hold:abcd"},
			{"unknown", "x:bg-1", "x:bg-1"},
			{"codex", "x:bg-1", "codex:x:bg-1"},
		}
		for _, c := range cases {
			if got := hookSessionKey(c.tool, c.id); got != c.want {
				t.Errorf("hookSessionKey(%q, %q) = %q, want %q", c.tool, c.id, got, c.want)
			}
		}
	})
}
