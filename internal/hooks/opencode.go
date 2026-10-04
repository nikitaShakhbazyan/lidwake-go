package hooks

import "strings"

const agentOpenCode = "opencode"

// openCodeIntegration: a TypeScript plugin at ~/.config/opencode/plugins/lidwake.ts. Detected by
// the opencode binary on the search path.
type openCodeIntegration struct{}

func init() { register(openCodeIntegration{}) }

func (openCodeIntegration) agent() string { return agentOpenCode }

func (openCodeIntegration) detected(ctx hookContext) bool { return ctx.binaryOnPath("opencode") }

func (openCodeIntegration) configPath(ctx hookContext) string {
	return ctx.homePath(".config", "opencode", "plugins", "lidwake.ts")
}

func (i openCodeIntegration) install(ctx hookContext, dryRun bool) (Result, error) {
	return i.plugin(ctx).install(dryRun)
}

func (i openCodeIntegration) uninstall(ctx hookContext, dryRun bool) (Result, error) {
	return i.plugin(ctx).uninstall(dryRun)
}

func (i openCodeIntegration) state(ctx hookContext) InstallState { return i.plugin(ctx).state() }

func (openCodeIntegration) plugin(ctx hookContext) filePlugin {
	return filePlugin{
		root:           ctx.homePath(".config", "opencode", "plugins"),
		fileName:       "lidwake.ts",
		content:        openCodePluginTS(ctx.cliPath),
		installSummary: "wrote OpenCode plugin",
	}
}

// openCodeEscaper escapes the CLI path for a spot that is inside a JS template literal and a
// double-quoted shell string at once: backslashes, backticks, $ (template interpolation) and
// double quotes all need a backslash.
var openCodeEscaper = strings.NewReplacer(`\`, `\\`, "`", "\\`", "$", `\$`, `"`, `\"`)

// openCodePluginTS is the canonical OpenCode plugin. It acquires on session.created only:
// session.idle fires per turn (every time the agent finishes responding, not at session end), so
// releasing on it would drop the hold mid-session — the same trap as Codex's per-turn Stop.
// Release instead rides the daemon's process-exit watcher when the opencode process exits. The
// session id of session.created (a Session object) is event.properties.info.id.
//
// Device-verified against opencode 1.2.17: plugins load from ~/.config/opencode/plugins/ (plural),
// ({ $ }) and ({ event }) destructuring work, and this exact plugin fired
// `acquire ses_… --tool opencode` on session.created.
func openCodePluginTS(cliPath string) string {
	return "export const Lidwake = async ({ $ }) => {\n" +
		"  return {\n" +
		"    event: async ({ event }) => {\n" +
		"      if (event.type === \"session.created\") await $`\"" + openCodeEscaper.Replace(cliPath) +
		"\" acquire ${event.properties.info.id} --tool opencode`\n" +
		"    }\n" +
		"  }\n" +
		"}"
}
