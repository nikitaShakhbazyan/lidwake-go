package cli

import (
	"errors"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/nikitaShakhbazyan/lidwake-go/internal/activity"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/agents"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/ipc"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/policy"
)

// hookFailure reports a failure of `acquire`/`release`, which run inside agent hooks where a
// nonzero exit is interpreted by the agent: Claude Code treats a UserPromptSubmit hook exiting 2
// as "block and erase the user's prompt". A missed acquire costs at most one unprotected turn; a
// blocked prompt destroys the user's input. So every failure warns on stderr and exits 0. Exit 2
// is reserved for a human at a TTY misusing the command.
func (a *app) hookFailure(message string) int {
	a.warnf("%s", message)
	if a.stdinIsTTY() {
		return 2
	}
	return 0
}

// resolveOwningPID is the PID the daemon should watch for a session-keyed hold, in trust order:
//
//  1. --pid from the hook itself. Pi's extension runs in-process in the host, so the
//     process.pid it passes is the agent's own PID, authoritative however the agent is packaged.
//     It is checked for liveness first, so a stale value can't bind the hold to a recycled PID
//     (the daemon would then dead-process-release a live turn at once).
//  2. The executable-path parent walk, the normal case for agents that run as their own binary.
//     The parent is the shell running the hook command, which exits as soon as lidwake returns,
//     so the walk continues to the first ancestor whose binary is a known agent.
//  3. For Pi only: an argv-based walk, authorized by the AI_AGENT=pi / PI_CODING_AGENT=true
//     markers Pi sets in its own environment (which this process inherits). It covers an
//     installed extension that predates --pid: a Node-hosted Pi's executable is Node's, so step
//     2 finds nothing. The marker gate keeps the walk from binding an arbitrary Node ancestor of
//     a non-Pi invocation.
//
// Returns -1 when nothing resolves: the daemon then must not process-watch, which is safer than
// watching the wrong PID.
func (a *app) resolveOwningPID(hookPID int, tool string) int {
	if hookPID > 0 {
		if a.resolver.ProcessAlive(hookPID) {
			return hookPID
		}
		a.warnf("--pid %d is not alive — falling back to process-tree resolution", hookPID)
	}
	if walked := a.resolver.OwningAgentPID(agents.AllBinaryNames()); walked > 0 {
		return walked
	}
	if tool == string(agents.Pi) && agents.EnvironmentMarksPi(a.environment()) {
		return a.resolver.OwningAgentPIDByArgv(agents.ArgvIsPi)
	}
	return -1
}

func (a *app) environment() map[string]string {
	env := map[string]string{}
	for _, kv := range a.environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	return env
}

func (a *app) acquire(args []string) int {
	p := ParseArgs(args)
	tool, ok := p.Option("--tool")
	if !ok {
		tool = policy.UnknownTool
	}
	reason, _ := p.Option("--reason")

	var ttl *float64
	if raw, given := p.Option("--ttl"); given {
		if v, err := strconv.ParseFloat(raw, 64); err == nil && !math.IsInf(v, 0) && !math.IsNaN(v) && v > 0 {
			ttl = &v
		} else {
			a.warnf("ignoring invalid --ttl '%s'", raw)
		}
	}
	hookPID := 0
	if raw, given := p.Option("--pid"); given {
		if n, err := strconv.ParseInt(raw, 10, 32); err == nil && n > 0 {
			hookPID = int(n)
		} else {
			a.warnf("ignoring invalid --pid '%s'", raw)
		}
	}

	var key string
	watched := -1
	switch kind := agents.Kind(tool); {
	case p.Flag("--if-background"):
		// Opt-in background-shell hook (PreToolUse/Bash). A command the agent launched with
		// run_in_background keeps running past the turn's Stop and fires no completion hook, so
		// this PreToolUse is the only signal, and the hold must be TTL-bounded. A foreground
		// command (or any non-background call) yields no plan: no hold, exit 0 silently. That is
		// the common case — every foreground Bash call — so it must never warn. A background
		// command gets a per-invocation `<tool>:bg-<id>` hold with the owning agent PID attached
		// (so the dead-process net still reaps it if the whole agent dies) and a TTL the daemon
		// clamps to manualHoldMaxHours.
		var requested *time.Duration
		if ttl != nil {
			d := secondsToDuration(*ttl)
			requested = &d
		}
		plan, ok := activity.PlanBackgroundHold(a.stdin.Payload(), tool, requested, activity.FreshBackgroundID())
		if !ok {
			return 0
		}
		key = plan.Key
		seconds := plan.TTL.Seconds()
		ttl = &seconds
		watched = a.resolver.OwningAgentPID(agents.AllBinaryNames())
	case p.Flag("--subagent"):
		// Sub-agent lifecycle hook (SubagentStart). Key on the sub-agent's own agent_id, not
		// session_id, which on these payloads is the parent's and would collide with the parent
		// turn's hold. This distinct hold survives the parent Stop and is released only by the
		// matching SubagentStop. No agent exposes the sub-agent id any other way, so a missing
		// agent_id fails soft rather than falling back to the parent session.
		agentID, ok := a.stdin.AgentID()
		if !ok {
			return a.hookFailure("acquire --subagent: no agent_id on stdin — ignored")
		}
		key = policy.SessionKey(tool, agentID)
		// Sub-agents run in-process under the parent agent, so the parent walk resolves the same
		// owning PID; the daemon's CPU-idle and dead-process nets then cover a missed SubagentStop.
		watched = a.resolver.OwningAgentPID(agents.AllBinaryNames())
	case kind.IsGatewayScoped():
		// Gateway-style agent (Hermes): one long-lived process multiplexes every session, so its
		// per-session hooks don't bracket a process lifetime and the session id is irrelevant.
		// Every session coalesces onto one `<tool>:gateway` hold that watches the gateway process
		// from its pid-file (the parent walk can't find it: the executable is a generic
		// interpreter). With the gateway PID attached, the CPU-idle and dead-process nets release
		// the hold when the gateway goes quiet or dies, which makes a missed end hook safe.
		key = tool + ":gateway"
		watched = a.resolver.GatewayPIDInHome(a.home, kind.GatewayPIDFileRelativePath())
	default:
		// Prefer the session id from the hook's stdin JSON over the positional (a shell env-var
		// expansion in the hook command, fragile across agents). An empty positional — an
		// expansion that came up empty — reads as "no key", not as the key "".
		id, ok := a.sessionID(p)
		if !ok {
			return a.hookFailure("acquire: no session key (stdin payload or positional) — ignored")
		}
		key = policy.SessionKey(tool, id)
		watched = a.resolveOwningPID(hookPID, tool)
	}
	a.log.Debug("acquire", "key", key, "pid", watched)

	pid := 0
	if watched > 0 {
		pid = watched
	}
	wantsDisplay := p.Flag("--display")
	resp, err := a.send(ipc.Request{
		Op:          ipc.OpAcquire,
		Key:         key,
		Tool:        tool,
		Reason:      reason,
		PID:         pid,
		ProcessName: tool,
		TTL:         ttl,
		Display:     wantsDisplay,
	})
	switch {
	case errors.Is(err, ipc.ErrDaemonUnreachable):
		a.warnf("daemon not running (acquire ignored)")
	case err != nil:
		// Any transport failure (timeout, short read, malformed reply) must not fail the hook.
		a.warnf("acquire failed (%v) — ignored", err)
	case !resp.OK:
		a.warnf("acquire refused: %s", orDefault(resp.Error, "?"))
	case wantsDisplay && resp.DisplayApplied == nil:
		// Version skew: an old daemon ignores the display field and omits the echo, so the
		// display is NOT protected and silence would hide that.
		a.warnf("the running daemon predates --display — the display is NOT being kept awake. Update lidwake and retry.")
	}
	return 0
}

// sessionID is the hook's session id: stdin's session_id, else the trimmed non-empty positional.
func (a *app) sessionID(p Args) (string, bool) {
	if id, ok := a.stdin.SessionID(); ok {
		return id, true
	}
	if positional, ok := p.Positional(0); ok {
		if trimmed := strings.TrimSpace(positional); trimmed != "" {
			return trimmed, true
		}
	}
	return "", false
}

func (a *app) release(args []string) int {
	p := ParseArgs(args)
	namedTool, named := p.Option("--tool")
	tool := namedTool
	if !named {
		tool = policy.UnknownTool
	}

	// `release --all` is a human command, not an agent hook, so unlike the paths below it
	// reports its outcome and exits nonzero on a transport failure instead of failing soft.
	if p.Flag("--all") {
		return a.releaseAll()
	}

	var key string
	switch kind := agents.Kind(tool); {
	case p.Flag("--subagent"):
		// SubagentStop: release the sub-agent's own `<tool>:<agent_id>` hold, never the parent's
		// session. No fallback: a missing agent_id fails soft (the daemon's idle and
		// dead-process nets recover the hold) rather than releasing the wrong key.
		agentID, ok := a.stdin.AgentID()
		if !ok {
			return a.hookFailure("release --subagent: no agent_id on stdin — ignored")
		}
		key = policy.SessionKey(tool, agentID)
	case kind.IsGatewayScoped():
		// The gateway hold is coalesced onto one key whatever the session (see acquire).
		key = tool + ":gateway"
	default:
		id, ok := a.sessionID(p)
		if !ok {
			return a.hookFailure("release: no session key (stdin payload or positional) — ignored")
		}
		key = releaseKey(tool, id)
	}
	a.log.Debug("release", "key", key)

	resp, err := a.send(ipc.Request{Op: ipc.OpRelease, Key: key, Tool: tool})
	if err != nil {
		// Never fail the hook, whatever the transport failure: the daemon's idle sweep and
		// process-exit watcher recover a missed release.
		a.warnf("release failed (%s) — ignored", errText(err))
		return 0
	}
	if resp.Warning != "" {
		a.warnf("%s", resp.Warning)
		// A release that matched nothing must be visible to scripts: a cleanup loop "releasing"
		// with exit 0 and no effect is the worst case. Scripts run without a TTY, so the reliable
		// marker is an omitted --tool: every generated hook names its tool, so the bare form is a
		// human or a script working from `status --json` keys. Hooks (named tool, no TTY) stay
		// fail-soft: their safety-net double releases are routine no-ops.
		if !named || a.stdinIsTTY() {
			return 1
		}
	}
	return 0
}

// releaseKey is the registry key a release targets. A hold id ("hold:…"), an already-prefixed
// "<tool>:<session>" key or a daemon-minted "sniffed:" key is used verbatim, so release accepts
// every key form `status --json` prints. Bare ids get the same "<tool>:" derivation acquire
// uses, so a session's Stop release targets exactly the key its UserPromptSubmit acquire placed.
func releaseKey(tool, id string) string {
	if strings.HasPrefix(id, policy.SniffedKeyPrefix) {
		return id
	}
	return policy.SessionKey(tool, id)
}

func (a *app) releaseAll() int {
	a.log.Debug("release --all")
	resp, err := a.send(ipc.Request{Op: ipc.OpReleaseAll})
	if err != nil {
		a.warnf("release --all failed (%s)", errText(err))
		return 1
	}
	if n := deref(resp.ReleasedCount); n > 0 {
		a.printf("Released %d assertion%s — your Mac can sleep.\n", n, plural(n))
	} else {
		a.printf("Nothing was held — released nothing.\n")
	}
	return 0
}

// secondsToDuration converts finite non-negative seconds, saturating instead of overflowing.
func secondsToDuration(s float64) time.Duration {
	if s >= float64(math.MaxInt64)/float64(time.Second) {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(s * float64(time.Second))
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func deref(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
