package hooks

const agentCodex = "codex"

// codexIntegration: ~/.codex/hooks.json, UserPromptSubmit → acquire, Stop → release.
//
//  1. Nested matcher-group shape. Codex copied Claude Code's hook events, stdin payload and nested
//     hooks.json structure; codexHookShape writes it. The flat form is silently ignored — it never
//     appears in /hooks and can't be trusted.
//  2. UserPromptSubmit, not SessionStart. SessionStart fires only for a brand-new session; Codex
//     resumes the last conversation by default and a resume does not fire it, so a SessionStart
//     hook misses most real use. UserPromptSubmit fires on every prompt, new session or resumed.
//  3. Stop releases at turn end, carrying the same session_id acquire keys on, so each turn is
//     bracketed like Claude Code's instead of leaning on the CPU-idle sweep. The sweep and the
//     process-exit watcher stay as backstops for an Esc-interrupt (which fires no Stop) and for
//     installs the daemon can't process-watch.
//  4. Hooks fire only in the interactive TUI, not `codex exec`, which ignores hooks.json; that path
//     relies on the daemon's process sniffing.
//  5. Hook trust. Codex won't run a command hook until the user trusts it via /hooks, which stamps
//     a trusted_hash into config.toml keyed per handler by position and command. lidwake can't
//     trust on the user's behalf, so the installer surfaces that step and a reinstall leaves a
//     correct handler in place so the hash keeps matching.
//  6. Sub-agents. SubagentStart/SubagentStop carry the parent's session_id and the sub-agent's own
//     id in agent_id; a backgrounded sub-agent outlives the parent turn's Stop, so those hooks use
//     --subagent and key on agent_id. Each new handler is its own one-time /hooks approval.
//
// Codex exposes no session-id env var (CODEX_THREAD_ID is something else, and only the stdin field
// is documented), so the commands carry no positional key: the CLI reads session_id from stdin.
type codexIntegration struct{}

func init() { register(codexIntegration{}) }

func (codexIntegration) agent() string { return agentCodex }

func (codexIntegration) detected(ctx hookContext) bool {
	return exists(ctx.homePath(".codex"))
}

func (codexIntegration) configPath(ctx hookContext) string {
	return ctx.homePath(".codex", "hooks.json")
}

func (i codexIntegration) install(ctx hookContext, dryRun bool) (Result, error) {
	return i.shape(ctx).install(dryRun)
}

func (i codexIntegration) uninstall(ctx hookContext, dryRun bool) (Result, error) {
	return i.shape(ctx).uninstall(dryRun)
}

func (i codexIntegration) state(ctx hookContext) InstallState {
	return i.shape(ctx).state()
}

func (i codexIntegration) shape(ctx hookContext) codexHookShape {
	tool := agentCodex
	subagent := cmdOpts{subagent: true}
	return codexHookShape{
		configPath:     i.configPath(ctx),
		acquireEvent:   "UserPromptSubmit",
		acquireCommand: ctx.hookCommand("acquire", tool, cmdOpts{}),
		releaseEvent:   "Stop",
		releaseCommand: ctx.hookCommand("release", tool, cmdOpts{}),
		obsoleteEvents: []string{"SessionStart"},
		extraHandlers: []eventCommand{
			{"SubagentStart", ctx.hookCommand("acquire", tool, subagent)},
			{"SubagentStop", ctx.hookCommand("release", tool, subagent)},
		},
	}
}
