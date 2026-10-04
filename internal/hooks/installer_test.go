package hooks

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/nikitaShakhbazyan/lidwake-go/internal/agents"
)

func TestHookInstaller(t *testing.T) {
	// ---- Detection ----

	t.Run("detects claude code by config dir", func(t *testing.T) {
		home := fakeHome(t, ".claude")
		detected := testInstaller("", home).DetectedAgents()
		if !slices.Contains(detected, agentClaudeCode) {
			t.Errorf("detected = %v, want claude-code", detected)
		}
		if slices.Contains(detected, agentCodex) {
			t.Errorf("detected = %v, want no codex", detected)
		}
	})

	t.Run("detects all tier 1 when all present", func(t *testing.T) {
		home := fakeHome(t, ".claude", ".codex", ".cursor", ".gemini")
		detected := testInstaller("", home).DetectedAgents()
		for _, a := range []string{agentClaudeCode, agentCodex, agentCursor, agentGeminiCLI} {
			if !slices.Contains(detected, a) {
				t.Errorf("detected = %v, missing %s", detected, a)
			}
		}
	})

	// ---- Install: Claude Code shape ----

	t.Run("install claude code writes activity scoped hooks", func(t *testing.T) {
		home := fakeHome(t, ".claude")
		in := testInstaller(testCLI, home)
		result := mustInstall(t, in, agentClaudeCode)
		wantContains(t, result.Summary, "UserPromptSubmit", "summary")
		wantContains(t, result.Summary, "Stop", "summary")

		hooks := hooksIn(t, filepath.Join(home, ".claude", "settings.json"))
		if hooks["UserPromptSubmit"] == nil || hooks["Stop"] == nil {
			t.Fatalf("missing per-turn pair: %v", hooks)
		}

		// Session-retirement bracketing — release-only cleanup, not the whole-session hold.
		// SessionEnd releases for every reason; SessionStart acquires only for source "clear".
		sessionEnd := objects(t, hooks["SessionEnd"], "SessionEnd")
		if _, ok := sessionEnd[0]["matcher"]; ok {
			t.Error("every end reason must release — clear, resume, logout, exit")
		}
		wantContains(t, firstInnerCommand(t, hooks, "SessionEnd"), "release", "SessionEnd")
		sessionStart := objects(t, hooks["SessionStart"], "SessionStart")
		if sessionStart[0]["matcher"] != "clear" {
			t.Error("an unmatched acquire would re-introduce the whole-session hold")
		}
		wantContains(t, firstInnerCommand(t, hooks, "SessionStart"), "acquire", "SessionStart")

		// Esc-interrupt release: Notification matched to idle_prompt, carrying the release command.
		notif := objects(t, hooks["Notification"], "Notification")
		i := slices.IndexFunc(notif, func(e map[string]any) bool { return e["matcher"] == "idle_prompt" })
		if i < 0 {
			t.Fatal("no idle_prompt Notification entry")
		}
		inner := objects(t, notif[i]["hooks"], "Notification inner")
		cmd, _ := inner[0]["command"].(string)
		wantContains(t, cmd, "release", "Notification")
		wantContains(t, cmd, "claude-code", "Notification")
	})

	t.Run("install claude code writes subagent hooks", func(t *testing.T) {
		home := fakeHome(t, ".claude")
		mustInstall(t, testInstaller(testCLI, home), agentClaudeCode)
		hooks := hooksIn(t, filepath.Join(home, ".claude", "settings.json"))

		start := firstInnerCommand(t, hooks, "SubagentStart")
		wantContains(t, start, "acquire --tool claude-code --subagent", "SubagentStart")
		wantNotContains(t, start, "$CLAUDE_CODE_SESSION_ID", "sub-agent id comes from stdin, not the parent session env var")

		stop := firstInnerCommand(t, hooks, "SubagentStop")
		wantContains(t, stop, "release --tool claude-code --subagent", "SubagentStop")
		wantNotContains(t, stop, "$CLAUDE_CODE_SESSION_ID", "SubagentStop")
	})

	t.Run("uninstall claude code removes subagent hooks", func(t *testing.T) {
		home := fakeHome(t, ".claude")
		in := testInstaller(testCLI, home)
		mustInstall(t, in, agentClaudeCode)
		mustUninstall(t, in, agentClaudeCode)

		hooks := hooksIn(t, filepath.Join(home, ".claude", "settings.json"))
		if hooks["SubagentStart"] != nil {
			t.Error("our SubagentStart entry must be gone")
		}
		if hooks["SubagentStop"] != nil {
			t.Error("our SubagentStop entry must be gone")
		}
		wantState(t, in.State(agentClaudeCode), StateNotInstalled, "after uninstall")
	})

	t.Run("install state modified when subagent hooks missing", func(t *testing.T) {
		home := fakeHome(t, ".claude")
		in := testInstaller(testCLI, home)
		mustInstall(t, in, agentClaudeCode)

		// An install from before the sub-agent hooks existed: drop just those two.
		path := filepath.Join(home, ".claude", "settings.json")
		dict := readJSONFile(t, path)
		hooks := object(t, dict["hooks"], "hooks")
		delete(hooks, "SubagentStart")
		delete(hooks, "SubagentStop")
		writeJSONFile(t, dict, path)

		wantState(t, in.State(agentClaudeCode), StateModifiedExternally, "partial install")

		// Reinstall self-heals.
		mustInstall(t, in, agentClaudeCode)
		wantState(t, in.State(agentClaudeCode), StateInstalled, "after reinstall")
	})

	t.Run("uninstall claude code removes notification hook", func(t *testing.T) {
		home := fakeHome(t, ".claude")
		in := testInstaller(testCLI, home)
		mustInstall(t, in, agentClaudeCode)
		wantState(t, in.State(agentClaudeCode), StateInstalled, "after install")
		mustUninstall(t, in, agentClaudeCode)
		wantState(t, in.State(agentClaudeCode), StateNotInstalled, "after uninstall")

		hooks := hooksIn(t, filepath.Join(home, ".claude", "settings.json"))
		if arr, ok := hooks["Notification"].([]any); ok {
			for _, e := range arr {
				if m, ok := e.(map[string]any); ok && m["matcher"] == "idle_prompt" {
					t.Error("our Notification entry must be gone")
				}
			}
		}
	})

	t.Run("install state not installed when notification hook missing", func(t *testing.T) {
		home := fakeHome(t, ".claude")
		in := testInstaller(testCLI, home)
		mustInstall(t, in, agentClaudeCode)

		path := filepath.Join(home, ".claude", "settings.json")
		dict := readJSONFile(t, path)
		delete(object(t, dict["hooks"], "hooks"), "Notification")
		writeJSONFile(t, dict, path)

		wantState(t, in.State(agentClaudeCode), StateModifiedExternally, "missing Notification")
	})

	t.Run("install claude code upgrades legacy session scoped hooks in place", func(t *testing.T) {
		home := fakeHome(t, ".claude")
		path := filepath.Join(home, ".claude", "settings.json")
		writeJSONFile(t, map[string]any{
			"hooks": map[string]any{
				"SessionStart": []any{map[string]any{"hooks": []any{map[string]any{
					"type": "command", "command": "lidwake acquire $CLAUDE_CODE_SESSION_ID --tool claude-code", "_lidwake": true,
				}}}},
				"SessionEnd": []any{map[string]any{"hooks": []any{map[string]any{
					"type": "command", "command": "lidwake release $CLAUDE_CODE_SESSION_ID --tool claude-code", "_lidwake": true,
				}}}},
			},
		}, path)

		in := testInstaller(testCLI, home)
		mustInstall(t, in, agentClaudeCode)

		hooks := hooksIn(t, path)
		if hooks["UserPromptSubmit"] == nil || hooks["Stop"] == nil {
			t.Fatal("per-turn pair missing")
		}
		sessionStart := objects(t, hooks["SessionStart"], "SessionStart")
		if len(sessionStart) != 1 {
			t.Errorf("SessionStart groups = %d, want 1", len(sessionStart))
		}
		if sessionStart[0]["matcher"] != "clear" {
			t.Error("legacy unmatched acquire narrowed to source clear")
		}
		wantContains(t, firstInnerCommand(t, hooks, "SessionEnd"), "release", "SessionEnd")
		wantState(t, in.State(agentClaudeCode), StateInstalled, "after upgrade")
	})

	t.Run("upgrade preserves user session start hook", func(t *testing.T) {
		home := fakeHome(t, ".claude")
		path := filepath.Join(home, ".claude", "settings.json")
		writeJSONFile(t, map[string]any{
			"hooks": map[string]any{
				"SessionStart": []any{
					map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "lidwake acquire X --tool claude-code", "_lidwake": true}}},
					map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "echo my-own-hook"}}},
				},
			},
		}, path)

		mustInstall(t, testInstaller(testCLI, home), agentClaudeCode)

		sessionStart := objects(t, hooksIn(t, path)["SessionStart"], "SessionStart")
		if len(sessionStart) != 2 {
			t.Fatalf("groups = %d: our group upgraded in place, the user's own group intact", len(sessionStart))
		}
		var ours, users map[string]any
		for _, g := range sessionStart {
			inner := objects(t, g["hooks"], "inner")
			if slices.ContainsFunc(inner, isTagged) {
				ours = g
			}
			if inner[0]["command"] == "echo my-own-hook" {
				users = g
			}
		}
		if ours == nil || users == nil {
			t.Fatalf("groups = %v", sessionStart)
		}
		if ours["matcher"] != "clear" {
			t.Error("our group must carry the clear matcher")
		}
		cmd, _ := objects(t, ours["hooks"], "ours")[0]["command"].(string)
		wantContains(t, cmd, "$CLAUDE_CODE_SESSION_ID", "stale command refreshed to canonical")
		if _, ok := users["matcher"]; ok {
			t.Error("the user's group must not gain a matcher")
		}
	})

	// ---- Codex ----

	t.Run("install codex writes user prompt submit and sources id from stdin", func(t *testing.T) {
		home := fakeHome(t, ".codex")
		mustInstall(t, testInstaller(testCLI, home), agentCodex)

		hooks := hooksIn(t, filepath.Join(home, ".codex", "hooks.json"))
		if hooks["UserPromptSubmit"] == nil {
			t.Error("acquire on UserPromptSubmit")
		}
		if hooks["Stop"] == nil {
			t.Error("Stop fires at turn completion — the release boundary")
		}
		if hooks["SessionStart"] != nil {
			t.Error("SessionStart misses resumed sessions — UserPromptSubmit instead")
		}
		if hooks["SessionEnd"] != nil {
			t.Error("no SessionEnd")
		}

		for _, tc := range []struct{ event, op string }{
			{"UserPromptSubmit", "acquire --tool codex"},
			{"Stop", "release --tool codex"},
		} {
			group := objects(t, hooks[tc.event], tc.event)[0]
			if _, ok := group["matcher"]; ok {
				t.Errorf("%s: no tool to match on lifecycle events — no matcher", tc.event)
			}
			handler := objects(t, group["hooks"], tc.event+": must be nested — inner hooks wrapper")[0]
			if handler["type"] != "command" {
				t.Errorf("%s: type = %v", tc.event, handler["type"])
			}
			if _, ok := handler["_lidwake"]; ok {
				t.Errorf("%s: no marker key — Codex may reject unknown fields", tc.event)
			}
			cmd, _ := handler["command"].(string)
			wantNotContains(t, cmd, "CODEX_THREAD_ID", tc.event)
			wantNotContains(t, cmd, "$", tc.event+": session id comes from stdin")
			wantContains(t, cmd, tc.op, tc.event)
		}
	})

	t.Run("reinstalling codex keeps the handler stable so trust survives", func(t *testing.T) {
		home := fakeHome(t, ".codex")
		in := testInstaller(testCLI, home)
		mustInstall(t, in, agentCodex)

		path := filepath.Join(home, ".codex", "hooks.json")
		dict := readJSONFile(t, path)
		handler := objects(t, objects(t, object(t, dict["hooks"], "hooks")["UserPromptSubmit"], "UPS")[0]["hooks"], "handlers")[0]
		originalCommand := handler["command"]
		handler["sibling"] = "keep-me"
		writeJSONFile(t, dict, path)

		mustInstall(t, in, agentCodex)

		groups := objects(t, hooksIn(t, path)["UserPromptSubmit"], "UPS")
		if len(groups) != 1 {
			t.Fatalf("reinstall must not duplicate the group: %d", len(groups))
		}
		handlers := objects(t, groups[0]["hooks"], "handlers")
		if len(handlers) != 1 {
			t.Fatalf("reinstall must not duplicate the handler: %d", len(handlers))
		}
		if handlers[0]["command"] != originalCommand {
			t.Error("command unchanged so the trust hash still matches")
		}
		if handlers[0]["sibling"] != "keep-me" {
			t.Error("sibling keys preserved")
		}
	})

	t.Run("install codex strips a stale SessionStart entry", func(t *testing.T) {
		home := fakeHome(t, ".codex")
		path := filepath.Join(home, ".codex", "hooks.json")
		writeJSONFile(t, map[string]any{
			"hooks": map[string]any{
				"SessionStart": []any{
					map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "/usr/local/bin/lidwake acquire --tool codex"}}},
					map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "/usr/local/bin/user-thing"}}},
				},
			},
		}, path)

		mustInstall(t, testInstaller(testCLI, home), agentCodex)

		hooks := hooksIn(t, path)
		if n := len(objects(t, hooks["UserPromptSubmit"], "UPS")); n != 1 {
			t.Errorf("UserPromptSubmit groups = %d", n)
		}
		sessionStart := objects(t, hooks["SessionStart"], "SessionStart")
		if len(sessionStart) != 1 {
			t.Fatalf("only the user's own SessionStart group remains: %v", sessionStart)
		}
		if got := firstInnerCommand(t, hooks, "SessionStart"); got != "/usr/local/bin/user-thing" {
			t.Errorf("user command = %q", got)
		}
	})

	t.Run("fresh codex install reads as installed", func(t *testing.T) {
		home := fakeHome(t, ".codex")
		in := testInstaller(testCLI, home)
		mustInstall(t, in, agentCodex)
		wantState(t, in.State(agentCodex), StateInstalled, "fresh install")
	})

	t.Run("codex install missing the Stop release reads as modified", func(t *testing.T) {
		home := fakeHome(t, ".codex")
		path := filepath.Join(home, ".codex", "hooks.json")
		writeJSONFile(t, map[string]any{
			"hooks": map[string]any{
				"UserPromptSubmit": []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "/usr/local/bin/lidwake acquire --tool codex"}}}},
			},
		}, path)

		in := testInstaller(testCLI, home)
		wantState(t, in.State(agentCodex), StateModifiedExternally, "acquire-only is a partial install")

		mustInstall(t, in, agentCodex)
		wantState(t, in.State(agentCodex), StateInstalled, "after reinstall")
		if hooksIn(t, path)["Stop"] == nil {
			t.Error("Stop must be added")
		}
	})

	t.Run("install codex writes subagent hooks", func(t *testing.T) {
		home := fakeHome(t, ".codex")
		mustInstall(t, testInstaller(testCLI, home), agentCodex)
		hooks := hooksIn(t, filepath.Join(home, ".codex", "hooks.json"))

		start := firstInnerCommand(t, hooks, "SubagentStart")
		wantContains(t, start, "acquire --tool codex --subagent", "SubagentStart")
		wantNotContains(t, start, "$", "Codex sources the sub-agent id from stdin — no env var")

		stop := firstInnerCommand(t, hooks, "SubagentStop")
		wantContains(t, stop, "release --tool codex --subagent", "SubagentStop")
		wantNotContains(t, stop, "$", "SubagentStop")
	})

	t.Run("codex install missing the subagent hooks reads as modified", func(t *testing.T) {
		home := fakeHome(t, ".codex")
		path := filepath.Join(home, ".codex", "hooks.json")
		writeJSONFile(t, map[string]any{
			"hooks": map[string]any{
				"UserPromptSubmit": []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "/usr/local/bin/lidwake acquire --tool codex"}}}},
				"Stop":             []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "/usr/local/bin/lidwake release --tool codex"}}}},
			},
		}, path)

		in := testInstaller(testCLI, home)
		wantState(t, in.State(agentCodex), StateModifiedExternally, "missing sub-agent hooks is a partial install")

		mustInstall(t, in, agentCodex)
		wantState(t, in.State(agentCodex), StateInstalled, "after reinstall")
		hooks := hooksIn(t, path)
		if hooks["SubagentStart"] == nil || hooks["SubagentStop"] == nil {
			t.Error("sub-agent hooks must be added")
		}
	})

	t.Run("uninstall codex removes subagent hooks", func(t *testing.T) {
		home := fakeHome(t, ".codex")
		in := testInstaller(testCLI, home)
		mustInstall(t, in, agentCodex)
		mustUninstall(t, in, agentCodex)

		hooks := hooksIn(t, filepath.Join(home, ".codex", "hooks.json"))
		if hooks["SubagentStart"] != nil || hooks["SubagentStop"] != nil {
			t.Errorf("sub-agent hooks must be gone: %v", hooks)
		}
		wantState(t, in.State(agentCodex), StateNotInstalled, "after uninstall")
	})

	t.Run("reinstalling codex keeps both handlers stable so trust survives", func(t *testing.T) {
		home := fakeHome(t, ".codex")
		in := testInstaller(testCLI, home)
		mustInstall(t, in, agentCodex)
		path := filepath.Join(home, ".codex", "hooks.json")

		dict := readJSONFile(t, path)
		hooks := object(t, dict["hooks"], "hooks")
		for _, event := range []string{"UserPromptSubmit", "Stop"} {
			objects(t, objects(t, hooks[event], event)[0]["hooks"], event)[0]["sibling"] = "keep-" + event
		}
		writeJSONFile(t, dict, path)

		mustInstall(t, in, agentCodex)

		after := hooksIn(t, path)
		for _, tc := range []struct{ event, op string }{{"UserPromptSubmit", "acquire"}, {"Stop", "release"}} {
			groups := objects(t, after[tc.event], tc.event)
			if len(groups) != 1 {
				t.Errorf("%s: no duplicate group (%d)", tc.event, len(groups))
			}
			handlers := objects(t, groups[0]["hooks"], tc.event)
			if len(handlers) != 1 {
				t.Errorf("%s: no duplicate handler (%d)", tc.event, len(handlers))
			}
			cmd, _ := handlers[0]["command"].(string)
			wantContains(t, cmd, tc.op+" --tool codex", tc.event)
			if handlers[0]["sibling"] != "keep-"+tc.event {
				t.Errorf("%s: sibling key preserved", tc.event)
			}
		}
	})

	t.Run("uninstall codex removes both acquire and release", func(t *testing.T) {
		home := fakeHome(t, ".codex")
		path := filepath.Join(home, ".codex", "hooks.json")
		writeJSONFile(t, map[string]any{
			"hooks": map[string]any{
				"Stop": []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "/usr/local/bin/user-stop"}}}},
			},
		}, path)

		in := testInstaller(testCLI, home)
		mustInstall(t, in, agentCodex)
		mustUninstall(t, in, agentCodex)

		hooks := hooksIn(t, path)
		if hooks["UserPromptSubmit"] != nil {
			t.Error("our acquire is gone")
		}
		var stopCmds []string
		for _, g := range objects(t, hooks["Stop"], "Stop") {
			inner := objects(t, g["hooks"], "inner")
			if cmd, ok := inner[0]["command"].(string); ok {
				stopCmds = append(stopCmds, cmd)
			}
		}
		if !slices.Equal(stopCmds, []string{"/usr/local/bin/user-stop"}) {
			t.Errorf("user's own Stop hook preserved, ours removed: %v", stopCmds)
		}
		wantState(t, in.State(agentCodex), StateNotInstalled, "after uninstall")
	})

	// ---- Pi (TypeScript extension) ----

	t.Run("install pi writes extension with correct events", func(t *testing.T) {
		home := fakeHome(t, ".pi")
		in := testInstaller(testCLI, home)
		mustInstall(t, in, agentPi)

		ts := readFile(t, filepath.Join(home, ".pi", "agent", "extensions", "lidwake.ts"))
		// Turn-scoped, not session-scoped: acquire on agent_start, release on agent_settled —
		// agent_end is too early (Pi may still auto-retry/compact/continue).
		wantContains(t, ts, `pi.on("agent_start"`, "acquire event")
		wantContains(t, ts, `pi.on("agent_settled"`, "release event")
		wantNotContains(t, ts, `pi.on("session_start"`, "session_start would hold for the whole process lifetime")
		wantNotContains(t, ts, `"agent_end"`, "agent_end can be followed by an automatic continuation")
		// session_shutdown stays as the safety net for a turn interrupted by exit.
		wantContains(t, ts, `pi.on("session_shutdown"`, "safety-net release")
		wantContains(t, ts, `stdio: "ignore"`, "the no-op safety-net release must not print into the TUI")
		wantContains(t, ts, "--tool", "tool flag")
		// Node-hosted owner + leak backstop: the extension runs in-process, so its process.pid is
		// the Pi host PID the parent walk can't find; the TTL caps a hold that missed every
		// release path.
		wantContains(t, ts, `"--pid", String(process.pid)`, "pid flag")
		wantContains(t, ts, `"--ttl", "`+strconv.Itoa(piHoldTTLSeconds)+`"`, "ttl flag")
		wantState(t, in.State(agentPi), StateInstalled, "after install")
	})

	t.Run("pi reinstall migrates the pid-less turn-scoped extension", func(t *testing.T) {
		home := fakeHome(t, ".pi", ".pi/agent/extensions")
		// An earlier turn-scoped extension whose acquire carried no --pid or --ttl, so a
		// Node-hosted Pi's hold had no owner and no expiry.
		path := filepath.Join(home, ".pi", "agent", "extensions", "lidwake.ts")
		writeFile(t, path, `import { execFileSync } from "node:child_process"

function run(args) {
  try { execFileSync("/usr/local/bin/lidwake", args, { stdio: "ignore" }) } catch (_) {}
}

export default function (pi) {
  const id = (ctx) => ctx?.sessionManager?.getSessionFile?.() ?? String(process.pid)
  pi.on("agent_start", async (_event, ctx) => run(["acquire", id(ctx), "--tool", "pi"]))
  pi.on("agent_settled", async (_event, ctx) => run(["release", id(ctx), "--tool", "pi"]))
  pi.on("session_shutdown", async (_event, ctx) => run(["release", id(ctx), "--tool", "pi"]))
}`)

		in := testInstaller(testCLI, home)
		wantState(t, in.State(agentPi), StateModifiedExternally,
			"the pid-less shape must read as drifted so the build migration reinstalls it")
		mustInstall(t, in, agentPi)

		wantContains(t, readFile(t, path), `"--pid", String(process.pid)`, "migrated extension")
		wantState(t, in.State(agentPi), StateInstalled, "after migration")
	})

	t.Run("pi reinstall migrates the session-scoped extension to turn-scoped events", func(t *testing.T) {
		home := fakeHome(t, ".pi", ".pi/agent/extensions")
		// The session-scoped extension: a hold for the whole process lifetime.
		path := filepath.Join(home, ".pi", "agent", "extensions", "lidwake.ts")
		writeFile(t, path, `import { execFileSync } from "node:child_process"

function run(args) {
  try { execFileSync("/usr/local/bin/lidwake", args) } catch (_) {}
}

export default function (pi) {
  const id = (ctx) => ctx?.sessionManager?.getSessionFile?.() ?? String(process.pid)
  pi.on("session_start", async (_event, ctx) => run(["acquire", id(ctx), "--tool", "pi"]))
  pi.on("session_shutdown", async (_event, ctx) => run(["release", id(ctx), "--tool", "pi"]))
}`)

		in := testInstaller(testCLI, home)
		wantState(t, in.State(agentPi), StateModifiedExternally,
			"the old shape must read as drifted so the build migration reinstalls it")
		mustInstall(t, in, agentPi)

		ts := readFile(t, path)
		wantNotContains(t, ts, `pi.on("session_start"`, "the stale session-scoped acquire must be gone")
		wantContains(t, ts, `pi.on("agent_start"`, "turn-scoped acquire")
		wantState(t, in.State(agentPi), StateInstalled, "after migration")
	})

	// ---- OpenCode (TypeScript plugin) ----

	t.Run("install open code uses info id and no idle release", func(t *testing.T) {
		home := fakeHome(t)
		// OpenCode is binary-detected; install through the integration directly to bypass the
		// search-path gate.
		must[Result](t)(openCodeIntegration{}.install(testContext(testCLI, home), false))

		ts := readFile(t, filepath.Join(home, ".config", "opencode", "plugins", "lidwake.ts"))
		wantContains(t, ts, "event.properties.info.id", "must use the correct session-id accessor")
		wantNotContains(t, ts, "session.idle", "must not release on the per-turn session.idle event")
		wantContains(t, ts, "session.created", "acquire event")
	})

	// ---- Hermes (YAML shell hooks + allowlist) ----

	t.Run("install hermes writes shell hook and allowlist", func(t *testing.T) {
		home := fakeHome(t, ".hermes")
		// A realistic Hermes config with the default empty hooks map.
		cfgPath := filepath.Join(home, ".hermes", "config.yaml")
		writeFile(t, cfgPath, "model:\n  default: x\nhooks: {}\nhooks_auto_accept: false\n")

		in := testInstaller(testCLI, home)
		mustInstall(t, in, agentHermes)

		// The shell hook lives in config.yaml's hooks: map — not a Python plugin.
		cfg := readFile(t, cfgPath)
		wantContains(t, cfg, "on_session_start", "config")
		wantContains(t, cfg, "pre_gateway_dispatch", "config")
		wantContains(t, cfg, "on_session_end", "config")
		wantContains(t, cfg, "acquire --tool hermes", "config")
		wantNotContains(t, cfg, "HERMES_SESSION_ID", "config")
		wantNotContains(t, cfg, "hooks: {}", "empty hooks map should have been replaced")

		// Every (event, command) pair must be allowlisted or Hermes skips that hook.
		approvals := objects(t, readJSONFile(t, filepath.Join(home, ".hermes", "shell-hooks-allowlist.json"))["approvals"], "approvals")
		if len(approvals) != 3 {
			t.Fatalf("approvals = %v, want 3", approvals)
		}
		for _, event := range []string{"on_session_start", "pre_gateway_dispatch", "on_session_end"} {
			if !slices.ContainsFunc(approvals, func(a map[string]any) bool { return a["event"] == event }) {
				t.Errorf("no approval for %s", event)
			}
		}

		wantState(t, in.State(agentHermes), StateInstalled, "after install")
	})

	t.Run("hermes uninstall restores empty hooks and revokes allowlist", func(t *testing.T) {
		home := fakeHome(t, ".hermes")
		cfgPath := filepath.Join(home, ".hermes", "config.yaml")
		writeFile(t, cfgPath, "model:\n  default: x\nhooks: {}\nhooks_auto_accept: false\n")

		in := testInstaller(testCLI, home)
		mustInstall(t, in, agentHermes)
		mustUninstall(t, in, agentHermes)

		cfg := readFile(t, cfgPath)
		wantNotContains(t, cfg, "lidwake", "our hooks must be gone")
		wantContains(t, cfg, "hooks: {}", "empty hooks map should be restored")
		wantState(t, in.State(agentHermes), StateNotInstalled, "after uninstall")
	})

	// Regression: uninstall must restore only the top-level hooks: map we own — a nested hooks:
	// elsewhere in the user's YAML must survive untouched. A global replace of "hooks:\n" matched
	// the tail of an indented "      hooks:\n" and rewrote that nested map to hooks: {}.
	t.Run("hermes uninstall leaves nested hooks map intact", func(t *testing.T) {
		home := fakeHome(t, ".hermes")
		// A user config with an unrelated nested hooks: map and the default top-level empty one.
		cfgPath := filepath.Join(home, ".hermes", "config.yaml")
		writeFile(t, cfgPath, "agents:\n  sub:\n    hooks:\n      on_x:\n        - command: \"echo hi\"\nhooks: {}\n")

		in := testInstaller(testCLI, home)
		mustInstall(t, in, agentHermes)
		mustUninstall(t, in, agentHermes)

		cfg := readFile(t, cfgPath)
		wantNotContains(t, cfg, "lidwake", "our hooks must be gone")
		wantContains(t, cfg, "    hooks:\n      on_x:", "the nested hooks map must be preserved verbatim")
		wantContains(t, cfg, "echo hi", "nested hook command must survive")
		wantContains(t, cfg, "hooks: {}", "the top-level map we owned is restored to empty")
	})

	// ---- Shared behavior ----

	t.Run("install is idempotent", func(t *testing.T) {
		home := fakeHome(t, ".claude")
		in := testInstaller(testCLI, home)
		mustInstall(t, in, agentClaudeCode)
		mustInstall(t, in, agentClaudeCode)
		hooks := hooksIn(t, filepath.Join(home, ".claude", "settings.json"))
		if n := len(objects(t, hooks["UserPromptSubmit"], "UPS")); n != 1 {
			t.Errorf("double-install should not duplicate hook entries: %d", n)
		}
	})

	t.Run("install preserves existing user hooks", func(t *testing.T) {
		home := fakeHome(t, ".claude")
		path := filepath.Join(home, ".claude", "settings.json")
		writeJSONFile(t, map[string]any{
			"hooks": map[string]any{
				"UserPromptSubmit": []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "echo user-hook"}}}},
			},
		}, path)

		mustInstall(t, testInstaller(testCLI, home), agentClaudeCode)
		if n := len(objects(t, hooksIn(t, path)["UserPromptSubmit"], "UPS")); n != 2 {
			t.Errorf("must preserve the user's existing hook alongside ours: %d", n)
		}
	})

	t.Run("dry run does not touch disk", func(t *testing.T) {
		home := fakeHome(t, ".claude")
		result := must[Result](t)(testInstaller(testCLI, home).Install(agentClaudeCode, true))
		if result.Diff == "" {
			t.Error("diff must not be empty")
		}
		if fileExists(filepath.Join(home, ".claude", "settings.json")) {
			t.Error("dry run wrote the config")
		}
	})

	t.Run("install skips undetected agent", func(t *testing.T) {
		home := fakeHome(t)
		_, err := testInstaller(testCLI, home).Install(agentClaudeCode, false)
		if s := requireSkip(t, err); s.Reason != SkipNotInstalled {
			t.Errorf("reason = %v", s.Reason)
		}
	})

	// ---- Uninstall ----

	t.Run("uninstall removes only lidwake entries", func(t *testing.T) {
		home := fakeHome(t, ".claude")
		in := testInstaller(testCLI, home)
		path := filepath.Join(home, ".claude", "settings.json")
		writeFile(t, path, `{"hooks":{"UserPromptSubmit":[{"hooks":[{"type":"command","command":"echo user-hook"}]}]}}`)

		mustInstall(t, in, agentClaudeCode)
		mustUninstall(t, in, agentClaudeCode)

		if n := len(objects(t, hooksIn(t, path)["UserPromptSubmit"], "UPS")); n != 1 {
			t.Errorf("uninstall must leave the user's hook intact: %d", n)
		}
	})

	t.Run("uninstall dry run does not touch disk", func(t *testing.T) {
		home := fakeHome(t, ".claude")
		in := testInstaller(testCLI, home)
		mustInstall(t, in, agentClaudeCode)

		path := filepath.Join(home, ".claude", "settings.json")
		before := readFile(t, path)
		result := must[Result](t)(in.Uninstall(agentClaudeCode, true))
		if result.Diff == "" || result.Diff == unchangedDiff {
			t.Errorf("diff = %q", result.Diff)
		}
		if readFile(t, path) != before {
			t.Error("dry-run uninstall must not modify files on disk")
		}
	})

	// ---- State ----

	t.Run("install state not installed when clean", func(t *testing.T) {
		home := fakeHome(t, ".claude")
		wantState(t, testInstaller(testCLI, home).State(agentClaudeCode), StateNotInstalled, "clean")
	})

	t.Run("install state installed after install", func(t *testing.T) {
		home := fakeHome(t, ".claude")
		in := testInstaller(testCLI, home)
		mustInstall(t, in, agentClaudeCode)
		wantState(t, in.State(agentClaudeCode), StateInstalled, "after install")
	})

	t.Run("install state not installed after uninstall", func(t *testing.T) {
		home := fakeHome(t, ".claude")
		in := testInstaller(testCLI, home)
		mustInstall(t, in, agentClaudeCode)
		mustUninstall(t, in, agentClaudeCode)
		wantState(t, in.State(agentClaudeCode), StateNotInstalled, "after uninstall")
	})

	t.Run("install state modified externally when command edited", func(t *testing.T) {
		home := fakeHome(t, ".claude")
		in := testInstaller(testCLI, home)
		mustInstall(t, in, agentClaudeCode)

		path := filepath.Join(home, ".claude", "settings.json")
		dict := readJSONFile(t, path)
		hooks := object(t, dict["hooks"], "hooks")
		startArr := hooks["UserPromptSubmit"].([]any)
		inner := objects(t, startArr[0].(map[string]any)["hooks"], "inner")
		inner[0]["command"] = "lidwake acquire TAMPERED --tool claude-code"
		startArr[0] = map[string]any{"hooks": []any{inner[0]}}
		writeJSONFile(t, dict, path)

		wantState(t, in.State(agentClaudeCode), StateModifiedExternally, "tampered")
	})

	// ---- Aider wrapper (both rc files + script) ----

	t.Run("aider install writes to both RC files", func(t *testing.T) {
		home := fakeHome(t, ".local/bin")
		writeFile(t, filepath.Join(home, ".zshrc"), "")
		writeFile(t, filepath.Join(home, ".bashrc"), "")
		// A fake aider binary; the integration is still called directly so the test doesn't depend
		// on the search path.
		writeFile(t, filepath.Join(home, ".local", "bin", "aider"), "")
		if err := os.Chmod(filepath.Join(home, ".local", "bin", "aider"), 0o755); err != nil {
			t.Fatal(err)
		}

		result := must[Result](t)(aiderIntegration{}.install(testContext(testCLI, home), false))

		wantContains(t, readFile(t, filepath.Join(home, ".zshrc")), "# lidwake-aider", "zshrc must contain marker")
		wantContains(t, readFile(t, filepath.Join(home, ".bashrc")), "# lidwake-aider", "bashrc must contain marker")
		if !fileExists(filepath.Join(home, ".local", "bin", "aider-lidwake")) {
			t.Error("wrapper script must be written")
		}
		if result.Summary == "" {
			t.Error("summary must not be empty")
		}
	})

	// Regression: the wrapper's release must carry the same --tool as its acquire. Without it,
	// release defaults to tool "unknown" and targets the key unknown:$$ — which never exists — so
	// the real aider:$$ hold leaks until the dead-process net reaps it (and never, if the PID
	// couldn't be resolved at acquire time).
	t.Run("aider wrapper script acquire and release both carry tool", func(t *testing.T) {
		home := fakeHome(t)
		writeFile(t, filepath.Join(home, ".zshrc"), "")
		writeFile(t, filepath.Join(home, ".bashrc"), "")

		must[Result](t)(aiderIntegration{}.install(testContext(testCLI, home), false))

		script := readFile(t, filepath.Join(home, ".local", "bin", "aider-lidwake"))
		wantContains(t, script, "acquire $$ --tool aider", "acquire must tag the hold with --tool aider")
		wantContains(t, script, "release $$ --tool aider", "release must target the same key acquire created")
		wantNotContains(t, script, "release $$\n", "release must not drop --tool (would target unknown:$$)")
	})

	t.Run("aider uninstall strips from both RC files and removes script", func(t *testing.T) {
		home := fakeHome(t)
		writeFile(t, filepath.Join(home, ".zshrc"), "")
		writeFile(t, filepath.Join(home, ".bashrc"), "")

		ctx := testContext(testCLI, home)
		must[Result](t)(aiderIntegration{}.install(ctx, false))
		result := must[Result](t)(aiderIntegration{}.uninstall(ctx, false))

		wantNotContains(t, readFile(t, filepath.Join(home, ".zshrc")), "lidwake-aider", "zshrc must not contain marker after uninstall")
		wantNotContains(t, readFile(t, filepath.Join(home, ".bashrc")), "lidwake-aider", "bashrc must not contain marker after uninstall")
		if fileExists(filepath.Join(home, ".local", "bin", "aider-lidwake")) {
			t.Error("wrapper script must be removed")
		}
		if result.Diff == "" {
			t.Error("diff must not be empty")
		}
	})

	t.Run("aider install state installed when both R cs and script present", func(t *testing.T) {
		home := fakeHome(t)
		writeFile(t, filepath.Join(home, ".zshrc"), "")
		writeFile(t, filepath.Join(home, ".bashrc"), "")

		must[Result](t)(aiderIntegration{}.install(testContext(testCLI, home), false))

		wantState(t, testInstaller(testCLI, home).State(agentAider), StateInstalled, "after install")
	})

	// ---- Cursor (flat JSON shape) ----

	t.Run("install cursor writes flat shape", func(t *testing.T) {
		home := fakeHome(t, ".cursor")
		mustInstall(t, testInstaller(testCLI, home), agentCursor)

		hooks := hooksIn(t, filepath.Join(home, ".cursor", "hooks.json"))
		acquire, _ := objects(t, hooks["beforeSubmitPrompt"], "beforeSubmitPrompt")[0]["command"].(string)
		wantContains(t, acquire, "acquire --tool cursor", "acquire")
		wantContains(t, acquire, "--ttl "+strconv.Itoa(cursorHoldTTLSeconds), "the acquire must be TTL-capped — stop is the only release signal")
		release, _ := objects(t, hooks["stop"], "stop")[0]["command"].(string)
		wantContains(t, release, "release --tool cursor", "release")
	})

	t.Run("cursor reinstall migrates session-scoped hooks to turn-scoped events", func(t *testing.T) {
		home := fakeHome(t, ".cursor")
		path := filepath.Join(home, ".cursor", "hooks.json")
		writeFile(t, path, `{"version":1,"hooks":{`+
			`"sessionStart":[{"command":"/Applications/lidwake.app/Contents/Helpers/lidwake acquire --tool cursor","_lidwake":true},{"command":"echo user-hook"}],`+
			`"sessionEnd":[{"command":"/Applications/lidwake.app/Contents/Helpers/lidwake release --tool cursor","_lidwake":true}]}}`)

		in := testInstaller(testCLI, home)
		wantState(t, in.State(agentCursor), StateModifiedExternally, "the old shape must read as drifted so the reinstall runs")
		mustInstall(t, in, agentCursor)

		hooks := hooksIn(t, path)
		start := objects(t, hooks["sessionStart"], "sessionStart")
		if len(start) != 1 || start[0]["command"] != "echo user-hook" {
			t.Errorf("our stale sessionStart acquire must be stripped, the user's kept: %v", start)
		}
		if hooks["sessionEnd"] != nil {
			t.Error("an event emptied of our entries must be dropped")
		}
		if hooks["beforeSubmitPrompt"] == nil || hooks["stop"] == nil {
			t.Error("turn-scoped events must be wired")
		}
		wantState(t, in.State(agentCursor), StateInstalled, "after migration")
	})
}

// TestHookInstallerGo covers what the Go port adds or does differently.
func TestHookInstallerGo(t *testing.T) {
	t.Run("default cli path resolves to the installed binary through symlinks", func(t *testing.T) {
		const installed = "/usr/local/libexec/lidwake/lidwake"
		links := map[string]string{
			"/usr/local/bin/lidwake": installed,
			installed:                installed,
			"/Users/me/bin/lw":       "/Users/me/src/lidwake/lidwake",
		}
		eval := func(p string) (string, error) {
			if r, ok := links[p]; ok {
				return r, nil
			}
			return "", os.ErrNotExist
		}
		for exe, want := range map[string]string{
			"/usr/local/bin/lidwake":  installed,
			installed:                 installed,
			"/Users/me/bin/lw":        "/Users/me/src/lidwake/lidwake",
			"/tmp/go-build/x/lidwake": "/tmp/go-build/x/lidwake",
		} {
			if got := resolveCLIPath(exe, eval); got != want {
				t.Errorf("resolveCLIPath(%q) = %q, want %q", exe, got, want)
			}
		}
	})

	t.Run("default cli path canonicalizes a symlinked install dir", func(t *testing.T) {
		const resolved = "/private/usr/local/libexec/lidwake/lidwake"
		eval := func(p string) (string, error) {
			switch p {
			case "/usr/local/bin/lidwake", "/usr/local/libexec/lidwake/lidwake":
				return resolved, nil
			}
			return p, nil
		}
		if got := resolveCLIPath("/usr/local/bin/lidwake", eval); got != "/usr/local/libexec/lidwake/lidwake" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("default cli path of a dev build is its own absolute path", func(t *testing.T) {
		if p := DefaultCLIPath(); !filepath.IsAbs(p) {
			t.Errorf("DefaultCLIPath() = %q, want absolute", p)
		}
	})

	t.Run("new fills defaults", func(t *testing.T) {
		in := New("", "")
		if in.CLIPath == "" || in.Home == "" {
			t.Errorf("New = %+v", in)
		}
		in = New("/x/lidwake", "/h")
		if in.CLIPath != "/x/lidwake" || in.Home != "/h" {
			t.Errorf("New = %+v", in)
		}
	})

	t.Run("new falls back to the account home when HOME is empty", func(t *testing.T) {
		t.Setenv("HOME", "")
		if in := New("/x/lidwake", ""); !filepath.IsAbs(in.Home) {
			t.Errorf("Home = %q, want an absolute path, never one relative to the working directory", in.Home)
		}
	})

	t.Run("agents are listed in canonical order", func(t *testing.T) {
		want := []string{
			agentClaudeCode, agentCodex, agentCursor, agentGeminiCLI,
			agentAider, agentHermes, agentOpenCode, agentCline, agentPi,
		}
		if got := Agents(); !slices.Equal(got, want) {
			t.Errorf("Agents() = %v, want %v", got, want)
		}
	})

	// The agent names are the --tool values the CLI parses into agents.Kind; the two lists must
	// not drift apart.
	t.Run("agent names match the agent kinds", func(t *testing.T) {
		var kinds []string
		for _, k := range agents.All() {
			kinds = append(kinds, string(k))
		}
		if got := Agents(); !slices.Equal(got, kinds) {
			t.Errorf("Agents() = %v, agents.All() = %v", got, kinds)
		}
	})

	// The commands are the contract with the CLI's acquire/release argument parsing.
	t.Run("hook commands are exact", func(t *testing.T) {
		home := fakeHome(t, ".claude", ".codex", ".cursor")
		in := testInstaller(testCLI, home)
		for _, agent := range []string{agentClaudeCode, agentCodex, agentCursor} {
			mustInstall(t, in, agent)
		}
		const cli = testCLI
		claude := hooksIn(t, filepath.Join(home, ".claude", "settings.json"))
		for event, want := range map[string]string{
			"UserPromptSubmit": cli + " acquire $CLAUDE_CODE_SESSION_ID --tool claude-code",
			"Stop":             cli + " release $CLAUDE_CODE_SESSION_ID --tool claude-code",
			"Notification":     cli + " release $CLAUDE_CODE_SESSION_ID --tool claude-code",
			"SubagentStart":    cli + " acquire --tool claude-code --subagent",
			"SubagentStop":     cli + " release --tool claude-code --subagent",
			"SessionEnd":       cli + " release $CLAUDE_CODE_SESSION_ID --tool claude-code",
			"SessionStart":     cli + " acquire $CLAUDE_CODE_SESSION_ID --tool claude-code",
		} {
			if got := firstInnerCommand(t, claude, event); got != want {
				t.Errorf("claude-code %s = %q, want %q", event, got, want)
			}
		}
		for event, want := range map[string]string{"Notification": "idle_prompt", "SessionStart": "clear"} {
			if got := objects(t, claude[event], event)[0]["matcher"]; got != want {
				t.Errorf("claude-code %s matcher = %v, want %q", event, got, want)
			}
		}
		if len(claude) != 7 {
			t.Errorf("claude-code events = %d, want 7: %v", len(claude), claude)
		}

		codex := hooksIn(t, filepath.Join(home, ".codex", "hooks.json"))
		for event, want := range map[string]string{
			"UserPromptSubmit": cli + " acquire --tool codex",
			"Stop":             cli + " release --tool codex",
			"SubagentStart":    cli + " acquire --tool codex --subagent",
			"SubagentStop":     cli + " release --tool codex --subagent",
		} {
			if got := firstInnerCommand(t, codex, event); got != want {
				t.Errorf("codex %s = %q, want %q", event, got, want)
			}
		}
		if len(codex) != 4 {
			t.Errorf("codex events = %d, want 4: %v", len(codex), codex)
		}

		cursor := hooksIn(t, filepath.Join(home, ".cursor", "hooks.json"))
		for event, want := range map[string]string{
			"beforeSubmitPrompt": cli + " acquire --tool cursor --ttl 3600",
			"stop":               cli + " release --tool cursor",
		} {
			if got, _ := objects(t, cursor[event], event)[0]["command"].(string); got != want {
				t.Errorf("cursor %s = %q, want %q", event, got, want)
			}
		}
		if v := readJSONFile(t, filepath.Join(home, ".cursor", "hooks.json"))["version"]; v != float64(1) {
			t.Errorf("a fresh Cursor file is seeded with version 1, got %v", v)
		}
	})

	t.Run("capabilities an agent lacks are refused or no-ops", func(t *testing.T) {
		home := fakeHome(t, ".codex")
		in := testInstaller(testCLI, home)
		_, err := in.InstallBackgroundHold(agentCodex, false)
		if s := requireSkip(t, err); s.Reason != SkipUnsupported ||
			s.Detail != "Background-shell keep-awake not supported for codex" {
			t.Errorf("err = %+v", s)
		}
		for name, uninstall := range map[string]func(string, bool) (Result, error){
			"mcp": in.UninstallMCP, "background": in.UninstallBackgroundHold,
		} {
			if r := must[Result](t)(uninstall(agentCodex, false)); r != nothingRemoved() {
				t.Errorf("%s uninstall = %+v, want nothing to remove", name, r)
			}
		}
		wantState(t, in.MCPState(agentCodex), StateNotInstalled, "mcp")
		wantState(t, in.BackgroundHoldState(agentCodex), StateNotInstalled, "background")
		if fileExists(filepath.Join(home, ".codex", "hooks.json")) {
			t.Error("a refused capability must write nothing")
		}
	})

	t.Run("detects every agent by its own signal", func(t *testing.T) {
		home := fakeHome(t, ".hermes", ".pi", "bin")
		in := testInstaller(testCLI, home)
		if got := in.DetectedAgents(); !slices.Equal(got, []string{agentHermes, agentPi}) {
			t.Errorf("dir-detected = %v, want [hermes pi]", got)
		}
		// Aider, OpenCode and Cline have no config dir to probe: they are found by binary.
		in.SearchPath = []string{filepath.Join(home, "bin")}
		for _, name := range []string{"aider", "opencode", "cline"} {
			writeFile(t, filepath.Join(home, "bin", name), "")
			if err := os.Chmod(filepath.Join(home, "bin", name), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		want := []string{agentAider, agentHermes, agentOpenCode, agentCline, agentPi}
		if got := in.DetectedAgents(); !slices.Equal(got, want) {
			t.Errorf("detected = %v, want %v", got, want)
		}
	})

	t.Run("binary-detected agents skip install when the binary is missing", func(t *testing.T) {
		in := testInstaller(testCLI, fakeHome(t))
		for _, agent := range []string{agentAider, agentCline, agentOpenCode, agentHermes, agentPi} {
			if _, err := in.Install(agent, false); !IsSkip(err, SkipNotInstalled) {
				t.Errorf("%s: err = %v, want SkipNotInstalled", agent, err)
			}
		}
	})

	t.Run("unknown agent", func(t *testing.T) {
		in := testInstaller(testCLI, fakeHome(t))
		if _, err := in.Install("nope", false); err == nil || !strings.Contains(err.Error(), "nope") {
			t.Errorf("Install err = %v", err)
		}
		if _, err := in.Uninstall("nope", false); err == nil {
			t.Error("Uninstall of an unknown agent must fail")
		}
		wantState(t, in.State("nope"), StateNotInstalled, "unknown")
		if in.ConfigPath("nope") != "" || in.SupportsMCP("nope") || in.SupportsBackgroundHold("nope") {
			t.Error("unknown agent has no config or capabilities")
		}
	})

	t.Run("config paths", func(t *testing.T) {
		home := fakeHome(t)
		in := testInstaller(testCLI, home)
		for agent, want := range map[string]string{
			agentClaudeCode: filepath.Join(home, ".claude", "settings.json"),
			agentCodex:      filepath.Join(home, ".codex", "hooks.json"),
			agentCursor:     filepath.Join(home, ".cursor", "hooks.json"),
			agentGeminiCLI:  filepath.Join(home, ".gemini", "settings.json"),
			agentAider:      filepath.Join(home, ".zshrc"),
			agentCline:      filepath.Join(home, ".zshrc"),
			agentHermes:     filepath.Join(home, ".hermes", "config.yaml"),
			agentOpenCode:   filepath.Join(home, ".config", "opencode", "plugins", "lidwake.ts"),
			agentPi:         filepath.Join(home, ".pi", "agent", "extensions", "lidwake.ts"),
		} {
			if got := in.ConfigPath(agent); got != want {
				t.Errorf("ConfigPath(%s) = %q, want %q", agent, got, want)
			}
		}
	})

	t.Run("cursor is detected by its app bundle", func(t *testing.T) {
		home := fakeHome(t, "Applications/Cursor.app")
		if !slices.Contains(testInstaller(testCLI, home).DetectedAgents(), agentCursor) {
			t.Error("Cursor.app in the applications dir must detect Cursor")
		}
	})

	t.Run("binary detection probes the search path", func(t *testing.T) {
		home := fakeHome(t, "bin")
		ctx := testContext(testCLI, home)
		ctx.searchPath = []string{filepath.Join(home, "bin")}
		if ctx.binaryOnPath("tool") {
			t.Error("missing binary detected")
		}
		writeFile(t, filepath.Join(home, "bin", "tool"), "")
		if ctx.binaryOnPath("tool") {
			t.Error("a non-executable file is not a binary")
		}
		if err := os.Chmod(filepath.Join(home, "bin", "tool"), 0o755); err != nil {
			t.Fatal(err)
		}
		if !ctx.binaryOnPath("tool") {
			t.Error("executable not detected")
		}
	})

	t.Run("gemini cli wires SessionStart and SessionEnd from stdin", func(t *testing.T) {
		home := fakeHome(t, ".gemini")
		in := testInstaller(testCLI, home)
		result := mustInstall(t, in, agentGeminiCLI)
		if result.Summary != "wired SessionStart acquire + SessionEnd release hooks" {
			t.Errorf("summary = %q", result.Summary)
		}
		hooks := hooksIn(t, filepath.Join(home, ".gemini", "settings.json"))
		if got := firstInnerCommand(t, hooks, "SessionStart"); got != testCLI+" acquire --tool gemini-cli" {
			t.Errorf("acquire = %q", got)
		}
		if got := firstInnerCommand(t, hooks, "SessionEnd"); got != testCLI+" release --tool gemini-cli" {
			t.Errorf("release = %q", got)
		}
		wantState(t, in.State(agentGeminiCLI), StateInstalled, "gemini")
		mustUninstall(t, in, agentGeminiCLI)
		wantState(t, in.State(agentGeminiCLI), StateNotInstalled, "gemini after uninstall")
	})

	t.Run("summaries match the CLI wording", func(t *testing.T) {
		home := fakeHome(t, ".claude", ".codex", ".cursor")
		in := testInstaller(testCLI, home)
		for agent, want := range map[string]string{
			agentClaudeCode: "wired UserPromptSubmit acquire + Stop release hooks (+ 5 lifecycle hooks)",
			agentCodex:      "wired UserPromptSubmit acquire + Stop release hooks plus 2 sub-agent hooks; trust each in Codex with /hooks",
			agentCursor:     "wired beforeSubmitPrompt/stop hooks",
		} {
			if got := mustInstall(t, in, agent).Summary; got != want {
				t.Errorf("%s summary = %q, want %q", agent, got, want)
			}
		}
		for agent, want := range map[string]string{
			agentClaudeCode: "removed hook entries",
			agentCodex:      "removed Codex hook entries",
			agentCursor:     "removed Cursor hook entries",
		} {
			if got := mustUninstall(t, in, agent).Summary; got != want {
				t.Errorf("%s uninstall summary = %q, want %q", agent, got, want)
			}
		}
	})

	t.Run("dry run reports the same diff the real run applies", func(t *testing.T) {
		home := fakeHome(t, ".claude")
		in := testInstaller(testCLI, home)
		path := filepath.Join(home, ".claude", "settings.json")
		writeFile(t, path, `{"model": "opus"}`)

		dry := must[Result](t)(in.Install(agentClaudeCode, true))
		applied := mustInstall(t, in, agentClaudeCode)
		if dry.Diff != applied.Diff {
			t.Errorf("dry diff differs:\n%s\n---\n%s", dry.Diff, applied.Diff)
		}
		if !strings.HasPrefix(dry.Diff, "BEFORE:\n{\n  \"model\": \"opus\"\n}\n---\nAFTER:\n{") {
			t.Errorf("diff format: %q", dry.Diff)
		}
		if again := mustInstall(t, in, agentClaudeCode); again.Diff != unchangedDiff {
			t.Errorf("reinstall diff = %q, want (unchanged)", again.Diff)
		}
	})

	t.Run("hook commands quote an unusual cli path", func(t *testing.T) {
		home := fakeHome(t, ".claude")
		in := testInstaller("/Apps/My Tools/lidwake", home)
		mustInstall(t, in, agentClaudeCode)
		got := firstInnerCommand(t, hooksIn(t, filepath.Join(home, ".claude", "settings.json")), "UserPromptSubmit")
		if got != "'/Apps/My Tools/lidwake' acquire $CLAUDE_CODE_SESSION_ID --tool claude-code" {
			t.Errorf("acquire = %q", got)
		}
		wantState(t, in.State(agentClaudeCode), StateInstalled, "quoted path")
	})

	t.Run("written json is sorted, two-space indented and unescaped", func(t *testing.T) {
		home := fakeHome(t, ".cursor")
		mustInstall(t, testInstaller("/opt/a&b/lidwake", home), agentCursor)
		data, err := os.ReadFile(filepath.Join(home, ".cursor", "hooks.json"))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.HasPrefix(data, []byte("{\n  \"hooks\": {\n    \"beforeSubmitPrompt\": [")) {
			t.Errorf("layout:\n%s", data)
		}
		wantContains(t, string(data), "'/opt/a&b/lidwake' acquire", "no HTML or slash escaping")
		if bytes.HasSuffix(data, []byte("\n")) {
			t.Error("no trailing newline")
		}
	})
}
