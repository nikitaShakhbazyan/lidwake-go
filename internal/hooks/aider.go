package hooks

const agentAider = "aider"

// aiderIntegration: Aider has no hook system, so lidwake installs a shell wrapper — a script plus
// a shell alias in ~/.zshrc / ~/.bashrc that brackets `aider` with acquire/release. Detected by
// the aider binary on the search path.
type aiderIntegration struct{}

func init() { register(aiderIntegration{}) }

func (aiderIntegration) agent() string { return agentAider }

func (aiderIntegration) detected(ctx hookContext) bool { return ctx.binaryOnPath("aider") }

func (aiderIntegration) configPath(ctx hookContext) string { return ctx.homePath(".zshrc") }

func (i aiderIntegration) install(ctx hookContext, dryRun bool) (Result, error) {
	return i.wrapper(ctx).install(dryRun)
}

func (i aiderIntegration) uninstall(ctx hookContext, dryRun bool) (Result, error) {
	return i.wrapper(ctx).uninstall(dryRun)
}

func (i aiderIntegration) state(ctx hookContext) InstallState { return i.wrapper(ctx).state() }

func (aiderIntegration) wrapper(ctx hookContext) shellWrapper {
	return shellWrapper{tool: agentAider, ctx: ctx}
}
