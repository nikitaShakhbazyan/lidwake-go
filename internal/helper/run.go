// Package helper is the root helper (`lidwake helper`, a LaunchDaemon): the only privileged code.
// It owns the machine-wide sleep block — an idle-sleep power assertion plus the SleepDisabled
// setting for lid-closed sleep — and applies it for the per-user daemon over a Unix socket.
package helper

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/nikitaShakhbazyan/lidwake-go/internal/darwin"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/paths"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/policy"
)

// Run starts the helper and blocks until ctx is cancelled or a fatal error occurs.
// CONTRACT: `lidwake helper` calls exactly this.
//
// It logs to stderr, which launchd sends to paths.HelperLog. SIGTERM (machine shutdown,
// uninstall) and SIGINT cancel it like ctx does: the block is cleared on the way out. Building
// the sleep-block policy restores a disablesleep value a previous instance saved, so a helper
// that crashed or a Mac rebooted mid-block recovers at start (the LaunchDaemon has RunAtLoad).
// After stopping to adopt an updated binary it returns nil, and launchd (KeepAlive) relaunches
// it.
func Run(ctx context.Context) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("lidwake helper must run as root: it is the LaunchDaemon %s, which `lidwake setup` installs", paths.HelperLabel)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	slog.SetDefault(log)
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, os.Interrupt)
	defer stop()

	replaced := replacementCheck(runningExecutable())
	log.Info("helper starting", "version", paths.Version, "uid", os.Getuid(), "socket", paths.HelperSocket)
	// Checked before the policy is built: building it restores the saved value, which would undo
	// a block the live helper holds.
	if listening(paths.HelperSocket) {
		return fmt.Errorf("another lidwake helper is already listening on %s", paths.HelperSocket)
	}
	log.Info("restoring any disablesleep value a previous instance left behind")
	blocker := policy.NewSleepBlockPolicy(
		newIdleAssertion(log),
		pmsetClamshell{},
		originalFile{path: paths.OriginalSleepSetting, log: log},
	)
	err := Serve(ctx, Config{
		Socket:    paths.HelperSocket,
		RunDir:    paths.HelperRunDir,
		Authorize: authorizer(log),
		Blocker:   blocker,
		Replaced:  replaced,
		Version:   paths.Version,
		Logger:    log,
	})
	if errors.Is(err, ErrExecutableReplaced) {
		return nil
	}
	return err
}

// authorizer admits only the lidwake daemon, judged from the peer's audit token (see
// policy.AuthorizePeer), returns its UID and logs who the caller was — every accepted daemon,
// and rejected callers at most once per rejectLogInterval.
func authorizer(log *slog.Logger) func(*net.UnixConn) (int, bool) {
	rejected := &rejectLog{log: log, now: time.Now, interval: rejectLogInterval}
	return func(conn *net.UnixConn) (int, bool) {
		peer, err := darwin.PeerCodeOf(conn)
		if err != nil {
			rejected.Error("rejected a connection: the caller's code could not be resolved", "err", err)
			return 0, false
		}
		who := []any{"pid", peer.PID, "uid", peer.UID, "path", peer.Path, "identifier", peer.Identifier, "team", peer.Team}
		if !policy.AuthorizePeer(darwin.SelfTeam(), peer, policy.IsWritableByRootOnly) {
			rejected.Error("rejected a connection: the caller failed the code-signing check",
				append(who, "valid", peer.Valid, "hardenedRuntime", peer.HardenedRuntime)...)
			return 0, false
		}
		log.Info("accepted a daemon connection", who...)
		return peer.UID, true
	}
}

// rejectLogInterval spaces the rejection lines. The socket is world-connectable and the log is a
// plain root-owned file, so any local user connecting in a loop could otherwise grow it without
// bound.
const rejectLogInterval = time.Minute

// rejectLog writes at most one line per interval and adds to it how many it held back since the
// previous one. The first rejection of a burst is logged in full: that is the line that explains
// a misinstalled daemon.
type rejectLog struct {
	log      *slog.Logger
	now      func() time.Time
	interval time.Duration

	mu         sync.Mutex
	logged     bool
	last       time.Time
	suppressed int
}

func (r *rejectLog) Error(msg string, args ...any) {
	r.mu.Lock()
	now := r.now()
	if r.logged && now.Sub(r.last) < r.interval {
		r.suppressed++
		r.mu.Unlock()
		return
	}
	if r.suppressed > 0 {
		args = append(args[:len(args):len(args)], "suppressedSinceLast", r.suppressed)
	}
	r.logged, r.last, r.suppressed = true, now, 0
	r.mu.Unlock()
	r.log.Error(msg, args...)
}
