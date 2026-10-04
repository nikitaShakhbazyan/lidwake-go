package monitor

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/nikitaShakhbazyan/lidwake-go/internal/darwin"
)

// DefaultLidInterval is how often the lid is polled. A lid close reaches the user as a chime and a
// screen lock, so two seconds keeps that prompt while costing one registry read.
const DefaultLidInterval = 2 * time.Second

// LidMonitor reports lid open/close changes by polling AppleClamshellState on IOPMrootDomain.
//
// Polled rather than subscribed to IOKit interest notifications: a poll needs no run loop on a
// locked thread, and the property read is cheap. A Mac without a lid reads as open, always.
//
// A read that fails (no lid, or the registry lookup failed) keeps the last state rather than
// reading as open. A lid Mac whose lookup fails once would otherwise report an open and a close a
// poll apart, and a spurious open clears a closed-lid cutout latch and ends away tracking.
type LidMonitor struct {
	// Read reports the lid; ok false means the Mac has no lid or the read failed. Nil:
	// darwin.LidClosed.
	Read func() (closed, ok bool)
	// Interval between polls. Zero: DefaultLidInterval.
	Interval time.Duration
	// OnChange is called with the new state each time it changes. The state found by Start is the
	// baseline, not a change.
	OnChange func(closed bool)
	Log      *slog.Logger

	life   lifecycle
	mu     sync.Mutex
	closed bool
}

// NewLidMonitor returns a monitor reading the real lid.
func NewLidMonitor() *LidMonitor {
	return &LidMonitor{Read: darwin.LidClosed, Interval: DefaultLidInterval}
}

// Start reads the lid once (so Closed is current when Start returns; a failed read is open) and
// starts polling.
func (m *LidMonitor) Start(ctx context.Context) {
	m.life.start(ctx,
		func() {
			closed, ok := m.read()
			m.mu.Lock()
			m.closed = ok && closed
			m.mu.Unlock()
		},
		func(ctx context.Context, stop <-chan struct{}) {
			runTicking(ctx, stop, ticking{interval: orDefault(m.Interval, DefaultLidInterval), onTick: m.poll})
		})
}

// Stop ends polling.
func (m *LidMonitor) Stop() { m.life.halt() }

// Closed is the last lid state seen.
func (m *LidMonitor) Closed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed
}

func (m *LidMonitor) read() (closed, ok bool) {
	read := m.Read
	if read == nil {
		read = darwin.LidClosed
	}
	return read()
}

func (m *LidMonitor) poll() {
	closed, ok := m.read()
	if !ok {
		return
	}
	m.mu.Lock()
	changed := closed != m.closed
	m.closed = closed
	m.mu.Unlock()
	if !changed {
		return
	}
	state := "opened"
	if closed {
		state = "closed"
	}
	logger(m.Log, "lid").Info("lid " + state)
	if m.OnChange != nil {
		m.OnChange(closed)
	}
}
