package darwin

/*
#cgo CFLAGS: -Wall
#cgo LDFLAGS: -framework IOKit -framework CoreFoundation -framework Foundation -framework Security -framework CoreGraphics -lbsm
#include <stdlib.h>
#include "darwin.h"
*/
import "C"

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// rootBoolProperty reads a boolean IOPMrootDomain property: present is false when the property
// (or the root domain) does not exist.
func rootBoolProperty(key string) (value, present bool) {
	ck := C.CString(key)
	defer C.free(unsafe.Pointer(ck))
	var v C.bool
	if C.lw_root_bool_property(ck, &v) != 1 {
		return false, false
	}
	return bool(v), true
}

func sleepDisabled() bool {
	v, _ := rootBoolProperty("SleepDisabled")
	return v
}

func lidClosed() (bool, bool) {
	return rootBoolProperty("AppleClamshellState")
}

func readBattery() (Battery, bool) {
	var percent C.int
	var onBattery C.bool
	if C.lw_battery(&percent, &onBattery) != 1 {
		return Battery{}, false
	}
	return Battery{Percent: int(percent), OnBattery: bool(onBattery)}, true
}

func hasInternalBattery() bool { return bool(C.lw_has_internal_battery()) }

func thermalState() int { return int(C.lw_thermal_state()) }

func bootTime() (time.Time, error) {
	var sec C.int64_t
	var usec C.int32_t
	if rc := C.lw_boot_time(&sec, &usec); rc != 0 {
		return time.Time{}, fmt.Errorf("darwin: sysctl kern.boottime: %w", syscall.Errno(rc))
	}
	return time.Unix(int64(sec), int64(usec)*int64(time.Microsecond)), nil
}

// pmsetTimeout bounds how long pmset may run before it counts as wedged. It normally exits in
// well under a second; the helper calls it while holding its policy lock, so an unbounded wait
// would stall every later request — and the daemon's whole blocking loop behind them.
const pmsetTimeout = 10 * time.Second

// pmset only writes the preference and posts a change notification; powerd applies it to
// IOPMrootDomain later, on its own queue, so pmset can exit while SleepDisabled still reads the
// old value. Returning then would let the next block save that stale value as the setting from
// before the block, and a release would restore it — leaving sleep disabled for good. So
// setSleepDisabled waits, at most sleepSettleTimeout, for the kernel flag to take the new value.
const (
	sleepSettleTimeout  = 2 * time.Second
	sleepSettleInterval = 10 * time.Millisecond
)

func setSleepDisabled(disabled bool) error {
	value := "0"
	if disabled {
		value = "1"
	}
	if err := runWithWatchdog("/usr/bin/pmset", []string{"-a", "disablesleep", value}, pmsetTimeout); err != nil {
		return err
	}
	return awaitSleepDisabled(disabled, sleepDisabled, sleepSettleTimeout, sleepSettleInterval)
}

// awaitSleepDisabled polls read every interval until it reports want, failing after timeout.
func awaitSleepDisabled(want bool, read func() bool, timeout, interval time.Duration) error {
	deadline := time.Now().Add(timeout)
	for read() != want {
		if !time.Now().Before(deadline) {
			return fmt.Errorf("darwin: SleepDisabled still reads %v %s after pmset set it to %v", !want, timeout, want)
		}
		time.Sleep(interval)
	}
	return nil
}

// runWithWatchdog runs a short-lived tool and waits at most timeout for it: SIGTERM at the
// deadline, SIGKILL 2 s later. stderr is drained while the tool runs (a full pipe would wedge it)
// and becomes the error message on a non-zero exit.
func runWithWatchdog(path string, args []string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 2 * time.Second
	label := strings.Join(append([]string{path}, args...), " ")

	err := cmd.Run()
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return fmt.Errorf("darwin: %s did not exit within %s and was killed", label, timeout)
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = fmt.Sprintf("exited %d", exitErr.ExitCode())
		}
		return fmt.Errorf("darwin: %s: %s", label, msg)
	}
	return fmt.Errorf("darwin: run %s: %w", label, err)
}

func displayAsleep() bool { return bool(C.lw_display_asleep()) }

// externalDisplayCount is how many online displays are not the built-in panel.
func externalDisplayCount() (int, bool) {
	var total, external C.uint32_t
	if C.lw_display_counts(&total, &external) != 0 {
		return 0, false
	}
	return int(external), true
}
