package hooks

import (
	"os"
	"path/filepath"
	"testing"
)

// codexTrustHome is a temp home with .codex/hooks.json present (unless hooksJSON is nil) and
// config.toml holding configTOML (unless nil).
func codexTrustHome(t *testing.T, configTOML, hooksJSON *string) string {
	t.Helper()
	home := fakeHome(t, ".codex")
	if hooksJSON != nil {
		writeFile(t, filepath.Join(home, ".codex", "hooks.json"), *hooksJSON)
	}
	if configTOML != nil {
		writeFile(t, filepath.Join(home, ".codex", "config.toml"), *configTOML)
	}
	return home
}

func ptr(s string) *string { return &s }

var defaultHooksJSON = ptr(`{"hooks":{}}`)

// realisticTOML records trust the way Codex's toml_edit writer does: quoted dotted table headers,
// like the [projects."…"] entries it writes.
func realisticTOML(home string, events ...string) string {
	path := filepath.Join(home, ".codex", "hooks.json")
	toml := "model = \"gpt-5.5\"\n\n[projects.\"/tmp\"]\ntrust_level = \"trusted\"\n\n"
	for _, event := range events {
		toml += "[hooks.state.\"" + path + ":" + event + ":0:0\"]\ntrusted_hash = \"sha256:deadbeef\"\n\n"
	}
	return toml
}

func writeTOML(t *testing.T, home, toml string) {
	t.Helper()
	writeFile(t, filepath.Join(home, ".codex", "config.toml"), toml)
}

func TestCodexHookTrust(t *testing.T) {
	t.Run("both events trusted reads as trusted", func(t *testing.T) {
		home := codexTrustHome(t, ptr(""), defaultHooksJSON)
		writeTOML(t, home, realisticTOML(home, "user_prompt_submit", "stop"))
		if got := CodexTrustStatus(home); got != CodexTrusted {
			t.Errorf("status = %s", got)
		}
	})

	t.Run("only one event trusted reads as partial", func(t *testing.T) {
		home := codexTrustHome(t, nil, defaultHooksJSON)
		writeTOML(t, home, realisticTOML(home, "user_prompt_submit"))
		if got := CodexTrustStatus(home); got != CodexPartiallyTrusted {
			t.Errorf("status = %s", got)
		}
	})

	t.Run("no trust entries reads as untrusted", func(t *testing.T) {
		home := codexTrustHome(t, ptr("model = \"gpt-5.5\"\n[projects.\"/tmp\"]\ntrust_level = \"trusted\"\n"), defaultHooksJSON)
		if got := CodexTrustStatus(home); got != CodexUntrusted {
			t.Errorf("status = %s", got)
		}
	})

	t.Run("missing config toml reads as unknown", func(t *testing.T) {
		home := codexTrustHome(t, nil, defaultHooksJSON)
		if got := CodexTrustStatus(home); got != CodexTrustUnknown {
			t.Errorf("status = %s", got)
		}
	})

	t.Run("missing hooks json reads as unknown", func(t *testing.T) {
		home := codexTrustHome(t, ptr("model = \"x\"\n"), nil)
		if got := CodexTrustStatus(home); got != CodexTrustUnknown {
			t.Errorf("status = %s", got)
		}
	})

	t.Run("an empty trusted hash does not count", func(t *testing.T) {
		home := codexTrustHome(t, nil, defaultHooksJSON)
		path := filepath.Join(home, ".codex", "hooks.json")
		writeTOML(t, home, "[hooks.state.\""+path+":user_prompt_submit:0:0\"]\n"+
			"trusted_hash = \"\"\nenabled = true\n\n"+
			"[hooks.state.\""+path+":stop:0:0\"]\nenabled = true")
		if got := CodexTrustStatus(home); got != CodexUntrusted {
			t.Errorf("status = %s", got)
		}
	})

	t.Run("trust recorded for a different hooks file does not count", func(t *testing.T) {
		home := codexTrustHome(t, nil, defaultHooksJSON)
		writeTOML(t, home, "[hooks.state.\"/Users/someone/other/hooks.json:user_prompt_submit:0:0\"]\n"+
			"trusted_hash = \"sha256:abc\"\n\n"+
			"[hooks.state.\"/Users/someone/other/hooks.json:stop:0:0\"]\ntrusted_hash = \"sha256:abc\"")
		if got := CodexTrustStatus(home); got != CodexUntrusted {
			t.Errorf("status = %s", got)
		}
	})

	t.Run("trust at a non-zero group index still counts", func(t *testing.T) {
		home := codexTrustHome(t, nil, defaultHooksJSON)
		path := filepath.Join(home, ".codex", "hooks.json")
		writeTOML(t, home, "[hooks.state.\""+path+":user_prompt_submit:2:0\"]\n"+
			"trusted_hash = \"sha256:abc\"\n\n"+
			"[hooks.state.\""+path+":stop:1:0\"]\ntrusted_hash = \"sha256:def\"")
		if got := CodexTrustStatus(home); got != CodexTrusted {
			t.Errorf("status = %s", got)
		}
	})

	// Go-specific: CRLF line endings and indented headers parse the same.
	t.Run("crlf and indentation are tolerated", func(t *testing.T) {
		home := codexTrustHome(t, nil, defaultHooksJSON)
		path := filepath.Join(home, ".codex", "hooks.json")
		writeTOML(t, home, "  [hooks.state.\""+path+":user_prompt_submit:0:0\"]\r\n\ttrusted_hash = \"sha256:a\"\r\n"+
			"[hooks.state.\""+path+":stop:0:0\"]\r\ntrusted_hash=\"sha256:b\"\r\n")
		if got := CodexTrustStatus(home); got != CodexTrusted {
			t.Errorf("status = %s", got)
		}
	})

	t.Run("an unreadable config toml reads as unknown", func(t *testing.T) {
		home := codexTrustHome(t, nil, defaultHooksJSON)
		if err := os.Mkdir(filepath.Join(home, ".codex", "config.toml"), 0o755); err != nil {
			t.Fatal(err)
		}
		if got := CodexTrustStatus(home); got != CodexTrustUnknown {
			t.Errorf("status = %s", got)
		}
	})
}
