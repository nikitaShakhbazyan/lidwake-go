package hooks

const agentCline = "cline"

// clineIntegration: the same shell-wrapper approach as Aider — a wrapper script plus a shell
// alias, since Cline has no terminal hook system. Detected by the cline binary on the search path.
//
// Limited: this wraps only terminal `cline` invocations and misses in-editor VS Code sessions.
// Cline's native ~/Documents/Cline/Rules/Hooks/ would be the proper path for those.
type clineIntegration struct{}

func init() { register(clineIntegration{}) }

func (clineIntegration) agent() string { return agentCline }

func (clineIntegration) detected(ctx hookContext) bool { return ctx.binaryOnPath("cline") }

func (clineIntegration) configPath(ctx hookContext) string { return ctx.homePath(".zshrc") }

func (i clineIntegration) install(ctx hookContext, dryRun bool) (Result, error) {
	return i.wrapper(ctx).install(dryRun)
}

func (i clineIntegration) uninstall(ctx hookContext, dryRun bool) (Result, error) {
	return i.wrapper(ctx).uninstall(dryRun)
}

func (i clineIntegration) state(ctx hookContext) InstallState { return i.wrapper(ctx).state() }

func (clineIntegration) wrapper(ctx hookContext) shellWrapper {
	return shellWrapper{tool: agentCline, ctx: ctx}
}
