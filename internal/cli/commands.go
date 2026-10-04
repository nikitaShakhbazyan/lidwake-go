package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/nikitaShakhbazyan/lidwake-go/internal/agents"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/hooks"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/ipc"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/model"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/policy"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/tui"
)

func (a *app) printf(format string, args ...any) {
	fmt.Fprintf(a.stdout, format, args...)
}

// ---- hold ------------------------------------------------------------------------------------

// hold is `lidwake hold`: an explicit, reasoned, time-boxed sleep block that outlives the agent's
// turn, its session and the agent process itself. The minted hold id goes to stdout so a script
// can capture it (`HOLD=$(lidwake hold --reason "deploy" --for 30m)`); the human summary goes to
// stderr. It ends with `lidwake release <id>` or when it expires.
func (a *app) hold(args []string) int {
	p := ParseArgs(args)
	reason, _ := p.Option("--reason")
	tool, _ := p.Option("--tool")

	// --for takes a human duration ("30m", "2h", "1h30m"); --ttl stays raw seconds for parity
	// with acquire. The daemon clamps either to the configured cap.
	var ttl *float64
	if forArg, ok := p.Option("--for"); ok {
		secs, ok := policy.ParseDurationSeconds(forArg)
		if !ok {
			fmt.Fprintf(a.stderr, "hold: could not understand duration '%s' (try 30m, 2h, 1h30m)\n", forArg)
			return 2
		}
		ttl = &secs
	} else if ttlArg, ok := p.Option("--ttl"); ok {
		if v, err := strconv.ParseFloat(ttlArg, 64); err == nil && !math.IsInf(v, 0) && !math.IsNaN(v) {
			ttl = &v
		}
	}

	pid := 0
	if pidArg, ok := p.Option("--pid"); ok {
		n, err := strconv.ParseInt(pidArg, 10, 32)
		if err != nil || n <= 0 {
			fmt.Fprintln(a.stderr, "hold: --pid must be a positive process id")
			return 2
		}
		pid = int(n)
	}
	ttlText := "default"
	if ttl != nil {
		ttlText = strconv.FormatFloat(*ttl, 'f', -1, 64) + "s"
	}
	a.log.Debug("hold", "reason", reason, "ttl", ttlText, "pid", pid)

	wantsDisplay := p.Flag("--display")
	resp, err := a.send(ipc.Request{
		Op:          ipc.OpHold,
		Tool:        tool,
		Reason:      reason,
		PID:         pid,
		ProcessName: tool,
		TTL:         ttl,
		Display:     wantsDisplay,
	})
	if err != nil {
		// Unlike a hook acquire, a hold must report failure: the agent needs to know it did not
		// take, so it doesn't assume the Mac will stay awake.
		fmt.Fprintf(a.stderr, "hold failed: %s\n", errText(err))
		return 1
	}
	if !resp.OK || resp.HoldKey == "" {
		fmt.Fprintf(a.stderr, "hold failed: %s\n", orDefault(resp.Error, "unknown error"))
		return 1
	}
	if wantsDisplay && resp.DisplayApplied == nil {
		// Version skew: an old daemon ignores the display field. The caller still gets the
		// system hold, hence a warning rather than a failure.
		fmt.Fprintln(a.stderr, "hold: the running daemon predates --display — the system stays awake but the display is NOT held. Update lidwake.")
	}
	a.printf("%s\n", resp.HoldKey)
	// The daemon clamps the TTL to the configured cap, so the summary reports what was applied.
	applied := ttl
	if resp.AppliedTTL != nil {
		applied = resp.AppliedTTL
	}
	fmt.Fprint(a.stderr, holdSummary(resp.HoldKey, applied, pid))
	return 0
}

// errText is a daemon round trip's error as users and scripts have always seen it: the
// daemon-not-running case is a full sentence, period included.
func errText(err error) string {
	if errors.Is(err, ipc.ErrDaemonUnreachable) {
		return "lidwake daemon is not running."
	}
	return err.Error()
}

func holdSummary(key string, ttl *float64, pid int) string {
	parts := []string{"Keeping your Mac awake"}
	if pid > 0 {
		parts = append(parts, fmt.Sprintf("until process %d exits", pid))
	}
	if ttl != nil {
		parts = append(parts, "for up to "+compactDuration(*ttl))
	} else {
		parts = append(parts, "for up to 1h")
	}
	parts = append(parts, "· release with: lidwake release "+key)
	return strings.Join(parts, " ") + "\n"
}

// compactDuration renders seconds as "1h30m", "2h", "5m30s", "5m" or "45s".
func compactDuration(seconds float64) string {
	total := int(math.Round(seconds))
	h, m, s := total/3600, total%3600/60, total%60
	switch {
	case h > 0 && m > 0:
		return fmt.Sprintf("%dh%dm", h, m)
	case h > 0:
		return fmt.Sprintf("%dh", h)
	case m > 0 && s > 0:
		return fmt.Sprintf("%dm%ds", m, s)
	case m > 0:
		return fmt.Sprintf("%dm", m)
	}
	return fmt.Sprintf("%ds", s)
}

// ---- status, daemon-status -------------------------------------------------------------------

func (a *app) status(args []string) int {
	jsonMode := ParseArgs(args).Flag("--json")
	resp, err := a.send(ipc.Request{Op: ipc.OpStatus})
	if errors.Is(err, ipc.ErrDaemonUnreachable) {
		if jsonMode {
			a.printf("%s\n", `{"daemonRunning":false}`)
		} else {
			a.printf("lidwake daemon is not running.\n")
		}
		return 1
	}
	if err != nil {
		fmt.Fprintf(a.stderr, "Error: %v\n", err)
		return 1
	}

	if jsonMode {
		if resp.Status == nil {
			a.printf("%s\n", `{"daemonRunning":true,"statusUnavailable":true}`)
			return 0
		}
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(resp.Status); err != nil {
			fmt.Fprintf(a.stderr, "Error: %v\n", err)
			return 1
		}
		_, _ = a.stdout.Write(buf.Bytes())
		return 0
	}

	if resp.Status == nil {
		a.printf("Daemon responded but status could not be decoded.\n")
		return 0
	}
	a.printf("%s", statusText(resp.Status, a.now()))
	return 0
}

// statusText is the plain-text `lidwake status`.
func statusText(s *model.Status, now time.Time) string {
	var b strings.Builder
	state := "idle"
	if s.Blocking {
		state = "blocking sleep"
	}
	fmt.Fprintf(&b, "lidwake — %s\n", state)
	fmt.Fprintf(&b, "  Assertions: %d\n", len(s.Assertions))
	for _, as := range s.Assertions {
		age := now.Sub(as.AcquiredAt).Seconds()
		line := fmt.Sprintf("    • %s [%s] — %dm %ds", clean(as.Tool), clean(as.Key), int(age/60), int(age)%60)
		if as.Reason != "" {
			line += " — " + clean(as.Reason)
		}
		b.WriteString(line + "\n")
	}
	lid := "open"
	if s.LidClosed {
		lid = "closed"
	}
	fmt.Fprintf(&b, "  Lid: %s\n", lid)
	if s.CPUTemperature != nil {
		fmt.Fprintf(&b, "  CPU temp: %.1f°C\n", *s.CPUTemperature)
	}
	helper := "disconnected"
	if s.HelperConnected {
		helper = "connected"
	}
	fmt.Fprintf(&b, "  Helper: %s\n", helper)
	if away := s.AwaySummary; away != nil {
		b.WriteString(awayText(away, now.Location()))
	}
	return b.String()
}

// awayText recaps the last closed-lid period that had work going, as the dashboard does.
func awayText(a *model.AwaySummary, loc *time.Location) string {
	var b strings.Builder
	fmt.Fprintf(&b, "  While the lid was closed: %s · %s → %s\n",
		tui.FormatDuration(a.OpenedAt.Sub(a.ClosedAt), false),
		a.ClosedAt.In(loc).Format("15:04"), a.OpenedAt.In(loc).Format("15:04"))
	if len(a.Finished) == 0 {
		b.WriteString("    Finished: none\n")
	} else {
		fmt.Fprintf(&b, "    Finished: %s\n", agentTally(a.Finished))
	}
	if len(a.StillActive) > 0 {
		fmt.Fprintf(&b, "    Still working: %s\n", agentTally(a.StillActive))
	}
	if t := a.PeakTemperature; t != nil && !math.IsNaN(*t) && !math.IsInf(*t, 0) {
		fmt.Fprintf(&b, "    Peak CPU temp: %.1f°C\n", *t)
	}
	var fired []string
	if a.ThermalCutout {
		fired = append(fired, tui.CutoutName("thermal"))
	}
	if a.LowBatteryCutout {
		fired = append(fired, tui.CutoutName("lowBattery"))
	}
	if len(fired) == 0 {
		b.WriteString("    Safety cutouts: none fired\n")
	} else {
		fmt.Fprintf(&b, "    Safety cutouts: %s fired\n", strings.Join(fired, ", "))
	}
	return b.String()
}

func agentTally(list []model.FinishedAgent) string {
	noun := "agents"
	if len(list) == 1 {
		noun = "agent"
	}
	names := make([]string, 0, len(list))
	for _, f := range list {
		name := f.DisplayName
		if name == "" {
			name = f.Tool
		}
		names = append(names, clean(name)+" "+tui.FormatDuration(f.Duration, false))
	}
	return fmt.Sprintf("%d %s — %s", len(list), noun, strings.Join(names, ", "))
}

// clean replaces control characters (escape sequences, newlines) in text that came from hooks,
// so a crafted reason can't rewrite the terminal.
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == ' ' || r == ' ' {
			return ' '
		}
		return r
	}, s)
}

// daemonStatus is `lidwake daemon-status`, a quick liveness check. It exits 0 either way: the
// answer is informational, not a gate for agent hooks.
func (a *app) daemonStatus() int {
	resp, err := a.send(ipc.Request{Op: ipc.OpPing})
	switch {
	case errors.Is(err, ipc.ErrDaemonUnreachable):
		a.printf("daemon: not running (lidwake setup installs and starts it)\n")
	case err != nil:
		a.printf("daemon: unreachable (%v)\n", err)
	case resp.OK:
		a.printf("daemon: running\n")
	default:
		a.printf("daemon: running (reported not-ok: %s)\n", orDefault(resp.Error, "unknown"))
	}
	return 0
}

// ---- on, off, timer --------------------------------------------------------------------------

// control sends one on/off/timer request; ok is false after it reported the failure.
func (a *app) control(req ipc.Request) (ipc.Response, bool) {
	resp, err := a.send(req)
	if err != nil {
		a.warnf("%s", errText(err))
		return resp, false
	}
	if !resp.OK {
		a.warnf("%s", orDefault(resp.Error, "the daemon refused the request"))
		return resp, false
	}
	return resp, true
}

func (a *app) setOn(on bool) int {
	op := ipc.OpPause
	if on {
		op = ipc.OpResume
	}
	resp, ok := a.control(ipc.Request{Op: op})
	if !ok {
		return 1
	}
	if on {
		a.printf("lidwake is on — working agents keep the Mac awake, lid closed or not.\n")
		return 0
	}
	released := ""
	if n := deref(resp.ReleasedCount); n > 0 {
		released = fmt.Sprintf(" — released %d hold%s", n, plural(n))
	}
	a.printf("lidwake is off%s · the Mac sleeps normally.\n", released)
	return 0
}

// timer is `lidwake timer 1h` / `lidwake timer off`.
func (a *app) timer(args []string) int {
	if len(args) != 1 {
		return a.fail("usage: lidwake timer <duration, e.g. 30m, 1h, 1h30m> | off")
	}
	req := ipc.Request{Op: ipc.OpTimer} // no TTL cancels
	switch args[0] {
	case "off", "cancel", "none":
	default:
		secs, ok := policy.ParseDurationSeconds(args[0])
		if !ok || secs <= 0 {
			return a.fail("could not understand duration '%s' (try 30m, 1h, 1h30m)", args[0])
		}
		req.TTL = &secs
	}
	resp, ok := a.control(req)
	if !ok {
		return 1
	}
	if resp.AppliedTTL == nil {
		a.printf("Off timer cancelled.\n")
		return 0
	}
	left := secondsToDuration(max(0, *resp.AppliedTTL))
	a.printf("lidwake turns off at %s (in %s).\n", a.now().Add(left).Format("15:04"), tui.FormatDuration(left, false))
	return 0
}

// ---- run -------------------------------------------------------------------------------------

const runUsage = "usage: lidwake run [--for <duration>] [--reason <text>] [--display] -- <command> [args...]"

// runPlan is a parsed `lidwake run` command line.
type runPlan struct {
	duration *float64 // seconds; nil = as long as the configured cap allows
	reason   string
	display  bool
	command  []string
	// help is set for -h/--help: print the usage and exit 0.
	help bool
}

// parseRun reads options up to "--" or the first non-option word; the rest is the command.
// errMsg is set on a usage error.
func parseRun(args []string) (plan runPlan, errMsg string) {
	rest := args
parsing:
	for len(rest) > 0 {
		arg := rest[0]
		switch arg {
		case "--":
			rest = rest[1:]
			break parsing
		case "--for":
			if len(rest) < 2 {
				return plan, "--for needs a duration such as 2h or 1h30m"
			}
			secs, ok := policy.ParseDurationSeconds(rest[1])
			if !ok || secs <= 0 {
				return plan, "--for needs a duration such as 2h or 1h30m"
			}
			plan.duration = &secs
			rest = rest[2:]
		case "--reason":
			if len(rest) < 2 {
				return plan, "--reason needs a value"
			}
			plan.reason = rest[1]
			rest = rest[2:]
		case "--display":
			plan.display = true
			rest = rest[1:]
		case "-h", "--help":
			plan.help = true
			return plan, ""
		default:
			if strings.HasPrefix(arg, "-") {
				return plan, fmt.Sprintf("unknown option %s (put the command after --)", arg)
			}
			break parsing
		}
	}
	if len(rest) == 0 {
		return plan, "usage: lidwake run [--for <duration>] [--reason <text>] -- <command> [args...]"
	}
	plan.command = rest
	return plan, ""
}

// holdRequest is the hold `run` places on its own PID before exec'ing the command: the PID, and
// so the hold, belongs to the command from then on, and the daemon releases it the moment the
// command exits. Without --for it asks for 24 h, which the daemon clamps to the configured cap:
// the hold should last as long as the command does.
func (p runPlan) holdRequest(pid int) ipc.Request {
	name := filepath.Base(p.command[0])
	reason := p.reason
	if reason == "" {
		reason = "lidwake run " + name
	}
	ttl := 24 * time.Hour.Seconds()
	if p.duration != nil {
		ttl = *p.duration
	}
	return ipc.Request{
		Op:          ipc.OpHold,
		Tool:        name,
		Reason:      reason,
		PID:         pid,
		ProcessName: name,
		TTL:         &ttl,
		Display:     p.display,
	}
}

// runCommand is `lidwake run [--for <d>] [--reason <text>] [--display] -- <command> [args…]`.
// The command runs even if the hold can't be placed — lidwake never stands between you and your
// work — but says so on stderr. Its exit code passes straight through, because it replaces this
// process.
func (a *app) runCommand(args []string) int {
	plan, errMsg := parseRun(args)
	if plan.help {
		a.printf("%s\n", runUsage)
		return 0
	}
	if errMsg != "" {
		return a.fail("%s", errMsg)
	}
	program := plan.command[0]
	name := filepath.Base(program)

	resp, err := a.send(plan.holdRequest(a.getpid()))
	switch {
	case err != nil:
		a.warnf("%s. Running %s without keeping the Mac awake.", strings.TrimSuffix(err.Error(), "."), name)
	case resp.OK && resp.AppliedTTL != nil:
		a.warnf("keeping the Mac awake while %s runs (at most %s)", name,
			tui.FormatDuration(secondsToDuration(max(0, *resp.AppliedTTL)), false))
	default:
		a.warnf("could not keep the Mac awake: %s — running %s anyway", orDefault(resp.Error, "unknown error"), name)
	}

	// The command runs as execvp would run it: a match in a relative PATH entry (./bin,
	// node_modules/.bin) is the user's own PATH at work, not a hijack, and a script without a
	// #! line runs through /bin/sh.
	path, err := a.lookPath(program)
	if err != nil && !errors.Is(err, exec.ErrDot) {
		var execErr *exec.Error
		if errors.As(err, &execErr) {
			err = execErr.Err
		}
		return a.fail("%s: %v", program, err)
	}
	env := a.environ()
	err = a.exec(path, plan.command, env)
	if errors.Is(err, syscall.ENOEXEC) {
		err = a.exec("/bin/sh", append([]string{"sh", path}, plan.command[1:]...), env)
	}
	return a.fail("%s: %v", program, err)
}

// ---- install-hooks, uninstall-hooks ----------------------------------------------------------

// targetAgents resolves --tool to an agent, or every agent without it. A misspelled tool must
// never silently fan out to every agent: `uninstall-hooks --tool claude` (the name is
// claude-code) would otherwise strip hooks from all of them. ok is false after the error has
// been reported.
func (a *app) targetAgents(p Args) ([]string, bool) {
	all := hooks.Agents()
	raw, given := p.Option("--tool")
	if !given {
		return all, true
	}
	for _, agent := range all {
		if agent == raw {
			return []string{agent}, true
		}
	}
	fmt.Fprintf(a.stderr, "unknown tool '%s' — valid: %s\n", raw, strings.Join(all, ", "))
	return nil, false
}

func displayName(agent string) string { return agents.Kind(agent).DisplayName() }

// hookArgs accepts only --tool <name> and --dry-run: an argument the command doesn't know must
// not be ignored by a command that rewrites agent configs.
func (a *app) hookArgs(command string, args []string) (Args, bool) {
	p := ParseArgs(args)
	var unexpected []string
	unexpected = append(unexpected, p.Positionals...)
	for name := range p.Flags {
		if name != "--dry-run" {
			unexpected = append(unexpected, name)
		}
	}
	for name := range p.Options {
		if name != "--tool" {
			unexpected = append(unexpected, name)
		}
	}
	if len(unexpected) > 0 {
		slices.Sort(unexpected)
		a.warnf("%s: unexpected %s — usage: lidwake %s [--tool <name>] [--dry-run]", command, strings.Join(unexpected, " "), command)
		return p, false
	}
	return p, true
}

func (a *app) installHooks(args []string) int {
	p, ok := a.hookArgs("install-hooks", args)
	if !ok {
		return 2
	}
	targets, ok := a.targetAgents(p)
	if !ok {
		return 2
	}
	dryRun := p.Flag("--dry-run")
	installer := a.hooks()
	for _, agent := range targets {
		name := displayName(agent)
		result, err := installer.Install(agent, dryRun)
		var skip *hooks.SkipError
		switch {
		case err == nil && dryRun:
			a.printf("[%s] would write:\n%s\n", name, result.Diff)
		case err == nil:
			a.printf("[%s] %s\n", name, result.Summary)
		case hooks.IsSkip(err, hooks.SkipNotInstalled):
			a.printf("[%s] not detected, skipping\n", name)
		case errors.As(err, &skip) && skip.Reason == hooks.SkipUnsupported:
			a.printf("[%s] %s\n", name, skip.Detail)
		default:
			fmt.Fprintf(a.stderr, "[%s] error: %v\n", name, err)
		}
	}
	return 0
}

func (a *app) uninstallHooks(args []string) int {
	p, ok := a.hookArgs("uninstall-hooks", args)
	if !ok {
		return 2
	}
	targets, ok := a.targetAgents(p)
	if !ok {
		return 2
	}
	dryRun := p.Flag("--dry-run")
	installer := a.hooks()
	for _, agent := range targets {
		name := displayName(agent)
		result, err := installer.Uninstall(agent, dryRun)
		switch {
		case err != nil:
			fmt.Fprintf(a.stderr, "[%s] error: %v\n", name, err)
		case dryRun:
			a.printf("[%s] would remove:\n%s\n", name, result.Diff)
		default:
			a.printf("[%s] %s\n", name, result.Summary)
		}
	}
	return 0
}

// ---- setup, uninstall ------------------------------------------------------------------------

const (
	setupUsage     = "usage: lidwake setup — install or upgrade the daemon and the root helper"
	uninstallUsage = "usage: lidwake uninstall — remove hooks, the daemon and the helper (keeps settings)"
)

// installArgs parses the arguments of setup or uninstall, which take none: both act at once on
// the whole machine, so a flag borrowed from a sibling (`uninstall --dry-run`, which only
// uninstall-hooks knows) or a typo must stop them rather than be ignored. The exceptions are the
// hidden --root and the options its privileged half reads (setup's --uid); -h/--help prints the
// usage. ok is false when the command must not run, and code is then its exit code.
func (a *app) installArgs(command, usage string, args []string, rootOptions ...string) (p Args, code int, ok bool) {
	if slices.ContainsFunc(args, func(arg string) bool { return arg == "-h" || arg == "--help" }) {
		a.printf("%s\n", usage)
		return p, 0, false
	}
	p = ParseArgs(args)
	rootOption := func(name string) bool { return p.Flag("--root") && slices.Contains(rootOptions, name) }
	unexpected := slices.Clone(p.Positionals)
	for name := range p.Flags {
		if name != "--root" && !rootOption(name) {
			unexpected = append(unexpected, name)
		}
	}
	for name := range p.Options {
		if !rootOption(name) {
			unexpected = append(unexpected, name)
		}
	}
	if len(unexpected) > 0 {
		slices.Sort(unexpected)
		a.warnf("%s takes no arguments (got %s)", command, strings.Join(unexpected, " "))
		return p, 2, false
	}
	return p, 0, true
}

// setup is `lidwake setup`, run as the user. The hidden `setup --root --uid <uid>` is the
// privileged half it runs through sudo.
func (a *app) setup(args []string) int {
	p, code, ok := a.installArgs("setup", setupUsage, args, "--uid")
	if !ok {
		return code
	}
	in := a.installer()
	var err error
	if p.Flag("--root") {
		raw, _ := p.Option("--uid")
		uid, perr := strconv.Atoi(raw)
		if perr != nil || uid <= 0 {
			return a.fail("setup --root needs --uid <the installing user's uid>")
		}
		err = in.SetupRoot(uid)
	} else {
		err = in.Setup()
	}
	if err != nil {
		return a.fail("setup failed: %v", err)
	}
	return 0
}

// uninstall is `lidwake uninstall`, run as the user; `uninstall --root` is its privileged half.
func (a *app) uninstall(args []string) int {
	p, code, ok := a.installArgs("uninstall", uninstallUsage, args)
	if !ok {
		return code
	}
	in := a.installer()
	var err error
	if p.Flag("--root") {
		err = in.UninstallRoot()
	} else {
		err = in.Uninstall()
	}
	if err != nil {
		return a.fail("uninstall failed: %v", err)
	}
	return 0
}
