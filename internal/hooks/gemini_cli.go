package hooks

const agentGeminiCLI = "gemini-cli"

// geminiCLIIntegration: ~/.gemini/settings.json, SessionStart → acquire, SessionEnd → release. The
// same nested shape as Claude Code, but no session-id env var: the CLI reads session_id from the
// hook's stdin.
type geminiCLIIntegration struct{}

func init() { register(geminiCLIIntegration{}) }

func (geminiCLIIntegration) agent() string { return agentGeminiCLI }

func (geminiCLIIntegration) detected(ctx hookContext) bool {
	return exists(ctx.homePath(".gemini"))
}

func (geminiCLIIntegration) configPath(ctx hookContext) string {
	return ctx.homePath(".gemini", "settings.json")
}

func (i geminiCLIIntegration) install(ctx hookContext, dryRun bool) (Result, error) {
	return i.shape(ctx).install(dryRun)
}

func (i geminiCLIIntegration) uninstall(ctx hookContext, dryRun bool) (Result, error) {
	return i.shape(ctx).uninstall(dryRun)
}

func (i geminiCLIIntegration) state(ctx hookContext) InstallState {
	return i.shape(ctx).state()
}

func (i geminiCLIIntegration) shape(ctx hookContext) nestedJSONHookShape {
	return nestedJSONHookShape{
		configPath:     i.configPath(ctx),
		startEvent:     "SessionStart",
		endEvent:       "SessionEnd",
		acquireCommand: ctx.hookCommand("acquire", agentGeminiCLI, cmdOpts{}),
		releaseCommand: ctx.hookCommand("release", agentGeminiCLI, cmdOpts{}),
	}
}

// mcpShape is gated off until device-verified. Gemini CLI is believed to read MCP servers from
// mcpServers in the same ~/.gemini/settings.json that holds its hooks; once verified, return
//
//	&mcpServerShape{configPath: ctx.homePath(".gemini", "settings.json"), containerKey: "mcpServers",
//		serverName: mcpServerName, entry: ctx.mcpEntry(agentGeminiCLI)}
func (geminiCLIIntegration) mcpShape(hookContext) *mcpServerShape { return nil }
