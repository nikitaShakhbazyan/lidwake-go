package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/nikitaShakhbazyan/lidwake-go/internal/ipc"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/model"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/policy"
)

// socketServer is the CLI socket: one framed ipc.Request in, one ipc.Response out, per connection.
// The socket accepts any process of the user (that is how agent hooks reach the daemon), so every
// request is validated before it touches the registry.
type socketServer struct {
	d  *Daemon
	ln *net.UnixListener // nil when listening failed
	wg sync.WaitGroup
}

// startSocket listens on the CLI socket. A failure is logged and the daemon runs on without it:
// the monitors still manage what was restored, and the block is still cleared at shutdown.
func (d *Daemon) startSocket(ctx context.Context) *socketServer {
	srv := &socketServer{d: d}
	ln, err := listenSocket(d.socket)
	if err != nil {
		d.log.Error("CLI socket unavailable; the daemon runs without it", "err", err)
		return srv
	}
	srv.ln = ln
	d.log.Info("CLI socket listening", "socket", d.socket)
	srv.wg.Add(1)
	go func() {
		defer srv.wg.Done()
		srv.acceptLoop(ctx)
	}()
	return srv
}

// listenSocket creates the socket's directory (0700: the socket is the user's alone), clears a
// stale socket and listens with the socket at 0600. The directory already keeps everyone else out
// while the socket briefly has the umask's mode; the process-wide umask is not touched, since other
// goroutines create files concurrently.
func listenSocket(path string) (*net.UnixListener, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("daemon: create %s: %w", dir, err)
	}
	if info, err := os.Stat(dir); err == nil && info.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(dir, info.Mode().Perm()&^0o077); err != nil {
			return nil, fmt.Errorf("daemon: restrict %s: %w", dir, err)
		}
	}
	if err := removeStaleSocket(path); err != nil {
		return nil, err
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("daemon: listen on %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, fmt.Errorf("daemon: chmod %s: %w", path, err)
	}
	return ln, nil
}

// removeStaleSocket deletes a socket left by a daemon that died without cleaning up. One that still
// answers belongs to a live daemon and is left alone.
func removeStaleSocket(path string) error {
	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if listening(path) {
		return fmt.Errorf("daemon: %w on %s", errAlreadyRunning, path)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("daemon: remove stale socket %s: %w", path, err)
	}
	return nil
}

// listening reports whether something accepts connections on path.
func listening(path string) bool {
	conn, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// acceptLoop runs until the listener closes. Accept errors (out of descriptors under a flood) back
// off and retry rather than ending the CLI surface.
func (s *socketServer) acceptLoop(ctx context.Context) {
	var delay time.Duration
	for {
		conn, err := s.ln.AcceptUnix()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			delay = min(max(2*delay, 5*time.Millisecond), time.Second)
			s.d.log.Warn("CLI accept failed; retrying", "err", err, "in", delay)
			select {
			case <-time.After(delay):
				continue
			case <-ctx.Done():
				return
			}
		}
		delay = 0
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handle(ctx, conn)
		}()
	}
}

// handle serves one connection. Read and write are bounded, so a client that connects and never
// sends (or never reads) cannot park a goroutine forever. A malformed request gets no reply; the
// client sees EOF.
func (s *socketServer) handle(ctx context.Context, conn *net.UnixConn) {
	defer conn.Close()
	timeout := s.d.cfg.ConnTimeout
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	var req ipc.Request
	if err := ipc.ReadFrame(conn, &req); err != nil {
		if !errors.Is(err, io.EOF) {
			s.d.log.Debug("unreadable CLI request", "err", err)
		}
		return
	}
	resp := s.d.handleRequest(ctx, req)
	_ = conn.SetWriteDeadline(time.Now().Add(timeout))
	if err := ipc.WriteFrame(conn, resp); err != nil {
		s.d.log.Debug("cannot send a CLI reply", "op", string(req.Op), "err", err)
	}
}

// close stops accepting and removes the socket.
func (s *socketServer) close() {
	if s.ln != nil {
		s.ln.Close()
	}
}

// wait waits at most timeout for the connections in flight.
func (s *socketServer) wait(timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
	}
}

// handleRequest answers one CLI request.
func (d *Daemon) handleRequest(ctx context.Context, req ipc.Request) ipc.Response {
	if req.Op != ipc.OpPing && d.isClosing() {
		return ipc.Response{Error: "the lidwake daemon is shutting down"}
	}
	switch req.Op {
	case ipc.OpPing:
		return ipc.Response{OK: true}
	case ipc.OpStatus:
		st := d.status()
		return ipc.Response{OK: true, Blocking: ipc.Ptr(st.Blocking), AssertionCount: ipc.Ptr(len(st.Assertions)), Status: &st}
	case ipc.OpAcquire:
		return d.handleAcquireRequest(req)
	case ipc.OpRelease:
		return d.handleReleaseRequest(req)
	case ipc.OpHold:
		return d.handleHoldRequest(req)
	case ipc.OpReleaseAll:
		n := d.releaseAll(ctx)
		resp := ipc.Response{OK: true, Blocking: ipc.Ptr(false), AssertionCount: ipc.Ptr(0), ReleasedCount: ipc.Ptr(n)}
		if n == 0 {
			resp.Warning = "nothing was held — released nothing"
		}
		return resp
	case ipc.OpPause:
		n := d.setPaused(ctx, true)
		return ipc.Response{OK: true, Blocking: ipc.Ptr(false), AssertionCount: ipc.Ptr(0), ReleasedCount: ipc.Ptr(n)}
	case ipc.OpResume:
		n := d.setPaused(ctx, false)
		return ipc.Response{OK: true, AssertionCount: ipc.Ptr(n)}
	case ipc.OpTimer:
		resp := ipc.Response{OK: true}
		if deadline := d.setOffTimer(req.TTL); deadline != nil {
			resp.AppliedTTL = ipc.Ptr(deadline.Sub(d.now()).Seconds())
		}
		return resp
	case ipc.OpReloadSettings:
		d.reloadSettings()
		return ipc.Response{OK: true}
	default:
		return ipc.Response{Error: fmt.Sprintf("unknown op %q", truncateRunes(string(req.Op), policy.MaxToolLength))}
	}
}

func (d *Daemon) isClosing() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.closing
}

func (d *Daemon) handleAcquireRequest(req ipc.Request) ipc.Response {
	if req.Key == "" || req.Tool == "" {
		return ipc.Response{Error: "acquire requires key and tool"}
	}
	if msg := policy.AcquireRejection(req.Key, req.Tool); msg != "" {
		return ipc.Response{Error: msg}
	}
	var ttl *time.Duration
	if seconds := policy.ClampedTTL(req.TTL); seconds != nil {
		dur := secondsToDuration(*seconds)
		ttl = &dur
	}
	name := req.ProcessName
	if name == "" {
		name = req.Tool
	}
	a := model.New(req.Key, req.Tool, policy.ClampedReason(req.Reason), agentPID(req.PID),
		truncateRunes(name, policy.MaxKeyLength), d.now(), ttl, model.OriginHook)
	a.HoldsDisplay = req.Display

	d.mu.Lock()
	outcome, message := d.acquireLocked(a)
	count := d.registry.Count()
	d.mu.Unlock()
	switch outcome {
	case acquireAccepted:
		// DisplayApplied echoes even when false: its presence tells the CLI the daemon knows
		// about display holds.
		return ipc.Response{OK: true, Blocking: ipc.Ptr(count > 0), AssertionCount: ipc.Ptr(count), DisplayApplied: ipc.Ptr(req.Display)}
	case acquirePaused:
		// A paused daemon ignoring an acquire is expected, not a hook failure.
		return ipc.Response{OK: true, Blocking: ipc.Ptr(false), AssertionCount: ipc.Ptr(count), Warning: "lidwake is paused — acquire ignored"}
	case acquireOverCapacity:
		return ipc.Response{Error: "too many active assertions", Blocking: ipc.Ptr(count > 0), AssertionCount: ipc.Ptr(count)}
	case acquireLatched:
		return ipc.Response{Error: message, Blocking: ipc.Ptr(false), AssertionCount: ipc.Ptr(count)}
	default:
		return ipc.Response{Error: "the lidwake daemon is shutting down"}
	}
}

// handleReleaseRequest releases exactly the key given: a "hold:" id, a "sniffed:" key or a
// "<tool>:<session>" key, as `status --json` prints them. The CLI derives the key; an unknown key
// is a warning, not an error.
func (d *Daemon) handleReleaseRequest(req ipc.Request) ipc.Response {
	if req.Key == "" {
		return ipc.Response{Error: "release requires key"}
	}
	d.mu.Lock()
	existed := d.releaseLocked(req.Key)
	count := d.registry.Count()
	d.mu.Unlock()
	resp := ipc.Response{OK: true, Blocking: ipc.Ptr(count > 0), AssertionCount: ipc.Ptr(count)}
	if !existed {
		resp.Warning = fmt.Sprintf("no assertion for key '%s' — released nothing", truncateRunes(req.Key, policy.MaxKeyLength))
	}
	return resp
}

// handleHoldRequest places a hold under a key the daemon mints. The tool is only its display label,
// so an over-long one is clamped like the reason rather than refused: `run` sends the command's
// basename, and a refusal would run a long-named job without the hold it exists to place.
func (d *Daemon) handleHoldRequest(req ipc.Request) ipc.Response {
	tool := truncateRunes(req.Tool, policy.MaxToolLength)
	out := d.hold(policy.ClampedReason(req.Reason), req.TTL, agentPID(req.PID), tool, req.Display)
	switch {
	case out.placed:
		return ipc.Response{
			OK: true, Blocking: ipc.Ptr(out.count > 0), AssertionCount: ipc.Ptr(out.count),
			HoldKey: out.key, AppliedTTL: ipc.Ptr(out.ttl), DisplayApplied: ipc.Ptr(req.Display),
		}
	case out.disabled:
		return ipc.Response{Error: "Agent holds are turned off in lidwake settings."}
	case out.paused:
		return ipc.Response{Error: "lidwake is paused — resume it to place a hold."}
	default:
		return ipc.Response{Error: out.refusal}
	}
}

// agentPID maps a missing or non-positive PID to -1: the CLI could not identify a real agent
// process, so nothing is watched for it.
func agentPID(pid int) int {
	if pid <= 0 {
		return -1
	}
	return pid
}
