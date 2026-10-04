package hooks

import (
	"bytes"
	"encoding/json"
	"strconv"
)

const agentPi = "pi"

// piHoldTTLSeconds is the backstop TTL on each acquire, refreshed per turn (a re-acquire adopts
// the new expiry). TTL expiry releases unconditionally — even a CPU-busy tree — so it must sit
// safely above any real single agent run. With --pid attached, the dead-process and CPU-idle nets
// do the real cleanup; the TTL only catches a hold whose lifecycle hooks were all missed and to
// which no PID could attach, capping that leak at 4 hours instead of the 24-hour backstop.
const piHoldTTLSeconds = 4 * 3600

// piIntegration: a TypeScript extension at ~/.pi/agent/extensions/lidwake.ts. Detected by the
// ~/.pi directory.
type piIntegration struct{}

func init() { register(piIntegration{}) }

func (piIntegration) agent() string { return agentPi }

func (piIntegration) detected(ctx hookContext) bool { return exists(ctx.homePath(".pi")) }

func (piIntegration) configPath(ctx hookContext) string {
	return ctx.homePath(".pi", "agent", "extensions", "lidwake.ts")
}

func (i piIntegration) install(ctx hookContext, dryRun bool) (Result, error) {
	return i.plugin(ctx).install(dryRun)
}

func (i piIntegration) uninstall(ctx hookContext, dryRun bool) (Result, error) {
	return i.plugin(ctx).uninstall(dryRun)
}

func (i piIntegration) state(ctx hookContext) InstallState { return i.plugin(ctx).state() }

func (piIntegration) plugin(ctx hookContext) filePlugin {
	return filePlugin{
		root:           ctx.homePath(".pi", "agent", "extensions"),
		fileName:       "lidwake.ts",
		content:        piExtensionTS(ctx.cliPath),
		installSummary: "wrote Pi extension",
	}
}

// piExtensionTS is the canonical Pi extension. Pi auto-discovers .ts extensions and calls
// pi.on(<event>, handler) from the default export. The hold is turn-scoped: agent_start acquires,
// agent_settled releases, and session_shutdown releases again as a safety net for a turn
// interrupted by exit. Pi has no session-id env var or stdin payload, so the id is the session
// file path (undefined for ephemeral sessions, which fall back to the pid).
//
// agent_settled, not agent_end: after agent_end Pi may still auto-retry, auto-compact and retry,
// or drain queued follow-up messages; agent_settled means "Pi will not continue running
// automatically", which is exactly this hold's meaning.
//
// Bracketing the session instead (session_start/session_shutdown) was wrong both ways: the Mac
// stayed awake for the whole pi process lifetime, idle time at the prompt included, and since the
// only acquire was at the start, an idle release left the rest of the session unprotected with
// nothing to re-acquire. Re-acquiring per turn is safe because acquire is idempotent per key.
//
// stdio: "ignore" matters because the safety-net release is routinely a no-op: execFileSync
// inherits stderr by default, so release's "released nothing" would print into the TUI.
//
// The acquire carries --pid and --ttl. Pi is Node-hosted, so its executable path is Node's and the
// CLI's executable-path parent walk can't find it; without --pid the hold had no owner, out of
// reach of the dead-process and CPU-idle nets. Extensions load in-process, so process.pid is the
// Pi host PID whatever wrapper (npm, Homebrew, Nix) launched it. The TTL is a second backstop for
// the residual missed-everything case, refreshed on each agent_start like Cursor's.
//
// Device-verified against pi 0.83.0: the hold appears when a turn starts and is gone before the
// process exits, with no hold while sitting at the prompt.
func piExtensionTS(cliPath string) string {
	return `import { execFileSync } from "node:child_process"

function run(args) {
  try { execFileSync(` + jsStringLiteral(cliPath) + `, args, { stdio: "ignore" }) } catch (_) {}
}

export default function (pi) {
  const id = (ctx) => ctx?.sessionManager?.getSessionFile?.() ?? String(process.pid)
  pi.on("agent_start", async (_event, ctx) =>
    run(["acquire", id(ctx), "--tool", "pi", "--pid", String(process.pid), "--ttl", "` + strconv.Itoa(piHoldTTLSeconds) + `"]))
  pi.on("agent_settled", async (_event, ctx) => run(["release", id(ctx), "--tool", "pi"]))
  pi.on("session_shutdown", async (_event, ctx) => run(["release", id(ctx), "--tool", "pi"]))
}`
}

// jsStringLiteral renders s as a double-quoted JS string literal. JSON string encoding is a subset
// of JS, so quotes or backslashes in the path can't break the generated code; U+2028/U+2029 come
// out escaped, which older JS engines require.
func jsStringLiteral(s string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return strconv.Quote(s)
	}
	return string(bytes.TrimSuffix(buf.Bytes(), []byte("\n")))
}
