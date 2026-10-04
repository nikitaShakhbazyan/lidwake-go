package darwin

/*
#include "darwin.h"
*/
import "C"

import (
	"fmt"
	"log/slog"
)

func lockScreenAvailable() bool { return bool(C.lw_lock_screen_available()) }

// lockScreen uses SACLockScreenImmediate — the primitive behind "Lock Screen". It is explicit, so
// it locks even when another app holds an idle-lock-prevention assertion, unlike the implicit
// lock that rides on display sleep. That implicit lock is the fallback when the symbol is gone.
func lockScreen() error {
	if C.lw_lock_screen() == 0 {
		return nil
	}
	slog.Warn("SACLockScreenImmediate unavailable, locking via pmset displaysleepnow")
	if err := runWithWatchdog("/usr/bin/pmset", []string{"displaysleepnow"}, pmsetTimeout); err != nil {
		return fmt.Errorf("darwin: lock screen: %w", err)
	}
	return nil
}
