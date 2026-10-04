package daemon

import (
	"context"
	"errors"
	"math"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nikitaShakhbazyan/lidwake-go/internal/activity"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/chime"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/darwin"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/ipc"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/model"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/paths"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/policy"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/settings"
)

// lastIndex is the position of the last journal entry equal to s, or -1.
func lastIndex(entries []string, s string) int {
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i] == s {
			return i
		}
	}
	return -1
}

// cueBeforeUnblock checks that cue was journaled before the final unblock.
func cueBeforeUnblock(t *testing.T, j *journal, cue string) {
	t.Helper()
	entries := j.all()
	c, u := lastIndex(entries, cue), lastIndex(entries, "set(false)")
	if c < 0 || u < 0 || c > u {
		t.Fatalf("want %q before the last set(false); journal = %v", cue, entries)
	}
}

func TestBlockingDrivesTheHelper(t *testing.T) {
	t.Run("acquire and release push the block to the helper in order", func(t *testing.T) {
		h := newHarness(t)
		h.start()
		h.waitHelper(false) // the initial sync

		resp := h.acquire("claude-code:s1")
		if !resp.OK || resp.Blocking == nil || !*resp.Blocking || *resp.AssertionCount != 1 {
			t.Fatalf("acquire = %+v", resp)
		}
		if resp.DisplayApplied == nil || *resp.DisplayApplied {
			t.Fatalf("displayApplied = %v, want an explicit false", resp.DisplayApplied)
		}
		h.waitHelper(true)
		h.acquire("claude-code:s2")
		resp = h.send(ipc.Request{Op: ipc.OpRelease, Key: "claude-code:s1", Tool: "claude-code"})
		if !resp.OK || !*resp.Blocking || *resp.AssertionCount != 1 || resp.Warning != "" {
			t.Fatalf("release = %+v", resp)
		}
		stays(t, "no helper call while still blocking", 30*time.Millisecond, func() bool {
			return slices.Equal(h.helper.allSets(), []bool{false, true})
		})
		resp = h.send(ipc.Request{Op: ipc.OpRelease, Key: "claude-code:s2", Tool: "claude-code"})
		if !resp.OK || *resp.Blocking || *resp.AssertionCount != 0 {
			t.Fatalf("last release = %+v", resp)
		}
		h.waitHelper(false)
		if got := h.helper.allSets(); !slices.Equal(got, []bool{false, true, false}) {
			t.Fatalf("helper calls = %v", got)
		}
		want := []model.Event{model.EventAcquired, model.EventAcquired, model.EventReleased, model.EventReleased}
		if got := h.events(); !slices.Equal(got, want) {
			t.Fatalf("events = %v, want %v", got, want)
		}
		if n := len(h.j.all()); slices.ContainsFunc(h.j.all(), func(e string) bool { return strings.HasPrefix(e, "cue:") }) {
			t.Fatalf("a cue played with the lid open (journal of %d: %v)", n, h.j.all())
		}
	})

	t.Run("the last release with the lid closed plays the cue before the unblock", func(t *testing.T) {
		h := newHarness(t)
		h.lid.set(true)
		h.start()
		h.acquire("claude-code:s1")
		h.waitHelper(true)
		h.send(ipc.Request{Op: ipc.OpRelease, Key: "claude-code:s1"})
		h.waitHelper(false)
		cueBeforeUnblock(t, h.j, "cue:default:sleepWorkComplete")
	})

	t.Run("a force release with the lid closed plays the user-action cue", func(t *testing.T) {
		h := newHarness(t)
		h.lid.set(true)
		h.start()
		h.acquire("claude-code:s1")
		h.waitHelper(true)
		resp := h.send(ipc.Request{Op: ipc.OpReleaseAll})
		if !resp.OK || *resp.ReleasedCount != 1 {
			t.Fatalf("releaseAll = %+v", resp)
		}
		// releaseAll waits for the helper, so the unblock is in place when it answers.
		if got, _ := h.helper.lastSet(); got {
			t.Fatal("releaseAll answered before the unblock")
		}
		cueBeforeUnblock(t, h.j, "cue:default:sleepUserAction")
	})

	t.Run("a per-cause sound setting picks the cue", func(t *testing.T) {
		h := newHarness(t)
		h.writeSettings(func(s *settings.Settings) { s.SleepChimeWorkComplete = "Glass" })
		h.lid.set(true)
		h.start()
		h.acquire("claude-code:s1")
		h.waitHelper(true)
		h.send(ipc.Request{Op: ipc.OpRelease, Key: "claude-code:s1"})
		h.waitHelper(false)
		cueBeforeUnblock(t, h.j, "cue:Glass:")
	})

	t.Run("the reconcile re-pushes the block while blocking only", func(t *testing.T) {
		h := newHarness(t)
		h.cfg.ReconcileInterval = 5 * time.Millisecond
		h.start()
		h.acquire("claude-code:s1")
		h.waitHelper(true)
		n := h.helper.calls()
		eventually(t, "re-pushes", func() bool { return h.helper.calls() >= n+3 })
		for _, b := range h.helper.allSets()[1:] {
			if !b {
				t.Fatal("the reconcile pushed an unblock while blocking")
			}
		}
		h.send(ipc.Request{Op: ipc.OpRelease, Key: "claude-code:s1"})
		h.waitHelper(false)
		time.Sleep(20 * time.Millisecond)
		n = h.helper.calls()
		stays(t, "no re-push while idle", 50*time.Millisecond, func() bool { return h.helper.calls() == n })
	})

	t.Run("a helper that restarts without the block gets it back within the probe interval", func(t *testing.T) {
		h := newHarness(t)
		h.cfg.HelperProbeInterval = 5 * time.Millisecond
		h.start()
		h.acquire("claude-code:s1")
		h.waitHelper(true)
		n := h.helper.calls()
		h.helper.restart()
		eventually(t, "the block re-applied", func() bool {
			held, _ := h.helper.State()
			return held && h.helper.calls() > n
		})
		h.send(ipc.Request{Op: ipc.OpRelease, Key: "claude-code:s1"})
		h.waitHelper(false)
		time.Sleep(20 * time.Millisecond)
		n = h.helper.calls()
		stays(t, "no probe-driven sets while idle", 50*time.Millisecond, func() bool { return h.helper.calls() == n })
	})

	t.Run("a failed apply is retried and shown as a warning while blocking", func(t *testing.T) {
		h := newHarness(t)
		h.start()
		h.helper.fail(errors.New("helper unreachable"))
		h.acquire("claude-code:s1")
		eventually(t, "the failed apply", func() bool { return h.d.driver.LastApplyFailed() })
		if !slices.Contains(h.status().Warnings, "The sleep block couldn't be fully applied — your Mac may still sleep when the lid closes. Retrying.") {
			t.Fatalf("warnings = %v", h.status().Warnings)
		}
		h.helper.fail(nil)
		eventually(t, "the retry that takes", func() bool { return !h.d.driver.LastApplyFailed() })
		h.waitHelper(true)
		if len(h.status().Warnings) != 0 {
			t.Fatalf("warnings after recovery = %v", h.status().Warnings)
		}
	})

	t.Run("a wake re-pushes the block and relights a display hold", func(t *testing.T) {
		h := newHarness(t)
		h.start()
		h.send(ipc.Request{Op: ipc.OpAcquire, Key: "claude-code:s1", Tool: "claude-code", Display: true})
		h.waitHelper(true)
		eventually(t, "the display hold", func() bool { return h.j.count("display:raise") == 1 })
		calls, wakes := h.helper.calls(), h.j.count("display:wake")
		h.d.onWake()
		eventually(t, "the re-push", func() bool { return h.helper.calls() > calls })
		h.waitHelper(true)
		if h.j.count("display:wake") <= wakes {
			t.Fatal("the display was not relit on wake")
		}
		if h.j.count("display:raise") != 1 {
			t.Fatal("the display assertion was raised twice")
		}
	})
}

func TestDisplayHold(t *testing.T) {
	t.Run("a display-class assertion raises the display hold and every release path drops it", func(t *testing.T) {
		h := newHarness(t)
		h.start()
		resp := h.send(ipc.Request{Op: ipc.OpAcquire, Key: "claude-code:s1", Tool: "claude-code", Display: true})
		if resp.DisplayApplied == nil || !*resp.DisplayApplied {
			t.Fatalf("displayApplied = %v", resp.DisplayApplied)
		}
		eventually(t, "raise", func() bool { return h.j.count("display:raise") == 1 })
		h.send(ipc.Request{Op: ipc.OpPause})
		eventually(t, "drop on pause", func() bool { return h.j.count("display:drop") == 1 })
	})
}

func TestLidClose(t *testing.T) {
	t.Run("a lid close over a working agent chimes, locks and tracks the away summary", func(t *testing.T) {
		h := newHarness(t)
		h.start()
		h.acquire("claude-code:s1")
		h.waitHelper(true)
		h.waitLid(true)
		eventually(t, "the chime", func() bool { return h.j.count("chime:default") == 1 })
		eventually(t, "the lock", func() bool { return h.locks.count("lock") == 1 })

		h.clock.Advance(10 * time.Minute)
		h.send(ipc.Request{Op: ipc.OpRelease, Key: "claude-code:s1"})
		h.waitHelper(false)
		cueBeforeUnblock(t, h.j, "cue:default:sleepWorkComplete")
		h.clock.Advance(5 * time.Minute)
		h.waitLid(false)

		s := h.status().AwaySummary
		if s == nil {
			t.Fatal("no away summary")
		}
		if !s.ClosedAt.Equal(harnessEpoch) || !s.OpenedAt.Equal(harnessEpoch.Add(15*time.Minute)) {
			t.Fatalf("away period %v → %v", s.ClosedAt, s.OpenedAt)
		}
		if len(s.Finished) != 1 || len(s.StillActive) != 0 {
			t.Fatalf("summary = %+v", s)
		}
		f := s.Finished[0]
		if f.Key != "claude-code:s1" || f.DisplayName != "Claude Code" || f.Duration != 10*time.Minute {
			t.Fatalf("finished = %+v (the duration runs to the release, not the lid open)", f)
		}
		if s.ThermalCutout || s.LowBatteryCutout {
			t.Fatalf("no cutout fired: %+v", s)
		}
	})

	t.Run("an agent still working at lid open is reported as still active, with the peak temperature", func(t *testing.T) {
		h := newHarness(t)
		h.start()
		h.acquire("claude-code:s1")
		h.waitHelper(true)
		h.waitLid(true)
		h.sensor.set(66)
		eventually(t, "a reading of 66", func() bool { c, ok := h.d.thermal.Last(); return ok && c == 66 })
		h.sensor.set(55)
		eventually(t, "a reading of 55", func() bool { c, ok := h.d.thermal.Last(); return ok && c == 55 })
		h.clock.Advance(time.Hour)
		h.waitLid(false)
		s := h.status().AwaySummary
		if s == nil || len(s.StillActive) != 1 || len(s.Finished) != 0 {
			t.Fatalf("summary = %+v", s)
		}
		if s.StillActive[0].Duration != time.Hour {
			t.Fatalf("duration = %v", s.StillActive[0].Duration)
		}
		if s.PeakTemperature == nil || *s.PeakTemperature != 66 {
			t.Fatalf("peak = %v, want 66", s.PeakTemperature)
		}
	})

	t.Run("a lid close with nothing held does nothing", func(t *testing.T) {
		h := newHarness(t)
		h.start()
		h.waitLid(true)
		h.waitLid(false)
		time.Sleep(10 * time.Millisecond)
		if h.j.count("chime:default") != 0 || h.locks.count("lock") != 0 {
			t.Fatalf("journal = %v, locks = %v", h.j.all(), h.locks.all())
		}
		if h.status().AwaySummary != nil {
			t.Fatal("an away summary with nothing held")
		}
	})

	t.Run("the chime and the lock follow their settings", func(t *testing.T) {
		h := newHarness(t)
		h.writeSettings(func(s *settings.Settings) {
			s.SoundOnLidClose = false
			s.LockOnLidClose = false
		})
		h.start()
		h.acquire("claude-code:s1")
		h.waitHelper(true)
		h.waitLid(true)
		h.waitLid(false)
		time.Sleep(10 * time.Millisecond)
		if h.j.count("chime:default") != 0 || h.locks.count("lock") != 0 {
			t.Fatalf("journal = %v, locks = %v", h.j.all(), h.locks.all())
		}
		if h.status().AwaySummary == nil {
			t.Fatal("away tracking must begin whatever the chime and lock settings")
		}
	})

	t.Run("lid events are logged", func(t *testing.T) {
		h := newHarness(t)
		h.start()
		h.waitLid(true)
		h.waitLid(false)
		if got := h.events(); !slices.Equal(got, []model.Event{model.EventLidClosed, model.EventLidOpened}) {
			t.Fatalf("events = %v", got)
		}
		if st := h.status(); st.LastEvent != model.EventLidOpened {
			t.Fatalf("last event = %q", st.LastEvent)
		}
	})
}

func TestPauseResume(t *testing.T) {
	t.Run("pause releases everything and refuses acquires until resumed", func(t *testing.T) {
		h := newHarness(t)
		h.start()
		h.acquire("claude-code:a")
		h.acquire("claude-code:b")
		h.waitHelper(true)

		resp := h.send(ipc.Request{Op: ipc.OpPause})
		if !resp.OK || *resp.Blocking || *resp.AssertionCount != 0 || *resp.ReleasedCount != 2 {
			t.Fatalf("pause = %+v", resp)
		}
		if got, _ := h.helper.lastSet(); got {
			t.Fatal("pause answered before the unblock")
		}
		st := h.status()
		if !st.Paused || st.Blocking || len(st.Assertions) != 0 {
			t.Fatalf("status = %+v", st)
		}
		if ps := h.readState(); !ps.Paused || len(ps.Assertions) != 0 {
			t.Fatalf("persisted = %+v", ps)
		}
		if !h.hasEvent(model.EventPaused) {
			t.Fatalf("events = %v", h.events())
		}

		resp = h.acquire("claude-code:c")
		if !resp.OK || resp.Warning != "lidwake is paused — acquire ignored" || *resp.Blocking || *resp.AssertionCount != 0 {
			t.Fatalf("acquire while paused = %+v", resp)
		}
		resp = h.send(ipc.Request{Op: ipc.OpHold, Reason: "deploy"})
		if resp.OK || resp.Error != "lidwake is paused — resume it to place a hold." {
			t.Fatalf("hold while paused = %+v", resp)
		}
		resp = h.send(ipc.Request{Op: ipc.OpPause})
		if !resp.OK || *resp.ReleasedCount != 0 {
			t.Fatalf("second pause = %+v", resp)
		}

		resp = h.send(ipc.Request{Op: ipc.OpResume})
		if !resp.OK || resp.Blocking != nil || resp.ReleasedCount != nil || *resp.AssertionCount != 0 {
			t.Fatalf("resume = %+v", resp)
		}
		if ps := h.readState(); ps.Paused {
			t.Fatal("resume not persisted")
		}
		if !h.hasEvent(model.EventResumed) {
			t.Fatalf("events = %v", h.events())
		}
		if resp := h.acquire("claude-code:c"); !resp.OK || !*resp.Blocking {
			t.Fatalf("acquire after resume = %+v", resp)
		}
		h.waitHelper(true)
	})

	t.Run("a paused state survives a restart", func(t *testing.T) {
		h := newHarness(t)
		h.writeState(model.PersistedState{Paused: true})
		h.start()
		if !h.status().Paused {
			t.Fatal("paused bit not restored")
		}
		if resp := h.acquire("claude-code:s1"); resp.Warning != "lidwake is paused — acquire ignored" {
			t.Fatalf("acquire = %+v", resp)
		}
	})
}

func TestOffTimer(t *testing.T) {
	t.Run("setting a timer arms it and persists the deadline", func(t *testing.T) {
		h := newHarness(t)
		h.start()
		resp := h.send(ipc.Request{Op: ipc.OpTimer, TTL: ipc.Ptr(3600.0)})
		if !resp.OK || resp.AppliedTTL == nil || *resp.AppliedTTL != 3600 {
			t.Fatalf("timer = %+v", resp)
		}
		e, ok := h.timers.live()
		if !ok || e.delay != time.Hour {
			t.Fatalf("scheduled = %+v", e)
		}
		want := harnessEpoch.Add(time.Hour)
		if ps := h.readState(); ps.OffAt == nil || !ps.OffAt.Equal(want) {
			t.Fatalf("persisted offAt = %v", ps.OffAt)
		}
		if st := h.status(); st.OffAt == nil || !st.OffAt.Equal(want) {
			t.Fatalf("status offAt = %v", st.OffAt)
		}
	})

	t.Run("a due timer pauses lidwake", func(t *testing.T) {
		h := newHarness(t)
		h.start()
		h.acquire("claude-code:s1")
		h.waitHelper(true)
		h.send(ipc.Request{Op: ipc.OpTimer, TTL: ipc.Ptr(3600.0)})
		h.clock.Advance(time.Hour)
		h.timers.fire(t)
		st := h.status()
		if !st.Paused || st.OffAt != nil || st.Blocking {
			t.Fatalf("status = %+v", st)
		}
		h.waitHelper(false)
		if !h.hasEvent(model.EventOffTimer) {
			t.Fatalf("events = %v", h.events())
		}
		if ps := h.readState(); !ps.Paused || ps.OffAt != nil {
			t.Fatalf("persisted = %+v", ps)
		}
	})

	t.Run("a timer that fires before the wall-clock deadline re-arms", func(t *testing.T) {
		h := newHarness(t)
		h.start()
		h.send(ipc.Request{Op: ipc.OpTimer, TTL: ipc.Ptr(3600.0)})
		h.clock.Advance(30 * time.Minute)
		h.timers.fire(t)
		if h.status().Paused {
			t.Fatal("paused before the deadline")
		}
		if e, ok := h.timers.live(); !ok || e.delay != 30*time.Minute {
			t.Fatalf("re-armed = %+v", e)
		}
	})

	t.Run("an overdue deadline at start fires at once", func(t *testing.T) {
		h := newHarness(t)
		past := harnessEpoch.Add(-time.Minute)
		h.writeState(model.PersistedState{OffAt: &past})
		h.start()
		e, ok := h.timers.live()
		if !ok || e.delay != 0 {
			t.Fatalf("scheduled = %+v, want an immediate fire", e)
		}
		h.timers.fire(t)
		if !h.status().Paused {
			t.Fatal("an overdue timer did not pause")
		}
	})

	t.Run("a deadline that passed in sleep fires at once on wake", func(t *testing.T) {
		h := newHarness(t)
		h.start()
		h.send(ipc.Request{Op: ipc.OpTimer, TTL: ipc.Ptr(60.0)})
		h.clock.Advance(2 * time.Hour)
		h.d.onWake()
		e, ok := h.timers.live()
		if !ok || e.delay != 0 {
			t.Fatalf("scheduled = %+v, want an immediate fire", e)
		}
		h.timers.fire(t)
		if !h.status().Paused {
			t.Fatal("not paused after wake")
		}
	})

	t.Run("setting a timer resumes a paused lidwake", func(t *testing.T) {
		h := newHarness(t)
		h.start()
		h.send(ipc.Request{Op: ipc.OpPause})
		h.send(ipc.Request{Op: ipc.OpTimer, TTL: ipc.Ptr(900.0)})
		if st := h.status(); st.Paused || st.OffAt == nil {
			t.Fatalf("status = %+v", st)
		}
	})

	t.Run("pause and a missing ttl cancel the timer", func(t *testing.T) {
		h := newHarness(t)
		h.start()
		h.send(ipc.Request{Op: ipc.OpTimer, TTL: ipc.Ptr(900.0)})
		h.send(ipc.Request{Op: ipc.OpPause})
		if _, ok := h.timers.live(); ok {
			t.Fatal("pause left the timer armed")
		}
		if st := h.status(); st.OffAt != nil {
			t.Fatalf("offAt = %v", st.OffAt)
		}
		h.send(ipc.Request{Op: ipc.OpResume})
		h.send(ipc.Request{Op: ipc.OpTimer, TTL: ipc.Ptr(900.0)})
		resp := h.send(ipc.Request{Op: ipc.OpTimer})
		if !resp.OK || resp.AppliedTTL != nil {
			t.Fatalf("cancel = %+v", resp)
		}
		if _, ok := h.timers.live(); ok {
			t.Fatal("cancel left the timer armed")
		}
		if ps := h.readState(); ps.OffAt != nil {
			t.Fatal("cancel not persisted")
		}
	})

	t.Run("the timer is clamped to a minute", func(t *testing.T) {
		h := newHarness(t)
		h.start()
		resp := h.send(ipc.Request{Op: ipc.OpTimer, TTL: ipc.Ptr(5.0)})
		if resp.AppliedTTL == nil || *resp.AppliedTTL != 60 {
			t.Fatalf("applied = %v", resp.AppliedTTL)
		}
	})
}

func TestCutouts(t *testing.T) {
	t.Run("a thermal cutout releases everything and latches until the Mac cools", func(t *testing.T) {
		h := newHarness(t)
		h.start()
		h.sensor.set(90)
		h.acquire("claude-code:s1")
		eventually(t, "the cutout", func() bool { return h.hasEvent(model.EventThermalCutout) })
		h.waitHelper(false)
		st := h.status()
		msg := "The thermal cutout fired — acquires are paused until the Mac cools down."
		if st.Blocking || !slices.Equal(st.ActiveCutouts, []string{"thermal"}) || !slices.Contains(st.Warnings, msg) {
			t.Fatalf("status = %+v", st)
		}
		resp := h.acquire("claude-code:s2")
		if resp.OK || resp.Error != msg || *resp.Blocking {
			t.Fatalf("acquire while latched = %+v", resp)
		}
		if resp := h.send(ipc.Request{Op: ipc.OpHold}); resp.OK || resp.Error != msg {
			t.Fatalf("hold while latched = %+v", resp)
		}
		// Within the hysteresis band the latch holds.
		h.sensor.set(78)
		eventually(t, "a recheck at 78", func() bool { c, _ := h.d.thermal.Last(); return c == 78 })
		stays(t, "the latch while still warm", 30*time.Millisecond, func() bool { return len(h.status().ActiveCutouts) == 1 })
		h.sensor.set(70)
		eventually(t, "the latch to clear", func() bool { return len(h.status().ActiveCutouts) == 0 })
		if resp := h.acquire("claude-code:s2"); !resp.OK {
			t.Fatalf("acquire after recovery = %+v", resp)
		}
	})

	t.Run("a repeated cutout report is logged once", func(t *testing.T) {
		h := newHarness(t)
		// Hot throughout, so the latch cannot clear between the reports: a report after it
		// cleared would be a new cutout, logged again.
		h.sensor.set(90)
		h.start()
		h.acquire("claude-code:s1")
		eventually(t, "the cutout", func() bool { return h.hasEvent(model.EventThermalCutout) })
		h.d.onThermalCutout()
		h.d.onThermalCutout()
		stays(t, "a single thermalCutout event", 30*time.Millisecond, func() bool {
			return h.countEvents(model.EventThermalCutout) == 1
		})
	})

	t.Run("a low-battery cutout clears once the Mac is plugged in", func(t *testing.T) {
		h := newHarness(t)
		h.battery.set(15, true)
		h.start()
		h.acquire("claude-code:s1")
		eventually(t, "the cutout", func() bool { return h.hasEvent(model.EventLowBatteryCutout) })
		resp := h.acquire("claude-code:s2")
		if resp.Error != "The low-battery cutout fired — acquires are paused until charging resumes." {
			t.Fatalf("acquire while latched = %+v", resp)
		}
		h.battery.set(15, false)
		eventually(t, "the latch to clear", func() bool { return len(h.status().ActiveCutouts) == 0 })
	})

	t.Run("AC-only mode cuts out on battery power", func(t *testing.T) {
		h := newHarness(t)
		h.writeSettings(func(s *settings.Settings) { s.RequireACPower = true })
		h.battery.set(95, true)
		h.start()
		h.acquire("claude-code:s1")
		eventually(t, "the cutout", func() bool { return h.hasEvent(model.EventACPowerCutout) })
		st := h.status()
		if !slices.Equal(st.ActiveCutouts, []string{"onBattery"}) {
			t.Fatalf("cutouts = %v", st.ActiveCutouts)
		}
		resp := h.acquire("claude-code:s2")
		if resp.Error != "AC-only mode is on and the Mac is on battery — acquires are paused until it is plugged in." {
			t.Fatalf("acquire while latched = %+v", resp)
		}
		h.battery.set(95, false)
		eventually(t, "the latch to clear", func() bool { return len(h.status().ActiveCutouts) == 0 })
	})

	t.Run("a settings reload drops the latch of a disabled cutout", func(t *testing.T) {
		h := newHarness(t)
		h.start()
		h.sensor.set(90)
		h.acquire("claude-code:s1")
		eventually(t, "the cutout", func() bool { return h.hasEvent(model.EventThermalCutout) })
		h.writeSettings(func(s *settings.Settings) { s.ThermalCutoutEnabled = false })
		if resp := h.send(ipc.Request{Op: ipc.OpReloadSettings}); !resp.OK {
			t.Fatalf("reload = %+v", resp)
		}
		if st := h.status(); len(st.ActiveCutouts) != 0 || st.Settings.ThermalCutoutEnabled {
			t.Fatalf("status = %+v", st)
		}
		if resp := h.acquire("claude-code:s2"); !resp.OK {
			t.Fatalf("acquire = %+v", resp)
		}
		stays(t, "no cutout while disabled", 30*time.Millisecond, func() bool { return h.countEvents(model.EventThermalCutout) == 1 })
	})

	t.Run("opening the lid clears a latch that only guards a closed lid", func(t *testing.T) {
		h := newHarness(t)
		h.writeSettings(func(s *settings.Settings) { s.SafetyCutoutsWithLidOpen = false })
		h.lid.set(true)
		h.start()
		h.acquire("claude-code:s1")
		// Blocked first: a cutout racing the very first apply coalesces with it, and a Mac that
		// was never blocked gets no cue.
		h.waitHelper(true)
		h.sensor.set(90)
		eventually(t, "the cutout", func() bool { return h.hasEvent(model.EventThermalCutout) })
		h.waitHelper(false)
		cueBeforeUnblock(t, h.j, "cue:default:sleepSafetyCutout")
		resp := h.acquire("claude-code:s2")
		if resp.Error != "The thermal cutout fired — acquires are paused until the Mac cools down or the lid opens." {
			t.Fatalf("acquire while latched = %+v", resp)
		}
		h.waitLid(false)
		if st := h.status(); len(st.ActiveCutouts) != 0 {
			t.Fatalf("cutouts after lid open = %v", st.ActiveCutouts)
		}
	})

	t.Run("cutouts that only guard a closed lid do not fire with the lid open", func(t *testing.T) {
		h := newHarness(t)
		h.writeSettings(func(s *settings.Settings) { s.SafetyCutoutsWithLidOpen = false })
		h.sensor.set(90)
		h.start()
		h.acquire("claude-code:s1")
		eventually(t, "a reading", func() bool { c, _ := h.d.thermal.Last(); return c == 90 })
		stays(t, "no cutout", 30*time.Millisecond, func() bool { return !h.hasEvent(model.EventThermalCutout) })
	})

	t.Run("the first reading over restored holds already sees the armed lid", func(t *testing.T) {
		h := newHarness(t)
		// Only the reading taken as blocking begins can fire within the test.
		h.cfg.Thermal.Interval = time.Hour
		h.writeState(model.PersistedState{Assertions: []model.Assertion{
			model.New("claude-code:s1", "claude-code", "", -1, "claude-code", harnessEpoch.Add(-time.Minute), nil, model.OriginHook),
		}})
		h.sensor.set(90)
		h.start()
		eventually(t, "the cutout from the first reading", func() bool { return h.hasEvent(model.EventThermalCutout) })
	})

	t.Run("a cutout over an empty registry latches, is logged, and a repeat is not", func(t *testing.T) {
		h := newHarness(t)
		// Still hot, so no re-evaluation can clear the latch before the status reads it.
		h.sensor.set(90)
		h.start()
		h.d.onThermalCutout()
		h.d.onThermalCutout()
		if got := h.countEvents(model.EventThermalCutout); got != 1 {
			t.Fatalf("thermalCutout logged %d times, want once", got)
		}
		if st := h.status(); !slices.Equal(st.ActiveCutouts, []string{"thermal"}) {
			t.Fatalf("cutouts = %v", st.ActiveCutouts)
		}
	})

	t.Run("a cutout during an away period is reported in the summary", func(t *testing.T) {
		h := newHarness(t)
		h.start()
		h.acquire("claude-code:s1")
		h.waitHelper(true)
		h.waitLid(true)
		h.sensor.set(90)
		eventually(t, "the cutout", func() bool { return h.hasEvent(model.EventThermalCutout) })
		h.waitLid(false)
		s := h.status().AwaySummary
		if s == nil || !s.ThermalCutout || len(s.Finished) != 1 || s.PeakTemperature == nil || *s.PeakTemperature != 90 {
			t.Fatalf("summary = %+v", s)
		}
	})
}

func TestStatus(t *testing.T) {
	t.Run("reports every field", func(t *testing.T) {
		h := newHarness(t)
		h.writeSettings(func(s *settings.Settings) { s.ManualHoldMaxHours = 2 })
		h.lid.set(true)
		h.battery.set(55, true)
		h.sensor.set(61.5)
		h.sleepDisabled = true
		h.display.only = true
		h.start()
		resp := h.send(ipc.Request{Op: ipc.OpAcquire, Key: "claude-code:s1", Tool: "claude-code", Display: true})
		if !resp.OK {
			t.Fatalf("acquire = %+v", resp)
		}
		h.waitHelper(true)
		h.send(ipc.Request{Op: ipc.OpTimer, TTL: ipc.Ptr(600.0)})
		eventually(t, "a reading", func() bool { c, ok := h.d.thermal.Last(); return ok && c == 61.5 })
		h.helper.fail(errors.New("helper unreachable"))
		h.d.onWake()
		eventually(t, "the failed apply", func() bool { return h.d.driver.LastApplyFailed() })

		st := h.status()
		if st.Paused || !st.Blocking || joinKeys(st.Assertions) != "claude-code:s1" || !st.Assertions[0].HoldsDisplay {
			t.Fatalf("assertions: %+v", st)
		}
		if !st.LidClosed || !st.HelperConnected || !st.SleepDisabled || st.ThermalState != 1 {
			t.Fatalf("machine state: %+v", st)
		}
		if st.CPUTemperature == nil || *st.CPUTemperature != 61.5 {
			t.Fatalf("temperature = %v", st.CPUTemperature)
		}
		if st.BatteryPercent == nil || *st.BatteryPercent != 55 || st.OnBattery == nil || !*st.OnBattery {
			t.Fatalf("battery = %v %v", st.BatteryPercent, st.OnBattery)
		}
		if st.ActiveCutouts == nil || len(st.ActiveCutouts) != 0 {
			t.Fatalf("cutouts = %#v, want an empty list", st.ActiveCutouts)
		}
		if st.OffAt == nil || !st.OffAt.Equal(harnessEpoch.Add(10*time.Minute)) {
			t.Fatalf("offAt = %v", st.OffAt)
		}
		if st.LastEvent != model.EventAcquired || st.LastEventAt == nil || !st.LastEventAt.Equal(harnessEpoch) {
			t.Fatalf("last event = %q at %v", st.LastEvent, st.LastEventAt)
		}
		wantWarnings := []string{
			"The sleep block couldn't be fully applied — your Mac may still sleep when the lid closes. Retrying.",
			"A display hold is active but the lid is closed with no other display — the agent behind it can't see the screen.",
		}
		if !slices.Equal(st.Warnings, wantWarnings) {
			t.Fatalf("warnings = %q", st.Warnings)
		}
		if st.AwaySummary != nil || st.Settings.ManualHoldMaxHours != 2 || st.Version != paths.Version {
			t.Fatalf("summary/settings/version: %+v", st)
		}
	})

	t.Run("an unreadable temperature warns while the thermal cutout is on", func(t *testing.T) {
		h := newHarness(t)
		h.sensor.broken = true
		h.start()
		if st := h.status(); st.CPUTemperature != nil || len(st.Warnings) != 0 {
			t.Fatalf("idle status = %+v (no warning with nothing held)", st)
		}
		h.acquire("claude-code:s1")
		st := h.status()
		if st.CPUTemperature != nil || !slices.Contains(st.Warnings, "CPU temperature is unreadable, so the thermal cutout can't trigger.") {
			t.Fatalf("status = %+v", st)
		}
		h.writeSettings(func(s *settings.Settings) { s.ThermalCutoutEnabled = false })
		h.send(ipc.Request{Op: ipc.OpReloadSettings})
		if st := h.status(); len(st.Warnings) != 0 {
			t.Fatalf("warnings with the cutout off = %v", st.Warnings)
		}
	})

	t.Run("a non-finite temperature reads as unreadable and never reaches the summary", func(t *testing.T) {
		h := newHarness(t)
		h.sensor.set(math.NaN())
		h.start()
		h.acquire("claude-code:s1")
		h.waitHelper(true)
		eventually(t, "a NaN reading", func() bool { c, ok := h.d.thermal.Last(); return ok && math.IsNaN(c) })
		st := h.status()
		if st.CPUTemperature != nil || !slices.Contains(st.Warnings, "CPU temperature is unreadable, so the thermal cutout can't trigger.") {
			t.Fatalf("status = %+v", st)
		}
		h.waitLid(true)
		h.waitLid(false)
		s := h.status().AwaySummary
		if s == nil || s.PeakTemperature != nil {
			t.Fatalf("summary = %+v", s)
		}
	})

	t.Run("a Mac without a battery reports no battery", func(t *testing.T) {
		h := newHarness(t)
		h.cfg.Capabilities = func() darwin.DeviceCapabilities { return darwin.DeviceCapabilities{} }
		h.battery.none()
		h.start()
		if st := h.status(); st.BatteryPercent != nil || st.OnBattery != nil {
			t.Fatalf("battery = %v %v", st.BatteryPercent, st.OnBattery)
		}
	})

	t.Run("an idle status has empty lists and no last event", func(t *testing.T) {
		h := newHarness(t)
		h.start()
		st := h.status()
		if st.Assertions == nil || st.Warnings == nil || st.ActiveCutouts == nil || st.LastEvent != "" || st.LastEventAt != nil || st.OffAt != nil {
			t.Fatalf("status = %#v", st)
		}
		if st.Settings != settings.Defaults() {
			t.Fatalf("settings = %+v, want the defaults with no config file", st.Settings)
		}
	})
}

func TestRestoreAndPersistence(t *testing.T) {
	t.Run("live assertions are restored, stale ones dropped and the state rewritten", func(t *testing.T) {
		h := newHarness(t)
		h.procs.set(4242, "/Users/me/.local/share/claude/versions/2.1.0")
		h.procs.set(5151, "/usr/libexec/mdworker")
		expires := harnessEpoch.Add(time.Hour)
		h.writeState(model.PersistedState{Assertions: []model.Assertion{
			model.New("claude-code:live", "claude-code", "", 4242, "claude", harnessEpoch.Add(-time.Hour), nil, model.OriginHook),
			model.New("claude-code:gone", "claude-code", "", 5555, "claude", harnessEpoch.Add(-time.Hour), nil, model.OriginHook),
			model.New("claude-code:recycled", "claude-code", "", 5151, "claude", harnessEpoch.Add(-time.Hour), nil, model.OriginHook),
			model.New("claude-code:preboot", "claude-code", "", -1, "claude", harnessEpoch.Add(-48*time.Hour), nil, model.OriginHook),
			{Key: "hold:abcd1234", Tool: "manual", PID: -1, ProcessName: "manual", AcquiredAt: harnessEpoch.Add(-2 * time.Hour),
				LastActivityAt: harnessEpoch.Add(-2 * time.Hour), ExpiresAt: &expires, Origin: model.OriginManual},
		}})
		h.start()
		if got := joinKeys(h.status().Assertions); got != "hold:abcd1234,claude-code:live" {
			t.Fatalf("restored = %s", got)
		}
		h.waitHelper(true)
		eventually(t, "the exit watch on the restored pid", func() bool { return h.exits.isWatched(4242) })
		if got := joinKeys(h.readState().Assertions); got != "hold:abcd1234,claude-code:live" {
			t.Fatalf("persisted after restore = %s", got)
		}
	})

	t.Run("restored holds arm the safety cutouts at once", func(t *testing.T) {
		h := newHarness(t)
		h.writeState(model.PersistedState{Assertions: []model.Assertion{
			model.New("claude-code:s1", "claude-code", "", -1, "claude-code", harnessEpoch.Add(-time.Minute), nil, model.OriginHook),
		}})
		h.sensor.set(90)
		h.start()
		eventually(t, "the cutout over a restored hold", func() bool { return h.hasEvent(model.EventThermalCutout) })
		h.waitHelper(false)
		if len(h.status().Assertions) != 0 {
			t.Fatal("the restored hold survived the cutout")
		}
	})

	t.Run("every change is persisted", func(t *testing.T) {
		h := newHarness(t)
		h.start()
		h.acquire("claude-code:s1")
		if got := joinKeys(h.readState().Assertions); got != "claude-code:s1" {
			t.Fatalf("after acquire = %s", got)
		}
		hold := h.send(ipc.Request{Op: ipc.OpHold})
		if got := joinKeys(h.readState().Assertions); got != "claude-code:s1,"+hold.HoldKey {
			t.Fatalf("after hold = %s", got)
		}
		h.send(ipc.Request{Op: ipc.OpRelease, Key: "claude-code:s1"})
		if got := joinKeys(h.readState().Assertions); got != hold.HoldKey {
			t.Fatalf("after release = %s", got)
		}
		h.send(ipc.Request{Op: ipc.OpPause})
		if st := h.readState(); !st.Paused || len(st.Assertions) != 0 {
			t.Fatalf("after pause = %+v", st)
		}
		info, err := os.Stat(filepath.Join(h.dir, "state.json"))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("state.json mode = %v, %v", info.Mode(), err)
		}
	})

	t.Run("a restart restores what the previous run persisted", func(t *testing.T) {
		h := newHarness(t)
		h.start()
		h.send(ipc.Request{Op: ipc.OpAcquire, Key: "claude-code:s1", Tool: "claude-code"})
		if err := h.stop(); err != nil {
			t.Fatal(err)
		}
		h2 := newHarness(t)
		h2.dir, h2.cfg.SupportDir, h2.cfg.Socket = h.dir, h.dir, h.cfg.Socket
		h2.start()
		if got := joinKeys(h2.status().Assertions); got != "claude-code:s1" {
			t.Fatalf("restored = %s", got)
		}
	})
}

func TestReleasePaths(t *testing.T) {
	t.Run("an exited agent's assertions are released", func(t *testing.T) {
		h := newHarness(t)
		h.lid.set(true)
		h.start()
		h.send(ipc.Request{Op: ipc.OpAcquire, Key: "claude-code:s1", Tool: "claude-code", PID: 4242})
		eventually(t, "the exit watch", func() bool { return h.exits.isWatched(4242) })
		h.waitHelper(true)
		h.exits.ch <- 4242
		h.waitHelper(false)
		cueBeforeUnblock(t, h.j, "cue:default:sleepWorkComplete")
		if len(h.status().Assertions) != 0 || !h.hasEvent(model.EventReleased) {
			t.Fatalf("status = %+v, events = %v", h.status(), h.events())
		}
	})

	t.Run("an idle batch of expiries plays the expired-hold cue", func(t *testing.T) {
		h := newHarness(t)
		h.lid.set(true)
		h.start()
		h.acquire("claude-code:s1")
		h.acquire("claude-code:s2")
		h.waitHelper(true)
		h.idleSweep(
			activity.Release{Key: "claude-code:s1", Reason: activity.ReasonTTLExpired},
			activity.Release{Key: "claude-code:s2", Reason: activity.ReasonMaxAgeBackstop},
		)
		h.waitHelper(false)
		cueBeforeUnblock(t, h.j, "cue:default:sleepHoldExpired")
		if h.countEvents(model.EventIdleRelease) != 1 {
			t.Fatalf("events = %v", h.events())
		}
	})

	t.Run("an idle batch with a finished agent plays the work-complete cue", func(t *testing.T) {
		h := newHarness(t)
		h.lid.set(true)
		h.start()
		h.acquire("claude-code:s1")
		h.acquire("claude-code:s2")
		h.waitHelper(true)
		h.idleSweep(
			activity.Release{Key: "claude-code:s1", Reason: activity.ReasonTTLExpired},
			activity.Release{Key: "claude-code:s2", Reason: activity.ReasonCPUIdle},
		)
		h.waitHelper(false)
		cueBeforeUnblock(t, h.j, "cue:default:sleepWorkComplete")
	})

	t.Run("an idle release skips an assertion re-acquired during the sweep", func(t *testing.T) {
		h := newHarness(t)
		h.start()
		h.acquire("claude-code:turn")
		h.send(ipc.Request{Op: ipc.OpAcquire, Key: "claude-code:resumed", Tool: "claude-code", PID: 4242})
		h.send(ipc.Request{Op: ipc.OpAcquire, Key: "codex:ttl", Tool: "codex", TTL: ipc.Ptr(600.0)})
		h.acquire("claude-code:stale")
		h.waitHelper(true)
		// The sweep snapshots, then probes outside the lock. Meanwhile a resumed session re-acquires
		// under a new PID, a hook refreshes a TTL and, a moment later, a new turn re-acquires.
		h.d.idle.Assertions()
		h.send(ipc.Request{Op: ipc.OpAcquire, Key: "claude-code:resumed", Tool: "claude-code", PID: 5151})
		h.send(ipc.Request{Op: ipc.OpAcquire, Key: "codex:ttl", Tool: "codex", TTL: ipc.Ptr(1200.0)})
		h.clock.Advance(time.Second)
		h.acquire("claude-code:turn")
		h.d.onIdleRelease([]activity.Release{
			{Key: "claude-code:turn", Reason: activity.ReasonCPUIdle},
			{Key: "claude-code:resumed", Reason: activity.ReasonDeadProcess},
			{Key: "codex:ttl", Reason: activity.ReasonTTLExpired},
			{Key: "claude-code:stale", Reason: activity.ReasonCPUIdle},
		})
		if got := joinKeys(h.status().Assertions); got != "claude-code:resumed,claude-code:turn,codex:ttl" {
			t.Fatalf("held after the sweep = %s (only the unchanged assertion goes)", got)
		}
		if got := joinKeys(h.readState().Assertions); got != "claude-code:resumed,claude-code:turn,codex:ttl" {
			t.Fatalf("persisted = %s", got)
		}
		if h.countEvents(model.EventIdleRelease) != 1 {
			t.Fatalf("events = %v", h.events())
		}
		// The next sweep judges the current state.
		h.idleSweep(activity.Release{Key: "claude-code:turn", Reason: activity.ReasonCPUIdle})
		if got := joinKeys(h.status().Assertions); got != "claude-code:resumed,codex:ttl" {
			t.Fatalf("held after the next sweep = %s", got)
		}
	})

	t.Run("a release that removes nothing keeps the cue of the release that emptied the registry", func(t *testing.T) {
		h := newHarness(t)
		h.lid.set(true)
		h.start()
		h.acquire("claude-code:s1")
		h.waitHelper(true)
		// A re-push is in flight, so the unblock (and its cue) waits behind it.
		h.helper.hold()
		h.d.onWake()
		eventually(t, "the re-push in flight", h.helper.inFlight)
		h.idleSweep(activity.Release{Key: "claude-code:s1", Reason: activity.ReasonTTLExpired})
		// The agent's Stop hook arrives after the expiry dropped its key.
		if resp := h.send(ipc.Request{Op: ipc.OpRelease, Key: "claude-code:s1"}); resp.Warning == "" {
			t.Fatalf("late release = %+v", resp)
		}
		// So does the exit of a process that holds nothing.
		h.d.onProcessExit(999)
		h.helper.release()
		h.waitHelper(false)
		cueBeforeUnblock(t, h.j, "cue:default:sleepHoldExpired")
	})

	t.Run("session actions apply in order", func(t *testing.T) {
		h := newHarness(t)
		h.lid.set(true)
		h.start()
		h.acquire("claude-code:s1")
		h.waitHelper(true)
		h.d.onSessionActions([]activity.WaitAction{activity.SetWaitingFor{Key: "claude-code:s1", Label: "a question"}})
		a := h.status().Assertions[0]
		if a.WaitingFor != "a question" {
			t.Fatalf("waitingFor = %q", a.WaitingFor)
		}
		if got := h.readState().Assertions[0].WaitingFor; got != "a question" {
			t.Fatalf("persisted waitingFor = %q", got)
		}
		parkUntil := harnessEpoch.Add(10 * time.Minute)
		h.d.onSessionActions([]activity.WaitAction{activity.Park{Key: "claude-code:s1", ExpiresAt: parkUntil}})
		if a := h.status().Assertions[0]; a.ExpiresAt == nil || !a.ExpiresAt.Equal(parkUntil) {
			t.Fatalf("parked expiry = %v", a.ExpiresAt)
		}
		h.d.onSessionActions([]activity.WaitAction{activity.Restore{Key: "claude-code:s1"}})
		if a := h.status().Assertions[0]; a.ExpiresAt != nil {
			t.Fatalf("restored expiry = %v", a.ExpiresAt)
		}
		saved := h.status().Assertions[0]
		h.d.onSessionActions([]activity.WaitAction{activity.ReleaseWaiting{Key: "claude-code:s1"}})
		h.waitHelper(false)
		cueBeforeUnblock(t, h.j, "cue:default:sleepHoldExpired")
		h.d.onSessionActions([]activity.WaitAction{activity.Reacquire{Assertion: saved}})
		if got := joinKeys(h.status().Assertions); got != "claude-code:s1" {
			t.Fatalf("after reacquire = %s", got)
		}
		h.waitHelper(true)
	})
}

func TestSniffSweep(t *testing.T) {
	sniffHarness := func(t *testing.T, auto bool) *harness {
		h := newHarness(t)
		h.writeSettings(func(s *settings.Settings) { s.AutoAcquireForKnownAgents = auto })
		h.cfg.SweepInterval = 5 * time.Millisecond
		h.procs.set(100, "/opt/homebrew/bin/claude")
		h.procs.set(200, "/usr/bin/vim")
		h.procs.set(300, "/usr/local/bin/hermes")
		return h
	}

	t.Run("while holding, a running known agent is auto-acquired, gateway agents never", func(t *testing.T) {
		h := sniffHarness(t, true)
		h.start()
		h.acquire("claude-code:hook")
		eventually(t, "the sniffed assertion", func() bool { return len(h.status().Assertions) == 2 })
		var sniffed model.Assertion
		for _, a := range h.status().Assertions {
			if a.Key != "claude-code:hook" {
				sniffed = a
			}
		}
		if sniffed.Key != "sniffed:claude-code:100" || sniffed.Origin != model.OriginSniffed || sniffed.Tool != "claude-code" ||
			sniffed.PID != 100 || sniffed.ProcessName != "claude" || sniffed.Reason != "auto (process sniffing)" {
			t.Fatalf("sniffed = %+v", sniffed)
		}
		stays(t, "no other process sniffed", 30*time.Millisecond, func() bool { return len(h.status().Assertions) == 2 })
	})

	t.Run("a released sniffed assertion stays released while its process lives", func(t *testing.T) {
		h := sniffHarness(t, true)
		h.start()
		h.acquire("claude-code:hook")
		eventually(t, "the sniffed assertion", func() bool { return len(h.status().Assertions) == 2 })
		h.send(ipc.Request{Op: ipc.OpRelease, Key: "sniffed:claude-code:100"})
		stays(t, "no re-acquire", 50*time.Millisecond, func() bool { return len(h.status().Assertions) == 1 })
		// The process exits and its pid is reused by a new agent process.
		h.procs.set(100, "")
		h.exits.ch <- 100
		h.procs.set(100, "/opt/homebrew/bin/claude")
		eventually(t, "a new agent on the recycled pid", func() bool { return len(h.status().Assertions) == 2 })
	})

	t.Run("sniffing is opt-in", func(t *testing.T) {
		h := sniffHarness(t, false)
		h.start()
		h.acquire("claude-code:hook")
		stays(t, "no sniffed assertion", 50*time.Millisecond, func() bool { return len(h.status().Assertions) == 1 })
	})

	t.Run("there is no sweep while idle", func(t *testing.T) {
		h := sniffHarness(t, true)
		h.start()
		stays(t, "nothing acquired from nothing", 50*time.Millisecond, func() bool { return len(h.status().Assertions) == 0 })
	})
}

func TestStalenessRelaunch(t *testing.T) {
	replace := func(t *testing.T, path string) {
		t.Helper()
		if err := os.WriteFile(path+".new", []byte("a newer, longer build"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(path+".new", path); err != nil {
			t.Fatal(err)
		}
	}
	exe := func(t *testing.T) string {
		path := filepath.Join(t.TempDir(), "lidwake")
		if err := os.WriteFile(path, []byte("build 1"), 0o755); err != nil {
			t.Fatal(err)
		}
		return path
	}

	t.Run("a replaced binary is adopted only once idle", func(t *testing.T) {
		h := newHarness(t)
		path := exe(t)
		h.cfg.Executable = NewExecutableStaleness(path)
		h.start()
		h.acquire("claude-code:s1")
		h.waitHelper(true)
		replace(t, path)
		h.d.onWake() // an apply while blocking re-checks, and defers
		h.waitHelper(true)
		select {
		case err := <-h.done:
			t.Fatalf("exited while blocking: %v", err)
		case <-time.After(30 * time.Millisecond):
		}
		h.send(ipc.Request{Op: ipc.OpRelease, Key: "claude-code:s1"})
		select {
		case err := <-h.done:
			h.stopped = true
			if !errors.Is(err, ErrExecutableReplaced) {
				t.Fatalf("Serve = %v, want ErrExecutableReplaced", err)
			}
		case <-time.After(waitLimit):
			t.Fatal("did not exit once idle")
		}
		if _, err := os.Lstat(h.cfg.Socket); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("the socket survived the relaunch exit")
		}
		if got, _ := h.helper.lastSet(); got {
			t.Fatal("exited with the block in place")
		}
	})

	t.Run("a latched cutout defers the relaunch until it clears", func(t *testing.T) {
		h := newHarness(t)
		path := exe(t)
		h.cfg.Executable = NewExecutableStaleness(path)
		h.start()
		h.acquire("claude-code:s1")
		h.waitHelper(true) // the startup unblock and its relaunch check are behind us
		replace(t, path)
		h.sensor.set(90)
		eventually(t, "the cutout", func() bool { return h.hasEvent(model.EventThermalCutout) })
		h.waitHelper(false)
		select {
		case err := <-h.done:
			t.Fatalf("exited with the latch held: %v", err)
		case <-time.After(30 * time.Millisecond):
		}
		h.sensor.set(60)
		select {
		case err := <-h.done:
			h.stopped = true
			if !errors.Is(err, ErrExecutableReplaced) {
				t.Fatalf("Serve = %v", err)
			}
		case <-time.After(waitLimit):
			t.Fatal("did not exit once the latch cleared")
		}
	})
}

func TestShutdown(t *testing.T) {
	t.Run("shutdown clears the block, saves the state and removes the socket", func(t *testing.T) {
		h := newHarness(t)
		h.start()
		h.acquire("claude-code:s1")
		h.waitHelper(true)
		if err := h.stop(); err != nil {
			t.Fatal(err)
		}
		if got, _ := h.helper.lastSet(); got {
			t.Fatal("the block survived shutdown")
		}
		if _, err := os.Lstat(h.cfg.Socket); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("the socket survived shutdown")
		}
		if got := joinKeys(h.readState().Assertions); got != "claude-code:s1" {
			t.Fatalf("persisted = %s (live holds must survive a restart)", got)
		}
		cues := slices.ContainsFunc(h.j.all(), func(e string) bool { return strings.HasPrefix(e, "cue:") })
		if cues {
			t.Fatal("a shutdown is not a release: no cue")
		}
	})

	t.Run("shutdown is bounded when the helper is wedged", func(t *testing.T) {
		h := newHarness(t)
		h.cfg.ShutdownTimeout = 50 * time.Millisecond
		h.start()
		h.acquire("claude-code:s1")
		h.waitHelper(true)
		h.helper.hold()
		defer h.helper.release()
		h.d.onWake()
		eventually(t, "the call in flight", func() bool { return h.helper.inFlight() })
		start := time.Now()
		if err := h.stop(); err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("shutdown took %v with a wedged helper", elapsed)
		}
	})

	t.Run("Serve refuses to start while another daemon answers on the socket", func(t *testing.T) {
		h := newHarness(t)
		ln, err := net.Listen("unix", h.cfg.Socket)
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		err = New(h.cfg).Serve(context.Background())
		if !errors.Is(err, errAlreadyRunning) {
			t.Fatalf("Serve = %v", err)
		}
		if h.helper.calls() != 0 {
			t.Fatal("a refused start touched the helper")
		}
	})

	t.Run("requests during shutdown are refused", func(t *testing.T) {
		h := newHarness(t)
		h.start()
		h.d.mu.Lock()
		h.d.closing = true
		h.d.mu.Unlock()
		resp := h.d.handleRequest(context.Background(), ipc.Request{Op: ipc.OpAcquire, Key: "claude-code:s1", Tool: "claude-code"})
		if resp.OK || resp.Error != "the lidwake daemon is shutting down" {
			t.Fatalf("acquire during shutdown = %+v", resp)
		}
		if resp := h.d.handleRequest(context.Background(), ipc.Request{Op: ipc.OpPing}); !resp.OK {
			t.Fatal("ping during shutdown failed")
		}
	})
}

func TestSniffedPID(t *testing.T) {
	// The sniff sweep mints its keys in the namespace the validator reserves.
	if key := policy.SniffedKeyPrefix + "claude-code:4242"; key != "sniffed:claude-code:4242" {
		t.Fatalf("sniffed key = %q", key)
	}
	if pid, ok := sniffedPID("sniffed:claude-code:4242"); !ok || pid != 4242 {
		t.Fatalf("sniffedPID = %d %v", pid, ok)
	}
	for _, key := range []string{"claude-code:4242", "sniffed:claude-code:x", "sniffed:claude-code:-3"} {
		if _, ok := sniffedPID(key); ok {
			t.Fatalf("sniffedPID(%q) matched", key)
		}
	}
}

func TestConcurrency(t *testing.T) {
	t.Run("a request waiting on a wedged helper does not block the others", func(t *testing.T) {
		h := newHarness(t)
		h.start()
		h.acquire("claude-code:s1")
		h.waitHelper(true)
		h.helper.hold()
		defer h.helper.release()
		slow := make(chan ipc.Response, 1)
		go func() {
			resp, _ := ipc.SendTo(h.cfg.Socket, ipc.Request{Op: ipc.OpReleaseAll}, waitLimit)
			slow <- resp
		}()
		eventually(t, "the unblock in flight", func() bool { return h.helper.inFlight() })
		start := time.Now()
		st := h.status()
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("status took %v behind a wedged helper", elapsed)
		}
		if st.Blocking {
			t.Fatal("the registry is already empty while the helper catches up")
		}
		if resp := h.acquire("claude-code:s2"); !resp.OK {
			t.Fatalf("acquire behind a wedged helper = %+v", resp)
		}
		h.helper.release()
		if resp := <-slow; !resp.OK || *resp.ReleasedCount != 1 {
			t.Fatalf("releaseAll = %+v", resp)
		}
		h.waitHelper(true) // latest wins: s2 is held again
	})
}

func TestResync(t *testing.T) {
	t.Run("reconciles and wakes racing releases never leave a stale block or display hold", func(t *testing.T) {
		h := newHarness(t)
		h.start()
		var wg sync.WaitGroup
		for range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range 500 {
					h.d.reconcile()
					h.d.onWake()
				}
			}()
		}
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
	loop:
		for {
			select {
			case <-done:
				break loop
			default:
			}
			h.send(ipc.Request{Op: ipc.OpAcquire, Key: "claude-code:s1", Tool: "claude-code", Display: true})
			h.send(ipc.Request{Op: ipc.OpRelease, Key: "claude-code:s1"})
		}
		// Edges still queued may replay transient states; what matters is where it settles. A
		// stale block or display hold would stick.
		settled := func() bool {
			got, _ := h.helper.lastSet()
			return !got && !h.d.display.Held()
		}
		eventually(t, "the unblock", settled)
		time.Sleep(50 * time.Millisecond)
		eventually(t, "the unblock to stick", settled)
	})
}

func TestNewDefaults(t *testing.T) {
	t.Run("an empty config gets the production wiring without touching the machine", func(t *testing.T) {
		d := New(Config{Log: quiet()})
		if d.cfg.SupportDir != paths.SupportDir() || d.cfg.SettingsPath != paths.ConfigFile() || d.socket != paths.CLISocket() {
			t.Fatalf("paths: %q %q %q", d.cfg.SupportDir, d.cfg.SettingsPath, d.socket)
		}
		if _, ok := d.helper.(*ipc.HelperClient); !ok {
			t.Fatalf("helper = %T", d.helper)
		}
		if p, ok := d.chimes.(*chime.Player); !ok || p.Muted == nil {
			t.Fatalf("chimes = %T", d.chimes)
		}
		if d.lid == nil || d.wake == nil || d.battery == nil || d.thermal == nil || d.idle == nil || d.session == nil || d.processes == nil || d.display == nil || d.procs == nil {
			t.Fatal("a monitor is missing")
		}
		if d.lid.OnChange == nil || d.wake.OnWake == nil || d.battery.OnCutout == nil || d.thermal.OnCutout == nil ||
			d.idle.OnRelease == nil || d.session.OnActions == nil || d.processes.OnExit == nil {
			t.Fatal("a monitor callback is not wired")
		}
		if d.cfg.SweepInterval != DefaultSweepInterval || d.cfg.ReconcileInterval != DefaultReconcileInterval ||
			d.cfg.LatchRecheckInterval != DefaultLatchRecheckInterval || d.cfg.ShutdownTimeout != DefaultShutdownTimeout {
			t.Fatalf("intervals: %+v", d.cfg)
		}
		if !d.latch.ClearsOnLidOpen || d.latch.IsLatched() {
			t.Fatal("the latch must start empty, built by NewCutoutLatch")
		}
	})

	t.Run("a socket follows a custom support directory", func(t *testing.T) {
		d := New(Config{SupportDir: "/tmp/x", Log: quiet()})
		if d.socket != "/tmp/x/cli.sock" {
			t.Fatalf("socket = %q", d.socket)
		}
	})
}
