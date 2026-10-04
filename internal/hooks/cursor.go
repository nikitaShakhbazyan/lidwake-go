package hooks

const agentCursor = "cursor"

// cursorHoldTTLSeconds caps each Cursor hold: turns are minutes, not hours, so anything still held
// after this long is stale (a missed stop).
const cursorHoldTTLSeconds = 3600

// cursorIntegration: ~/.cursor/hooks.json, beforeSubmitPrompt → acquire, stop → release. Cursor
// uses the flat shape ({"command": …} entries, no inner hooks wrapper) and a "version" field on a
// fresh file. There is no session-id env var; Cursor passes session_id on the hook's stdin.
//
// Turn-scoped events, not sessionStart/sessionEnd: a Cursor session is a long-lived chat — turns
// finish without ending it — so a session-scoped hold pinned the Mac awake long after the agent
// stopped, and every open chat stacked another. The usual backstops can't catch these: each hold
// attaches to the single long-lived Cursor app process, so process-death cleanup never fires, and
// the app's own UI activity keeps its tree from ever reading CPU-idle. That is also why the acquire
// carries a TTL: with stop as the only release signal, the TTL is the last-resort cap, refreshed
// on every prompt.
type cursorIntegration struct{}

func init() { register(cursorIntegration{}) }

func (cursorIntegration) agent() string { return agentCursor }

func (cursorIntegration) detected(ctx hookContext) bool {
	return exists(ctx.homePath(".cursor")) || exists(ctx.applicationsPath("Cursor.app"))
}

func (cursorIntegration) configPath(ctx hookContext) string {
	return ctx.homePath(".cursor", "hooks.json")
}

func (i cursorIntegration) install(ctx hookContext, dryRun bool) (Result, error) {
	return i.shape(ctx).install(dryRun)
}

func (i cursorIntegration) uninstall(ctx hookContext, dryRun bool) (Result, error) {
	return i.shape(ctx).uninstall(dryRun)
}

func (i cursorIntegration) state(ctx hookContext) InstallState {
	return i.shape(ctx).state()
}

func (i cursorIntegration) shape(ctx hookContext) flatJSONHookShape {
	return flatJSONHookShape{
		configPath: i.configPath(ctx),
		entries: []eventCommand{
			{"beforeSubmitPrompt", ctx.hookCommand("acquire", agentCursor, cmdOpts{ttlSeconds: cursorHoldTTLSeconds})},
			{"stop", ctx.hookCommand("release", agentCursor, cmdOpts{})},
		},
		baseDocument:     map[string]any{"version": 1},
		installSummary:   "wired beforeSubmitPrompt/stop hooks",
		uninstallSummary: "removed Cursor hook entries",
	}
}

// mcpShape is gated off until device-verified: only agents whose MCP config format has been
// confirmed on a real install return a shape. Cursor is believed to read MCP servers from
// ~/.cursor/mcp.json under mcpServers (same entry shape as Claude Code); once verified, return
//
//	&mcpServerShape{configPath: ctx.homePath(".cursor", "mcp.json"), containerKey: "mcpServers",
//		serverName: mcpServerName, entry: ctx.mcpEntry(agentCursor)}
func (cursorIntegration) mcpShape(hookContext) *mcpServerShape { return nil }
