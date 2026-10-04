// Package darwin is every macOS system call lidwake makes: power management, the SMC, process
// information, kqueue exit watching, code-signing checks on socket peers and the screen lock.
//
// CONTRACT: the exported signatures in this file are what the other packages are written
// against. The cgo implementation lives in the other files of this package (C helpers in *.c and
// thermal.m, declared in darwin.h); additions beyond this file are listed in extra.go.
package darwin

import (
	"net"
	"sync"
	"time"
)

// ---- Sleep and power -------------------------------------------------------------------------

// SleepDisabled reports the kernel's SleepDisabled flag, read from IOPMrootDomain — what the
// kernel enforces. (`pmset -g` only lists SleepDisabled once the key exists in the preferences.)
func SleepDisabled() bool { return sleepDisabled() }

// SetSleepDisabled runs `/usr/bin/pmset -a disablesleep 1|0` (needs root) and waits at most
// 10 s, killing a wedged pmset. pmset is Apple's own implementation of the whole sequence; the
// in-process IOKit routes to SleepDisabled either fail as root or don't stick. powerd applies the
// value after pmset exits, so it then waits up to 2 s for SleepDisabled to read it — a read right
// after a successful call sees the new value — and fails if it never does.
func SetSleepDisabled(disabled bool) error { return setSleepDisabled(disabled) }

// LidClosed reports AppleClamshellState from IOPMrootDomain. ok is false when the Mac has no lid
// (no such property).
func LidClosed() (closed bool, ok bool) { return lidClosed() }

// Battery is the internal battery, from IOPSCopyPowerSourcesInfo.
type Battery struct {
	Percent   int  // 0–100, current/max capacity rounded
	OnBattery bool // power source state is "Battery Power"
}

// ReadBattery returns the internal battery; ok is false on Macs without one.
func ReadBattery() (b Battery, ok bool) { return readBattery() }

// ThermalState is NSProcessInfo.processInfo.thermalState: 0 nominal, 1 fair, 2 serious,
// 3 critical. Reported on Apple Silicon too, unlike `pmset -g therm`.
func ThermalState() int { return thermalState() }

// BootTime is kern.boottime.
func BootTime() (time.Time, error) { return bootTime() }

// ---- Power assertions ------------------------------------------------------------------------

// AssertionType is an IOPMAssertion type.
type AssertionType string

const (
	PreventUserIdleSystemSleep  AssertionType = "PreventUserIdleSystemSleep"
	PreventUserIdleDisplaySleep AssertionType = "PreventUserIdleDisplaySleep"
)

// PowerAssertion is a held IOPMAssertion. The kernel releases it if the process dies.
type PowerAssertion struct {
	id uint32 // IOPMAssertionID; 0 once released (accessed atomically)
}

// CreateAssertion creates an IOPMAssertion of the given type with a human-readable name (shown in
// `pmset -g assertions`; keep it ASCII, pmset renders anything else as `?`).
func CreateAssertion(t AssertionType, name string) (*PowerAssertion, error) {
	return createAssertion(t, name)
}

// Release releases the assertion; safe to call more than once and on nil.
func (a *PowerAssertion) Release() { a.release() }

// DeclareUserActivity wakes a dark display (IOPMAssertionDeclareUserActivity) — an assertion only
// prevents future display sleep, this relights it now. A no-op while the main display is awake,
// so a periodic reconcile can call it freely without resetting the user-idle clock; the
// user-activity assertion id is reused across calls, as IOPMLib asks.
func DeclareUserActivity(name string) error { return declareUserActivity(name) }

// PIDsPreventingSystemSleep returns the PIDs that hold a system-sleep-preventing assertion
// (PreventUserIdleSystemSleep, PreventSystemSleep, NoIdleSleepAssertion) per
// IOPMCopyAssertionsByProcess — the data `pmset -g assertions` shows. Claude Code spawns
// `caffeinate -i` exactly while it is working, so this is the "still thinking" signal for an
// agent whose own process is near idle. System-wide: callers must scope it to an agent's process
// tree. Empty on failure.
func PIDsPreventingSystemSleep() map[int]bool { return pidsPreventingSystemSleep() }

// ---- SMC temperature -------------------------------------------------------------------------

// SMC is an open connection to AppleSMC. Safe for concurrent use.
type SMC struct {
	conn uint32
	keys []string // discovered CPU sensor keys, cached after the first successful read
	mu   sync.Mutex
}

// OpenSMC opens AppleSMC (public IOKit, no entitlements).
func OpenSMC() (*SMC, error) { return openSMC() }

// CPUTemperature returns the CPU temperature in °C. Apple Silicon has no single CPU proximity
// sensor: average the Tp…/Te… keys reporting a plausible `flt ` value (discovered once by walking
// the key list). Intel: average whichever of TC0P, TC0D, TC0E, TC0F, TCXC (`sp78`) read at all.
// ok is false when nothing plausible (5–120 °C) can be read, and after Close.
func (s *SMC) CPUTemperature() (celsius float64, ok bool) { return s.cpuTemperature() }

// Close closes the SMC connection; safe on nil and more than once.
func (s *SMC) Close() { s.close() }

// ---- Processes -------------------------------------------------------------------------------

// ProcessPath is the executable path of pid (proc_pidpath).
func ProcessPath(pid int) (string, error) { return processPath(pid) }

// ProcessName is the short command name of pid (kinfo_proc p_comm, at most 16 bytes), "" if
// unknown.
func ProcessName(pid int) string { return processName(pid) }

// ParentPID is the parent of pid (kinfo_proc e_ppid).
func ParentPID(pid int) (int, error) { return parentPID(pid) }

// ProcessStartTime is when pid started (kinfo_proc p_starttime). Together with the PID it pins
// one process: a recycled PID has a different start time.
func ProcessStartTime(pid int) (time.Time, error) { return processStartTime(pid) }

// ProcessArgs is pid's argv (KERN_PROCARGS2), without the environment.
func ProcessArgs(pid int) ([]string, error) { return processArgs(pid) }

// ProcessAlive reports whether pid exists (kill(pid, 0) == 0 or EPERM). False for pid <= 0,
// which kill would read as a process group.
func ProcessAlive(pid int) bool { return processAlive(pid) }

// ChildPIDs are pid's direct children, from one KERN_PROC_ALL snapshot — the source `ps` and
// `pgrep -P` use. Not proc_listchildpids, which returns truncated counts on current macOS so
// children go missing. Walking a whole tree? Take one ChildMap snapshot instead.
func ChildPIDs(pid int) ([]int, error) { return childPIDs(pid) }

// AllPIDs lists every process (KERN_PROC_ALL), except the kernel (pid 0).
func AllPIDs() ([]int, error) { return allPIDs() }

// CPUTime is pid's total user+system CPU time (proc_pidinfo PROC_PIDTASKINFO; Mach absolute
// time converted with mach_timebase_info on Apple Silicon).
func CPUTime(pid int) (time.Duration, error) { return cpuTime(pid) }

// ExitWatcher reports process exits via kqueue EVFILT_PROC/NOTE_EXIT.
type ExitWatcher struct {
	kq    int
	exits chan int
	state exitWatcherState
}

// NewExitWatcher starts the kqueue goroutine.
func NewExitWatcher() (*ExitWatcher, error) { return newExitWatcher() }

// Watch arms an exit notification for pid. Watching an already-dead pid reports it at once;
// watching a pid that is already armed is a no-op.
func (w *ExitWatcher) Watch(pid int) error { return w.watch(pid) }

// Exits delivers the PIDs that exited. Closed by Close.
func (w *ExitWatcher) Exits() <-chan int { return w.exits }

// Close stops the watcher; safe to call more than once.
func (w *ExitWatcher) Close() error { return w.close() }

// ---- Code signing of socket peers ------------------------------------------------------------

// PeerCode describes the process on the other end of a Unix socket, resolved from its audit
// token (getsockopt LOCAL_PEERTOKEN) — not from its PID, which could be recycled.
type PeerCode struct {
	PID             int
	UID             int    // effective UID
	Path            string // SecCodeCopyPath of the static code
	Identifier      string // signing identifier
	Team            string // team identifier, "" for ad-hoc
	HardenedRuntime bool   // CodeDirectory flags contain kSecCodeSignatureRuntime (0x10000)
	Valid           bool   // SecCodeCheckValidity on the dynamic code passed
}

// PeerCodeOf resolves the peer of conn. Fails closed: any error means "unknown caller", and so
// does a peer without a signing identifier (unsigned code has nothing to anchor trust in). Code
// that fails SecCodeCheckValidity is still resolved, with Valid false and a nil error, so the
// caller can log who connected — and must reject it.
func PeerCodeOf(conn *net.UnixConn) (PeerCode, error) { return peerCodeOf(conn) }

// SelfTeam is this process's own team identifier, "" when ad-hoc signed.
func SelfTeam() string { return selfTeam() }

// ---- Screen ----------------------------------------------------------------------------------

// LockScreen locks the screen now (SACLockScreenImmediate from login.framework, loaded with
// dlopen), overriding idle-lock prevention from other apps. Falls back to
// `pmset displaysleepnow` (which locks per the user's "require password" setting) when the
// symbol can't be resolved.
func LockScreen() error { return lockScreen() }
