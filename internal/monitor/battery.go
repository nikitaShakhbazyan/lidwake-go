package monitor

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/nikitaShakhbazyan/lidwake-go/internal/darwin"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/policy"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/settings"
)

// DefaultBatteryInterval is how often the battery is read.
const DefaultBatteryInterval = 30 * time.Second

// BatteryMonitor reads the battery and fires the low-battery and AC-only cutouts, so a kept-awake
// Mac sleeps normally instead of draining to a hard shutdown in a bag.
//
// The battery is polled on a fixed interval, idle or not: every reading is also the cutout latch's
// recovery path (plugging in clears a low-battery latch), and AC-only mode must notice an unplug.
// On top of that, every gate change (lid, blocking, AC-only) reads at once: closing the lid while
// already on battery below the threshold — the drain-in-a-bag case the cutout exists for — is not
// a power-source event and must not wait for the next poll.
type BatteryMonitor struct {
	// Read returns the internal battery; ok false on a Mac without one. Nil: darwin.ReadBattery.
	Read func() (darwin.Battery, bool)
	// Interval between reads. Zero: DefaultBatteryInterval.
	Interval time.Duration
	// OnReading is called with every successful reading, before the cutouts are evaluated.
	OnReading func(darwin.Battery)
	// OnCutout is called with policy.CutoutOnBattery (AC-only mode, on battery) or
	// policy.CutoutLowBattery, on every reading that meets the cutout while blocking.
	OnCutout func(policy.CutoutCause)
	Log      *slog.Logger

	life lifecycle
	kick kicker

	mu         sync.Mutex
	cfg        *batteryConfig // nil: settings.Defaults()
	lidClosed  bool
	blocking   bool
	last       darwin.Battery
	haveLast   bool
	readFailed bool // logged once, not on every poll of a desktop Mac
}

type batteryConfig struct {
	enabled        bool
	thresholdPct   int
	requireACPower bool
}

func batteryConfigFor(s settings.Settings) batteryConfig {
	return batteryConfig{
		enabled:        s.LowBatteryCutoutEnabled,
		thresholdPct:   s.LowBatteryThresholdPercent,
		requireACPower: s.RequireACPower,
	}
}

// NewBatteryMonitor returns a monitor reading the real battery, configured with the default
// settings.
func NewBatteryMonitor() *BatteryMonitor {
	return &BatteryMonitor{Read: darwin.ReadBattery, Interval: DefaultBatteryInterval}
}

// Start takes a first reading, so Last is current when Start returns, and starts polling. The
// goroutine evaluates once right away (callbacks included), as it does on every gate change.
func (m *BatteryMonitor) Start(ctx context.Context) {
	m.life.start(ctx,
		func() {
			if r, ok := m.read(); ok {
				m.mu.Lock()
				m.last, m.haveLast = r, true
				m.mu.Unlock()
			}
			m.kick.signal()
		},
		func(ctx context.Context, stop <-chan struct{}) {
			runTicking(ctx, stop, ticking{
				interval: orDefault(m.Interval, DefaultBatteryInterval),
				kick:     m.kick.c(),
				onKick:   m.tick,
				onTick:   m.tick,
			})
		})
}

// Stop ends polling.
func (m *BatteryMonitor) Stop() { m.life.halt() }

// ApplySettings takes the low-battery cutout switch and threshold and the AC-only switch. Turning
// AC-only mode on or off reads the battery at once.
func (m *BatteryMonitor) ApplySettings(s settings.Settings) {
	cfg := batteryConfigFor(s)
	m.mu.Lock()
	acChanged := cfg.requireACPower != m.config().requireACPower
	m.cfg = &cfg
	m.mu.Unlock()
	if acChanged {
		m.kick.signal()
	}
}

// SetLidClosed sets the lid as the cutouts see it — the daemon's armed-lid value, which is true
// with the lid open as well when safety cutouts apply with the lid open. A change reads at once.
func (m *BatteryMonitor) SetLidClosed(closed bool) {
	m.mu.Lock()
	changed := m.lidClosed != closed
	m.lidClosed = closed
	m.mu.Unlock()
	if changed {
		m.kick.signal()
	}
}

// SetBlocking sets whether lidwake is keeping the Mac awake. The cutouts only fire while it is;
// with nothing held there is nothing to cut out. A change reads at once, so a hold acquired while
// already low on battery with the lid closed is caught immediately.
func (m *BatteryMonitor) SetBlocking(blocking bool) {
	m.mu.Lock()
	changed := m.blocking != blocking
	m.blocking = blocking
	m.mu.Unlock()
	if changed {
		m.kick.signal()
	}
}

// Last is the most recent successful reading; ok is false until there is one (always, on a Mac
// without a battery).
func (m *BatteryMonitor) Last() (b darwin.Battery, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.last, m.haveLast
}

// config is the current configuration; callers hold m.mu.
func (m *BatteryMonitor) config() batteryConfig {
	if m.cfg == nil {
		return batteryConfigFor(settings.Defaults())
	}
	return *m.cfg
}

func (m *BatteryMonitor) read() (darwin.Battery, bool) {
	read := m.Read
	if read == nil {
		read = darwin.ReadBattery
	}
	return read()
}

// tick reads the battery, reports the reading and evaluates the cutouts. The AC-only cutout
// comes first: it is the stricter rule, and one cutout per reading is enough.
func (m *BatteryMonitor) tick() {
	r, ok := m.read()
	m.mu.Lock()
	firstFailure := !ok && !m.readFailed
	m.readFailed = !ok
	if ok {
		m.last, m.haveLast = r, true
	}
	m.mu.Unlock()
	log := logger(m.Log, "battery")
	if !ok {
		if firstFailure {
			log.Debug("no battery reading")
		}
		return
	}
	if m.OnReading != nil {
		m.OnReading(r)
	}

	m.mu.Lock()
	cfg, lidClosed, blocking := m.config(), m.lidClosed, m.blocking
	m.mu.Unlock()
	if policy.ShouldCutoutOnBattery(r.OnBattery, cfg.requireACPower, blocking) {
		log.Warn("AC-only cutout: running on battery")
		m.cutout(policy.CutoutOnBattery)
		return
	}
	cut := policy.LowBatteryCutout{
		BatteryPercent:   r.Percent,
		ThresholdPercent: cfg.thresholdPct,
		OnBattery:        r.OnBattery,
		Enabled:          cfg.enabled,
		LidClosed:        lidClosed,
		Blocking:         blocking,
	}
	if !cut.ShouldFire() {
		return
	}
	log.Warn("low-battery cutout: on battery at or under the threshold", "percent", r.Percent, "threshold", cfg.thresholdPct)
	m.cutout(policy.CutoutLowBattery)
}

func (m *BatteryMonitor) cutout(c policy.CutoutCause) {
	if m.OnCutout != nil {
		m.OnCutout(c)
	}
}
