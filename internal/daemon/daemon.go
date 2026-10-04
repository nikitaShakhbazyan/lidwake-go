// Package daemon is the per-user policy daemon (`lidwake daemon`, a LaunchAgent).
//
// It owns the assertion registry — "which agents are working right now" — and turns it into the
// machine state: while anything is held it has the root helper block sleep (lid-close sleep
// included), and when the last hold goes it plays the pre-sleep cue and lets the Mac sleep. Around
// that it runs every safety net: the thermal, low-battery and AC-only cutouts with their latch, the
// idle and dead-process releases, the waiting-session policy, the off timer and the pause switch.
// Agents reach it through the per-user CLI socket.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/nikitaShakhbazyan/lidwake-go/internal/agents"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/chime"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/darwin"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/ipc"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/model"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/monitor"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/paths"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/policy"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/registry"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/settings"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/store"
)

// ErrExecutableReplaced is returned by Serve when it stopped, idle, to adopt a binary an update put
// on disk. launchd (KeepAlive) then relaunches the daemon from the new image.
var ErrExecutableReplaced = errors.New("daemon: executable replaced on disk")

// errAlreadyRunning means another daemon answers on the CLI socket.
var errAlreadyRunning = errors.New("another lidwake daemon is already running")

// Hard ceilings on the registry's size. Agent hooks produce a handful of live assertions; hundreds
// means a runaway or hostile local caller, and every new key rewrites state.json in full.
const (
	maxAssertions       = 128
	maxAssertionsPerPID = 32
)

// Defaults for the Config intervals and timeouts.
const (
	// DefaultSweepInterval is the opt-in sniff sweep's period.
	DefaultSweepInterval = 30 * time.Second
	// DefaultReconcileInterval is how often the block is re-pushed to the helper while blocking.
	DefaultReconcileInterval = 60 * time.Second
	// DefaultHelperProbeInterval is how often, while blocking, the daemon checks that the helper
	// still holds the block.
	DefaultHelperProbeInterval = 5 * time.Second
	// DefaultLatchRecheckInterval is how often a held thermal latch re-reads the temperature.
	DefaultLatchRecheckInterval = 60 * time.Second
	// DefaultShutdownTimeout bounds the final unblock at shutdown.
	DefaultShutdownTimeout = 2 * time.Second
	// DefaultHelperWait bounds how long releaseAll and pause wait for the helper's unblock before
	// answering; the CLI gives up after 10 s.
	DefaultHelperWait = 8 * time.Second
	// DefaultConnTimeout bounds reading a request from, and writing a reply to, a CLI connection.
	DefaultConnTimeout = 5 * time.Second
)

// Chimes plays the daemon's sounds. *chime.Player implements it.
type Chimes interface {
	// PlayLidCloseChime plays the lid-close chime without waiting.
	PlayLidCloseChime(volume float64, chimeName string)
	// PlaySleepCue plays the pre-sleep cue and returns when it has finished, timed out or ctx ended.
	PlaySleepCue(ctx context.Context, soundName string, cue chime.Cue, volume float64)
}

// Config holds every dependency of the daemon. The zero value of a field means its production
// default; tests replace whatever touches the machine.
type Config struct {
	// SupportDir holds state.json and events.log. "": paths.SupportDir().
	SupportDir string
	// SettingsPath is config.json. "": paths.ConfigFile().
	SettingsPath string
	// Socket is the CLI socket. "": paths.CLISocket().
	Socket string
	// Helper is the root helper connection. Nil: ipc.NewHelperClient(HelperSocket).
	Helper HelperClient
	// HelperSocket is used only when Helper is nil. "": paths.HelperSocket.
	HelperSocket string
	// Chimes plays the lid-close chime and the pre-sleep cue. Nil: a chime.Player whose mute
	// check is darwin.OutputMuted.
	Chimes Chimes

	// The monitors. Nil: the monitor package's NewX. The daemon sets their callbacks.
	Lid       *monitor.LidMonitor
	Wake      *monitor.WakeDetector
	Battery   *monitor.BatteryMonitor
	Thermal   *monitor.ThermalMonitor
	Idle      *monitor.IdleMonitor
	Session   *monitor.SessionStatusMonitor
	Processes *monitor.ProcessWatcher
	Display   *monitor.DisplayHold

	// Procs lists running processes for the sniff sweep. Nil: agents.System().
	Procs *agents.Resolver

	// Now is the wall clock. Nil: time.Now.
	Now func() time.Time
	// AfterFunc schedules the off timer. Nil: time.AfterFunc.
	AfterFunc func(d time.Duration, f func()) (stop func() bool)

	// Machine probes. Nil: the package darwin function of the same name.
	BootTime      func() (time.Time, error)
	ProcessPath   func(pid int) (string, error)
	ProcessAlive  func(pid int) bool
	SleepDisabled func() bool
	ThermalState  func() int
	Capabilities  func() darwin.DeviceCapabilities

	// LockScreen locks the screen. It is called from daemon goroutines and must not block: macOS
	// wants the lock on the main thread, so Run posts it to the main goroutine. Nil: no lock.
	LockScreen func()

	// Executable is the binary watched for replacement. Nil: never replaced.
	Executable *ExecutableStaleness

	// Intervals and timeouts. Zero: the Default constants.
	SweepInterval        time.Duration
	ReconcileInterval    time.Duration
	HelperProbeInterval  time.Duration
	LatchRecheckInterval time.Duration
	ShutdownTimeout      time.Duration
	HelperWait           time.Duration
	ConnTimeout          time.Duration
	// HelperRetryBase and HelperRetryMax shape the backoff of a failed helper apply. Zero: 1 s
	// and 30 s.
	HelperRetryBase time.Duration
	HelperRetryMax  time.Duration

	// Log defaults to slog.Default().
	Log *slog.Logger
}

// Daemon is the policy engine. Build it with New and run it with Serve.
//
// Monitor callbacks, timers, CLI connections and the registry's edge consumers all run on their
// own goroutines; mu serializes every state change and every save, so state.json is always written
// in mutation order. Nothing that can block for long — a helper round-trip, the pre-sleep cue, an
// SMC read, a monitor's Stop — runs under mu.
type Daemon struct {
	cfg      Config
	log      *slog.Logger
	registry *registry.Registry
	state    *store.StateStore
	events   *store.EventLog
	helper   HelperClient
	driver   *helperDriver
	chimes   Chimes
	procs    *agents.Resolver

	lid       *monitor.LidMonitor
	wake      *monitor.WakeDetector
	battery   *monitor.BatteryMonitor
	thermal   *monitor.ThermalMonitor
	idle      *monitor.IdleMonitor
	session   *monitor.SessionStatusMonitor
	processes *monitor.ProcessWatcher
	display   *monitor.DisplayHold

	socket string
	// exit ends Serve with a cause (ErrExecutableReplaced); set once Serve runs.
	exit context.CancelCauseFunc
	// displayMu serializes syncDisplay.
	displayMu sync.Mutex

	mu       sync.Mutex
	settings settings.Settings
	// paused is the user's master switch: everything is released and acquires are refused until
	// resumed. Persisted, so a restart or reboot never silently turns lidwake back on.
	paused bool
	// offAt is when the off timer pauses lidwake; persisted with the state.
	offAt   *time.Time
	offStop func() bool
	offGen  uint64
	// latch holds fired cutouts, so the agent that was just cut off cannot immediately re-pin a hot
	// or draining Mac. While latched, acquires are refused.
	latch policy.CutoutLatch
	// blocking mirrors the registry's blocking state as the edge consumer last saw it; it gates
	// the periodic work that only matters while the Mac is kept awake.
	blocking bool
	// lastReleaseCause is why the registry last emptied, read on the way to the unblock to pick
	// the pre-sleep cue. Every release site that removes something stamps it under mu, so the last
	// writer before the edge — the release that took the registry to zero — wins.
	lastReleaseCause policy.ReleaseCause
	// sniffSuppressed are PIDs whose sniffed assertion was released (by the user or the idle sweep)
	// and must not be re-acquired by the sniff sweep while the process lives.
	sniffSuppressed map[int]bool
	// idleEvaluated is, by key, the snapshot the idle sweep last evaluated: what its releases were
	// decided on. Sweeps run one at a time, so each release batch pairs with the latest snapshot.
	idleEvaluated map[string]model.Assertion
	away          awayTracking
	awaySummary   *model.AwaySummary
	closing       bool

	sweepTicker     gatedTicker
	reconcileTicker gatedTicker
	probeTicker     gatedTicker
	latchTicker     gatedTicker
}

// awayTracking is the "while the lid was closed" bookkeeping, active from a lid close over held
// assertions to the next lid open.
type awayTracking struct {
	active     bool
	closedAt   time.Time
	held       []policy.HeldAgent
	releasedAt map[string]time.Time
	peak       *float64
	thermal    bool
	lowBattery bool
}

// New builds a daemon from cfg, filling in production defaults. It touches nothing until Serve.
func New(cfg Config) *Daemon {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	cfg.Log = log
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.AfterFunc == nil {
		cfg.AfterFunc = func(d time.Duration, f func()) func() bool { return time.AfterFunc(d, f).Stop }
	}
	if cfg.SupportDir == "" {
		cfg.SupportDir = paths.SupportDir()
	}
	if cfg.SettingsPath == "" {
		cfg.SettingsPath = paths.ConfigFile()
	}
	if cfg.Socket == "" {
		cfg.Socket = filepath.Join(cfg.SupportDir, filepath.Base(paths.CLISocket()))
	}
	if cfg.HelperSocket == "" {
		cfg.HelperSocket = paths.HelperSocket
	}
	if cfg.BootTime == nil {
		cfg.BootTime = darwin.BootTime
	}
	if cfg.ProcessPath == nil {
		cfg.ProcessPath = darwin.ProcessPath
	}
	if cfg.ProcessAlive == nil {
		cfg.ProcessAlive = darwin.ProcessAlive
	}
	if cfg.SleepDisabled == nil {
		cfg.SleepDisabled = darwin.SleepDisabled
	}
	if cfg.ThermalState == nil {
		cfg.ThermalState = darwin.ThermalState
	}
	if cfg.Capabilities == nil {
		cfg.Capabilities = darwin.Capabilities
	}
	cfg.SweepInterval = orDefault(cfg.SweepInterval, DefaultSweepInterval)
	cfg.ReconcileInterval = orDefault(cfg.ReconcileInterval, DefaultReconcileInterval)
	cfg.HelperProbeInterval = orDefault(cfg.HelperProbeInterval, DefaultHelperProbeInterval)
	cfg.LatchRecheckInterval = orDefault(cfg.LatchRecheckInterval, DefaultLatchRecheckInterval)
	cfg.ShutdownTimeout = orDefault(cfg.ShutdownTimeout, DefaultShutdownTimeout)
	cfg.HelperWait = orDefault(cfg.HelperWait, DefaultHelperWait)
	cfg.ConnTimeout = orDefault(cfg.ConnTimeout, DefaultConnTimeout)

	d := &Daemon{
		cfg:              cfg,
		log:              log,
		state:            store.NewStateStore(cfg.SupportDir),
		events:           store.NewEventLog(cfg.SupportDir),
		helper:           cfg.Helper,
		chimes:           cfg.Chimes,
		procs:            cfg.Procs,
		lid:              cfg.Lid,
		wake:             cfg.Wake,
		battery:          cfg.Battery,
		thermal:          cfg.Thermal,
		idle:             cfg.Idle,
		session:          cfg.Session,
		processes:        cfg.Processes,
		display:          cfg.Display,
		socket:           cfg.Socket,
		latch:            policy.NewCutoutLatch(),
		lastReleaseCause: policy.ReleaseWorkComplete,
		sniffSuppressed:  map[int]bool{},
	}
	d.registry = registry.New(d.now)
	d.events.Now = d.now
	d.events.Log = log
	if d.helper == nil {
		d.helper = ipc.NewHelperClient(cfg.HelperSocket)
	}
	if d.chimes == nil {
		p := chime.NewPlayer(log)
		p.Muted = darwin.OutputMuted
		d.chimes = p
	}
	if d.procs == nil {
		d.procs = agents.System()
	}
	if d.lid == nil {
		d.lid = monitor.NewLidMonitor()
	}
	if d.wake == nil {
		d.wake = monitor.NewWakeDetector()
	}
	if d.battery == nil {
		d.battery = monitor.NewBatteryMonitor()
	}
	if d.thermal == nil {
		d.thermal = monitor.NewThermalMonitor()
	}
	if d.idle == nil {
		d.idle = monitor.NewIdleMonitor()
	}
	if d.session == nil {
		d.session = monitor.NewSessionStatusMonitor()
	}
	if d.processes == nil {
		d.processes = monitor.NewProcessWatcher()
	}
	if d.display == nil {
		d.display = monitor.NewDisplayHold()
	}
	d.driver = newHelperDriver(d.helper, log.With("component", "helper"))
	d.driver.beforeUnblock = d.playSleepCue
	// Going idle is the safe moment to adopt a binary an update swapped in while holding.
	d.driver.afterApply = func(blocked, _ bool) {
		if !blocked {
			d.relaunchIfUpdatedWhenIdle()
		}
	}
	d.driver.expectedVersion = paths.Version
	d.driver.retryBase = orDefault(cfg.HelperRetryBase, defaultRetryBase)
	d.driver.retryMax = orDefault(cfg.HelperRetryMax, defaultRetryMax)
	d.wireMonitors()
	return d
}

func orDefault(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}

// now is the wall clock without its monotonic reading: deadlines and ages must count time spent
// asleep, which Go's monotonic clock does not on macOS.
func (d *Daemon) now() time.Time { return d.cfg.Now().Round(0) }

// Serve runs the daemon until ctx is cancelled, then shuts down: the monitors stop, the helper is
// asked to clear the block (bounded by ShutdownTimeout), the state is saved and the socket removed.
//
// It returns nil after a cancellation, ErrExecutableReplaced after stopping to adopt an updated
// binary, or an error when it could not start (another daemon already answers on the socket).
func (d *Daemon) Serve(ctx context.Context) error {
	if listening(d.socket) {
		return fmt.Errorf("daemon: %w on %s", errAlreadyRunning, d.socket)
	}
	ctx, exit := context.WithCancelCause(ctx)
	defer exit(nil)
	d.mu.Lock()
	d.exit = exit
	d.mu.Unlock()

	d.log.Info("lidwake daemon starting", "version", paths.Version)
	d.mu.Lock()
	d.settings = settings.Load(d.cfg.SettingsPath)
	d.mu.Unlock()
	d.restore()

	var wg sync.WaitGroup
	driverCtx, stopDriver := context.WithCancel(context.Background())
	defer stopDriver()
	driverDone := make(chan struct{})
	go func() { defer close(driverDone); d.driver.run(driverCtx) }()
	wg.Add(2)
	go func() { defer wg.Done(); d.consumeBlockingEdges() }()
	go func() { defer wg.Done(); d.consumeDisplayEdges() }()

	d.startMonitors(ctx)
	// Re-arm the exit watch for every restored process. (The edge consumers apply the restored
	// blocking and display state themselves.)
	for _, a := range d.registry.Snapshot() {
		if a.PID > 0 {
			d.processes.Watch(a.PID)
		}
	}
	d.mu.Lock()
	// A deadline that passed while the daemon was down fires right away.
	d.scheduleOffTimerLocked()
	d.mu.Unlock()
	// Last, so no request sees a half-started daemon.
	srv := d.startSocket(ctx)

	<-ctx.Done()
	cause := context.Cause(ctx)
	d.shutdown(srv, stopDriver)
	wg.Wait()
	// The driver may be stuck in a call to a wedged helper; the process is about to exit anyway.
	select {
	case <-driverDone:
	case <-time.After(d.cfg.ShutdownTimeout):
		d.log.Warn("the helper driver is still waiting on the helper; leaving it")
	}
	if errors.Is(cause, ErrExecutableReplaced) {
		return ErrExecutableReplaced
	}
	return nil
}

// restore loads state.json. Assertions are validated first: after a reboot every stored PID is
// stale and recycled, and restoring one that now names a busy system process would block sleep with
// no agent behind it.
func (d *Daemon) restore() {
	restored, ok := d.state.Load()
	if !ok {
		return
	}
	boot, err := d.cfg.BootTime()
	if err != nil {
		d.log.Warn("cannot read the boot time; restoring without the reboot check", "err", err)
		boot = time.Time{}
	}
	outcome := policy.PartitionRestored(restored.Assertions, boot, d.cfg.ProcessPath)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.paused = restored.Paused
	d.offAt = cloneTime(restored.OffAt)
	d.registry.ReplaceAll(outcome.Kept)
	if len(outcome.Dropped) > 0 {
		d.log.Info("dropped stale persisted assertions (pre-boot or recycled pid)", "count", len(outcome.Dropped))
		d.persistLocked()
	}
	d.log.Info("restored state", "assertions", len(outcome.Kept), "paused", d.paused)
}

// shutdown stops everything Serve started, in an order where nothing can re-block after the final
// unblock.
func (d *Daemon) shutdown(srv *socketServer, stopDriver context.CancelFunc) {
	d.log.Info("shutting down; clearing the sleep block")
	d.mu.Lock()
	d.closing = true
	d.sweepTicker.set(false, 0, nil)
	d.reconcileTicker.set(false, 0, nil)
	d.probeTicker.set(false, 0, nil)
	d.latchTicker.set(false, 0, nil)
	if d.offStop != nil {
		d.offStop()
		d.offStop = nil
	}
	d.mu.Unlock()

	srv.close()
	d.stopMonitors()
	d.registry.Close()
	stopDriver()

	d.mu.Lock()
	d.persistLocked()
	d.mu.Unlock()
	if d.driver.stop(d.cfg.ShutdownTimeout) {
		d.helper.Close()
	} else {
		// The client's Close waits for the wedged call (ipc.HelperClient serializes them), which
		// would stretch the bounded shutdown to the call's own deadline; the process exits anyway.
		go d.helper.Close()
	}
	srv.wait(d.cfg.ConnTimeout)
	if err := d.events.Close(); err != nil {
		d.log.Warn("closing the event log failed", "err", err)
	}
}

// requestExitLocked ends Serve with cause. Callers hold mu.
func (d *Daemon) requestExitLocked(cause error) {
	if d.exit != nil {
		d.exit(cause)
	}
}

// relaunchIfUpdatedWhenIdle adopts a binary an update put on disk by ending Serve, so launchd
// (KeepAlive) relaunches the daemon from the new image. Only while idle: restarting while holding
// would briefly drop the helper connection for no reason, and every blocking → idle edge checks
// again. Not while a cutout is latched either: the latch is not persisted, so a relaunch would let
// the agent that was just cut off re-acquire while the hazard is still present; the latch clearing
// checks again. A no-op unless the binary actually changed.
func (d *Daemon) relaunchIfUpdatedWhenIdle() {
	if !d.cfg.Executable.HasBeenReplaced() {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.relaunchCheckedLocked()
}

// relaunchCheckedLocked is relaunchIfUpdatedWhenIdle after the replacement was seen.
func (d *Daemon) relaunchCheckedLocked() {
	switch {
	case d.closing:
	case d.registry.IsBlocking():
		d.log.Info("daemon binary replaced by an update; deferring the relaunch until idle")
	case d.latch.IsLatched():
		d.log.Info("daemon binary replaced by an update; deferring the relaunch until the safety cutout clears")
	default:
		d.log.Info("daemon binary replaced by an update; exiting so launchd relaunches the new daemon")
		d.requestExitLocked(ErrExecutableReplaced)
	}
}

// persistAndLogLocked records event and saves the state.
func (d *Daemon) persistAndLogLocked(event model.Event) {
	d.events.Append(event)
	d.persistLocked()
}

// persistLocked saves the registry, the paused bit and the off timer, and stamps the release time
// of any lid-close-held assertion that has gone (every release path persists, so this catches them
// all). Persistence is best effort: a failure is logged.
func (d *Daemon) persistLocked() {
	snapshot := d.registry.Snapshot()
	d.recordAwayReleasesLocked(snapshot)
	st := model.PersistedState{Assertions: snapshot, Paused: d.paused, OffAt: cloneTime(d.offAt)}
	if err := d.state.Save(st); err != nil {
		d.log.Error("cannot save the state", "err", err)
	}
}

// --- Operations (the CLI socket's surface) ---

type acquireOutcome int

const (
	acquireAccepted acquireOutcome = iota
	acquirePaused
	acquireOverCapacity
	acquireLatched
	acquireClosing
)

// acquireLocked adds or refreshes an assertion. It is refused while paused, while a cutout is
// latched, and when the registry is at capacity. message is the latch's explanation.
func (d *Daemon) acquireLocked(a model.Assertion) (outcome acquireOutcome, message string) {
	if d.closing {
		return acquireClosing, ""
	}
	if d.paused {
		d.log.Info("acquire ignored: lidwake is paused", "key", a.Key)
		return acquirePaused, ""
	}
	if msg := d.latch.RejectionMessage(); msg != "" {
		d.log.Info("acquire rejected: a safety cutout is latched", "key", a.Key)
		return acquireLatched, msg
	}
	// Cap any hook-carried TTL to the user's live max hold: the background-shell hook asks for the
	// CLI's 24 h ceiling so that this clamp, not a value baked in at install time, governs. Holds
	// arrive already clamped, so this is a no-op for them.
	a.ExpiresAt = policy.ClampExpiry(a.ExpiresAt, a.AcquiredAt, d.settings.ManualHoldMaxHours)
	snapshot := d.registry.Snapshot()
	if !containsKey(snapshot, a.Key) {
		if len(snapshot) >= maxAssertions {
			d.log.Error("acquire rejected: too many active assertions", "count", len(snapshot), "key", a.Key)
			return acquireOverCapacity, ""
		}
		if a.PID > 0 && countPID(snapshot, a.PID) >= maxAssertionsPerPID {
			d.log.Error("acquire rejected: the process already holds too many assertions", "pid", a.PID, "max", maxAssertionsPerPID)
			return acquireOverCapacity, ""
		}
	}
	isNew := d.registry.Acquire(a)
	// Watch the owning process so its assertions are released if the agent dies without firing
	// its end hook. pid <= 0 means the CLI could not identify a real agent process.
	if a.PID > 0 {
		d.processes.Watch(a.PID)
	}
	d.log.Info("acquire", "key", a.Key, "tool", a.Tool, "pid", a.PID, "new", isNew, "active", d.registry.Count())
	// A duplicate acquire refreshes the assertion but changes nothing worth a log line or a save.
	if isNew {
		d.persistAndLogLocked(model.EventAcquired)
	}
	return acquireAccepted, ""
}

// releaseLocked releases key, an agent's own "I'm done", and reports whether it existed.
//
// Every release site stamps lastReleaseCause only when it actually released something: the cue is
// chosen when the driver applies the unblock, so a no-op release arriving in between (a Stop hook
// for a key a pause already dropped) must not overwrite the cause of the release that emptied the
// registry.
func (d *Daemon) releaseLocked(key string) bool {
	existed := d.registry.Release(key)
	if !existed {
		d.log.Warn("release for an unknown key; nothing released", "key", truncateRunes(key, policy.MaxKeyLength))
		return false
	}
	d.lastReleaseCause = policy.ReleaseWorkComplete
	d.suppressSniffedLocked(key)
	d.log.Info("release", "key", key, "active", d.registry.Count())
	d.persistAndLogLocked(model.EventReleased)
	return true
}

// suppressSniffedLocked keeps a released sniffed assertion released: the sweep re-acquires any
// matched process it is not holding, so without this a user's release (or an idle release) would
// be undone 30 seconds later. Suppression lasts until the process exits.
func (d *Daemon) suppressSniffedLocked(key string) {
	if pid, ok := sniffedPID(key); ok {
		d.sniffSuppressed[pid] = true
	}
}

// releaseAll force-releases everything (a user action) and waits, bounded, for the helper's
// unblock, so a caller that releases before tearing the helper down finds the block cleared.
// It returns how many assertions were held.
func (d *Daemon) releaseAll(ctx context.Context) int {
	d.mu.Lock()
	if d.closing {
		d.mu.Unlock()
		return 0
	}
	count := d.registry.Count()
	if count > 0 {
		d.lastReleaseCause = policy.ReleaseUserAction
	}
	d.registry.RemoveAll()
	d.persistAndLogLocked(model.EventReleased)
	gen := d.driver.Request(d.registry.IsBlocking())
	d.mu.Unlock()
	d.waitForHelper(ctx, gen)
	return count
}

// setPaused pauses or resumes lidwake and returns how many assertions were held. Pausing releases
// everything and refuses acquires until resumed; resuming lets agents re-acquire on their next
// hook event. Pausing cancels the off timer: off already, it has nothing left to do.
func (d *Daemon) setPaused(ctx context.Context, paused bool) int {
	d.mu.Lock()
	count := d.registry.Count()
	gen, wait := d.setPausedLocked(paused, model.EventPaused)
	d.mu.Unlock()
	if wait {
		d.waitForHelper(ctx, gen)
	}
	return count
}

// setPausedLocked applies a pause or resume; wait reports a helper request to wait for.
func (d *Daemon) setPausedLocked(paused bool, event model.Event) (gen uint64, wait bool) {
	if d.closing {
		return 0, false
	}
	hadTimer := d.offAt != nil
	if paused {
		d.cancelOffTimerLocked()
	}
	if paused == d.paused {
		if paused && hadTimer {
			d.persistLocked()
		}
		return 0, false
	}
	d.paused = paused
	if !paused {
		d.log.Info("resumed: agents can keep the Mac awake again")
		d.persistAndLogLocked(model.EventResumed)
		return 0, false
	}
	d.log.Info("paused: releasing every assertion and ignoring acquires until resumed")
	if d.registry.Count() > 0 {
		d.lastReleaseCause = policy.ReleaseUserAction
	}
	d.registry.RemoveAll()
	d.persistAndLogLocked(event)
	return d.driver.Request(d.registry.IsBlocking()), true
}

func (d *Daemon) waitForHelper(ctx context.Context, gen uint64) {
	ctx, cancel := context.WithTimeout(ctx, d.cfg.HelperWait)
	defer cancel()
	if !d.driver.Wait(ctx, gen) {
		d.log.Warn("the helper has not confirmed the unblock yet; it keeps retrying")
	}
}

// setOffTimer arms the off timer for seconds from now, or cancels it for a missing or
// non-positive duration, and returns the deadline. Setting a timer means "keep agents awake until
// then", so it also resumes a paused lidwake.
func (d *Daemon) setOffTimer(seconds *float64) *time.Time {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closing {
		return nil
	}
	d.offAt = policy.OffTimerDeadline(seconds, d.now())
	if d.offAt != nil && d.paused {
		d.setPausedLocked(false, model.EventResumed)
	}
	d.scheduleOffTimerLocked()
	d.persistLocked()
	if d.offAt != nil {
		d.log.Info("off timer set", "at", *d.offAt)
	} else {
		d.log.Info("off timer cancelled")
	}
	return cloneTime(d.offAt)
}

// scheduleOffTimerLocked (re)arms the off timer for offAt. The timer runs on the monotonic clock,
// which stops while the Mac sleeps, so the wake handler reschedules it: a deadline that passed in
// sleep fires at once.
func (d *Daemon) scheduleOffTimerLocked() {
	if d.offStop != nil {
		d.offStop()
		d.offStop = nil
	}
	d.offGen++
	if d.offAt == nil || d.closing {
		return
	}
	delay := max(d.offAt.Sub(d.now()), 0)
	gen := d.offGen
	d.offStop = d.cfg.AfterFunc(delay, func() { d.offTimerFired(gen) })
}

func (d *Daemon) cancelOffTimerLocked() {
	d.offAt = nil
	d.scheduleOffTimerLocked()
}

// offTimerFired pauses lidwake once the deadline has really passed by the wall clock.
func (d *Daemon) offTimerFired(gen uint64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closing || gen != d.offGen {
		return
	}
	d.offStop = nil
	if !policy.OffTimerDue(d.offAt, d.now()) {
		d.scheduleOffTimerLocked()
		return
	}
	d.log.Info("off timer reached; turning lidwake off")
	d.setPausedLocked(true, model.EventOffTimer)
}

// holdOutcome is the result of an agent hold request.
type holdOutcome struct {
	placed   bool
	key      string
	ttl      float64
	count    int
	refusal  string
	disabled bool
	paused   bool
}

// hold places an explicit agent hold: it clamps the TTL to the configured cap, mints a "hold:" key
// and acquires a manual assertion (exempt from the idle release, bounded by its TTL). It honors the
// agent-holds switch and the pause.
func (d *Daemon) hold(reason string, requestedTTL *float64, pid int, tool string, display bool) holdOutcome {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closing {
		return holdOutcome{refusal: "the lidwake daemon is shutting down"}
	}
	if !d.settings.AgentHoldsEnabled {
		d.log.Info("hold rejected: agent holds are disabled in settings")
		return holdOutcome{disabled: true}
	}
	if d.paused {
		d.log.Info("hold rejected: lidwake is paused")
		return holdOutcome{paused: true}
	}
	ttl := policy.ClampHoldTTL(requestedTTL, d.settings.ManualHoldMaxHours)
	key := policy.NewHoldKey()
	label := tool
	if label == "" {
		label = policy.DefaultHoldTool
	}
	dur := secondsToDuration(ttl)
	a := model.New(key, label, reason, pid, label, d.now(), &dur, model.OriginManual)
	a.HoldsDisplay = display
	switch outcome, message := d.acquireLocked(a); outcome {
	case acquirePaused:
		return holdOutcome{paused: true}
	case acquireOverCapacity:
		return holdOutcome{refusal: "Too many active assertions — hold not placed."}
	case acquireLatched:
		return holdOutcome{refusal: message}
	case acquireClosing:
		return holdOutcome{refusal: "the lidwake daemon is shutting down"}
	}
	count := d.registry.Count()
	d.log.Info("hold placed", "key", key, "ttl", int(ttl), "pid", pid, "reason", reason)
	return holdOutcome{placed: true, key: key, ttl: ttl, count: count}
}

// reloadSettings re-reads config.json and pushes it into every monitor, the cutout scope and the
// sniff sweep's gate.
func (d *Daemon) reloadSettings() {
	s := settings.Load(d.cfg.SettingsPath)
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closing {
		return
	}
	d.settings = s
	d.applySettingsToMonitors(s)
	d.applyCutoutScopeLocked()
	d.updateTickersLocked()
	d.log.Info("settings reloaded")
}

// status builds the status `lidwake status` and the dashboard show.
func (d *Daemon) status() model.Status {
	snapshot := d.registry.Snapshot()
	// While blocking the monitor polls and its reading is current; otherwise read once on demand.
	// Outside mu: the first SMC read can take a while.
	temp, tempOK := d.thermal.Current()
	tempOK = tempOK && finite(temp)
	sleepDisabled, thermalState := d.cfg.SleepDisabled(), d.cfg.ThermalState()
	lidClosed := d.lid.Closed()
	displayWarning := d.display.Warning(registry.AnyHoldsDisplay(snapshot), lidClosed)
	battery, batteryOK := d.battery.Last()
	last, lastAt, lastOK := d.events.Last()
	helperFailed, helperConnected := d.driver.LastApplyFailed(), d.driver.Connected()

	d.mu.Lock()
	defer d.mu.Unlock()
	s := d.settings
	warnings := []string{}
	if msg := d.latch.RejectionMessage(); msg != "" {
		warnings = append(warnings, msg)
	}
	if helperFailed && len(snapshot) > 0 {
		warnings = append(warnings, "The sleep block couldn't be fully applied — your Mac may still sleep when the lid closes. Retrying.")
	}
	if s.ThermalCutoutEnabled && len(snapshot) > 0 && !tempOK {
		warnings = append(warnings, "CPU temperature is unreadable, so the thermal cutout can't trigger.")
	}
	if displayWarning != "" {
		warnings = append(warnings, displayWarning)
	}
	cutouts := []string{}
	for _, c := range d.latch.Active() {
		cutouts = append(cutouts, string(c))
	}
	st := model.Status{
		Paused:          d.paused,
		Blocking:        len(snapshot) > 0,
		Assertions:      snapshot,
		LidClosed:       lidClosed,
		HelperConnected: helperConnected,
		SleepDisabled:   sleepDisabled,
		ThermalState:    thermalState,
		ActiveCutouts:   cutouts,
		OffAt:           cloneTime(d.offAt),
		Warnings:        warnings,
		AwaySummary:     d.awaySummary,
		Settings:        s,
		Version:         paths.Version,
	}
	if st.Assertions == nil {
		st.Assertions = []model.Assertion{}
	}
	if tempOK {
		st.CPUTemperature = &temp
	}
	if batteryOK {
		pct, onBattery := battery.Percent, battery.OnBattery
		st.BatteryPercent, st.OnBattery = &pct, &onBattery
	}
	if lastOK {
		st.LastEvent, st.LastEventAt = last, &lastAt
	}
	return st
}

// --- small helpers ---

func containsKey(as []model.Assertion, key string) bool {
	for _, a := range as {
		if a.Key == key {
			return true
		}
	}
	return false
}

func countPID(as []model.Assertion, pid int) int {
	n := 0
	for _, a := range as {
		if a.PID == pid {
			n++
		}
	}
	return n
}

func cloneTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	c := *t
	return &c
}
