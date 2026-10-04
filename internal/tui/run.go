package tui

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"golang.org/x/sys/unix"
)

// Stats is `lidwake stats [--once]` and returns the process exit code. It runs the live
// dashboard when both in and out are terminals; with --once, or when either is not a terminal,
// it prints a single frame for scripts and SSH one-liners and exits 1 if the daemon gave no
// status. Colour needs a terminal and NO_COLOR unset.
func Stats(a Actions, once bool, in, out *os.File) int {
	_, noColor := os.LookupEnv("NO_COLOR")
	outTTY := IsTerminal(out.Fd())
	opts := Options{Color: outTTY && !noColor, Width: TerminalWidth(out.Fd())}
	if once || !outTTY || !IsTerminal(in.Fd()) {
		if err := RenderOnce(out, a, opts); err != nil {
			return 1
		}
		return 0
	}
	if err := Run(a, opts, tea.WithInput(in), tea.WithOutput(out)); err != nil {
		fmt.Fprintf(os.Stderr, "lidwake: %v\n", err)
		return 1
	}
	return 0
}

// Run shows the live dashboard on the alternate screen until the user quits. bubbletea restores
// the terminal on quit and on SIGINT/SIGTERM; a SIGHUP quits the same way, because left to its
// default it would kill the process with the terminal still raw and on the alternate screen.
func Run(a Actions, opts Options, progOpts ...tea.ProgramOption) error {
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	return run(a, opts, hup, progOpts...)
}

// run is Run with the hangup signal injected, so tests need not signal the test process.
func run(a Actions, opts Options, hup <-chan os.Signal, progOpts ...tea.ProgramOption) error {
	p := tea.NewProgram(New(a, opts), append([]tea.ProgramOption{tea.WithAltScreen()}, progOpts...)...)
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-hup:
			// Quit returns once the program is gone too: Run cancels its context on the way out.
			p.Quit()
		case <-done:
		}
	}()
	if _, err := p.Run(); err != nil {
		return fmt.Errorf("dashboard: %w", err)
	}
	return nil
}

// RenderOnce writes one frame to w. It returns why the daemon gave no status (the frame then
// shows the "daemon not running" screen) or a write error; nil means a full dashboard.
func RenderOnce(w io.Writer, a Actions, opts Options) error {
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}
	width := opts.Width
	if width <= 0 {
		width = 80
	}
	s, err := a.Status()
	if s == nil && err == nil {
		err = errNoStatus
	}
	lines := Render(Frame{Status: s, DaemonError: err, Now: now(), Width: width, Color: opts.Color})
	if _, werr := io.WriteString(w, strings.Join(lines, "\n")+"\n"); werr != nil {
		return fmt.Errorf("write dashboard: %w", werr)
	}
	if s == nil {
		return err
	}
	return nil
}

// IsTerminal reports whether fd is a terminal.
func IsTerminal(fd uintptr) bool {
	_, err := unix.IoctlGetTermios(int(fd), unix.TIOCGETA)
	return err == nil
}

// TerminalWidth is fd's column count, or 80 when it is not a terminal.
func TerminalWidth(fd uintptr) int {
	ws, err := unix.IoctlGetWinsize(int(fd), unix.TIOCGWINSZ)
	if err != nil || ws.Col == 0 {
		return 80
	}
	return int(ws.Col)
}
