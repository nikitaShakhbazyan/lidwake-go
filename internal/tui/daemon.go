package tui

import (
	"errors"
	"fmt"
	"time"

	"github.com/nikitaShakhbazyan/lidwake-go/internal/ipc"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/model"
)

// errNoStatus is a status reply without a status in it.
var errNoStatus = errors.New("the daemon sent no status")

// RefusedError is a request the daemon answered with ok=false.
type RefusedError struct {
	Op      ipc.Op
	Message string
}

func (e *RefusedError) Error() string {
	return fmt.Sprintf("the daemon refused %s: %s", e.Op, e.Message)
}

// DaemonActions sends the dashboard's requests to the daemon over the per-user CLI socket.
type DaemonActions struct {
	// Send makes one round trip; nil means ipc.Send.
	Send func(ipc.Request) (ipc.Response, error)
}

var _ Actions = DaemonActions{}

// Status asks the daemon for its status. Transport errors come back unwrapped: their text is
// what the "daemon not running" screen shows.
func (d DaemonActions) Status() (*model.Status, error) {
	resp, err := d.send(ipc.Request{Op: ipc.OpStatus})
	if err != nil {
		return nil, err
	}
	if resp.Status == nil {
		return nil, errNoStatus
	}
	return resp.Status, nil
}

// SetPaused sends pause or resume.
func (d DaemonActions) SetPaused(paused bool) (int, error) {
	op := ipc.OpResume
	if paused {
		op = ipc.OpPause
	}
	resp, err := d.send(ipc.Request{Op: op})
	if err != nil {
		return 0, err
	}
	return deref(resp.ReleasedCount), nil
}

// SetTimer arms the off timer, or cancels it for a zero duration (no TTL on the wire).
func (d DaemonActions) SetTimer(dur time.Duration) (time.Duration, bool, error) {
	req := ipc.Request{Op: ipc.OpTimer}
	if dur > 0 {
		req.TTL = ipc.Ptr(dur.Seconds())
	}
	resp, err := d.send(req)
	if err != nil {
		return 0, false, err
	}
	if resp.AppliedTTL == nil {
		return 0, false, nil
	}
	return time.Duration(*resp.AppliedTTL * float64(time.Second)), true, nil
}

// ReleaseAll force-releases every hold.
func (d DaemonActions) ReleaseAll() (int, error) {
	resp, err := d.send(ipc.Request{Op: ipc.OpReleaseAll})
	if err != nil {
		return 0, err
	}
	return deref(resp.ReleasedCount), nil
}

func (d DaemonActions) send(req ipc.Request) (ipc.Response, error) {
	send := d.Send
	if send == nil {
		send = ipc.Send
	}
	resp, err := send(req)
	if err != nil {
		return resp, err
	}
	if !resp.OK {
		return resp, &RefusedError{Op: req.Op, Message: resp.Error}
	}
	return resp, nil
}

func deref(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}
