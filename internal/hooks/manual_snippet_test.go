package hooks

import (
	"strings"
	"testing"
)

// TestManualHookSnippet: slug derivation and the exact commands shown for copy-paste.
func TestManualHookSnippet(t *testing.T) {
	slugCases := func(t *testing.T, cases map[string]string) {
		t.Helper()
		for in, want := range cases {
			if got := Slug(in); got != want {
				t.Errorf("Slug(%q) = %q, want %q", in, got, want)
			}
		}
	}

	// ---- Slug derivation ----

	t.Run("a plain name lowercases and hyphenates", func(t *testing.T) {
		slugCases(t, map[string]string{"My Agent": "my-agent", "Amp": "amp"})
	})

	t.Run("runs of separators collapse to a single hyphen", func(t *testing.T) {
		slugCases(t, map[string]string{"my   agent": "my-agent", "my___agent": "my-agent", "my - _ agent": "my-agent"})
	})

	t.Run("leading and trailing separators are trimmed", func(t *testing.T) {
		slugCases(t, map[string]string{"  Agent  ": "agent", "--agent--": "agent", "_agent_": "agent"})
	})

	t.Run("punctuation is stripped and acts as a boundary", func(t *testing.T) {
		slugCases(t, map[string]string{"Café Bot!": "caf-bot", "agent.v2": "agent-v2", "gpt/codex": "gpt-codex"})
	})

	t.Run("digits are preserved", func(t *testing.T) {
		slugCases(t, map[string]string{"Agent 3000": "agent-3000"})
	})

	t.Run("empty or all-stripped names fall back to the default", func(t *testing.T) {
		slugCases(t, map[string]string{"": FallbackSlug, "   ": FallbackSlug, "!!!": FallbackSlug, "日本語": FallbackSlug})
		if FallbackSlug != "my-agent" {
			t.Errorf("FallbackSlug = %q", FallbackSlug)
		}
	})

	// ---- Rendered snippets ----

	t.Run("hook snippets embed the slug and quote the session id", func(t *testing.T) {
		s := NewManualHookSnippet("My Agent")
		if s.Slug != "my-agent" {
			t.Errorf("slug = %q", s.Slug)
		}
		if got := s.Acquire(); got != `lidwake acquire "$SESSION_ID" --tool my-agent` {
			t.Errorf("acquire = %q", got)
		}
		if got := s.Release(); got != `lidwake release "$SESSION_ID" --tool my-agent` {
			t.Errorf("release = %q", got)
		}
	})

	t.Run("the wrapper acquires and releases on the same PID-keyed tool", func(t *testing.T) {
		w := NewManualHookSnippet("Amp").WrapperScript()
		wantContains(t, w, "lidwake acquire $$ --tool amp", "wrapper")
		wantContains(t, w, "lidwake release $$ --tool amp", "wrapper")
		if !strings.HasPrefix(w, "#!/bin/sh") {
			t.Error("wrapper must start with a shebang")
		}
		wantContains(t, w, "exit $status", "must preserve the wrapped command's exit code")
	})

	t.Run("the one-shot hold names the agent in its reason", func(t *testing.T) {
		// The reason uses the display name, not the slug, so the row reads naturally.
		if got := NewManualHookSnippet("My Agent").OneShotHold(); got != `lidwake hold --for 2h --pid $$ --reason "My Agent session"` {
			t.Errorf("one-shot = %q", got)
		}
	})

	t.Run("a blank name falls back to the slug for the reason too", func(t *testing.T) {
		s := NewManualHookSnippet("   ")
		if s.Slug != "my-agent" || s.Name != "my-agent" {
			t.Errorf("snippet = %+v", s)
		}
		wantContains(t, s.OneShotHold(), `--reason "my-agent session"`, "blank name")
	})

	// Go-specific: the full wrapper text, line for line.
	t.Run("the wrapper script text", func(t *testing.T) {
		want := "#!/bin/sh\n" +
			"# Keep your Mac awake while Amp runs, then let it sleep again.\n" +
			"lidwake acquire $$ --tool amp\n" +
			"<your-agent-command> \"$@\"\n" +
			"status=$?\n" +
			"lidwake release $$ --tool amp\n" +
			"exit $status"
		if got := NewManualHookSnippet(" Amp\n").WrapperScript(); got != want {
			t.Errorf("wrapper =\n%s\nwant\n%s", got, want)
		}
	})
}
