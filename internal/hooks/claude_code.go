package hooks

const agentClaudeCode = "claude-code"

// claudeCodeIntegration: ~/.claude/settings.json, UserPromptSubmit → acquire, Stop → release.
//
// Activity-scoped, not session-scoped. Wiring SessionStart/SessionEnd held a sleep block for the
// entire session, including all the time it sat idle at the prompt. Claude Code brackets each
// working turn instead: UserPromptSubmit fires when a prompt is submitted and Stop when the agent
// finishes responding, so the Mac is kept awake only while the agent actually works.
//
// An Esc-interrupt is the one turn end that fires no Stop, and Claude Code has no interrupt hook.
// The reliable catch is the daemon's CPU-idle sweep. The Notification hook matched to idle_prompt
// is a best-effort fast path: when Claude does emit its "waiting for your input" notification the
// hold is released sooner, but that notification is gated by version, focus and notification
// channel and often doesn't fire. The process-exit watcher covers a terminal closed mid-turn.
//
// Session retirement. Claude Code retires a session id inside a living process: /clear, in-REPL
// /resume and /fork, and plan approval with "clear context" all end session A and mint a new id B
// in place. No Stop fires for A on the plan-approval path, so A's hold would linger as a phantom
// "1 agent working" until the CPU-idle sweep reaped it, while every net keyed on the still-alive
// pid stays blind. SessionEnd fires exactly at that boundary with the retiring id on stdin, so
// SessionEnd → release is an exact match for the orphaned hold. It also fires on graceful exit,
// which just releases sooner than the process-exit watcher. This is release-only cleanup; it does
// not bring back session-scoped holding.
//
// The companion gap: after a clear-context plan approval, B's first message (the plan) bypasses
// UserPromptSubmit, yet B immediately starts a long run and fires Stop at its end. SessionStart
// matched to source "clear" acquires B at the boundary, so the plan run is held from its first
// instant. For a plain /clear (same source, no auto-run) the acquire is reaped by the idle sweep —
// a bounded cost, taken deliberately over an unheld plan run. Other sources (startup, resume,
// fork) land at an idle prompt, where holding would be the session-scoped mistake again.
//
// Claude Code also exposes the session id to hooks as $CLAUDE_CODE_SESSION_ID (not
// $CLAUDE_SESSION_ID, which expands empty). The CLI prefers session_id from the hook's stdin; the
// env-var positional is a fallback. One session id keys all hooks, so a multi-turn session cycles
// acquire → release → acquire on one idempotent key.
//
// Sub-agents. A backgrounded sub-agent keeps running after the parent turn's Stop, which would
// release the turn hold and let the Mac sleep mid-work. SubagentStart/SubagentStop carry the
// parent's session_id and the sub-agent's own agent_id, so those hooks use --subagent and key on
// agent_id: a distinct <tool>:<agent_id> hold that survives the parent's Stop and is released only
// by that sub-agent's own SubagentStop. (Keying them on session_id would drop the parent's turn
// hold the moment a foreground sub-agent finished.) The sub-agent hold carries the parent's PID —
// sub-agents run in-process — so the CPU-idle and dead-process nets still cover a missed
// SubagentStop.
type claudeCodeIntegration struct{}

func init() { register(claudeCodeIntegration{}) }

func (claudeCodeIntegration) agent() string { return agentClaudeCode }

func (claudeCodeIntegration) detected(ctx hookContext) bool {
	return exists(ctx.homePath(".claude"))
}

func (claudeCodeIntegration) configPath(ctx hookContext) string {
	return ctx.homePath(".claude", "settings.json")
}

func (i claudeCodeIntegration) install(ctx hookContext, dryRun bool) (Result, error) {
	return i.shape(ctx).install(dryRun)
}

func (i claudeCodeIntegration) uninstall(ctx hookContext, dryRun bool) (Result, error) {
	return i.shape(ctx).uninstall(dryRun)
}

func (i claudeCodeIntegration) state(ctx hookContext) InstallState {
	return i.shape(ctx).state()
}

const claudeSessionVar = "$CLAUDE_CODE_SESSION_ID"

func (i claudeCodeIntegration) shape(ctx hookContext) nestedJSONHookShape {
	tool := agentClaudeCode
	session := cmdOpts{sessionVar: claudeSessionVar}
	subagent := cmdOpts{subagent: true}
	return nestedJSONHookShape{
		configPath:     i.configPath(ctx),
		startEvent:     "UserPromptSubmit",
		endEvent:       "Stop",
		acquireCommand: ctx.hookCommand("acquire", tool, session),
		releaseCommand: ctx.hookCommand("release", tool, session),
		// Notification/idle_prompt: the fast-path release after an Esc-interrupt.
		// SubagentStart/SubagentStop key on the sub-agent's agent_id from stdin (no session
		// positional — that would be the parent's), so a foreground sub-agent's start+stop is
		// net-neutral on the parent hold. SessionEnd/SessionStart(clear) bracket in-process
		// session retirement; both read the right id from stdin (the retiring one, the new one).
		extraHandlers: []extraHandler{
			{event: "Notification", command: ctx.hookCommand("release", tool, session), matcher: "idle_prompt"},
			{event: "SubagentStart", command: ctx.hookCommand("acquire", tool, subagent)},
			{event: "SubagentStop", command: ctx.hookCommand("release", tool, subagent)},
			{event: "SessionEnd", command: ctx.hookCommand("release", tool, session)},
			{event: "SessionStart", command: ctx.hookCommand("acquire", tool, session), matcher: "clear"},
		},
	}
}

// mcpShape: MCP lives in the global ~/.claude.json (mcpServers, user scope), whose existing entries
// are {"type":"stdio","command":…,"args":[…]}.
func (claudeCodeIntegration) mcpShape(ctx hookContext) *mcpServerShape {
	return &mcpServerShape{
		configPath:   ctx.homePath(".claude.json"),
		containerKey: "mcpServers",
		serverName:   mcpServerName,
		entry:        ctx.mcpEntry(agentClaudeCode),
	}
}

// backgroundBashShape: the opt-in PreToolUse/Bash `acquire --if-background`, in the same
// settings.json as the core hooks but installed and removed independently. Claude Code's Bash tool
// exposes a run_in_background boolean in the PreToolUse tool_input, and the matcher narrows the
// hook to tool_name, so "Bash" fires only for the shell tool. The command requests the 24 h
// ceiling as its --ttl; the daemon clamps it to the live manualHoldMaxHours.
func (i claudeCodeIntegration) backgroundBashShape(ctx hookContext) *backgroundBashHookShape {
	return &backgroundBashHookShape{
		configPath: i.configPath(ctx),
		matcher:    "Bash",
		command:    ctx.backgroundAcquireCommand(agentClaudeCode, backgroundBashTTLSeconds),
	}
}
