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

// DefaultThermalInterval is how often the CPU temperature is read while lidwake keeps the Mac
// awake.
const DefaultThermalInterval = 15 * time.Second

// TemperatureSensor reads the CPU temperature. *darwin.SMC implements it.
type TemperatureSensor interface {
	CPUTemperature() (celsius float64, ok bool)
	Close()
}

// openSMC opens AppleSMC as a TemperatureSensor (never a typed nil).
func openSMC() (TemperatureSensor, error) {
	s, err := darwin.OpenSMC()
	if err != nil {
		return nil, err
	}
	return s, nil
}

// ThermalMonitor reads the CPU temperature from the SMC and fires the thermal cutout when it
// reaches the user's threshold while lidwake keeps the Mac awake with the lid closed (or with the
// lid open too, when safety cutouts apply with the lid open: the daemon passes that in as the lid
// state). Thermals with nothing held are macOS's own business.
//
// The 15 s poll runs only while blocking, so the daemon makes no periodic SMC reads for the vast
// majority of its life when no agent is active. ReadNow serves a current value while the poll is
// stopped.
type ThermalMonitor struct {
	// Open connects to the sensor. Nil: darwin.OpenSMC. A failed open is retried on every later
	// read, so one failure at startup cannot leave the cutout dead for the daemon's whole life.
	Open func() (TemperatureSensor, error)
	// Interval between reads while blocking. Zero: DefaultThermalInterval.
	Interval time.Duration
	// OnReading is called with every successful polled reading (the daemon tracks the peak while
	// the lid is closed); not for ReadNow.
	OnReading func(celsius float64)
	// OnCutout is called on every polled reading that meets the cutout.
	OnCutout func()
	Log      *slog.Logger

	life lifecycle
	kick kicker

	// sensorMu serializes the sensor connection: the poll and ReadNow share it.
	sensorMu   sync.Mutex
	sensor     TemperatureSensor
	openFailed bool // logged once, until an open succeeds
	closed     bool

	mu        sync.Mutex
	cfg       *thermalConfig // nil: settings.Defaults()
	lidClosed bool
	blocking  bool
	last      float64
	haveLast  bool
}

type thermalConfig struct {
	enabled   bool
	threshold float64
}

func thermalConfigFor(s settings.Settings) thermalConfig {
	return thermalConfig{enabled: s.ThermalCutoutEnabled, threshold: s.ThermalThresholdCelsius}
}

// NewThermalMonitor returns a monitor reading the real SMC, configured with the default settings.
func NewThermalMonitor() *ThermalMonitor {
	return &ThermalMonitor{Open: openSMC, Interval: DefaultThermalInterval}
}

// Start opens the sensor and starts the loop. Polling begins only once SetBlocking(true); the
// first poll of every blocking stretch reads at once.
func (m *ThermalMonitor) Start(ctx context.Context) {
	m.life.start(ctx,
		func() {
			m.sensorMu.Lock()
			m.ensureSensor()
			m.sensorMu.Unlock()
		},
		func(ctx context.Context, stop <-chan struct{}) {
			runTicking(ctx, stop, ticking{
				interval: orDefault(m.Interval, DefaultThermalInterval),
				armed:    m.isBlocking,
				onArm:    m.tick, // a fresh reading the moment blocking begins
				kick:     m.kick.c(),
				onTick:   m.tick,
			})
		})
}

// Stop ends polling and closes the sensor. ReadNow returns the last reading afterwards.
func (m *ThermalMonitor) Stop() {
	m.life.halt()
	m.sensorMu.Lock()
	defer m.sensorMu.Unlock()
	m.closed = true
	if m.sensor != nil {
		m.sensor.Close()
		m.sensor = nil
	}
}

// ApplySettings takes the thermal cutout switch and threshold.
func (m *ThermalMonitor) ApplySettings(s settings.Settings) {
	cfg := thermalConfigFor(s)
	m.mu.Lock()
	m.cfg = &cfg
	m.mu.Unlock()
}

// SetLidClosed sets the lid as the cutout sees it — the daemon's armed-lid value. Read on the next
// poll.
func (m *ThermalMonitor) SetLidClosed(closed bool) {
	m.mu.Lock()
	m.lidClosed = closed
	m.mu.Unlock()
}

// SetBlocking sets whether lidwake is keeping the Mac awake, which arms (with an immediate read)
// or disarms the poll. With nothing held there is nothing to cut out.
func (m *ThermalMonitor) SetBlocking(blocking bool) {
	m.mu.Lock()
	changed := m.blocking != blocking
	m.blocking = blocking
	m.mu.Unlock()
	if changed {
		m.kick.signal()
	}
}

// Last is the most recent successful reading, polled or from ReadNow.
func (m *ThermalMonitor) Last() (celsius float64, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.last, m.haveLast
}

// ReadNow reads the temperature once, for callers that want a current value while the poll is
// stopped, and refreshes Last. When the read fails it returns Last. No callbacks.
func (m *ThermalMonitor) ReadNow() (celsius float64, ok bool) {
	if t, ok := m.read(); ok {
		m.record(t)
		return t, true
	}
	return m.Last()
}

// Current is the temperature a status reports: while blocking, the polled reading, at most one
// interval old; otherwise a fresh ReadNow, because the poll is stopped. While blocking with no
// reading yet (the first poll is still on its way) it reads now too, so a status taken in that
// moment does not report the sensor as unreadable. ok false means the temperature is unreadable,
// so the thermal cutout cannot trigger.
func (m *ThermalMonitor) Current() (celsius float64, ok bool) {
	if m.isBlocking() {
		if c, ok := m.Last(); ok {
			return c, true
		}
	}
	return m.ReadNow()
}

func (m *ThermalMonitor) isBlocking() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.blocking
}

func (m *ThermalMonitor) record(t float64) {
	m.mu.Lock()
	m.last, m.haveLast = t, true
	m.mu.Unlock()
}

// ensureSensor opens the sensor if it is not open; callers hold sensorMu.
func (m *ThermalMonitor) ensureSensor() bool {
	if m.closed {
		return false
	}
	if m.sensor != nil {
		return true
	}
	open := m.Open
	if open == nil {
		open = openSMC
	}
	s, err := open()
	if err != nil || s == nil {
		if !m.openFailed {
			m.openFailed = true
			logger(m.Log, "thermal").Warn("cannot open the SMC; retrying on the next read", "err", err)
		}
		return false
	}
	if m.openFailed {
		m.openFailed = false
		logger(m.Log, "thermal").Info("SMC opened")
	}
	m.sensor = s
	return true
}

func (m *ThermalMonitor) read() (float64, bool) {
	m.sensorMu.Lock()
	defer m.sensorMu.Unlock()
	if !m.ensureSensor() {
		return 0, false
	}
	return m.sensor.CPUTemperature()
}

// tick reads the temperature, reports it and evaluates the cutout.
func (m *ThermalMonitor) tick() {
	t, ok := m.read()
	if !ok {
		return
	}
	m.record(t)
	if m.OnReading != nil {
		m.OnReading(t)
	}
	m.mu.Lock()
	cfg := thermalConfigFor(settings.Defaults())
	if m.cfg != nil {
		cfg = *m.cfg
	}
	cut := policy.ThermalCutout{
		TemperatureCelsius: t,
		ThresholdCelsius:   cfg.threshold,
		Enabled:            cfg.enabled,
		LidClosed:          m.lidClosed,
		Blocking:           m.blocking,
	}
	m.mu.Unlock()
	if !cut.ShouldFire() {
		return
	}
	logger(m.Log, "thermal").Warn("thermal cutout: at or over the threshold", "celsius", t, "threshold", cfg.threshold)
	if m.OnCutout != nil {
		m.OnCutout()
	}
}
