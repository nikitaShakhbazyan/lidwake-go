package hooks

import (
	"path/filepath"
	"slices"
	"testing"
)

// TestMCPServerShape covers MCP server registration (`lidwake mcp`) through the installer's MCP
// API. Claude Code is the only device-verified MCP agent; registration is gated on the shape, not
// detection, so a bare temp home suffices.
func TestMCPServerShape(t *testing.T) {
	claudeConfig := func(home string) string { return filepath.Join(home, ".claude.json") }
	voicemode := map[string]any{"voicemode": map[string]any{"type": "stdio", "command": "uvx", "args": []any{"voice-mode"}}}

	// ---- Capability gating ----

	t.Run("supports MCP only for verified agents", func(t *testing.T) {
		in := testInstaller(testCLI, fakeHome(t))
		if !in.SupportsMCP(agentClaudeCode) {
			t.Error("claude-code supports MCP")
		}
		for _, agent := range allAgentNames[1:] {
			if in.SupportsMCP(agent) {
				t.Errorf("%s should be gated until device-verified", agent)
			}
		}
	})

	t.Run("install MCP throws for unsupported agent", func(t *testing.T) {
		_, err := testInstaller(testCLI, fakeHome(t)).InstallMCP(agentCodex, false)
		if s := requireSkip(t, err); s.Reason != SkipUnsupported || s.Detail != "MCP not supported for codex" {
			t.Errorf("err = %+v", s)
		}
	})

	// ---- Install ----

	t.Run("install registers stdio server with tool flag", func(t *testing.T) {
		home := fakeHome(t)
		must[Result](t)(testInstaller(testCLI, home).InstallMCP(agentClaudeCode, false))

		servers := object(t, readJSONFile(t, claudeConfig(home))["mcpServers"], "mcpServers")
		entry := object(t, servers["lidwake"], "lidwake")
		if entry["type"] != "stdio" || entry["command"] != "/usr/local/bin/lidwake" {
			t.Errorf("entry = %v", entry)
		}
		if args, _ := entry["args"].([]any); !slices.Equal(args, []any{"mcp", "--tool", "claude-code"}) {
			t.Errorf("args = %v", entry["args"])
		}
	})

	t.Run("install preserves existing servers", func(t *testing.T) {
		home := fakeHome(t)
		writeJSONFile(t, map[string]any{"anonymousId": "abc-123", "mcpServers": voicemode}, claudeConfig(home))

		must[Result](t)(testInstaller(testCLI, home).InstallMCP(agentClaudeCode, false))

		dict := readJSONFile(t, claudeConfig(home))
		if dict["anonymousId"] != "abc-123" {
			t.Error("unrelated top-level keys must survive")
		}
		servers := object(t, dict["mcpServers"], "mcpServers")
		if servers["voicemode"] == nil {
			t.Error("the user's other MCP servers must survive")
		}
		if servers["lidwake"] == nil {
			t.Error("ours must be added")
		}
	})

	t.Run("install is idempotent", func(t *testing.T) {
		home := fakeHome(t)
		in := testInstaller(testCLI, home)
		must[Result](t)(in.InstallMCP(agentClaudeCode, false))
		must[Result](t)(in.InstallMCP(agentClaudeCode, false))

		servers := object(t, readJSONFile(t, claudeConfig(home))["mcpServers"], "mcpServers")
		if len(servers) != 1 {
			t.Errorf("servers = %v", servers)
		}
		wantState(t, in.MCPState(agentClaudeCode), StateInstalled, "idempotent")
	})

	t.Run("dry run does not touch disk", func(t *testing.T) {
		home := fakeHome(t)
		result := must[Result](t)(testInstaller(testCLI, home).InstallMCP(agentClaudeCode, true))
		if result.Diff == "" {
			t.Error("diff must not be empty")
		}
		if fileExists(claudeConfig(home)) {
			t.Error("dry run wrote the config")
		}
	})

	t.Run("gated agents refuse MCP install and write nothing", func(t *testing.T) {
		home := fakeHome(t)
		in := testInstaller(testCLI, home)
		for _, agent := range []string{agentCursor, agentGeminiCLI} {
			_, err := in.InstallMCP(agent, false)
			requireSkip(t, err)
		}
		if fileExists(filepath.Join(home, ".cursor", "mcp.json")) || fileExists(filepath.Join(home, ".gemini", "settings.json")) {
			t.Error("a gated agent's config must not be written")
		}
	})

	// ---- Uninstall ----

	t.Run("uninstall removes only our server", func(t *testing.T) {
		home := fakeHome(t)
		writeJSONFile(t, map[string]any{"mcpServers": voicemode}, claudeConfig(home))

		in := testInstaller(testCLI, home)
		must[Result](t)(in.InstallMCP(agentClaudeCode, false))
		must[Result](t)(in.UninstallMCP(agentClaudeCode, false))

		servers := object(t, readJSONFile(t, claudeConfig(home))["mcpServers"], "mcpServers")
		if servers["lidwake"] != nil {
			t.Error("our server must be gone")
		}
		if servers["voicemode"] == nil {
			t.Error("the user's server must remain")
		}
	})

	t.Run("uninstall is no op when absent", func(t *testing.T) {
		result := must[Result](t)(testInstaller(testCLI, fakeHome(t)).UninstallMCP(agentClaudeCode, false))
		if result.Diff != unchangedDiff {
			t.Errorf("diff = %q", result.Diff)
		}
	})

	// ---- State ----

	t.Run("state transitions", func(t *testing.T) {
		in := testInstaller(testCLI, fakeHome(t))
		wantState(t, in.MCPState(agentClaudeCode), StateNotInstalled, "initial")
		must[Result](t)(in.InstallMCP(agentClaudeCode, false))
		wantState(t, in.MCPState(agentClaudeCode), StateInstalled, "installed")
		must[Result](t)(in.UninstallMCP(agentClaudeCode, false))
		wantState(t, in.MCPState(agentClaudeCode), StateNotInstalled, "removed")
	})

	t.Run("state is modified when entry tampered", func(t *testing.T) {
		home := fakeHome(t)
		in := testInstaller(testCLI, home)
		must[Result](t)(in.InstallMCP(agentClaudeCode, false))

		dict := readJSONFile(t, claudeConfig(home))
		object(t, dict["mcpServers"], "mcpServers")["lidwake"] = map[string]any{
			"type": "stdio", "command": "/somewhere/else/lidwake", "args": []any{"mcp"},
		}
		writeJSONFile(t, dict, claudeConfig(home))

		wantState(t, in.MCPState(agentClaudeCode), StateModifiedExternally, "tampered")
	})

	// Go-specific: the emptied container stays, and the summaries match the CLI wording.
	t.Run("uninstall keeps an emptied container and reports what it did", func(t *testing.T) {
		home := fakeHome(t)
		in := testInstaller(testCLI, home)
		if got := must[Result](t)(in.InstallMCP(agentClaudeCode, false)).Summary; got != "registered lidwake MCP server" {
			t.Errorf("install summary = %q", got)
		}
		if got := must[Result](t)(in.UninstallMCP(agentClaudeCode, false)).Summary; got != "removed lidwake MCP server" {
			t.Errorf("uninstall summary = %q", got)
		}
		if servers := object(t, readJSONFile(t, claudeConfig(home))["mcpServers"], "mcpServers"); len(servers) != 0 {
			t.Errorf("servers = %v", servers)
		}
	})
}
