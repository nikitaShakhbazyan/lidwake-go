package monitor

import (
	"log/slog"
	"sync"

	"github.com/nikitaShakhbazyan/lidwake-go/internal/darwin"
)

const (
	// displayAssertionName is shown by `pmset -g assertions`; ASCII only, pmset renders anything
	// else as `?`.
	displayAssertionName = "lidwake display-class hold - an agent is reading the screen"
	displayWakeName      = "lidwake display-class hold wake"

	// DisplayBlindWarning is the status warning for a display hold that cannot work: the only
	// display is a closed built-in panel, so the agent behind the hold cannot see the screen.
	DisplayBlindWarning = "A display hold is active but the lid is closed with no other display — the agent behind it can't see the screen."
)

// AssertionHandle is a held power assertion. *darwin.PowerAssertion implements it.
type AssertionHandle interface {
	Release()
}

// createDisplayAssertion raises a PreventUserIdleDisplaySleep assertion (never a typed nil).
func createDisplayAssertion(name string) (AssertionHandle, error) {
	a, err := darwin.CreateAssertion(darwin.PreventUserIdleDisplaySleep, name)
	if err != nil {
		return nil, err
	}
	return a, nil
}

// DisplayHold is the daemon-owned display assertion for display-class holds
// (model.Assertion.HoldsDisplay).
//
// It lives in the daemon, not the privileged helper: PreventUserIdleDisplaySleep needs no root,
// and the helper's root surface stays minimal. The assertion is idempotent to set and reclaimed by
// the kernel if the daemon dies, so a stale display hold cannot outlive its process.
//
// Holding is not enough on its own: an IOPM assertion only prevents future display sleep. If the
// panel is already dark when the hold is raised, the agent behind it stays blind (a sleeping
// display collapses every app's accessibility tree to the bare application element), so raising
// the hold — and every later Set(true) — also declares user activity, the one call that relights
// a dark panel. That call does nothing while the display is awake.
//
// DisplayHold has no goroutine: Set is called by the daemon on display-state edges, by the 60 s
// reconcile and on wake. Safe for concurrent use.
type DisplayHold struct {
	// Create raises the display assertion. Nil: darwin.CreateAssertion with
	// darwin.PreventUserIdleDisplaySleep.
	Create func(name string) (AssertionHandle, error)
	// DeclareUserActivity relights a dark display. Nil: darwin.DeclareUserActivity.
	DeclareUserActivity func(name string) error
	// OnlyDisplayIsClosedBuiltIn reports that the only display is a closed built-in panel. Nil:
	// darwin.OnlyDisplayIsClosedBuiltIn.
	OnlyDisplayIsClosedBuiltIn func(lidClosed bool) bool
	Log                        *slog.Logger

	mu      sync.Mutex
	held    AssertionHandle
	stopped bool
}

// NewDisplayHold returns a hold backed by the real power-management calls.
func NewDisplayHold() *DisplayHold {
	return &DisplayHold{
		Create:                     createDisplayAssertion,
		DeclareUserActivity:        darwin.DeclareUserActivity,
		OnlyDisplayIsClosedBuiltIn: darwin.OnlyDisplayIsClosedBuiltIn,
	}
}

// Set raises or drops the display assertion. Idempotent: the reconcile re-calls it with the
// desired state, so an assertion that was lost is restored, and a panel that went dark through a
// path the assertion does not govern (a system sleep/wake cycle) is relit. A failed raise is
// logged and retried on the next Set(true).
func (h *DisplayHold) Set(wanted bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	log := logger(h.Log, "display")
	switch {
	case h.stopped:
		return
	case wanted && h.held == nil:
		create := h.Create
		if create == nil {
			create = createDisplayAssertion
		}
		a, err := create(displayAssertionName)
		if err != nil || a == nil {
			log.Error("display assertion failed", "err", err)
			return
		}
		h.held = a
		log.Info("display hold raised")
		h.wake(log)
	case !wanted && h.held != nil:
		h.held.Release()
		h.held = nil
		log.Info("display hold dropped")
	case wanted:
		h.wake(log)
	}
}

// Held reports whether the display assertion is raised.
func (h *DisplayHold) Held() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.held != nil
}

// Stop drops the assertion for good: Set does nothing afterwards.
func (h *DisplayHold) Stop() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.stopped = true
	if h.held != nil {
		h.held.Release()
		h.held = nil
	}
}

// Warning returns DisplayBlindWarning when a display hold is wanted while the only display is a
// closed built-in panel (nothing can relight a closed lid), else "". lidClosed is the real lid,
// not the cutouts' armed value. External and virtual displays count as real targets.
func (h *DisplayHold) Warning(displayWanted, lidClosed bool) string {
	if !displayWanted {
		return ""
	}
	only := h.OnlyDisplayIsClosedBuiltIn
	if only == nil {
		only = darwin.OnlyDisplayIsClosedBuiltIn
	}
	if !only(lidClosed) {
		return ""
	}
	return DisplayBlindWarning
}

// wake relights the display if it is asleep. Safe against the lock screen: lock is not sleep, and
// the wake relights the locked screen without dismissing it. Callers hold h.mu.
func (h *DisplayHold) wake(log *slog.Logger) {
	declare := h.DeclareUserActivity
	if declare == nil {
		declare = darwin.DeclareUserActivity
	}
	if err := declare(displayWakeName); err != nil {
		log.Error("display wake failed", "err", err)
	}
}
