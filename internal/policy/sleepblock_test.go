package policy

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
)

// fakeIdle models the real idle assertion: Acquire is idempotent (the production implementation
// guards on a held assertion), so a double acquire holds exactly one assertion.
type fakeIdle struct {
	held                       bool
	acquireCount, releaseCount int
}

func (f *fakeIdle) IsHeld() bool { return f.held }
func (f *fakeIdle) Acquire() {
	if f.held {
		return
	}
	f.acquireCount++
	f.held = true
}
func (f *fakeIdle) Release() {
	if !f.held {
		return
	}
	f.releaseCount++
	f.held = false
}

var errBoom = errors.New("boom")

// fakeClamshell is the machine-global flag: IsDisabled reflects the last successful SetDisabled.
type fakeClamshell struct {
	calls    []bool
	disabled bool
	// throwOn, when set, makes SetDisabled(*throwOn) fail.
	throwOn *bool
}

func (f *fakeClamshell) IsDisabled() bool { return f.disabled }
func (f *fakeClamshell) SetDisabled(d bool) error {
	if f.throwOn != nil && *f.throwOn == d {
		return errBoom
	}
	f.calls = append(f.calls, d)
	f.disabled = d
	return nil
}

var errFull = errors.New("disk full")

type fakeStore struct {
	value     *bool
	saveCount int
	failSave  bool
}

func (f *fakeStore) Load() (bool, bool) {
	if f.value == nil {
		return false, false
	}
	return *f.value, true
}
func (f *fakeStore) Save(d bool) error {
	if f.failSave {
		return errFull
	}
	f.saveCount++
	f.value = &d
	return nil
}
func (f *fakeStore) Clear() { f.value = nil }

func storeWith(v bool) *fakeStore { return &fakeStore{value: &v} }

func TestSleepBlockPolicy(t *testing.T) {
	t.Run("init leaves the flag alone when nothing was saved", func(t *testing.T) {
		clam := &fakeClamshell{disabled: true}
		policy := NewSleepBlockPolicy(&fakeIdle{}, clam, &fakeStore{})
		if len(clam.calls) != 0 {
			t.Fatalf("calls=%v", clam.calls)
		}
		if !clam.disabled {
			t.Fatal("the owner's own disablesleep=1 did not survive")
		}
		if policy.Blocked() {
			t.Fatal("blocked at start")
		}
	})

	t.Run("init restores a value saved by a crashed instance", func(t *testing.T) {
		for _, saved := range []bool{false, true} {
			t.Run(fmt.Sprint(saved), func(t *testing.T) {
				clam, store := &fakeClamshell{disabled: true}, storeWith(saved)
				NewSleepBlockPolicy(&fakeIdle{}, clam, store)
				if !slices.Equal(clam.calls, []bool{saved}) {
					t.Fatalf("calls=%v", clam.calls)
				}
				if store.value != nil {
					t.Fatal("saved value not cleared")
				}
			})
		}
	})

	t.Run("set(true) saves the original once, then disables clamshell sleep", func(t *testing.T) {
		idle, clam, store := &fakeIdle{}, &fakeClamshell{}, &fakeStore{}
		policy := NewSleepBlockPolicy(idle, clam, store)
		if err := policy.Set(true); err != nil {
			t.Fatal(err)
		}
		if idle.acquireCount != 1 || !idle.held {
			t.Fatalf("idle acquires=%d held=%v", idle.acquireCount, idle.held)
		}
		if store.value == nil || *store.value {
			t.Fatalf("saved=%v, want false", store.value)
		}
		if !slices.Equal(clam.calls, []bool{true}) {
			t.Fatalf("calls=%v", clam.calls)
		}
		if !policy.Blocked() {
			t.Fatal("not blocked")
		}
	})

	t.Run("set(false) puts back the value the Mac had before", func(t *testing.T) {
		for _, before := range []bool{false, true} {
			t.Run(fmt.Sprint(before), func(t *testing.T) {
				idle, clam, store := &fakeIdle{}, &fakeClamshell{disabled: before}, &fakeStore{}
				policy := NewSleepBlockPolicy(idle, clam, store)
				if err := policy.Set(true); err != nil {
					t.Fatal(err)
				}
				if err := policy.Set(false); err != nil {
					t.Fatal(err)
				}
				if idle.releaseCount != 1 || idle.held {
					t.Fatalf("idle releases=%d held=%v", idle.releaseCount, idle.held)
				}
				if clam.disabled != before {
					t.Fatalf("disabled=%v, want %v", clam.disabled, before)
				}
				if store.value != nil {
					t.Fatal("saved value not cleared")
				}
				if policy.Blocked() {
					t.Fatal("still blocked")
				}
			})
		}
	})

	t.Run("repeated set(true) re-asserts clamshell but keeps the first saved value", func(t *testing.T) {
		idle, clam, store := &fakeIdle{}, &fakeClamshell{}, &fakeStore{}
		policy := NewSleepBlockPolicy(idle, clam, store)
		for range 2 {
			if err := policy.Set(true); err != nil {
				t.Fatal(err)
			}
		}
		if idle.acquireCount != 1 {
			t.Fatalf("acquires=%d (acquire is idempotent)", idle.acquireCount)
		}
		if !slices.Equal(clam.calls, []bool{true, true}) {
			t.Fatalf("calls=%v (clamshell must be re-asserted for wake recovery)", clam.calls)
		}
		if store.saveCount != 1 || store.value == nil || *store.value {
			t.Fatalf("saves=%d value=%v (must not be overwritten by our own block)", store.saveCount, store.value)
		}
	})

	t.Run("a clamshell failure while blocking propagates but keeps the restore point", func(t *testing.T) {
		idle, clam, store := &fakeIdle{}, &fakeClamshell{}, &fakeStore{}
		policy := NewSleepBlockPolicy(idle, clam, store)
		clam.throwOn = boolp(true)
		if err := policy.Set(true); !errors.Is(err, errBoom) {
			t.Fatalf("err=%v, want boom", err)
		}
		if policy.Blocked() {
			t.Fatal("block did not complete but reports blocked")
		}
		if !idle.held {
			t.Fatal("the idle assertion should already be held")
		}
		if store.value == nil || *store.value {
			t.Fatalf("restore point=%v, want false", store.value)
		}
	})

	t.Run("a failure to save the original refuses to block", func(t *testing.T) {
		clam, store := &fakeClamshell{}, &fakeStore{failSave: true}
		policy := NewSleepBlockPolicy(&fakeIdle{}, clam, store)
		if err := policy.Set(true); !errors.Is(err, errFull) {
			t.Fatalf("err=%v, want disk full", err)
		}
		if len(clam.calls) != 0 {
			t.Fatalf("disabled sleep without a way back: calls=%v", clam.calls)
		}
	})

	t.Run("a clamshell failure while unblocking is reported and keeps the block for a retry", func(t *testing.T) {
		idle, clam, store := &fakeIdle{}, &fakeClamshell{}, &fakeStore{}
		policy := NewSleepBlockPolicy(idle, clam, store)
		if err := policy.Set(true); err != nil {
			t.Fatal(err)
		}
		clam.throwOn = boolp(false)
		if err := policy.Set(false); !errors.Is(err, errBoom) {
			t.Fatalf("err=%v, want boom: a failed restore must not look like success", err)
		}
		// Sleep is still disabled, so the helper's dead-man switch must stay armed.
		if !policy.Blocked() || !clam.disabled {
			t.Fatalf("blocked=%v disabled=%v, want both true", policy.Blocked(), clam.disabled)
		}
		if idle.held {
			t.Fatal("the idle assertion was not released")
		}
		if store.value == nil || *store.value {
			t.Fatalf("restore point=%v, want kept false for the retry", store.value)
		}
		clam.throwOn = nil
		if err := policy.Set(false); err != nil {
			t.Fatal(err)
		}
		if policy.Blocked() || clam.disabled || store.value != nil {
			t.Fatalf("after the retry: blocked=%v disabled=%v saved=%v", policy.Blocked(), clam.disabled, store.value)
		}
	})

	t.Run("the next start restores a value a failed unblock kept", func(t *testing.T) {
		clam, store := &fakeClamshell{}, &fakeStore{}
		policy := NewSleepBlockPolicy(&fakeIdle{}, clam, store)
		if err := policy.Set(true); err != nil {
			t.Fatal(err)
		}
		clam.throwOn = boolp(false)
		if err := policy.Set(false); err == nil {
			t.Fatal("the failed restore was not reported")
		}
		clam.throwOn = nil
		NewSleepBlockPolicy(&fakeIdle{}, clam, store)
		if clam.disabled {
			t.Fatal("the next start did not restore")
		}
		if store.value != nil {
			t.Fatal("restore point not cleared after restoring")
		}
	})

	t.Run("a failure clearing the flag without a saved value is reported and retried", func(t *testing.T) {
		clam, store := &fakeClamshell{}, &fakeStore{}
		policy := NewSleepBlockPolicy(&fakeIdle{}, clam, store)
		if err := policy.Set(true); err != nil {
			t.Fatal(err)
		}
		store.Clear() // the restore point went missing
		clam.throwOn = boolp(false)
		if err := policy.Set(false); !errors.Is(err, errBoom) {
			t.Fatalf("err=%v, want boom", err)
		}
		if !policy.Blocked() {
			t.Fatal("reports unblocked while sleep is still disabled")
		}
		clam.throwOn = nil
		if err := policy.Set(false); err != nil {
			t.Fatal(err)
		}
		if policy.Blocked() || clam.disabled {
			t.Fatalf("after the retry: blocked=%v disabled=%v", policy.Blocked(), clam.disabled)
		}
	})

	t.Run("unblocking without a saved value clears the flag only if we blocked", func(t *testing.T) {
		clam, store := &fakeClamshell{}, &fakeStore{}
		policy := NewSleepBlockPolicy(&fakeIdle{}, clam, store)
		if err := policy.Set(false); err != nil {
			t.Fatal(err)
		}
		if len(clam.calls) != 0 {
			t.Fatalf("never blocked, nothing to undo: calls=%v", clam.calls)
		}
		if err := policy.Set(true); err != nil {
			t.Fatal(err)
		}
		store.Clear() // the restore point went missing
		if err := policy.Set(false); err != nil {
			t.Fatal(err)
		}
		if clam.disabled {
			t.Fatal("fail safe: sleep must be allowed again")
		}
	})

	// The helper serves every socket connection on its own goroutine. The fakes are not
	// synchronized, so the race detector flags any call the policy fails to serialize.
	t.Run("concurrent calls are serialized", func(t *testing.T) {
		idle, clam, store := &fakeIdle{}, &fakeClamshell{}, &fakeStore{}
		policy := NewSleepBlockPolicy(idle, clam, store)
		var wg sync.WaitGroup
		for i := range 16 {
			wg.Go(func() {
				_ = policy.Set(i%2 == 0)
				_ = policy.Blocked()
			})
		}
		wg.Wait()
		if err := policy.Set(false); err != nil {
			t.Fatal(err)
		}
		if policy.Blocked() || idle.held || clam.disabled || store.value != nil {
			t.Fatalf("after the final unblock: blocked=%v idle=%v disabled=%v saved=%v",
				policy.Blocked(), idle.held, clam.disabled, store.value)
		}
	})
}
