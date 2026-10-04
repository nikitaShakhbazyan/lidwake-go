package cli

import (
	"slices"
	"testing"
)

func TestArgParser(t *testing.T) {
	t.Run("positionals collected in order", func(t *testing.T) {
		p := ParseArgs([]string{"foo", "bar", "baz"})
		if !slices.Equal(p.Positionals, []string{"foo", "bar", "baz"}) {
			t.Fatalf("positionals %q", p.Positionals)
		}
		if v, ok := p.Positional(0); !ok || v != "foo" {
			t.Errorf("positional 0 = %q, %v", v, ok)
		}
		if v, ok := p.Positional(2); !ok || v != "baz" {
			t.Errorf("positional 2 = %q, %v", v, ok)
		}
		if v, ok := p.Positional(99); ok {
			t.Errorf("positional 99 = %q", v)
		}
		if _, ok := p.Positional(-1); ok {
			t.Error("negative index returned a value")
		}
	})

	t.Run("option with space separated value", func(t *testing.T) {
		p := ParseArgs([]string{"--tool", "claude-code"})
		if v, ok := p.Option("--tool"); !ok || v != "claude-code" {
			t.Errorf("--tool = %q, %v", v, ok)
		}
	})

	t.Run("option with equals syntax", func(t *testing.T) {
		p := ParseArgs([]string{"--tool=claude-code", "--reason=tests"})
		if v, _ := p.Option("--tool"); v != "claude-code" {
			t.Errorf("--tool = %q", v)
		}
		if v, _ := p.Option("--reason"); v != "tests" {
			t.Errorf("--reason = %q", v)
		}
	})

	t.Run("flag without value is recognized", func(t *testing.T) {
		p := ParseArgs([]string{"--dry-run"})
		if !p.Flag("--dry-run") {
			t.Error("flag missing")
		}
		if _, ok := p.Option("--dry-run"); ok {
			t.Error("flag also read as an option")
		}
	})

	t.Run("flag followed by another flag", func(t *testing.T) {
		p := ParseArgs([]string{"--dry-run", "--json"})
		if !p.Flag("--dry-run") || !p.Flag("--json") {
			t.Errorf("flags %v", p.Flags)
		}
	})

	t.Run("mixed positionals options flags", func(t *testing.T) {
		p := ParseArgs([]string{"session-key-123", "--tool", "codex", "--reason", "running tests", "--json"})
		if v, _ := p.Positional(0); v != "session-key-123" {
			t.Errorf("positional %q", v)
		}
		if v, _ := p.Option("--tool"); v != "codex" {
			t.Errorf("--tool = %q", v)
		}
		if v, _ := p.Option("--reason"); v != "running tests" {
			t.Errorf("--reason = %q", v)
		}
		if !p.Flag("--json") {
			t.Error("--json missing")
		}
	})

	t.Run("empty args", func(t *testing.T) {
		p := ParseArgs(nil)
		if len(p.Positionals) != 0 || len(p.Options) != 0 || len(p.Flags) != 0 {
			t.Errorf("got %+v", p)
		}
	})

	t.Run("unknown option returns nil", func(t *testing.T) {
		p := ParseArgs([]string{"--tool", "claude-code"})
		if v, ok := p.Option("--missing"); ok {
			t.Errorf("--missing = %q", v)
		}
		if p.Flag("--missing") {
			t.Error("--missing flag set")
		}
	})

	t.Run("dry run flag parsed alongside tool", func(t *testing.T) {
		p := ParseArgs([]string{"--tool", "claude-code", "--dry-run"})
		if v, _ := p.Option("--tool"); v != "claude-code" {
			t.Errorf("--tool = %q", v)
		}
		if !p.Flag("--dry-run") {
			t.Error("--dry-run missing")
		}
	})

	t.Run("dry run flag parsed before tool", func(t *testing.T) {
		p := ParseArgs([]string{"--dry-run", "--tool", "aider"})
		if !p.Flag("--dry-run") {
			t.Error("--dry-run missing")
		}
		if v, _ := p.Option("--tool"); v != "aider" {
			t.Errorf("--tool = %q", v)
		}
	})

	t.Run("equals syntax with empty value", func(t *testing.T) {
		p := ParseArgs([]string{"--reason="})
		if v, ok := p.Option("--reason"); !ok || v != "" {
			t.Errorf("--reason = %q, %v", v, ok)
		}
		if p.Flag("--reason") {
			t.Error("--reason also read as a flag")
		}
	})

	t.Run("repeated option takes last value", func(t *testing.T) {
		p := ParseArgs([]string{"--tool", "a", "--tool", "b"})
		if v, _ := p.Option("--tool"); v != "b" {
			t.Errorf("--tool = %q", v)
		}
	})

	// Go-specific: the value after "=" may itself contain "=", and a single-dash word is a value.
	t.Run("equals value keeps later equals signs and single dash values", func(t *testing.T) {
		p := ParseArgs([]string{"--reason=a=b", "--ttl", "-5"})
		if v, _ := p.Option("--reason"); v != "a=b" {
			t.Errorf("--reason = %q", v)
		}
		if v, _ := p.Option("--ttl"); v != "-5" {
			t.Errorf("--ttl = %q", v)
		}
	})
}
