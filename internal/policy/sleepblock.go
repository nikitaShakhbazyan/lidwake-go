package policy

import (
	"fmt"
	"log/slog"
	"sync"
)

// IdleSleepAsserting holds and releases the idle-system-sleep assertion — the standard,
// reference-counted IOPMAssertion (PreventUserIdleSystemSleep). Stateful: it tracks whether the
// assertion is currently held, so a repeated Acquire is a no-op.
type IdleSleepAsserting interface {
	IsHeld() bool
	Acquire()
	Release()
}

// ClamshellSleepControlling applies and clears the global clamshell-sleep block — the
// SleepDisabled power setting (`pmset -a disablesleep`), which can fail.
type ClamshellSleepControlling interface {
	// IsDisabled is the flag as the kernel enforces it right now.
	IsDisabled() bool
	SetDisabled(disabled bool) error
}

// OriginalSleepSettingStoring remembers the SleepDisabled value the Mac had before the first
// block. It must outlive the process (and a reboot), because so does the flag.
type OriginalSleepSettingStoring interface {
	// Load returns the saved value; ok is false when nothing is saved.
	Load() (disabled bool, ok bool)
	Save(disabled bool) error
	Clear()
}

// SleepBlockPolicy is the compose / idempotence / crash-recovery logic for keeping the Mac
// awake, independent of the concrete IOKit and pmset mechanisms behind IdleSleepAsserting and
// ClamshellSleepControlling.
//
// Before the first block it saves the current SleepDisabled value, and every release puts that
// value back instead of forcing 0: a Mac its owner set up never to sleep stays that way. On
// construction it restores a value saved by a prior — possibly crashed — instance, since
// disablesleep persists across process death and reboot. With nothing saved it leaves the flag
// alone.
//
// Set is deliberately not short-circuited on an unchanged value: a repeated Set(true) re-asserts
// the clamshell block, which the daemon relies on to recover after a sleep/wake transition.
// Every step is idempotent. An error from any step is returned and leaves Blocked unchanged, so
// a failed unblock still reports blocked: the flag may still be disabled, and the error is what
// makes the daemon retry the unblock while the helper's dead-man switch stays armed. A failed
// restore keeps the saved value for that retry, or for the next start.
//
// SleepBlockPolicy is safe for concurrent use; calls are serialized.
type SleepBlockPolicy struct {
	mu        sync.Mutex
	blocked   bool
	idle      IdleSleepAsserting
	clamshell ClamshellSleepControlling
	original  OriginalSleepSettingStoring
}

// NewSleepBlockPolicy wires the mechanisms together and restores any value a previous instance
// saved.
func NewSleepBlockPolicy(idle IdleSleepAsserting, clamshell ClamshellSleepControlling, original OriginalSleepSettingStoring) *SleepBlockPolicy {
	p := &SleepBlockPolicy{idle: idle, clamshell: clamshell, original: original}
	if _, err := p.restoreOriginal(); err != nil {
		slog.Warn("restoring the original sleep setting failed; the next unblock retries", "err", err)
	}
	return p
}

// Blocked reports the state the last successful Set left: a failed unblock still reports
// blocked, since the flag may still be disabled.
func (p *SleepBlockPolicy) Blocked() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.blocked
}

// Set blocks or unblocks sleep.
func (p *SleepBlockPolicy) Set(blocked bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if blocked {
		// Keep the idle assertion even if the clamshell block fails: partial protection (idle
		// sleep still blocked) serves the purpose better than rolling back to none, and the
		// daemon re-issues Set(true) on its next reconcile — every step is idempotent, so the
		// clamshell block is retried while the idle assertion stays continuously held. The error
		// is returned so the daemon knows the block isn't complete yet.
		p.idle.Acquire()
		if _, ok := p.original.Load(); !ok {
			if err := p.original.Save(p.clamshell.IsDisabled()); err != nil {
				return fmt.Errorf("save the original sleep setting: %w", err)
			}
		}
		if err := p.clamshell.SetDisabled(true); err != nil {
			return fmt.Errorf("disable clamshell sleep: %w", err)
		}
	} else {
		p.idle.Release()
		restored, err := p.restoreOriginal()
		if err != nil {
			return err
		}
		if !restored && p.blocked {
			// We blocked but lost the saved value: leaving sleep disabled forever is the worse
			// failure, so fall back to clearing it.
			if err := p.clamshell.SetDisabled(false); err != nil {
				return fmt.Errorf("clear the sleep block: %w", err)
			}
		}
	}
	p.blocked = blocked
	return nil
}

// restoreOriginal puts back the saved value, if any, and reports whether there was one. The
// saved value is cleared only once it has been applied, so a failed restore can be retried.
func (p *SleepBlockPolicy) restoreOriginal() (bool, error) {
	saved, ok := p.original.Load()
	if !ok {
		return false, nil
	}
	if err := p.clamshell.SetDisabled(saved); err != nil {
		return true, fmt.Errorf("restore the original sleep setting (disabled=%v): %w", saved, err)
	}
	p.original.Clear()
	return true, nil
}
