package helper

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"

	"github.com/nikitaShakhbazyan/lidwake-go/internal/ipc"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/paths"
)

// DeadManGrace is how long the helper stays blocked with no daemon connected before concluding
// the daemon is gone for good (SIGKILLed at logout, force-quit) and clearing the block itself.
// Long enough for a daemon crash, a launchd relaunch and a reconnect; short enough that a
// lid-closed Mac isn't pinned awake indefinitely by a block nobody owns.
const DeadManGrace = 60 * time.Second

// writeTimeout bounds sending one response, so a daemon that stops reading can't park a
// connection goroutine forever.
const writeTimeout = 5 * time.Second

// maxAuthorizing bounds how many new connections are being authorized at once. Any local user may
// connect, and each authorization is a code-signing check in this root process; past the bound
// the accept loop waits, so a connection flood queues in the listen backlog instead of fanning out
// into unbounded concurrent checks.
const maxAuthorizing = 4

// ErrExecutableReplaced is returned by Serve when it stopped to adopt a binary an update put on
// disk. launchd (KeepAlive) then relaunches the helper from the new image.
var ErrExecutableReplaced = errors.New("helper: executable replaced on disk")

// SleepBlocker is the machine-wide sleep block the helper drives: *policy.SleepBlockPolicy in
// production. Serve serializes every call, so an implementation needs no locking of its own.
type SleepBlocker interface {
	Set(blocked bool) error
	Blocked() bool
}

// Config is what Serve needs; Run fills it for production.
type Config struct {
	// Socket is the path to listen on (paths.HelperSocket).
	Socket string
	// RunDir, if set, is created with mode 0755 before listening (paths.HelperRunDir).
	RunDir string
	// Authorize decides whether the process on the other end of a new connection may drive the
	// helper, logs who it was and returns its UID, which owns the connection's requests. A
	// rejected connection is closed before any request is read and never counts as a live
	// daemon. Required.
	Authorize func(conn *net.UnixConn) (uid int, ok bool)
	// Blocker is the one process-wide block. Every user who runs lidwake has a daemon of their
	// own, so the block is held while any of them asks for it, and each user's request is kept
	// across their daemon's reconnects, which pick up the live state instead of starting over.
	// Required.
	Blocker SleepBlocker
	// DeadManGrace defaults to DeadManGrace.
	DeadManGrace time.Duration
	// AfterFunc runs f in its own goroutine once d has passed; defaults to time.AfterFunc.
	// Tests inject a manual clock.
	AfterFunc func(d time.Duration, f func())
	// Replaced reports whether the helper's executable has been replaced on disk since launch;
	// nil means never.
	Replaced func() bool
	// Version answers ipc.HelperVersion; defaults to paths.Version.
	Version string
	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

// Serve listens on cfg.Socket and answers daemon requests until ctx is cancelled (SIGTERM at
// shutdown or uninstall). On the way out it clears the block — disablesleep is a persistent
// setting that outlives the helper and a reboot, so a Mac shut down mid-block must not stay
// unable to sleep — closes every connection and removes the socket.
//
// It returns nil after a cancellation, ErrExecutableReplaced after stopping to adopt an updated
// binary, or an error if it could not start listening.
func Serve(ctx context.Context, cfg Config) error {
	if cfg.Socket == "" || cfg.Authorize == nil || cfg.Blocker == nil {
		return errors.New("helper: Config needs Socket, Authorize and Blocker")
	}
	ln, err := listen(cfg.Socket, cfg.RunDir)
	if err != nil {
		return err
	}
	ctx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	s := newServer(cfg, stop)
	s.log.Info("listening", "socket", cfg.Socket, "version", s.version)

	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		s.acceptLoop(ctx, ln)
	}()

	<-ctx.Done()
	s.shutdown(ln)
	<-acceptDone
	if errors.Is(context.Cause(ctx), ErrExecutableReplaced) {
		return ErrExecutableReplaced
	}
	return nil
}

type server struct {
	cfg     Config
	log     *slog.Logger
	grace   time.Duration
	after   func(time.Duration, func())
	version string
	stop    context.CancelCauseFunc

	// policyMu serializes every Blocker call — the block is machine-global, and pmset must
	// never run twice at once — and guards closing. Lock order: policyMu before mu.
	policyMu sync.Mutex
	// closing is set once the helper is on its way out; later blocks are refused, so nothing can
	// re-block after the final unblock.
	closing bool

	// authorizing holds a slot for each connection being authorized (maxAuthorizing).
	authorizing chan struct{}

	// mu guards the connection bookkeeping.
	mu sync.Mutex
	// owners is every user whose daemon is connected, or asked for the block and has not been
	// written off by its dead-man check yet, keyed by UID.
	owners map[int]*owner
	// generation changes whenever a connection is accepted or ends. Each owner records the value
	// of its own latest change, which invalidates a dead-man check scheduled for it under an
	// older picture.
	generation uint64
	conns      map[*net.UnixConn]struct{}
	stopped    bool
	wg         sync.WaitGroup
}

// owner is one user's daemon as the helper sees it. A daemon owns its block: another user's
// unblock must not clear it, and another user's connection must not keep it alive once its
// daemon is gone.
type owner struct {
	// live counts the owner's authorized connections still open.
	live int
	// wants is what the owner's latest set asked for. It outlives the owner's connections until
	// their dead-man check, so a daemon that reconnects within the grace keeps its block.
	wants bool
	// generation is the server generation of the owner's latest connection change.
	generation uint64
}

func newServer(cfg Config, stop context.CancelCauseFunc) *server {
	s := &server{
		cfg:         cfg,
		log:         cfg.Logger,
		grace:       cfg.DeadManGrace,
		after:       cfg.AfterFunc,
		version:     cfg.Version,
		stop:        stop,
		authorizing: make(chan struct{}, maxAuthorizing),
		owners:      map[int]*owner{},
		conns:       map[*net.UnixConn]struct{}{},
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.grace <= 0 {
		s.grace = DeadManGrace
	}
	if s.after == nil {
		s.after = func(d time.Duration, f func()) { time.AfterFunc(d, f) }
	}
	if s.version == "" {
		s.version = paths.Version
	}
	return s
}

// listen creates the run directory, clears a stale socket and listens. The socket is
// world-connectable on purpose: any local user may connect, and each connection is authorized
// on its own from the peer's audit token before a request is read. Until the chmod the socket
// has the umask's mode, which only denies.
func listen(socket, runDir string) (*net.UnixListener, error) {
	if runDir != "" {
		if err := os.MkdirAll(runDir, 0o755); err != nil {
			return nil, fmt.Errorf("helper: create %s: %w", runDir, err)
		}
		if err := os.Chmod(runDir, 0o755); err != nil {
			return nil, fmt.Errorf("helper: chmod %s: %w", runDir, err)
		}
	}
	if err := removeStaleSocket(socket); err != nil {
		return nil, err
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("helper: listen on %s: %w", socket, err)
	}
	if err := os.Chmod(socket, 0o666); err != nil {
		ln.Close()
		return nil, fmt.Errorf("helper: chmod %s: %w", socket, err)
	}
	return ln, nil
}

// removeStaleSocket deletes a socket file left by a helper that died without cleaning up. One
// that still answers belongs to a live helper: taking it over would leave two helpers fighting
// over one machine-wide setting.
func removeStaleSocket(socket string) error {
	if _, err := os.Lstat(socket); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if listening(socket) {
		return fmt.Errorf("helper: another helper is already listening on %s", socket)
	}
	if err := os.Remove(socket); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("helper: remove stale socket %s: %w", socket, err)
	}
	return nil
}

// listening reports whether something accepts connections on socket.
func listening(socket string) bool {
	conn, err := net.DialTimeout("unix", socket, time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// acceptLoop runs until the listener is closed. Accept errors (running out of descriptors under
// a connection flood) back off and retry instead of ending the helper, which would drop the
// block. A connection is handed on only once an authorization slot is free.
func (s *server) acceptLoop(ctx context.Context, ln *net.UnixListener) {
	var delay time.Duration
	for {
		conn, err := ln.AcceptUnix()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			delay = min(max(2*delay, 5*time.Millisecond), time.Second)
			s.log.Warn("accept failed; retrying", "err", err, "in", delay)
			select {
			case <-time.After(delay):
				continue
			case <-ctx.Done():
				return
			}
		}
		delay = 0
		select {
		case s.authorizing <- struct{}{}:
		case <-ctx.Done():
			conn.Close()
			return
		}
		if !s.track(conn) {
			<-s.authorizing
			conn.Close()
			continue
		}
		go s.handle(conn)
	}
}

func (s *server) track(conn *net.UnixConn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return false
	}
	s.conns[conn] = struct{}{}
	s.wg.Add(1)
	return true
}

func (s *server) untrack(conn *net.UnixConn) {
	conn.Close()
	s.mu.Lock()
	delete(s.conns, conn)
	s.mu.Unlock()
}

// handle serves one connection: authorize it, then answer requests in order until the daemon
// hangs up. The daemon keeps this connection open as its heartbeat.
func (s *server) handle(conn *net.UnixConn) {
	defer s.wg.Done()
	defer s.untrack(conn)
	uid, ok := s.cfg.Authorize(conn)
	<-s.authorizing
	if !ok {
		return
	}
	s.connectionStarted(uid)
	defer s.connectionEnded(uid)
	for {
		var req ipc.HelperRequest
		if err := ipc.ReadFrame(conn, &req); err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				s.log.Warn("reading a request failed; closing the connection", "err", err)
			}
			return
		}
		resp, mayRelaunch := s.respond(uid, req)
		_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
		if err := ipc.WriteFrame(conn, resp); err != nil {
			s.log.Warn("sending a response failed; closing the connection", "err", err)
			return
		}
		if mayRelaunch {
			s.relaunchIfReplaced()
		}
	}
}

// respond answers one request from uid's daemon under the policy lock. mayRelaunch reports a
// safe point to adopt an updated binary: an unblock returns the helper to idle, and the daemon
// asks for the version once at startup, so a freshly updated daemon is what triggers adopting a
// new helper.
//
// The state reported is the block as uid sees it: its own request, in effect. An unblock while
// another user's daemon still wants the block leaves the Mac blocked and reports it cleared,
// which is all this daemon asked for.
func (s *server) respond(uid int, req ipc.HelperRequest) (resp ipc.HelperResponse, mayRelaunch bool) {
	s.policyMu.Lock()
	defer s.policyMu.Unlock()
	switch req.Op {
	case ipc.HelperSet:
		if s.closing {
			// The final unblock already ran under this lock, so an unblock (the daemon's own
			// SIGTERM cleanup, racing ours at machine shutdown) has what it asked for.
			blocked := s.cfg.Blocker.Blocked()
			if !req.Blocked && !blocked {
				return ipc.HelperResponse{OK: true}, false
			}
			return ipc.HelperResponse{Error: "lidwake helper is shutting down", Blocked: blocked}, false
		}
		was := s.cfg.Blocker.Blocked()
		// The daemon re-asserts the block every minute while it holds; only changes are worth a
		// line in a log that launchd keeps open for good.
		level := slog.LevelInfo
		if was == req.Blocked {
			level = slog.LevelDebug
		}
		s.log.Log(context.Background(), level, "set requested", "uid", uid, "blocked", req.Blocked, "was", was)
		if wanted := s.want(uid, req.Blocked); wanted && !req.Blocked {
			s.log.Info("set complete; the block stays for another user's daemon", "uid", uid)
			return ipc.HelperResponse{OK: true}, false
		}
		err := s.cfg.Blocker.Set(req.Blocked)
		now := s.cfg.Blocker.Blocked()
		if err != nil {
			s.log.Error("set failed", "blocked", req.Blocked, "err", err, "now", now)
			return ipc.HelperResponse{Error: err.Error(), Blocked: now}, !req.Blocked
		}
		s.log.Log(context.Background(), level, "set complete", "blocked", now)
		return ipc.HelperResponse{OK: true, Blocked: now}, !req.Blocked
	case ipc.HelperState:
		blocked := s.blockedFor(uid)
		s.log.Debug("state requested", "uid", uid, "blocked", blocked)
		return ipc.HelperResponse{OK: true, Blocked: blocked}, false
	case ipc.HelperVersion:
		return ipc.HelperResponse{OK: true, Blocked: s.blockedFor(uid), Version: s.version}, true
	default:
		return ipc.HelperResponse{Error: fmt.Sprintf("unknown op %q", req.Op), Blocked: s.blockedFor(uid)}, false
	}
}

// want records what uid's daemon asks for and reports whether any daemon wants the block now.
// Called under policyMu.
func (s *server) want(uid int, blocked bool) (wanted bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if o := s.owners[uid]; o != nil {
		o.wants = blocked
	}
	return s.wantedLocked()
}

// wantedLocked reports whether any owner wants the block. Called under mu.
func (s *server) wantedLocked() bool {
	for _, o := range s.owners {
		if o.wants {
			return true
		}
	}
	return false
}

// blockedFor is the block as uid's daemon sees it: in effect, and asked for by that daemon.
// Called under policyMu.
func (s *server) blockedFor(uid int) bool {
	s.mu.Lock()
	o := s.owners[uid]
	wants := o != nil && o.wants
	s.mu.Unlock()
	return wants && s.cfg.Blocker.Blocked()
}

// relaunchIfReplaced stops the helper so launchd relaunches it from a binary an update put on
// disk — only while no daemon wants the block, since exiting clears it. The state is read under
// the policy lock and closing is set there too, so a concurrent set can't slip in and be dropped.
func (s *server) relaunchIfReplaced() {
	if s.cfg.Replaced == nil {
		return
	}
	s.policyMu.Lock()
	defer s.policyMu.Unlock()
	s.mu.Lock()
	wanted := s.wantedLocked()
	s.mu.Unlock()
	if s.closing || wanted || s.cfg.Blocker.Blocked() || !s.cfg.Replaced() {
		return
	}
	s.closing = true
	s.log.Info("the helper binary was replaced by an update; exiting so launchd relaunches the new one")
	s.stop(ErrExecutableReplaced)
}

func (s *server) connectionStarted(uid int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.owners[uid]
	if o == nil {
		o = &owner{}
		s.owners[uid] = o
	}
	o.live++
	s.generation++
	o.generation = s.generation
}

// connectionEnded is the dead-man switch, per owner: when a daemon's last connection drops while
// it wants the block and it does not reconnect within the grace window, drop its request, and
// clear the block unless another user's daemon still wants it. The daemon owns its block — if it
// was SIGKILLed (logout kills LaunchAgents; this root LaunchDaemon survives), nothing else would
// ever release disablesleep, and another user's daemon, connected but idle, never asks to.
func (s *server) connectionEnded(uid int) {
	s.policyMu.Lock()
	closing := s.closing
	s.policyMu.Unlock()
	s.mu.Lock()
	o := s.owners[uid]
	o.live--
	s.generation++
	o.generation = s.generation
	generation := o.generation
	if o.live > 0 {
		s.mu.Unlock()
		return
	}
	if !o.wants || closing {
		delete(s.owners, uid)
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	s.log.Warn("a blocking daemon's last connection dropped; dead-man check scheduled", "uid", uid, "grace", s.grace)
	s.after(s.grace, func() { s.deadManCheck(uid, generation) })
}

// deadManCheck writes off uid's request unless its connections changed since the check was
// scheduled, then clears the block if no other daemon wants it. It runs whether or not the block
// took: a request whose pmset failed still holds the idle assertion and would otherwise outlive
// its daemon.
func (s *server) deadManCheck(uid int, generation uint64) {
	s.policyMu.Lock()
	defer s.policyMu.Unlock()
	if s.closing {
		return
	}
	s.mu.Lock()
	o := s.owners[uid]
	if o == nil || o.generation != generation || o.live > 0 || !o.wants {
		s.mu.Unlock()
		return
	}
	delete(s.owners, uid)
	others := s.wantedLocked()
	s.mu.Unlock()
	if others {
		s.log.Warn("no connection from a blocking daemon; dropping its request, another user's daemon keeps the block", "uid", uid, "grace", s.grace)
		return
	}
	s.log.Warn("no daemon connection while blocked; clearing the sleep block", "uid", uid, "grace", s.grace)
	if err := s.cfg.Blocker.Set(false); err != nil {
		s.log.Error("dead-man unblock failed; retrying", "err", err, "in", s.grace)
		s.after(s.grace, s.retryUnblock)
	}
}

// retryUnblock repeats a failed dead-man unblock until it takes: with no daemon left, nothing else
// would ever clear the block. It stops once a daemon wants the block again.
func (s *server) retryUnblock() {
	s.policyMu.Lock()
	defer s.policyMu.Unlock()
	if s.closing {
		return
	}
	s.mu.Lock()
	wanted := s.wantedLocked()
	s.mu.Unlock()
	if wanted {
		return
	}
	if err := s.cfg.Blocker.Set(false); err != nil {
		s.log.Error("dead-man unblock failed again; retrying", "err", err, "in", s.grace)
		s.after(s.grace, s.retryUnblock)
	}
}

// shutdown stops accepting (closing the listener removes the socket file), clears the block,
// then closes every connection and waits for its goroutine.
func (s *server) shutdown(ln *net.UnixListener) {
	ln.Close()
	s.policyMu.Lock()
	s.closing = true
	s.log.Info("shutting down; clearing the sleep block")
	if err := s.cfg.Blocker.Set(false); err != nil {
		s.log.Error("clearing the sleep block at shutdown failed", "err", err)
	}
	s.policyMu.Unlock()

	s.mu.Lock()
	s.stopped = true
	for conn := range s.conns {
		conn.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
}
