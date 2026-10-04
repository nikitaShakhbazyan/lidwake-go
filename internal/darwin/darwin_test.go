package darwin

// Read-only smoke tests against the real Mac. They never change power settings, lock the screen
// or declare user activity; the only system state they touch is a short-lived power assertion
// owned by the test process (and a `caffeinate` child), both released before the test ends.

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"syscall"
	"testing"
	"time"
)

// requireHardware fails a hardware expectation locally but skips it on CI hosts, which are often
// VMs without a lid, battery or SMC sensors.
func requireHardware(t *testing.T, ok bool, what string) {
	t.Helper()
	if ok {
		return
	}
	if os.Getenv("CI") != "" {
		t.Skipf("%s unavailable on this CI host", what)
	}
	t.Fatalf("%s unavailable", what)
}

// spawn starts a child and kills and reaps it when the test ends.
func spawn(t *testing.T, name string, args ...string) *exec.Cmd {
	t.Helper()
	return start(t, exec.Command(name, args...))
}

// start starts cmd and kills and reaps it when the test ends.
func start(t *testing.T, cmd *exec.Cmd) *exec.Cmd {
	t.Helper()
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", cmd.Path, err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd
}

// eventually polls cond every 50 ms until it holds or the timeout passes.
func eventually(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func realPath(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatalf("resolve %s: %v", p, err)
	}
	return r
}

func TestSleepDisabled(t *testing.T) {
	// Either value is legitimate; the read must simply work and be stable.
	if a, b := SleepDisabled(), SleepDisabled(); a != b {
		t.Fatalf("SleepDisabled flipped between two reads: %v then %v", a, b)
	}
}

func TestLidClosed(t *testing.T) {
	closed, ok := LidClosed()
	requireHardware(t, ok, "lid (AppleClamshellState)")
	t.Logf("lid closed: %v", closed)
}

func TestReadBattery(t *testing.T) {
	b, ok := ReadBattery()
	requireHardware(t, ok, "internal battery")
	t.Logf("battery: %d%%, on battery: %v", b.Percent, b.OnBattery)
	if b.Percent < 0 || b.Percent > 100 {
		t.Fatalf("battery percent %d out of range", b.Percent)
	}
}

func TestCapabilities(t *testing.T) {
	c := Capabilities()
	requireHardware(t, c.HasLid && c.HasBattery, "laptop hardware")
	if c.IsDesktop() {
		t.Fatal("a MacBook was labeled a desktop")
	}
	if Capabilities() != c {
		t.Fatal("capabilities changed between probes")
	}
}

func TestRootBoolPropertyAbsent(t *testing.T) {
	// The desktop path of LidClosed and Capabilities: a property IOPMrootDomain does not publish
	// reads as absent, not as false.
	if v, present := rootBoolProperty("LidwakeNoSuchProperty"); present || v {
		t.Fatalf("missing property = %v, present %v; want absent", v, present)
	}
}

func TestThermalState(t *testing.T) {
	if s := ThermalState(); s < 0 || s > 3 {
		t.Fatalf("thermal state %d out of 0...3", s)
	}
}

func TestBootTime(t *testing.T) {
	t.Run("system boot time is available and in the past", func(t *testing.T) {
		boot, err := BootTime()
		if err != nil {
			t.Fatal(err)
		}
		if !boot.Before(time.Now()) {
			t.Fatalf("boot time %v is not in the past", boot)
		}
		// A mis-decoded timeval (seconds read as microseconds, or a zeroed struct) lands near the
		// epoch, which would make every restored assertion look like it predates this boot.
		if boot.Year() < 2020 {
			t.Fatalf("boot time %v is implausibly old", boot)
		}
	})
	t.Run("boot time is stable between reads", func(t *testing.T) {
		a, errA := BootTime()
		b, errB := BootTime()
		if errA != nil || errB != nil || !a.Equal(b) {
			t.Fatalf("BootTime changed between reads: %v (%v) then %v (%v)", a, errA, b, errB)
		}
	})
}

func TestOnlyDisplayIsClosedBuiltIn(t *testing.T) {
	if OnlyDisplayIsClosedBuiltIn(false) {
		t.Fatal("an open lid can never leave only a closed built-in panel")
	}
	_ = OnlyDisplayIsClosedBuiltIn(true) // reads the display list; any answer is valid here
}

func TestSMCCPUTemperature(t *testing.T) {
	s, err := OpenSMC()
	if err != nil {
		requireHardware(t, false, "AppleSMC: "+err.Error())
	}
	defer s.Close()
	c, ok := s.CPUTemperature()
	requireHardware(t, ok, "SMC CPU temperature")
	t.Logf("CPU temperature %.1f °C from %d sensors", c, len(s.keys))
	if c <= 5 || c >= 120 {
		t.Fatalf("CPU temperature %.1f °C is implausible", c)
	}
	if len(s.keys) == 0 {
		t.Fatal("a successful read did not cache the sensor keys")
	}
	if again, ok := s.CPUTemperature(); !ok || again <= 5 || again >= 120 {
		t.Fatalf("second (cached) read = %.1f, %v", again, ok)
	}
	s.Close()
	s.Close()
	if _, ok := s.CPUTemperature(); ok {
		t.Fatal("read a temperature after Close")
	}
	var nilSMC *SMC
	nilSMC.Close()
	if _, ok := nilSMC.CPUTemperature(); ok {
		t.Fatal("nil SMC reported a temperature")
	}
}

func TestProcessesSelf(t *testing.T) {
	self := os.Getpid()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	t.Run("name of current process is non nil", func(t *testing.T) {
		if got := ProcessName(self); got == "" {
			t.Fatal("ProcessName(self) is empty")
		}
	})
	t.Run("name of current process is its p_comm", func(t *testing.T) {
		wantName := filepath.Base(exe)
		if len(wantName) > maxComLen {
			wantName = wantName[:maxComLen]
		}
		if got := ProcessName(self); got != wantName {
			t.Errorf("ProcessName(self) = %q, want %q", got, wantName)
		}
	})
	t.Run("parent of current process is positive", func(t *testing.T) {
		ppid, err := ParentPID(self)
		if err != nil {
			t.Fatal(err)
		}
		if ppid <= 0 {
			t.Fatalf("ParentPID(self) = %d, want > 0", ppid)
		}
		if ppid != os.Getppid() {
			t.Errorf("ParentPID(self) = %d, want %d", ppid, os.Getppid())
		}
	})
	t.Run("arguments of self includes executable arg", func(t *testing.T) {
		// KERN_PROCARGS2 of this process must parse to exactly its argv: argv[0] first (so the exec
		// path and its alignment padding were skipped), every flag the test runner passed, and
		// nothing from the environment that follows.
		args, err := ProcessArgs(self)
		if err != nil {
			t.Fatal(err)
		}
		if len(args) == 0 || args[0] == "" {
			t.Fatalf("ProcessArgs(self) = %q, want a non-empty argv[0]", args)
		}
		if !slices.Equal(args, os.Args) {
			t.Errorf("ProcessArgs(self) = %q, want %q", args, os.Args)
		}
	})
	t.Run("running processes is non empty and includes self", func(t *testing.T) {
		all, err := AllPIDs()
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(all, self) || !slices.Contains(all, 1) {
			t.Errorf("AllPIDs (%d entries) lacks self or launchd", len(all))
		}
		if slices.Contains(all, 0) {
			t.Error("AllPIDs includes the kernel")
		}
	})
	t.Run("cpu time of self is non negative", func(t *testing.T) {
		if d, err := CPUTime(self); err != nil || d < 0 {
			t.Fatalf("CPUTime(self) = %v, %v", d, err)
		}
	})
	t.Run("path of current process is its executable", func(t *testing.T) {
		path, err := ProcessPath(self)
		if err != nil {
			t.Fatal(err)
		}
		if realPath(t, path) != realPath(t, exe) {
			t.Errorf("ProcessPath(self) = %q, want %q", path, exe)
		}
	})
	t.Run("start time of current process is after boot and recent", func(t *testing.T) {
		start, err := ProcessStartTime(self)
		if err != nil {
			t.Fatal(err)
		}
		if boot, err := BootTime(); err == nil && start.Before(boot) {
			t.Errorf("start time %v is before boot %v", start, boot)
		}
		if age := time.Since(start); age < 0 || age > time.Hour {
			t.Errorf("this test process claims to be %v old", age)
		}
	})
	t.Run("current process and launchd are alive", func(t *testing.T) {
		if !ProcessAlive(self) || !ProcessAlive(1) {
			t.Error("self or launchd reported dead")
		}
	})
	t.Run("non-positive pids are never alive", func(t *testing.T) {
		// kill(0, 0) and kill(-1, 0) address process groups and would succeed.
		for _, pid := range []int{0, -1} {
			if ProcessAlive(pid) {
				t.Errorf("ProcessAlive(%d) = true", pid)
			}
		}
	})
}

// maxComLen is MAXCOMLEN: the kernel keeps at most this many bytes of a command name in p_comm.
const maxComLen = 16

// helperEnv makes this test binary, run as a child, sleep in TestHelperSleep instead of testing.
const helperEnv = "LIDWAKE_DARWIN_TEST_HELPER"

// TestHelperSleep is not a test: it is the body of a child process the tests spawn from a copy of
// this binary, a long-running process whose executable name they choose.
func TestHelperSleep(t *testing.T) {
	if os.Getenv(helperEnv) != "sleep" {
		return
	}
	time.Sleep(5 * time.Second)
	os.Exit(0)
}

func TestProcessNameIsTruncatedPComm(t *testing.T) {
	// ProcessName is p_comm, which the kernel cuts to MAXCOMLEN bytes; agent matching must use the
	// executable path instead. Run this test binary under a long name to see both. (A copy of
	// /bin/sleep would be killed for leaving the system volume, and exec through a symlink names
	// p_comm after the target.)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	long := filepath.Join(t.TempDir(), "lidwake-long-command-name")
	if err := os.WriteFile(long, src, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(long, "-test.run=^TestHelperSleep$")
	cmd.Env = append(os.Environ(), helperEnv+"=sleep")
	child := start(t, cmd).Process.Pid

	var name string
	eventually(2*time.Second, func() bool {
		name = ProcessName(child)
		return name == "lidwake-long-com"
	})
	if name != "lidwake-long-com" {
		t.Errorf("ProcessName(child) = %q, want the first %d bytes of the command name", name, maxComLen)
	}
	if path, err := ProcessPath(child); err != nil || realPath(t, path) != realPath(t, long) {
		t.Errorf("ProcessPath(child) = %q, %v; want the full path %q", path, err, long)
	}
}

func TestProcessesInvalidPID(t *testing.T) {
	// -1 is the usual "no pid" sentinel; 1<<40 would truncate into some real pid_t; 2147483600 fits a
	// pid_t but no process has it, so it reaches the kernel and must come back as "no such process".
	invalid := []int{-1, 1 << 40, 2147483600}

	t.Run("name of invalid pid is nil", func(t *testing.T) {
		for _, pid := range invalid {
			if got := ProcessName(pid); got != "" {
				t.Errorf("ProcessName(%d) = %q, want empty", pid, got)
			}
		}
	})
	t.Run("arguments of invalid pid is nil", func(t *testing.T) {
		for _, pid := range invalid {
			if args, err := ProcessArgs(pid); err == nil || args != nil {
				t.Errorf("ProcessArgs(%d) = %q, %v; want an error", pid, args, err)
			}
		}
	})
	t.Run("cpu time of invalid pid is nil", func(t *testing.T) {
		for _, pid := range invalid {
			if d, err := CPUTime(pid); err == nil {
				t.Errorf("CPUTime(%d) = %v, want an error", pid, d)
			}
		}
	})
	t.Run("parent path and start time of invalid pid fail", func(t *testing.T) {
		for _, pid := range invalid {
			if _, err := ParentPID(pid); err == nil {
				t.Errorf("ParentPID(%d) succeeded", pid)
			}
			if _, err := ProcessPath(pid); err == nil {
				t.Errorf("ProcessPath(%d) succeeded", pid)
			}
			if _, err := ProcessStartTime(pid); err == nil {
				t.Errorf("ProcessStartTime(%d) succeeded", pid)
			}
			if ProcessAlive(pid) {
				t.Errorf("ProcessAlive(%d) = true", pid)
			}
		}
	})
	t.Run("a pid no process has reads as no such process", func(t *testing.T) {
		// sysctl KERN_PROC_PID reports a missing pid as an empty result, not an error; the C side
		// turns that into ESRCH so callers can tell "gone" from a failed read.
		if _, err := ParentPID(2147483600); !errors.Is(err, syscall.ESRCH) {
			t.Errorf("ParentPID(missing) error = %v, want ESRCH", err)
		}
	})
}

func TestProcessArgsRealArgv(t *testing.T) {
	// Empty and space-containing arguments must survive KERN_PROCARGS2 intact: an empty argument
	// is two adjacent NULs and must not be mistaken for alignment padding. `; :` keeps sh from
	// exec-ing sleep in its place, which would replace the argv under test.
	want := []string{"/bin/sh", "-c", "sleep 5; :", "lidwake-arg0", "", "a b", "--"}
	child := spawn(t, want[0], want[1:]...).Process.Pid
	var args []string
	eventually(2*time.Second, func() bool {
		args, _ = ProcessArgs(child)
		return slices.Equal(args, want)
	})
	if !slices.Equal(args, want) {
		t.Errorf("ProcessArgs(child) = %q, want %q", args, want)
	}
}

func TestProcessesChild(t *testing.T) {
	self := os.Getpid()
	before := time.Now()
	cmd := spawn(t, "/bin/sleep", "5")
	child := cmd.Process.Pid

	if ppid, err := ParentPID(child); err != nil || ppid != self {
		t.Errorf("ParentPID(child) = %d, %v; want %d", ppid, err, self)
	}
	if path, err := ProcessPath(child); err != nil || path != "/bin/sleep" {
		t.Errorf("ProcessPath(child) = %q, %v", path, err)
	}
	if name := ProcessName(child); name != "sleep" {
		t.Errorf("ProcessName(child) = %q", name)
	}
	if start, err := ProcessStartTime(child); err != nil ||
		start.Before(before.Add(-time.Second)) || start.After(time.Now().Add(time.Second)) {
		t.Errorf("ProcessStartTime(child) = %v, %v; spawned at %v", start, err, before)
	}
	// The kernel fills KERN_PROCARGS2 at exec, so poll briefly in case it is read mid-exec.
	var args []string
	eventually(2*time.Second, func() bool {
		args, _ = ProcessArgs(child)
		return slices.Equal(args, []string{"/bin/sleep", "5"})
	})
	if !slices.Equal(args, []string{"/bin/sleep", "5"}) {
		t.Errorf("ProcessArgs(child) = %q", args)
	}
	children, err := ChildPIDs(self)
	if err != nil || !slices.Contains(children, child) {
		t.Errorf("ChildPIDs(self) = %v, %v; want it to contain %d", children, err, child)
	}
	m, err := ChildMap()
	if err != nil || !slices.Contains(m[self], child) {
		t.Errorf("ChildMap()[self] = %v, %v; want it to contain %d", m[self], err, child)
	}
	all, err := AllPIDs()
	if err != nil || !slices.Contains(all, child) {
		t.Errorf("AllPIDs lacks the child %d (err %v)", child, err)
	}
	if !ProcessAlive(child) {
		t.Error("live child reported dead")
	}
	if _, err := CPUTime(child); err != nil {
		t.Errorf("CPUTime(child): %v", err)
	}

	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if ProcessAlive(child) {
		t.Error("reaped child reported alive")
	}
	if _, err := ParentPID(child); err == nil {
		t.Error("ParentPID of a reaped child succeeded")
	}
	if _, err := ProcessPath(child); err == nil {
		t.Error("ProcessPath of a reaped child succeeded")
	}
	if ProcessName(child) != "" {
		t.Error("ProcessName of a reaped child is not empty")
	}
}

// rusageCPU is this process's user+system time per getrusage, an independent clock for CPUTime;
// 0 if the call fails.
func rusageCPU() time.Duration {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

func TestCPUTime(t *testing.T) {
	self := os.Getpid()

	t.Run("cpu time tracks wall clock under load", func(t *testing.T) {
		// Regression guard for the mach timebase conversion: proc_taskinfo reports Mach ticks, not
		// nanoseconds, so reading them unscaled under-reports CPU ~41.7x on Apple Silicon (a pinned
		// core reads ~2.4%). Busy-spin ~300 ms on one thread and require CPU time to grow by at
		// least 150 ms: impossible under that bug (~7 ms), comfortably true with it fixed.
		const (
			spinFor  = 300 * time.Millisecond
			spinCap  = 5 * time.Second
			minDelta = 150 * time.Millisecond
		)
		before, err := CPUTime(self)
		if err != nil {
			t.Fatal(err)
		}
		ruBefore := rusageCPU()
		spun := make(chan uint64)
		go func() {
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			var spin uint64
			start := time.Now()
			for {
				spin++
				elapsed := time.Since(start)
				if elapsed < spinFor {
					continue
				}
				// A loaded host may give this thread only part of a core: keep spinning until the
				// process has really burned spinFor of CPU by getrusage. Within spinCap, the
				// unscaled-ticks bug still stays under minDelta (5 s / 41.7 = 120 ms).
				if elapsed >= spinCap || rusageCPU()-ruBefore >= spinFor {
					break
				}
			}
			spun <- spin
		}()
		if <-spun == 0 {
			t.Fatal("the spin loop never ran")
		}
		after, err := CPUTime(self)
		if err != nil {
			t.Fatal(err)
		}
		if delta := after - before; delta < minDelta {
			t.Fatalf("CPUTime advanced only %v over ~%v of busy CPU (getrusage: %v)",
				delta, spinFor, rusageCPU()-ruBefore)
		}
	})
	t.Run("cpu time agrees with getrusage", func(t *testing.T) {
		before, err := CPUTime(self)
		if err != nil {
			t.Fatal(err)
		}
		x := 0
		for spin := time.Now(); time.Since(spin) < 150*time.Millisecond; {
			for i := range 10000 {
				x ^= i * i
			}
		}
		_ = x
		after, err := CPUTime(self)
		if err != nil {
			t.Fatal(err)
		}
		if after <= 0 || after <= before {
			t.Fatalf("CPU time did not grow with busy work: %v -> %v", before, after)
		}
		// Cross-check the unit with getrusage: reading Mach ticks as nanoseconds would
		// under-report by ~41.7x on Apple Silicon.
		rusage := rusageCPU()
		if rusage == 0 {
			t.Fatal("getrusage failed")
		}
		if after < rusage/2 || after > rusage*2+100*time.Millisecond {
			t.Fatalf("CPUTime %v disagrees with getrusage %v", after, rusage)
		}
	})
}

func TestExitWatcher(t *testing.T) {
	waitExit := func(t *testing.T, w *ExitWatcher, pid int) {
		t.Helper()
		timeout := time.After(5 * time.Second)
		for {
			select {
			case got, ok := <-w.Exits():
				if !ok {
					t.Fatal("exits closed early")
				}
				if got == pid {
					return
				}
			case <-timeout:
				t.Fatalf("no exit reported for %d", pid)
			}
		}
	}

	t.Run("reports a spawned process exiting", func(t *testing.T) {
		w, err := NewExitWatcher()
		if err != nil {
			t.Fatal(err)
		}
		defer w.Close()
		cmd := exec.Command("/bin/sleep", "0.2")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		pid := cmd.Process.Pid
		if err := w.Watch(pid); err != nil {
			t.Fatal(err)
		}
		if err := w.Watch(pid); err != nil {
			t.Fatalf("re-watching an armed pid: %v", err)
		}
		go cmd.Wait()
		waitExit(t, w, pid)
		select {
		case got := <-w.Exits():
			if got == pid {
				t.Fatal("a pid watched twice was reported twice")
			}
		case <-time.After(300 * time.Millisecond):
		}
	})
	t.Run("an already dead pid is reported at once", func(t *testing.T) {
		w, err := NewExitWatcher()
		if err != nil {
			t.Fatal(err)
		}
		defer w.Close()
		cmd := exec.Command("/usr/bin/true")
		if err := cmd.Run(); err != nil {
			t.Fatal(err)
		}
		if err := w.Watch(cmd.Process.Pid); err != nil {
			t.Fatal(err)
		}
		waitExit(t, w, cmd.Process.Pid)
	})
	t.Run("invalid pids are rejected", func(t *testing.T) {
		w, err := NewExitWatcher()
		if err != nil {
			t.Fatal(err)
		}
		defer w.Close()
		for _, pid := range []int{0, -1, 1 << 40} {
			if err := w.Watch(pid); err == nil {
				t.Errorf("Watch(%d) succeeded", pid)
			}
		}
	})
	t.Run("close ends exits and refuses new watches", func(t *testing.T) {
		w, err := NewExitWatcher()
		if err != nil {
			t.Fatal(err)
		}
		cmd := spawn(t, "/bin/sleep", "5")
		if err := w.Watch(cmd.Process.Pid); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- w.Close() }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Close did not return")
		}
		if _, ok := <-w.Exits(); ok {
			t.Fatal("exits still open after Close")
		}
		if err := w.Watch(os.Getpid()); err == nil {
			t.Fatal("Watch after Close succeeded")
		}
		if err := w.Close(); err != nil {
			t.Fatalf("second Close: %v", err)
		}
	})
	t.Run("close with an undelivered exit does not hang", func(t *testing.T) {
		w, err := NewExitWatcher()
		if err != nil {
			t.Fatal(err)
		}
		// Fill the buffer and leave one more exit pending, nobody reading.
		for range cap(w.exits) + 1 {
			cmd := exec.Command("/usr/bin/true")
			if err := cmd.Run(); err != nil {
				t.Fatal(err)
			}
			_ = w.Watch(cmd.Process.Pid)
		}
		done := make(chan error, 1)
		go func() { done <- w.Close() }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("Close hung on an undelivered exit")
		}
	})
}

func TestPIDsPreventingSystemSleep(t *testing.T) {
	t.Run("sees caffeinate -i", func(t *testing.T) {
		cmd := spawn(t, "/usr/bin/caffeinate", "-i", "-t", "5")
		pid := cmd.Process.Pid
		if !eventually(3*time.Second, func() bool { return PIDsPreventingSystemSleep()[pid] }) {
			t.Fatalf("caffeinate -i (pid %d) not among %v", pid, PIDsPreventingSystemSleep())
		}
	})
	t.Run("own system assertion appears and goes away on release", func(t *testing.T) {
		self := os.Getpid()
		a, err := CreateAssertion(PreventUserIdleSystemSleep, "lidwake test assertion")
		if err != nil {
			t.Fatal(err)
		}
		defer a.Release()
		if !eventually(3*time.Second, func() bool { return PIDsPreventingSystemSleep()[self] }) {
			t.Fatal("own PreventUserIdleSystemSleep assertion not reported")
		}
		a.Release()
		a.Release()
		if !eventually(3*time.Second, func() bool { return !PIDsPreventingSystemSleep()[self] }) {
			t.Fatal("released assertion still reported")
		}
		var nilAssertion *PowerAssertion
		nilAssertion.Release()
	})
	t.Run("display-only assertion does not count", func(t *testing.T) {
		self := os.Getpid()
		a, err := CreateAssertion(PreventUserIdleDisplaySleep, "lidwake test display assertion")
		if err != nil {
			t.Fatal(err)
		}
		defer a.Release()
		time.Sleep(200 * time.Millisecond)
		if PIDsPreventingSystemSleep()[self] {
			t.Fatal("a display-only assertion was counted as preventing system sleep")
		}
	})
}

// unixPair is a connected in-process Unix socket pair.
func unixPair(t *testing.T) (*net.UnixConn, *net.UnixConn) {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	conns := make([]*net.UnixConn, 2)
	for i, fd := range fds {
		f := os.NewFile(uintptr(fd), "socketpair")
		c, err := net.FileConn(f)
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		conns[i] = c.(*net.UnixConn)
		t.Cleanup(func() { c.Close() })
	}
	return conns[0], conns[1]
}

func TestPeerCodeOf(t *testing.T) {
	a, _ := unixPair(t)
	pc, err := PeerCodeOf(a)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("peer: %+v", pc)
	if pc.PID != os.Getpid() {
		t.Errorf("peer PID = %d, want %d", pc.PID, os.Getpid())
	}
	if pc.UID != os.Geteuid() {
		t.Errorf("peer UID = %d, want %d", pc.UID, os.Geteuid())
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if pc.Path == "" || realPath(t, pc.Path) != realPath(t, exe) {
		t.Errorf("peer path = %q, want %q", pc.Path, exe)
	}
	if !pc.Valid {
		t.Error("this test binary's code is not valid")
	}
	if pc.Identifier == "" {
		t.Error("no signing identifier")
	}
	if pc.Team != "" {
		t.Errorf("ad-hoc test binary has team %q", pc.Team)
	}
	if pc.HardenedRuntime {
		t.Error("the test binary is not signed with the hardened runtime")
	}

	if _, err := PeerCodeOf(nil); err == nil {
		t.Error("PeerCodeOf(nil) succeeded")
	}
}

func TestPeerCodeOfUnconnected(t *testing.T) {
	// A socket with no peer has no audit token to read: fail closed.
	fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	f := os.NewFile(uintptr(fd), "unconnected")
	c, err := net.FileConn(f)
	f.Close()
	if err != nil {
		t.Skipf("can't wrap an unconnected socket: %v", err)
	}
	defer c.Close()
	uc, ok := c.(*net.UnixConn)
	if !ok {
		t.Skipf("unconnected socket wrapped as %T", c)
	}
	if pc, err := PeerCodeOf(uc); err == nil {
		t.Fatalf("resolved a peer for an unconnected socket: %+v", pc)
	}
}

func TestSelfTeam(t *testing.T) {
	if team := SelfTeam(); team != "" {
		t.Fatalf("ad-hoc test binary reports team %q", team)
	}
}

func TestLockScreenSymbol(t *testing.T) {
	// Resolves SACLockScreenImmediate without calling it.
	if !lockScreenAvailable() {
		t.Fatal("SACLockScreenImmediate could not be resolved; LockScreen would fall back to pmset")
	}
}

func TestOutputMutedDoesNotCrash(t *testing.T) {
	_ = OutputMuted() // either value is fine; the call must succeed on a real Mac
}
