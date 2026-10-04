package daemon

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/nikitaShakhbazyan/lidwake-go/internal/ipc"
)

// HelperClient is the daemon's connection to the root helper. *ipc.HelperClient implements it: it
// keeps one connection open (the helper treats it as the daemon's heartbeat and clears the block
// itself a minute after the last daemon connection drops) and redials a broken one on the next call.
type HelperClient interface {
	// SetBlocked asks the helper to block or unblock sleep and returns the state it reached.
	SetBlocked(blocked bool) (bool, error)
	// Version is the helper's version.
	Version() (string, error)
	// State is the helper's view of the block.
	State() (bool, error)
	// Connected reports whether a connection is open.
	Connected() bool
	// Close drops the connection.
	Close()
}

const (
	defaultRetryBase = time.Second
	defaultRetryMax  = 30 * time.Second
)

// helperDriver is the one goroutine that pushes the desired blocked state to the helper.
//
// Requests are applied serially and coalesced, latest wins: an apply always sends the state that
// is wanted when it starts, so a burst of edges (block, unblock, block) costs at most one helper
// round-trip behind the one in flight and can never leave the helper in a stale state the way
// out-of-order applies could. A failed apply — the helper unreachable, a call that timed out, or a
// helper that answered but could not reach the requested state (pmset failed) — is recorded for the
// status warning and retried with exponential backoff (1, 2, 4 … 30 s) until one succeeds — except
// an unblock the helper could not even be reached for, which has nothing to clear.
//
// Right before an apply turns a block into an unblock, beforeUnblock runs: the pre-sleep cue. With
// the lid closed, clearing the block is what lets the Mac sleep, so playing (and waiting for) the
// cue first guarantees it is heard. If a new acquire arrives while the cue plays, the apply keeps
// the block instead.
type helperDriver struct {
	client HelperClient
	log    *slog.Logger
	// beforeUnblock runs before a block → unblock apply; it may block, and its ctx ends at stop.
	beforeUnblock func(ctx context.Context)
	// afterApply runs after every apply attempt, with the state sent and whether it took.
	afterApply func(blocked, ok bool)
	// expectedVersion is compared with the helper's version once, after the first good apply.
	expectedVersion string
	retryBase       time.Duration
	retryMax        time.Duration

	kick chan struct{}

	mu       sync.Mutex
	desired  bool
	want     uint64 // generation of the latest request
	done     uint64 // newest request generation a finished attempt covered
	asked    bool   // the state the last attempt sent
	askedAny bool   // whether any attempt has been made
	failed   bool   // the last attempt did not take
	// connected is the client's connection state as of the last call. Cached because the
	// client's own Connected waits behind a call in flight.
	connected bool
	changed   chan struct{} // closed and replaced whenever done advances

	// callMu is held across every helper call, so stop's final unblock lands after any call in
	// flight and no driver call can follow it.
	callMu  sync.Mutex
	stopped bool
}

func newHelperDriver(client HelperClient, log *slog.Logger) *helperDriver {
	return &helperDriver{
		client:    client,
		log:       log,
		retryBase: defaultRetryBase,
		retryMax:  defaultRetryMax,
		kick:      make(chan struct{}, 1),
		changed:   make(chan struct{}),
	}
}

// Request asks for blocked to be applied and returns the request's generation for Wait. It never
// blocks.
func (h *helperDriver) Request(blocked bool) uint64 {
	h.mu.Lock()
	h.desired = blocked
	h.want++
	gen := h.want
	h.mu.Unlock()
	select {
	case h.kick <- struct{}{}:
	default:
	}
	return gen
}

// Wait blocks until an apply attempt that covers generation gen has finished (successfully or
// not), or ctx is done. It reports whether the attempt finished.
func (h *helperDriver) Wait(ctx context.Context, gen uint64) bool {
	for {
		h.mu.Lock()
		done, changed := h.done, h.changed
		h.mu.Unlock()
		if done >= gen {
			return true
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return false
		}
	}
}

// LastApplyFailed reports whether the most recent apply did not reach the requested state.
func (h *helperDriver) LastApplyFailed() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.failed
}

// Connected reports whether the connection to the helper was open after the last call; false
// before the first one.
func (h *helperDriver) Connected() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.connected
}

// Holds asks the helper whether it holds the block right now. False when it can't be reached or
// says no: a helper that crashed comes back without the block, because its start restores the
// saved setting. Serialized with the driver's own calls; after stop it reports true, as nothing
// is left to heal.
func (h *helperDriver) Holds() bool {
	h.callMu.Lock()
	defer h.callMu.Unlock()
	if h.stopped {
		return true
	}
	blocked, err := h.client.State()
	connected := h.client.Connected()
	h.mu.Lock()
	h.connected = connected
	h.mu.Unlock()
	return err == nil && blocked
}

// run applies requests until ctx is done.
func (h *helperDriver) run(ctx context.Context) {
	retry := time.NewTimer(time.Hour)
	retry.Stop()
	defer retry.Stop()
	attempts := 0
	versionChecked := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-h.kick:
		case <-retry.C:
		}
		res := h.apply(ctx)
		h.dropCoveredKick()
		if !res.applied {
			if ctx.Err() != nil {
				return
			}
			continue
		}
		if res.ok {
			attempts = 0
			retry.Stop()
			if !versionChecked {
				versionChecked = true
				h.checkVersion()
			}
			continue
		}
		// An unreachable helper holds no block to clear: it restores the original setting when it
		// starts, and a running one clears the block itself once the daemon's connection drops. A
		// block, or an unblock the helper answered but failed (pmset), is retried.
		if !res.blocked && errors.Is(res.err, ipc.ErrHelperUnreachable) {
			attempts = 0
			retry.Stop()
			continue
		}
		delay := h.backoff(attempts)
		attempts++
		h.log.Warn("helper apply failed; retrying", "in", delay)
		retry.Reset(delay)
	}
}

// dropCoveredKick discards a pending kick whose request the last apply already covered (one that
// arrived during the cue), so it does not cost a second, identical helper call. The kick is
// drained before the check, so a request racing it re-kicks and is never lost.
func (h *helperDriver) dropCoveredKick() {
	select {
	case <-h.kick:
	default:
		return
	}
	h.mu.Lock()
	pending := h.done < h.want
	h.mu.Unlock()
	if pending {
		select {
		case h.kick <- struct{}{}:
		default:
		}
	}
}

func (h *helperDriver) backoff(attempts int) time.Duration {
	delay := h.retryBase
	for i := 0; i < attempts && delay < h.retryMax; i++ {
		delay *= 2
	}
	return min(delay, h.retryMax)
}

// applyResult is one apply attempt: applied is false when nothing was sent because the driver is
// stopping; otherwise blocked is the state sent, ok whether it took and err the call's error.
type applyResult struct {
	applied, blocked, ok bool
	err                  error
}

// beforeUnblockLimit caps the pre-sleep cue: whatever it does, the unblock goes out after this.
const beforeUnblockLimit = 8 * time.Second

// runBeforeUnblock runs the pre-sleep hook but never lets it hold the unblock past
// beforeUnblockLimit, even if it ignores its context: a cue that can't finish must not keep the
// Mac awake with nothing holding it.
func (h *helperDriver) runBeforeUnblock(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, beforeUnblockLimit)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.beforeUnblock(ctx)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		h.log.Warn("the pre-sleep cue overran; clearing the sleep block anyway")
	}
}

// apply sends the currently wanted state.
func (h *helperDriver) apply(ctx context.Context) applyResult {
	h.mu.Lock()
	desired, gen := h.desired, h.want
	unblocking := !desired && h.askedAny && h.asked
	h.mu.Unlock()

	if unblocking && h.beforeUnblock != nil {
		h.runBeforeUnblock(ctx)
		h.mu.Lock()
		desired, gen = h.desired, h.want
		h.mu.Unlock()
	}
	if ctx.Err() != nil {
		return applyResult{}
	}

	h.callMu.Lock()
	if h.stopped {
		h.callMu.Unlock()
		return applyResult{}
	}
	h.mu.Lock()
	h.asked, h.askedAny = desired, true
	h.mu.Unlock()
	actual, err := h.client.SetBlocked(desired)
	connected := h.client.Connected()
	h.callMu.Unlock()

	ok := err == nil && actual == desired
	switch {
	case err != nil:
		h.log.Error("helper set failed", "blocked", desired, "err", err)
	case !ok:
		h.log.Error("helper applied a different state than requested", "requested", desired, "applied", actual)
	default:
		h.log.Info("helper applied", "blocked", desired)
	}

	h.mu.Lock()
	h.failed = !ok
	h.connected = connected
	if gen > h.done {
		h.done = gen
	}
	close(h.changed)
	h.changed = make(chan struct{})
	h.mu.Unlock()

	if h.afterApply != nil {
		h.afterApply(desired, ok)
	}
	return applyResult{applied: true, blocked: desired, ok: ok, err: err}
}

// checkVersion logs the helper's version once, so an old helper still resident after an update
// shows up in the log instead of only as failing calls. Asking is also the helper's cue that a
// freshly started daemon is connected, its safe point to adopt an updated binary of its own.
func (h *helperDriver) checkVersion() {
	h.callMu.Lock()
	if h.stopped {
		h.callMu.Unlock()
		return
	}
	version, err := h.client.Version()
	h.callMu.Unlock()
	switch {
	case err != nil:
		h.log.Warn("cannot read the helper version", "err", err)
	case h.expectedVersion != "" && version != h.expectedVersion:
		h.log.Warn("helper version differs from the daemon's; an old helper may still be resident", "helper", version, "daemon", h.expectedVersion)
	default:
		h.log.Info("helper version", "version", version)
	}
}

// stop sends the final unblock, after any call in flight and with no driver call after it, and
// waits at most timeout for it. It reports whether the unblock finished; when it did not, a call
// is still holding the client. The helper also clears the block on its own a minute after the
// daemon's connection drops, so a wedged helper costs at most that.
func (h *helperDriver) stop(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.callMu.Lock()
		defer h.callMu.Unlock()
		h.stopped = true
		if _, err := h.client.SetBlocked(false); err != nil {
			h.log.Error("clearing the sleep block at shutdown failed", "err", err)
		}
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		h.log.Warn("the helper did not confirm the unblock in time; it clears the block itself once the connection drops", "waited", timeout)
		return false
	}
}
