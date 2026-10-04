// Command lidwake keeps a MacBook awake with the lid closed while AI agents work. One binary
// plays three roles: the CLI, the per-user daemon (`lidwake daemon`) and the root helper
// (`lidwake helper`).
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/nikitaShakhbazyan/lidwake-go/internal/cli"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/daemon"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/helper"
)

// init pins the main goroutine to the main OS thread before anything else runs: the daemon does
// work that macOS only allows on the main thread, and it does that work on this goroutine.
func init() { runtime.LockOSThread() }

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "daemon":
			os.Exit(serve("daemon", daemon.Run))
		case "helper":
			os.Exit(serve("helper", helper.Run))
		}
	}
	os.Exit(cli.Main(os.Args[1:]))
}

// serve runs a launchd job on the main goroutine until it fails or launchd (or a terminal)
// stops it with SIGTERM or SIGINT. Both jobs restore what they changed when their context ends.
func serve(name string, run func(context.Context) error) int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := run(ctx); err != nil {
		slog.Error("lidwake "+name+" stopped", "err", err)
		return 1
	}
	return 0
}
