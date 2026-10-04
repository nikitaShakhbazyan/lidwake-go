// Package registry is the daemon's reference-counted store of assertions — its source of truth
// for "is any agent currently active". Acquire and release are idempotent by key.
package registry

import (
	"sort"
	"sync"
	"time"

	"github.com/nikitaShakhbazyan/lidwake-go/internal/model"
)

// Registry is a goroutine-safe set of assertions keyed by model.Assertion.Key. The Mac is
// blocked while it is non-empty.
//
// Edge notifications: BlockingChanges and DisplayChanges deliver every flip of IsBlocking and
// WantsDisplay, in mutation order, to one consumer each. Mutations only enqueue the edge (see
// edgeStream); delivery happens on a separate goroutine, never under the registry lock, so a
// consumer that takes its time applying an edge never stalls acquire/release, and a consumer
// calling back into the registry cannot deadlock.
type Registry struct {
	now func() time.Time

	mu                sync.Mutex
	assertions        map[string]model.Assertion
	version           uint64
	wasBlocking       bool
	wasWantingDisplay bool

	blocking *edgeStream
	display  *edgeStream
}

// New returns an empty registry. now stamps activity on re-acquire and touch; nil means
// time.Now.
func New(now func() time.Time) *Registry {
	if now == nil {
		now = time.Now
	}
	return &Registry{
		now:        now,
		assertions: map[string]model.Assertion{},
		blocking:   newEdgeStream(),
		display:    newEdgeStream(),
	}
}

// Close stops edge delivery and closes both change channels. Undelivered edges are dropped.
// The store itself keeps working. Safe to call more than once.
func (r *Registry) Close() {
	r.blocking.close()
	r.display.close()
}

// BlockingChanges delivers the new value of IsBlocking whenever it flips (false→true or
// true→false). Meant for a single consumer (the daemon) that drives the sleep-blocking helper.
// Edges emitted before the first read are buffered, and values arrive in order, so a consumer
// applying them serially never leaves the helper in a stale state.
func (r *Registry) BlockingChanges() <-chan bool { return r.blocking.channel() }

// DisplayChanges delivers the new value of WantsDisplay whenever it flips — the display-class
// sibling of BlockingChanges, consumed to raise/drop the display assertion. Derived from the
// assertions themselves, so every release path (explicit, idle sweep, TTL expiry, process exit,
// pause, cutouts) drops the display hold with no extra bookkeeping.
func (r *Registry) DisplayChanges() <-chan bool { return r.display.channel() }

// IsBlocking reports whether at least one assertion exists.
func (r *Registry) IsBlocking() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.assertions) > 0
}

// WantsDisplay reports whether any assertion carries the display class (HoldsDisplay).
func (r *Registry) WantsDisplay() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.wantsDisplayLocked()
}

// Count is the number of assertions.
func (r *Registry) Count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.assertions)
}

// Version is a monotonic change counter, bumped on every mutation of the store. Paired with a
// snapshot (VersionedSnapshot) it totally orders full-state payloads by content, so a receiver
// of payloads over racing paths can drop the stale one instead of letting the last writer win.
func (r *Registry) Version() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.version
}

// Snapshot returns a copy of every assertion, oldest AcquiredAt first (ties by key, so the
// order is deterministic).
func (r *Registry) Snapshot() []model.Assertion {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.snapshotLocked()
}

// VersionedSnapshot returns the assertions and the version they correspond to, read atomically
// — a version read separately from its snapshot could describe a different state.
func (r *Registry) VersionedSnapshot() ([]model.Assertion, uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.snapshotLocked(), r.version
}

// Acquire adds an assertion and reports whether it was new. A duplicate key does not change the
// count, but refreshes the existing assertion:
//   - LastActivityAt advances to now (the idle sweep treats a re-acquire as activity);
//   - a positive incoming PID is adopted together with its ProcessName — a resumed session
//     reuses its key under a new process, and keeping the old PID would leave the exit watcher
//     and the dead-PID rule bound to a process that no longer exists;
//   - a non-empty Reason and a non-nil ExpiresAt are adopted, otherwise the existing ones stay;
//   - HoldsDisplay is sticky: a re-acquire can upgrade to the display class but never downgrade
//     (dropping it mid-work would blind a screen-reading agent);
//   - Key, Tool, AcquiredAt and Origin are kept; WaitingFor is cleared (the agent is working
//     again; the session-status sweep re-stamps it if not).
func (r *Registry) Acquire(a model.Assertion) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.assertions[a.Key]; ok {
		updated := model.Assertion{
			Key:            existing.Key,
			Tool:           existing.Tool,
			Reason:         existing.Reason,
			PID:            existing.PID,
			ProcessName:    existing.ProcessName,
			AcquiredAt:     existing.AcquiredAt,
			LastActivityAt: r.now(),
			ExpiresAt:      existing.ExpiresAt,
			Origin:         existing.Origin,
			HoldsDisplay:   a.HoldsDisplay || existing.HoldsDisplay,
		}
		if a.Reason != "" {
			updated.Reason = a.Reason
		}
		if a.PID > 0 {
			updated.PID = a.PID
			updated.ProcessName = a.ProcessName
		}
		if a.ExpiresAt != nil {
			updated.ExpiresAt = cloneTime(a.ExpiresAt)
		}
		r.assertions[a.Key] = updated
		r.version++
		// A duplicate can't flip IsBlocking, but the sticky upgrade can flip WantsDisplay.
		r.notifyLocked()
		return false
	}
	r.assertions[a.Key] = clone(a)
	r.version++
	r.notifyLocked()
	return true
}

// Release removes the assertion with key and reports whether it existed. An unknown key is a
// no-op (the caller may surface a warning) and leaves the version untouched.
func (r *Registry) Release(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.assertions[key]; !ok {
		return false
	}
	delete(r.assertions, key)
	r.version++
	r.notifyLocked()
	return true
}

// ReleaseAllMatchingPID removes every assertion owned by pid and returns how many it removed.
// A non-positive pid matches nothing: such PIDs are sentinels (the CLI could not identify a real
// agent process), and one process-exit event must never drop every PID-less assertion at once.
func (r *Registry) ReleaseAllMatchingPID(pid int) int {
	if pid <= 0 {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for k, a := range r.assertions {
		if a.PID == pid {
			delete(r.assertions, k)
			n++
		}
	}
	if n > 0 {
		r.version++
	}
	r.notifyLocked()
	return n
}

// RemoveAll empties the store (pause, cutouts).
func (r *Registry) RemoveAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	clear(r.assertions)
	r.version++
	r.notifyLocked()
}

// ReplaceAll replaces the whole store, as on restore from state.json. Duplicate keys resolve
// last-wins rather than failing: a corrupted or hand-edited state file must not stop the daemon.
func (r *Registry) ReplaceAll(values []model.Assertion) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := make(map[string]model.Assertion, len(values))
	for _, a := range values {
		m[a.Key] = clone(a)
	}
	r.assertions = m
	r.version++
	r.notifyLocked()
}

// Touch marks activity on key (LastActivityAt = now). Unknown keys are ignored.
func (r *Registry) Touch(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.assertions[key]
	if !ok {
		return
	}
	a.LastActivityAt = r.now()
	r.assertions[key] = a
	r.version++
}

// SetExpiry overwrites an assertion's expiry outright; nil clears it. Unlike Acquire, whose TTL
// adoption can set but never clear one, this lets the session-status sweep arm a grace TTL
// while the owning agent waits for the user and put the original expiry (usually none) back
// when the wait resolves. Unknown keys are ignored.
func (r *Registry) SetExpiry(key string, at *time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.assertions[key]
	if !ok {
		return
	}
	a.ExpiresAt = cloneTime(at)
	r.assertions[key] = a
	r.version++
}

// SetWaitingFor stamps the assertion's waiting-on-user label; "" clears it. Unknown keys are
// ignored.
func (r *Registry) SetWaitingFor(key, label string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.assertions[key]
	if !ok {
		return
	}
	a.WaitingFor = label
	r.assertions[key] = a
	r.version++
}

// AnyHoldsDisplay reports whether any of the assertions carries the display class. Derived from
// the list, so a status built from a snapshot can never disagree with it.
func AnyHoldsDisplay(as []model.Assertion) bool {
	for _, a := range as {
		if a.HoldsDisplay {
			return true
		}
	}
	return false
}

func (r *Registry) wantsDisplayLocked() bool {
	for _, a := range r.assertions {
		if a.HoldsDisplay {
			return true
		}
	}
	return false
}

func (r *Registry) snapshotLocked() []model.Assertion {
	out := make([]model.Assertion, 0, len(r.assertions))
	for _, a := range r.assertions {
		out = append(out, clone(a))
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].AcquiredAt.Equal(out[j].AcquiredAt) {
			return out[i].AcquiredAt.Before(out[j].AcquiredAt)
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// notifyLocked enqueues an edge for each derived flag that flipped. Enqueuing under r.mu is what
// keeps edges in mutation order; it never waits for the consumer.
func (r *Registry) notifyLocked() {
	if now := len(r.assertions) > 0; now != r.wasBlocking {
		r.wasBlocking = now
		r.blocking.push(now)
	}
	if now := r.wantsDisplayLocked(); now != r.wasWantingDisplay {
		r.wasWantingDisplay = now
		r.display.push(now)
	}
}

// clone copies a so the store never shares its ExpiresAt pointer with a caller.
func clone(a model.Assertion) model.Assertion {
	a.ExpiresAt = cloneTime(a.ExpiresAt)
	return a
}

func cloneTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	c := *t
	return &c
}
