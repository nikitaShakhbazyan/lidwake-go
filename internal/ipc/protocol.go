// Package ipc is the wire protocol between the CLI and the daemon (a per-user Unix socket) and
// between the daemon and the root helper (a system Unix socket). Both use the same framing: a
// 4-byte big-endian length followed by that many bytes of JSON.
package ipc

import (
	"github.com/nikitaShakhbazyan/lidwake-go/internal/model"
)

// Op is a CLI → daemon operation.
type Op string

const (
	OpPing    Op = "ping"
	OpStatus  Op = "status"
	OpAcquire Op = "acquire"
	OpRelease Op = "release"
	// OpHold places an explicit hold; the daemon mints the "hold:" key and clamps the TTL.
	OpHold Op = "hold"
	// OpReleaseAll force-releases every assertion (a user action).
	OpReleaseAll Op = "releaseAll"
	// OpPause turns lidwake off: release everything, refuse acquires until OpResume.
	OpPause  Op = "pause"
	OpResume Op = "resume"
	// OpTimer pauses automatically after TTL seconds; a missing or zero TTL cancels the timer.
	// Setting a timer also resumes a paused lidwake.
	OpTimer Op = "timer"
	// OpReloadSettings re-reads config.json.
	OpReloadSettings Op = "reloadSettings"
)

// Request is a CLI → daemon message.
type Request struct {
	Op          Op     `json:"op"`
	Key         string `json:"key,omitempty"`
	Tool        string `json:"tool,omitempty"`
	Reason      string `json:"reason,omitempty"`
	PID         int    `json:"pid,omitempty"`
	ProcessName string `json:"processName,omitempty"`
	// TTL in seconds; nil means "none" (acquire/hold) or "cancel" (timer).
	TTL *float64 `json:"ttl,omitempty"`
	// Display also keeps the display awake (hold/acquire).
	Display bool `json:"display,omitempty"`
}

// Response is the daemon's reply.
type Response struct {
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
	Warning string `json:"warning,omitempty"`
	// Blocking is whether the Mac is being kept awake after the request.
	Blocking       *bool `json:"blocking,omitempty"`
	AssertionCount *int  `json:"assertionCount,omitempty"`
	// Status answers OpStatus.
	Status *model.Status `json:"status,omitempty"`
	// HoldKey is the minted key for OpHold.
	HoldKey string `json:"holdKey,omitempty"`
	// AppliedTTL is the TTL actually applied (OpHold) or the seconds until the off timer
	// fires (OpTimer), nil when cancelled.
	AppliedTTL     *float64 `json:"appliedTTL,omitempty"`
	ReleasedCount  *int     `json:"releasedCount,omitempty"`
	DisplayApplied *bool    `json:"displayApplied,omitempty"`
}

// HelperOp is a daemon → helper operation.
type HelperOp string

const (
	// HelperSet blocks (Blocked=true) or unblocks system sleep, lid-close sleep included.
	// Idempotent; a repeated set(true) re-applies the block.
	HelperSet HelperOp = "set"
	// HelperState reports whether the block is on for the caller: its own daemon asked for it and
	// the helper holds it. False after a helper restart, which is how the daemon notices one.
	HelperState HelperOp = "state"
	// HelperVersion reports the helper's version.
	HelperVersion HelperOp = "version"
)

type HelperRequest struct {
	Op      HelperOp `json:"op"`
	Blocked bool     `json:"blocked,omitempty"`
}

type HelperResponse struct {
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
	Blocked bool   `json:"blocked"`
	Version string `json:"version,omitempty"`
}

// Ptr returns a pointer to v, for the optional fields above.
func Ptr[T any](v T) *T { return &v }
