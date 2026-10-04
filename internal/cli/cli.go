// Package cli implements every `lidwake` subcommand except the two launchd entry points
// (`lidwake daemon`, `lidwake helper`), which cmd/lidwake runs on the main thread.
package cli

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/nikitaShakhbazyan/lidwake-go/internal/agents"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/hooks"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/install"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/ipc"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/mcp"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/paths"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/policy"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/tui"
)

// app is one invocation's view of the outside world. Main wires it to the real process; tests
// substitute every field, so no command reaches the daemon, the terminal or the file system
// unless a test says so.
type app struct {
	stdout io.Writer
	stderr io.Writer
	// stdin is the hook payload on standard input, read at most once and only when a command
	// asks for it (acquire, release).
	stdin *agents.Stdin
	// stdinIsTTY reports whether standard input is a terminal — a human at the keyboard rather
	// than an agent hook.
	stdinIsTTY func() bool
	send       func(ipc.Request) (ipc.Response, error)
	resolver   *agents.Resolver
	environ    func() []string
	getpid     func() int
	now        func() time.Time
	home       string
	configPath string
	lookPath   func(file string) (string, error)
	// exec replaces the process image; it returns only on failure.
	exec  func(path string, argv, env []string) error
	hooks func() *hooks.Installer
	// stats runs `lidwake stats`.
	stats func(actions tui.Actions, once bool) int
	// mcpIn is the MCP server's input stream.
	mcpIn io.Reader
	// installer runs `lidwake setup` / `lidwake uninstall`.
	installer func() *install.Installer
	log       *slog.Logger
}

// Main runs the command line and returns the process exit code.
func Main(args []string) int {
	if len(args) == 0 || args[0] != "run" {
		// A SIGPIPE (the daemon closed the socket mid-write, an MCP client closed stdout) would
		// kill the process by signal, which an agent hook reports as a hard failure; write
		// errors are handled at the call sites instead. Not for `run`: an ignored signal stays
		// ignored across exec, and the command it runs must get the default behavior.
		signal.Ignore(syscall.SIGPIPE)
	}
	return newApp().run(args)
}

func newApp() *app {
	return &app{
		stdout:     os.Stdout,
		stderr:     os.Stderr,
		stdin:      agents.OSStdin(),
		stdinIsTTY: func() bool { return tui.IsTerminal(uintptr(syscall.Stdin)) },
		send:       ipc.Send,
		resolver:   agents.System(),
		environ:    os.Environ,
		getpid:     os.Getpid,
		now:        time.Now,
		home:       paths.Home(),
		configPath: paths.ConfigFile(),
		lookPath:   exec.LookPath,
		exec:       syscall.Exec,
		hooks:      func() *hooks.Installer { return hooks.New("", "") },
		stats: func(actions tui.Actions, once bool) int {
			return tui.Stats(actions, once, os.Stdin, os.Stdout)
		},
		mcpIn:     os.Stdin,
		installer: func() *install.Installer { return install.New(os.Stdout, os.Stderr) },
		log:       debugLogger(os.Stderr),
	}
}

// debugLogger logs to w when LIDWAKE_DEBUG is set and discards otherwise: these commands run
// inside agent hooks, whose stderr the agent may show to the user.
func debugLogger(w io.Writer) *slog.Logger {
	if os.Getenv("LIDWAKE_DEBUG") == "" {
		return slog.New(slog.DiscardHandler)
	}
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func (a *app) run(args []string) int {
	if len(args) == 0 {
		a.printShortUsage()
		return 1
	}
	first, rest := args[0], args[1:]
	// `lidwake <command> --help` explains the command and does nothing else: a command that
	// ignored the flag would act — install-hooks would rewrite every agent's config.
	// (run, setup and uninstall print their own, fuller usage.)
	if usage, ok := commandUsage(first); ok && asksForHelp(first, rest) {
		fmt.Fprintf(a.stdout, "%s\n", usage)
		return 0
	}
	switch first {
	case "on":
		return a.setOn(true)
	case "off":
		return a.setOn(false)
	case "timer":
		return a.timer(rest)
	case "stats", "--stats", "top":
		return a.stats(tui.DaemonActions{Send: a.send}, ParseArgs(rest).Flag("--once"))
	case "run":
		return a.runCommand(rest)
	case "config":
		return a.config(rest)
	case "setup":
		return a.setup(rest)
	case "uninstall":
		return a.uninstall(rest)
	case "acquire":
		return a.acquire(rest)
	case "hold":
		return a.hold(rest)
	case "release":
		return a.release(rest)
	case "status":
		return a.status(rest)
	case "install-hooks":
		return a.installHooks(rest)
	case "uninstall-hooks":
		return a.uninstallHooks(rest)
	case "daemon-status":
		return a.daemonStatus()
	case "mcp":
		return a.mcp(rest)
	case "version", "--version", "-v":
		fmt.Fprintf(a.stdout, "lidwake %s\n", paths.Version)
		return 0
	case "help", "--help", "-h":
		a.printFullUsage()
		return 0
	default:
		fmt.Fprintf(a.stderr, "Unknown command: %s\n", first)
		a.printShortUsage()
		return 2
	}
}

// asksForHelp reports whether args ask for help. For run, only the options before the command
// count: `lidwake run -- tool --help` passes --help to the tool.
func asksForHelp(command string, args []string) bool {
	for _, arg := range args {
		if arg == "--" || (command == "run" && !strings.HasPrefix(arg, "-")) {
			return false
		}
		if arg == "-h" || arg == "--help" {
			return true
		}
	}
	return false
}

// commandUsage is the command's line from the full usage text, as "usage: …" plus its
// description. False for commands that print their own usage.
func commandUsage(command string) (string, bool) {
	switch command {
	case "run", "setup", "uninstall":
		return "", false
	}
	for _, line := range strings.Split(fullUsage, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "lidwake" {
			continue
		}
		if fields[1] != command && (len(fields) < 4 || fields[2] != "|" || fields[3] != command) {
			continue
		}
		usage, description, _ := strings.Cut(strings.TrimSpace(line), "  ")
		usage = "usage: " + usage
		if description = strings.TrimSpace(description); description != "" {
			usage += "\n  " + description
		}
		return usage, true
	}
	return "", false
}

func (a *app) mcp(args []string) int {
	tool, ok := ParseArgs(args).Option("--tool")
	if !ok {
		// Registered per agent (`lidwake mcp --tool claude-code`), so holds carry the agent's
		// name; without one they get the generic "manual" label.
		tool = policy.DefaultHoldTool
	}
	srv := &mcp.Server{Tool: tool, Send: a.send, Now: a.now, Log: a.stderr}
	if err := srv.Serve(a.mcpIn, a.stdout); err != nil {
		fmt.Fprintf(a.stderr, "[lidwake mcp] %v\n", err)
		return 1
	}
	return 0
}

// warnf writes one "lidwake: …" line to stderr.
func (a *app) warnf(format string, args ...any) {
	fmt.Fprintf(a.stderr, "lidwake: "+format+"\n", args...)
}

// fail reports a failed human command: "lidwake: …" on stderr, exit 1.
func (a *app) fail(format string, args ...any) int {
	a.warnf(format, args...)
	return 1
}

const shortUsage = `usage: lidwake <command> [args]
commands: stats | on | off | timer | run | config | setup | uninstall | hold | release | status | acquire | install-hooks | uninstall-hooks | daemon-status | mcp | version
`

const fullUsage = `lidwake — keep your Mac awake only while AI agents are working, lid closed or not

USAGE:
  lidwake stats [--once]                 live dashboard (space on/off, t timer, r release, q quit)
  lidwake on | off                       let agents keep the Mac awake, or stop and release all
  lidwake timer <duration> | off         turn off automatically, e.g. lidwake timer 1h
  lidwake run [--for <d>] -- <command>   keep the Mac awake while <command> runs
  lidwake config [<key> [<value>]]       show or change a setting
  lidwake setup                          install or upgrade the daemon and the root helper
  lidwake uninstall                      remove hooks, the daemon and the helper (keeps settings)
  lidwake hold [--reason <text>] [--for <duration>] [--pid <n>] [--tool <name>] [--display]
  lidwake release <hold-id | session-key> | --all
  lidwake acquire <session-key> --tool <name> [--reason <text>] [--ttl <seconds>] [--display]
  lidwake status [--json]
  lidwake install-hooks [--tool <name>] [--dry-run]
  lidwake uninstall-hooks [--tool <name>] [--dry-run]
  lidwake daemon-status
  lidwake mcp
  lidwake version

AGENT HOLDS:
  ` + "`hold`" + ` keeps the Mac awake past the end of your turn — for a background job you
  kicked off — and prints a hold id. The hold ends when you ` + "`release <id>`" + `, when the
  --pid you named exits, or when its time runs out (default 1h, capped in settings).

    HOLD=$(lidwake hold --reason "running migration" --for 30m)
    ./migrate.sh
    lidwake release "$HOLD"

  --display additionally keeps the DISPLAY awake (and wakes it if dark) — for agents
  that read the screen: a sleeping display collapses every app's accessibility tree,
  so a system-only hold keeps the machine on while blinding the agent.

` + "`acquire`/`release`" + ` are the reference-counted hooks wired into agents at setup.
` + "`release`" + ` accepts a key in any form ` + "`status --json`" + ` prints: a bare session id, the
prefixed ` + "`<tool>:<session>`" + `, a ` + "`sniffed:`" + ` key, or a ` + "`hold:`" + ` id. Releasing a key that
matches nothing warns and exits 1 at a TTY (hooks always exit 0).
` + "`mcp`" + ` runs a Model Context Protocol server exposing holds as agent-callable tools.
`

func (a *app) printShortUsage() { _, _ = io.WriteString(a.stdout, shortUsage) }
func (a *app) printFullUsage()  { _, _ = io.WriteString(a.stdout, fullUsage) }
