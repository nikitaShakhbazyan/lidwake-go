package ipc

import (
	"errors"
	"net"
	"sync"
	"time"

	"github.com/nikitaShakhbazyan/lidwake-go/internal/paths"
)

// ErrDaemonUnreachable means nothing is listening on the CLI socket.
var ErrDaemonUnreachable = errors.New("lidwake daemon is not running")

// ErrHelperUnreachable means nothing is listening on the helper socket.
var ErrHelperUnreachable = errors.New("lidwake helper is not running")

// Send makes one request to the daemon and returns its reply. The reply can wait on a helper
// round-trip, so the deadline is generous; connecting is instant or fails.
func Send(req Request) (Response, error) {
	return SendTo(paths.CLISocket(), req, 10*time.Second)
}

// SendTo is Send against an explicit socket path (tests, alternate homes).
func SendTo(socket string, req Request, timeout time.Duration) (Response, error) {
	conn, err := net.DialTimeout("unix", socket, time.Second)
	if err != nil {
		return Response{}, ErrDaemonUnreachable
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	if err := WriteFrame(conn, req); err != nil {
		return Response{}, err
	}
	var resp Response
	if err := ReadFrame(conn, &resp); err != nil {
		return Response{}, err
	}
	return resp, nil
}

// HelperClient is the daemon's connection to the root helper. It keeps one connection open for
// as long as it can: the helper treats the connection as the daemon's heartbeat and restores the
// sleep setting itself when no daemon has been connected for a minute while blocked. Calls are
// serialized; a broken connection is redialled once per call. Connected and Close never wait for
// a call in flight: Close shuts the socket, which fails that call at once.
type HelperClient struct {
	socket string
	callMu sync.Mutex // serializes calls
	connMu sync.Mutex // guards conn and closes
	conn   net.Conn
	closes uint64 // bumped by Close, so a call it interrupted fails instead of redialling
}

// NewHelperClient returns a client for the helper socket (paths.HelperSocket in production).
func NewHelperClient(socket string) *HelperClient {
	return &HelperClient{socket: socket}
}

// Connected reports whether a connection is currently open.
func (c *HelperClient) Connected() bool {
	c.connMu.Lock()
	defer c.connMu.Unlock()
	return c.conn != nil
}

// Connect opens the connection if it isn't open; used at startup and by reconnect loops.
func (c *HelperClient) Connect() error {
	_, err := c.connection()
	return err
}

// SetBlocked asks the helper to block or unblock sleep and returns the resulting state.
func (c *HelperClient) SetBlocked(blocked bool) (bool, error) {
	resp, err := c.call(HelperRequest{Op: HelperSet, Blocked: blocked})
	if err != nil {
		return false, err
	}
	if !resp.OK {
		return resp.Blocked, errors.New(resp.Error)
	}
	return resp.Blocked, nil
}

// State reports the helper's view of the block.
func (c *HelperClient) State() (bool, error) {
	resp, err := c.call(HelperRequest{Op: HelperState})
	return resp.Blocked, err
}

// Version reports the helper's version.
func (c *HelperClient) Version() (string, error) {
	resp, err := c.call(HelperRequest{Op: HelperVersion})
	return resp.Version, err
}

// Close drops the connection, failing a call in flight.
func (c *HelperClient) Close() {
	c.connMu.Lock()
	defer c.connMu.Unlock()
	c.closes++
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}
}

func (c *HelperClient) call(req HelperRequest) (HelperResponse, error) {
	c.callMu.Lock()
	defer c.callMu.Unlock()
	var lastErr error
	c.connMu.Lock()
	closes := c.closes
	c.connMu.Unlock()
	for attempt := 0; attempt < 2; attempt++ {
		conn, err := c.connection()
		if err != nil {
			return HelperResponse{}, err
		}
		// pmset runs under the helper's own 10 s watchdog; leave room for it.
		_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
		var resp HelperResponse
		err = WriteFrame(conn, req)
		if err == nil {
			err = ReadFrame(conn, &resp)
		}
		if err == nil {
			_ = conn.SetDeadline(time.Time{})
			return resp, nil
		}
		lastErr = err
		if c.drop(conn, closes) {
			break // Close interrupted the call: report it, don't redial
		}
	}
	return HelperResponse{}, lastErr
}

// connection returns the open connection, dialling one if needed.
func (c *HelperClient) connection() (net.Conn, error) {
	c.connMu.Lock()
	defer c.connMu.Unlock()
	if c.conn != nil {
		return c.conn, nil
	}
	conn, err := net.DialTimeout("unix", c.socket, time.Second)
	if err != nil {
		return nil, ErrHelperUnreachable
	}
	c.conn = conn
	return conn, nil
}

// drop closes conn if it is still the current connection and reports whether Close ran since
// the call began.
func (c *HelperClient) drop(conn net.Conn, closes uint64) bool {
	c.connMu.Lock()
	defer c.connMu.Unlock()
	conn.Close()
	if c.conn == conn {
		c.conn = nil
	}
	return c.closes != closes
}
