package daemon

import (
	"context"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nikitaShakhbazyan/lidwake-go/internal/activity"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/agents"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/chime"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/darwin"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/model"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/policy"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/settings"
)

// sniffReason is the reason stored on a process-sniffed assertion.
const sniffReason = "auto (process sniffing)"

// wireMonitors sets every monitor's callbacks. The callbacks run on the monitors' goroutines.
func (d *Daemon) wireMonitors() {
	d.lid.OnChange = d.onLidChange
	d.wake.OnWake = d.onWake
	// Battery readings keep coming while idle, so every one is the battery latch's recovery
	// path: plugging in clears it within one poll.
	d.battery.OnReading = func(darwin.Battery) {
		d.mu.Lock()
		defer d.mu.Unlock()
		if !d.closing {
			d.reevaluateLatchLocked()
		}
	}
	d.battery.OnCutout = d.onBatteryCutout
	d.thermal.OnReading = d.onThermalReading
	d.thermal.OnCutout = d.onThermalCutout
	d.idle.Assertions = d.idleAssertions
	d.idle.OnRelease = d.onIdleRelease
	d.session.Assertions = d.registry.Snapshot
	d.session.OnActions = d.onSessionActions
	d.processes.OnExit = d.onProcessExit
	if d.lid.Log == nil {
		d.lid.Log = d.log
	}
	if d.wake.Log == nil {
		d.wake.Log = d.log
	}
	if d.battery.Log == nil {
		d.battery.Log = d.log
	}
	if d.thermal.Log == nil {
		d.thermal.Log = d.log
	}
	if d.idle.Log == nil {
		d.idle.Log = d.log
	}
	if d.session.Log == nil {
		d.session.Log = d.log
	}
	if d.processes.Log == nil {
		d.processes.Log = d.log
	}
	if d.display.Log == nil {
		d.display.Log = d.log
	}
}

// startMonitors configures and starts every monitor. The lid starts first, so its state is
// current for the cutout scope, which is pushed before the battery and thermal monitors start:
// their first reading (at once, over restored holds) already sees the armed lid. The battery
// monitor always runs: on a Mac without one its readings are simply absent, and a battery that a
// single probe at launch missed must not leave both battery cutouts off for good.
func (d *Daemon) startMonitors(ctx context.Context) {
	d.lid.Start(ctx)
	d.mu.Lock()
	d.applySettingsToMonitors(d.settings)
	d.applyCutoutScopeLocked()
	d.mu.Unlock()
	if !d.cfg.Capabilities().HasBattery {
		d.log.Info("no battery found yet; the battery cutouts act once one reports")
	}
	d.battery.Start(ctx)
	d.thermal.Start(ctx)
	d.idle.Start(ctx)
	d.session.Start(ctx)
	d.processes.Start(ctx)
	d.wake.Start(ctx)
}

// stopMonitors stops every monitor and waits for callbacks in flight. Never call it under mu: a
// callback may be waiting for it.
func (d *Daemon) stopMonitors() {
	d.wake.Stop()
	d.lid.Stop()
	d.battery.Stop()
	d.thermal.Stop()
	d.idle.Stop()
	d.session.Stop()
	d.processes.Stop()
	d.display.Stop()
}

func (d *Daemon) applySettingsToMonitors(s settings.Settings) {
	d.idle.ApplySettings(s)
	d.session.ApplySettings(s)
	d.thermal.ApplySettings(s)
	d.battery.ApplySettings(s)
}

// setMonitorsBlocking gates the monitors whose work only matters while the Mac is kept awake.
func (d *Daemon) setMonitorsBlocking(blocking bool) {
	d.thermal.SetBlocking(blocking)
	d.battery.SetBlocking(blocking)
	d.idle.SetBlocking(blocking)
	d.session.SetBlocking(blocking)
	d.processes.SetBlocking(blocking)
}

// consumeBlockingEdges is the single long-lived consumer of the registry's blocking edges. It
// gates the monitors and timers and hands the state to the helper driver, which applies it serially
// (and plays the pre-sleep cue before an unblock). It ends when the registry is closed.
//
// The registry reports only changes, so the state at start (restored assertions, or none — the
// initial helper sync) is applied first. Edges queued before or after that read are applied after
// it, in order, so the last state applied is always the registry's current one; nothing else sets
// the monitors' blocking gate, so no stale seed can overwrite a newer edge.
func (d *Daemon) consumeBlockingEdges() {
	d.applyBlocking(d.registry.IsBlocking())
	for blocking := range d.registry.BlockingChanges() {
		d.applyBlocking(blocking)
	}
}

func (d *Daemon) applyBlocking(blocking bool) {
	d.setMonitorsBlocking(blocking)
	d.mu.Lock()
	d.blocking = blocking
	d.updateTickersLocked()
	d.mu.Unlock()
	d.driver.Request(blocking)
}

// consumeDisplayEdges raises and drops the display hold as WantsDisplay flips, starting from the
// state at start. Derived from the assertions, so every release path — explicit, idle, TTL, process
// exit, pause, cutouts — drops it with no extra bookkeeping: the safety nets outrank the hold.
func (d *Daemon) consumeDisplayEdges() {
	d.syncDisplay()
	for range d.registry.DisplayChanges() {
		d.syncDisplay()
	}
}

// syncDisplay sets the display hold to the registry's current WantsDisplay. The edge consumer, the
// reconcile and the wake handler all call it; reading the state and setting the hold under one
// lock means a caller that read the state just before a release cannot raise the hold again after
// the release's edge dropped it, which would keep the display awake with nothing behind it.
func (d *Daemon) syncDisplay() {
	d.displayMu.Lock()
	defer d.displayMu.Unlock()
	d.display.Set(d.registry.WantsDisplay())
}

// resyncHelperLocked asks the driver to re-push the registry's current blocking state. Under mu,
// where every registry mutation happens: a state read outside it could be overtaken by a release
// whose unblock request goes first, and the stale block would then stick with nothing held.
func (d *Daemon) resyncHelperLocked() {
	d.driver.Request(d.registry.IsBlocking())
}

// updateTickersLocked arms or disarms the periodic work for the current state.
func (d *Daemon) updateTickersLocked() {
	s := d.settings
	live := !d.closing
	d.sweepTicker.set(live && d.blocking && s.ProcessSniffingEnabled && s.AutoAcquireForKnownAgents, d.cfg.SweepInterval, d.sniffSweep)
	d.reconcileTicker.set(live && d.blocking, d.cfg.ReconcileInterval, d.reconcile)
	d.probeTicker.set(live && d.blocking, d.cfg.HelperProbeInterval, d.probeHelper)
	d.latchTicker.set(live && d.latch.Has(policy.CutoutThermal), d.cfg.LatchRecheckInterval, d.recheckLatch)
}

// reconcile re-pushes the block to the helper while blocking. The helper's set(true) re-runs the
// whole mechanism, so this heals every way the block can silently rot: a pmset run that failed
// once, a helper relaunch, the kernel resetting disablesleep. It also re-asserts (and relights) the
// display hold.
func (d *Daemon) reconcile() {
	d.mu.Lock()
	live := !d.closing && d.blocking
	if live {
		d.resyncHelperLocked()
	}
	d.mu.Unlock()
	if live {
		d.syncDisplay()
	}
}

// probeHelper checks, every few seconds while blocking, that the helper still holds the block. A
// helper that crashed restarts without it (its start restores the saved setting), and the
// reconcile alone would notice only up to a minute later — long enough for a closed lid to sleep
// the Mac mid-task. A miss re-pushes the block at once.
func (d *Daemon) probeHelper() {
	d.mu.Lock()
	live := !d.closing && d.blocking
	d.mu.Unlock()
	if !live || d.driver.Holds() {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.closing && d.blocking {
		d.log.Warn("the helper no longer holds the sleep block; re-applying it")
		d.resyncHelperLocked()
	}
}

// playSleepCue plays the pre-sleep cue for the release that just emptied the registry — the
// audible "your Mac is going back to sleep" for a user who closed the lid and walked away. The
// decision is policy.DecideSleepCue's: silent with the lid open, with the feature off, or when the
// cause's sound is "off". Runs on the helper driver right before the unblock.
func (d *Daemon) playSleepCue(ctx context.Context) {
	d.mu.Lock()
	cause, s, closing := d.lastReleaseCause, d.settings, d.closing
	d.mu.Unlock()
	if closing {
		return
	}
	decision := policy.DecideSleepCue(cause, d.lid.Closed(), s)
	if decision.Silent() {
		return
	}
	d.log.Info("pre-sleep cue before clearing the sleep block", "cause", string(cause))
	d.chimes.PlaySleepCue(ctx, decision.SoundName, chime.Cue(decision.Cue), s.SoundVolume)
}

// onLidChange logs the lid, re-arms the cutouts for it, and on a close over working agents plays
// the chime, locks the screen and starts the away summary; on an open it finishes the summary.
func (d *Daemon) onLidChange(closed bool) {
	event := model.EventLidOpened
	if closed {
		event = model.EventLidClosed
	}
	d.mu.Lock()
	if d.closing {
		d.mu.Unlock()
		return
	}
	d.events.Append(event)
	s := d.settings
	// Opening the lid clears a closed-lid-only latch: the user is present.
	d.applyCutoutScopeLocked()
	var decision policy.LidCloseDecision
	if closed {
		decision = policy.DecideLidClose(d.registry.IsBlocking(), s.LockOnLidClose, s.SoundOnLidClose)
		// A lid already open again must not start tracking: it would dangle until the next open
		// and report a summary spanning the whole gap.
		if decision.ShouldBeginAwayTracking && d.lid.Closed() {
			d.beginAwayTrackingLocked()
		}
	} else if !d.lid.Closed() {
		d.finishAwayTrackingLocked()
	}
	d.mu.Unlock()
	if decision.ShouldChime {
		d.chimes.PlayLidCloseChime(s.SoundVolume, s.ChimeName)
	}
	// Secure the kept-awake machine. An explicit lock works even while an idle-lock-prevention
	// assertion is held, and the system stays awake, so the agent keeps running.
	if decision.ShouldLock {
		d.lockScreen()
	}
}

func (d *Daemon) lockScreen() {
	if d.cfg.LockScreen == nil {
		d.log.Warn("cannot lock the screen: no screen locker configured")
		return
	}
	d.log.Info("locking the screen")
	d.cfg.LockScreen()
}

// onWake re-pushes the block (the kernel can reset the clamshell setting across sleep), drops the
// idle baselines (a pre-sleep CPU sample would read a mid-work agent as long idle), relights the
// display for a display hold, and fires an off timer that came due in sleep.
func (d *Daemon) onWake() {
	d.idle.ResetBaselines()
	d.mu.Lock()
	if d.closing {
		d.mu.Unlock()
		return
	}
	d.scheduleOffTimerLocked()
	d.resyncHelperLocked()
	d.mu.Unlock()
	d.syncDisplay()
}

// onThermalReading tracks the peak temperature while the lid is closed, for the away summary.
func (d *Daemon) onThermalReading(celsius float64) {
	if !finite(celsius) {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.away.active {
		return
	}
	if d.away.peak == nil || celsius > *d.away.peak {
		c := celsius
		d.away.peak = &c
	}
}

func (d *Daemon) onThermalCutout() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closing {
		return
	}
	d.log.Warn("thermal cutout: releasing every assertion")
	if d.away.active {
		d.away.thermal = true
	}
	d.tripLocked(policy.CutoutThermal, model.EventThermalCutout)
}

func (d *Daemon) onBatteryCutout(cause policy.CutoutCause) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closing {
		return
	}
	event := model.EventLowBatteryCutout
	if cause == policy.CutoutOnBattery {
		event = model.EventACPowerCutout
		d.log.Warn("AC-only cutout: releasing every assertion")
	} else {
		d.log.Warn("low-battery cutout: releasing every assertion")
	}
	if d.away.active {
		d.away.lowBattery = true
	}
	d.tripLocked(cause, event)
}

// tripLocked latches cause and releases everything. A monitor can report the same cutout again
// before the unblock reaches it, so this is idempotent: the event is recorded when the cause newly
// latched or something was released, not for a repeated report that changes nothing.
func (d *Daemon) tripLocked(cause policy.CutoutCause, event model.Event) {
	newly := !d.latch.Has(cause)
	d.lastReleaseCause = policy.ReleaseSafetyCutout
	d.latch.Trip(cause)
	d.updateTickersLocked()
	released := d.registry.Count() > 0
	if !newly && !released {
		return
	}
	d.registry.RemoveAll()
	d.persistAndLogLocked(event)
}

// recheckLatch samples the temperature while the thermal latch is held. The thermal poll only
// runs while blocking, which the cutout just ended, so without this "has it cooled?" would not be
// asked again until the lid opens. The battery latch needs no timer: the battery poll never stops.
func (d *Daemon) recheckLatch() {
	d.thermal.ReadNow()
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.closing {
		d.reevaluateLatchLocked()
	}
}

// applyCutoutScopeLocked pushes the cutout settings into the monitors and the latch: the armed lid,
// whether opening the lid clears the latch, and dropping the latch of a cutout the user turned off
// (it would otherwise refuse acquires until a hazard nobody watches for recedes).
func (d *Daemon) applyCutoutScopeLocked() {
	s := d.settings
	// With safety cutouts on an open lid too, disablesleep blocks the kernel's own emergency
	// sleep whatever the lid does, so the cutouts guard an open lid as well.
	armed := d.lid.Closed() || s.SafetyCutoutsWithLidOpen
	d.thermal.SetLidClosed(armed)
	d.battery.SetLidClosed(armed)
	d.latch.ClearsOnLidOpen = !s.SafetyCutoutsWithLidOpen
	if dropped := d.latch.DropDisabled(s.ThermalCutoutEnabled, s.LowBatteryCutoutEnabled, s.RequireACPower); len(dropped) > 0 {
		d.log.Info("cutout latch dropped with its setting", "causes", causeNames(dropped))
		d.updateTickersLocked()
	}
	d.reevaluateLatchLocked()
}

// reevaluateLatchLocked clears latched causes whose hazard receded (with hysteresis), or all of
// them on a lid open when the cutouts only guard a closed lid.
func (d *Daemon) reevaluateLatchLocked() {
	if !d.latch.IsLatched() {
		return
	}
	s := d.settings
	c := policy.Conditions{
		ThermalThresholdCelsius: s.ThermalThresholdCelsius,
		BatteryThresholdPercent: s.LowBatteryThresholdPercent,
		LidClosed:               d.lid.Closed(),
	}
	if t, ok := d.thermal.Last(); ok {
		c.TemperatureCelsius = &t
	}
	if b, ok := d.battery.Last(); ok {
		pct, onBattery := b.Percent, b.OnBattery
		c.BatteryPercent, c.OnBattery = &pct, &onBattery
	}
	cleared := d.latch.Update(c)
	if len(cleared) == 0 {
		return
	}
	d.log.Info("cutout latch cleared", "causes", causeNames(cleared))
	d.updateTickersLocked()
	// A relaunch deferred while the latch was held can go ahead now, if still idle.
	if d.cfg.Executable.HasBeenReplaced() {
		d.relaunchCheckedLocked()
	}
}

// idleAssertions is the idle sweep's snapshot, remembered so onIdleRelease can tell whether a
// verdict still applies.
func (d *Daemon) idleAssertions() []model.Assertion {
	d.mu.Lock()
	defer d.mu.Unlock()
	snapshot := d.registry.Snapshot()
	d.idleEvaluated = make(map[string]model.Assertion, len(snapshot))
	for _, a := range snapshot {
		d.idleEvaluated[a.Key] = a
	}
	return snapshot
}

// onIdleRelease releases what the sweep decided on, except an assertion that changed since its
// snapshot. The probes run outside mu, so a hook can re-acquire a key meanwhile — a new turn, a
// resumed session under a new PID, a refreshed TTL — and nothing would acquire it again this turn.
// The next sweep judges the new state.
func (d *Daemon) onIdleRelease(releases []activity.Release) {
	d.mu.Lock()
	defer d.mu.Unlock()
	evaluated := d.idleEvaluated
	d.idleEvaluated = nil
	if d.closing {
		return
	}
	current := map[string]model.Assertion{}
	for _, a := range d.registry.Snapshot() {
		current[a.Key] = a
	}
	reasons := make([]activity.ReleaseReason, 0, len(releases))
	for _, r := range releases {
		reasons = append(reasons, r.Reason)
	}
	released := 0
	for _, r := range releases {
		if live, ok := current[r.Key]; ok && !sameIdleInputs(evaluated[r.Key], live) {
			d.log.Info("idle release skipped: the assertion changed during the sweep", "key", r.Key, "reason", string(r.Reason))
			continue
		}
		if d.registry.Release(r.Key) {
			released++
			d.suppressSniffedLocked(r.Key)
			d.log.Info("idle release", "key", r.Key, "reason", string(r.Reason))
		}
	}
	if released > 0 {
		d.lastReleaseCause = policy.ReleaseCauseForIdleBatch(reasons)
		d.persistAndLogLocked(model.EventIdleRelease)
	}
}

// onProcessExit releases the assertions of an agent process that exited — from the away user's
// point of view, its work is done.
func (d *Daemon) onProcessExit(pid int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closing {
		return
	}
	delete(d.sniffSuppressed, pid)
	if removed := d.registry.ReleaseAllMatchingPID(pid); removed > 0 {
		d.lastReleaseCause = policy.ReleaseWorkComplete
		d.log.Info("released the assertions of an exited process", "pid", pid, "count", removed)
		d.persistAndLogLocked(model.EventReleased)
	}
}

// onSessionActions applies one session-status sweep's decisions, in order. Expiry and mark changes
// are saved once at the end; releases and re-acquires go through the usual paths.
func (d *Daemon) onSessionActions(actions []activity.WaitAction) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closing {
		return
	}
	mutated := false
	for _, action := range actions {
		switch a := action.(type) {
		case activity.SetWaitingFor:
			d.registry.SetWaitingFor(a.Key, a.Label)
			mutated = true
		case activity.Park:
			at := a.ExpiresAt
			d.registry.SetExpiry(a.Key, &at)
			mutated = true
		case activity.Restore:
			d.registry.SetExpiry(a.Key, a.Expiry)
			mutated = true
		case activity.ReleaseWaiting:
			// The agent is waiting, not finished: with the lid closed the cue must say "the work
			// may not be done", which is what the expired-hold cue is for.
			if d.registry.Release(a.Key) {
				d.lastReleaseCause = policy.ReleaseHoldExpired
				d.persistAndLogLocked(model.EventReleased)
			}
		case activity.Reacquire:
			d.acquireLocked(a.Assertion)
		}
	}
	if mutated {
		d.persistLocked()
	}
}

// sniffSweep auto-acquires for name-matched agents (claude, codex, …) found running without their
// hooks. Opt-in (autoAcquireForKnownAgents), and only while already holding for an agent: sniffing
// catches a concurrent agent while awake anyway, never starts a hold from nothing, and makes no
// wakeups while idle. Gateway agents (Hermes) are never sniffed: their always-running process would
// pin the Mac forever; their hooks fire on real turns.
func (d *Daemon) sniffSweep() {
	d.mu.Lock()
	if !d.sniffingLocked() {
		d.mu.Unlock()
		return
	}
	watched := map[int]bool{}
	for _, a := range d.registry.Snapshot() {
		watched[a.PID] = true
	}
	for pid := range d.sniffSuppressed {
		if !d.cfg.ProcessAlive(pid) {
			delete(d.sniffSuppressed, pid)
		}
	}
	suppressed := make(map[int]bool, len(d.sniffSuppressed))
	for pid := range d.sniffSuppressed {
		suppressed[pid] = true
	}
	d.mu.Unlock()

	// The process scan is a syscall per process: outside the lock.
	for _, p := range d.procs.RunningProcesses() {
		if watched[p.PID] || suppressed[p.PID] {
			continue
		}
		kind, ok := agents.ForRunningProcess(p.Name, p.Path)
		if !ok || kind.IsGatewayScoped() {
			continue
		}
		key := policy.SniffedKeyPrefix + string(kind) + ":" + strconv.Itoa(p.PID)
		d.mu.Lock()
		if d.sniffingLocked() && !d.sniffSuppressed[p.PID] {
			d.log.Info("auto-acquiring for a sniffed agent", "tool", string(kind), "pid", p.PID)
			a := model.New(key, string(kind), sniffReason, p.PID, p.Name, d.now(), nil, model.OriginSniffed)
			d.acquireLocked(a)
		}
		d.mu.Unlock()
	}
}

func (d *Daemon) sniffingLocked() bool {
	s := d.settings
	return !d.closing && d.blocking && s.ProcessSniffingEnabled && s.AutoAcquireForKnownAgents
}

// --- "While the lid was closed" ---

func (d *Daemon) beginAwayTrackingLocked() {
	snapshot := d.registry.Snapshot()
	held := make([]policy.HeldAgent, 0, len(snapshot))
	for _, a := range snapshot {
		held = append(held, policy.HeldAgent{
			Key:         a.Key,
			Tool:        a.Tool,
			DisplayName: agents.Kind(a.Tool).DisplayName(),
			AcquiredAt:  a.AcquiredAt,
		})
	}
	d.away = awayTracking{active: true, closedAt: d.now(), held: held, releasedAt: map[string]time.Time{}}
	if t, ok := d.thermal.Last(); ok && finite(t) {
		d.away.peak = &t
	}
}

// recordAwayReleasesLocked stamps the release time of every lid-close-held assertion that is gone,
// so the summary reports real run durations rather than "until the lid opened".
func (d *Daemon) recordAwayReleasesLocked(snapshot []model.Assertion) {
	if !d.away.active {
		return
	}
	live := make(map[string]bool, len(snapshot))
	for _, a := range snapshot {
		live[a.Key] = true
	}
	now := d.now()
	for _, h := range d.away.held {
		if _, done := d.away.releasedAt[h.Key]; !done && !live[h.Key] {
			d.away.releasedAt[h.Key] = now
		}
	}
}

func (d *Daemon) finishAwayTrackingLocked() {
	away := d.away
	d.away = awayTracking{}
	if !away.active || len(away.held) == 0 {
		return
	}
	active := map[string]bool{}
	for _, a := range d.registry.Snapshot() {
		active[a.Key] = true
	}
	summary := policy.BuildAwaySummary(policy.AwayPeriod{
		HeldAtClose:      away.held,
		ActiveKeys:       active,
		ReleasedAt:       away.releasedAt,
		ClosedAt:         away.closedAt,
		OpenedAt:         d.now(),
		PeakTemperature:  away.peak,
		ThermalCutout:    away.thermal,
		LowBatteryCutout: away.lowBattery,
	})
	if summary != nil {
		d.awaySummary = summary
		d.log.Info("away summary", "finished", len(summary.Finished), "stillActive", len(summary.StillActive))
	}
}

// --- helpers ---

// sniffedPID is the PID encoded in a "sniffed:<tool>:<pid>" key.
func sniffedPID(key string) (int, bool) {
	if !strings.HasPrefix(key, policy.SniffedKeyPrefix) {
		return 0, false
	}
	pid, err := strconv.Atoi(key[strings.LastIndex(key, ":")+1:])
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

// sameIdleInputs reports whether two versions of an assertion agree on every field the idle
// evaluator reads. A re-acquire advances LastActivityAt and can adopt a PID or an expiry; a release
// and a fresh acquire under the same key restamp AcquiredAt.
func sameIdleInputs(a, b model.Assertion) bool {
	sameExpiry := a.ExpiresAt == nil && b.ExpiresAt == nil ||
		a.ExpiresAt != nil && b.ExpiresAt != nil && a.ExpiresAt.Equal(*b.ExpiresAt)
	return a.Key == b.Key && a.PID == b.PID && a.Origin == b.Origin && sameExpiry &&
		a.AcquiredAt.Equal(b.AcquiredAt) && a.LastActivityAt.Equal(b.LastActivityAt)
}

func causeNames(causes []policy.CutoutCause) string {
	names := make([]string, len(causes))
	for i, c := range causes {
		names[i] = string(c)
	}
	return strings.Join(names, ",")
}

// truncateRunes cuts s to at most n code points.
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos]
		}
		i++
	}
	return s
}

// finite reports whether a reading is a real number. A sensor that returned NaN or ±Inf has given
// no reading, and such a value would make the status impossible to encode as JSON.
func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

// secondsToDuration converts seconds to a Duration, saturating instead of overflowing; NaN and
// negative values are zero.
func secondsToDuration(s float64) time.Duration {
	if math.IsNaN(s) || s <= 0 {
		return 0
	}
	ns := s * float64(time.Second)
	if ns >= math.MaxInt64 {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(ns)
}
