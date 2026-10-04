package monitor

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/nikitaShakhbazyan/lidwake-go/internal/activity"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/darwin"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/model"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/settings"
)

// DefaultSessionInterval is the session-status sweep's period: fast enough that a wait is parked
// well inside any sensible grace window and an answer from a phone re-arms the hold promptly,
// cheap enough not to matter while the Mac is awake anyway (a handful of sub-kilobyte reads).
const DefaultSessionInterval = 10 * time.Second

// SessionStatusMonitor watches Claude Code's session status files (`~/.claude/sessions/<pid>.json`)
// so a session that stopped mid-turn to wait for its user — a question, a plan approval, a
// permission prompt — is handled by the user's waiting policy instead of holding until the CPU-idle
// sweep. activity.ClaudeSessionStatus describes the files and activity.SessionWaitEvaluator makes
// the decisions; this monitor supplies the reads, the process probe and the timer, and hands the
// actions to the daemon.
//
// Polling, not file watching, on purpose: Claude Code rewrites the files in place (no rename), so
// a directory kqueue never fires for status flips. The poll runs while an assertion is held, plus
// while a parked wait is outstanding (a hold this policy released must still see its session turn
// busy again). With nothing held and nothing parked there is no timer and no wakeups.
type SessionStatusMonitor struct {
	// Assertions returns the live assertions (the registry snapshot). Nil: sweeps do nothing.
	Assertions func() []model.Assertion
	// OnActions receives each sweep's decisions, in order, for the daemon to apply to the registry.
	// Not called for a sweep with no decisions.
	OnActions func([]activity.WaitAction)
	// Dir is the status-file directory. "": activity.DefaultSessionsDir().
	Dir string
	// ProcessAlive tells a live session from a crashed one's leftover file. Nil:
	// darwin.ProcessAlive.
	ProcessAlive func(pid int) bool
	// Now is the wall clock. Nil: time.Now.
	Now func() time.Time
	// Interval between sweeps while armed. Zero: DefaultSessionInterval.
	Interval time.Duration
	Log      *slog.Logger

	life lifecycle
	kick kicker

	// mu guards the configuration, the blocking gate and the evaluator (whose parked set is part of
	// the gate).
	mu       sync.Mutex
	cfg      *activity.WaitConfig // nil: from settings.Defaults()
	blocking bool
	eval     activity.SessionWaitEvaluator
}

// WaitConfigFor builds the waiting policy from the user's settings.
func WaitConfigFor(s settings.Settings) activity.WaitConfig {
	return activity.WaitConfig{
		Policy: s.AgentWaitingPolicy,
		Grace:  time.Duration(s.AgentWaitingGraceMinutes) * time.Minute,
	}
}

// NewSessionStatusMonitor returns a monitor reading the user's Claude Code session directory,
// configured with the default settings.
func NewSessionStatusMonitor() *SessionStatusMonitor {
	return &SessionStatusMonitor{
		Dir:          activity.DefaultSessionsDir(),
		ProcessAlive: darwin.ProcessAlive,
		Now:          time.Now,
		Interval:     DefaultSessionInterval,
	}
}

// Start starts the loop. Sweeps run every interval while blocking or while a wait is parked.
func (m *SessionStatusMonitor) Start(ctx context.Context) {
	m.life.start(ctx, nil, func(ctx context.Context, stop <-chan struct{}) {
		runTicking(ctx, stop, ticking{
			interval: orDefault(m.Interval, DefaultSessionInterval),
			// Re-checked after every sweep: a sweep can park the last hold (dropping blocking) or
			// resolve the last parked wait.
			armed:  m.timerNeeded,
			kick:   m.kick.c(),
			onTick: m.sweep,
		})
	})
}

// Stop ends the sweeps.
func (m *SessionStatusMonitor) Stop() { m.life.halt() }

// ApplySettings takes the waiting policy and grace period.
func (m *SessionStatusMonitor) ApplySettings(s settings.Settings) {
	cfg := WaitConfigFor(s)
	m.mu.Lock()
	m.cfg = &cfg
	m.mu.Unlock()
}

// SetBlocking re-gates the sweep. Unlike the idle sweep, a parked wait keeps it running through
// the blocking→idle edge its own release caused.
func (m *SessionStatusMonitor) SetBlocking(blocking bool) {
	m.mu.Lock()
	changed := m.blocking != blocking
	m.blocking = blocking
	m.mu.Unlock()
	if changed {
		m.kick.signal()
	}
}

// HasParked reports whether a wait this monitor acted on is still outstanding.
func (m *SessionStatusMonitor) HasParked() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.eval.HasParked()
}

func (m *SessionStatusMonitor) timerNeeded() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.blocking || m.eval.HasParked()
}

func (m *SessionStatusMonitor) dir() string {
	if m.Dir == "" {
		return activity.DefaultSessionsDir()
	}
	return m.Dir
}

// sweep reads every status file, evaluates the live assertions against them and hands the actions
// to OnActions.
func (m *SessionStatusMonitor) sweep() {
	if m.Assertions == nil {
		return
	}
	assertions := m.Assertions()
	// A missing directory (Claude Code absent or never run) reads as no sessions, which disables
	// the whole policy gracefully.
	statuses := activity.ReadSessionStatuses(m.dir())
	alive := m.ProcessAlive
	if alive == nil {
		alive = darwin.ProcessAlive
	}
	now := nowOr(m.Now)
	m.mu.Lock()
	cfg := WaitConfigFor(settings.Defaults())
	if m.cfg != nil {
		cfg = *m.cfg
	}
	actions := m.eval.Evaluate(assertions, statuses, now, cfg, alive)
	m.mu.Unlock()
	if len(actions) == 0 {
		return
	}
	log := logger(m.Log, "session")
	for _, a := range actions {
		switch a := a.(type) {
		case activity.Park:
			log.Info("session waiting: grace TTL armed", "key", a.Key, "until", a.ExpiresAt)
		case activity.ReleaseWaiting:
			log.Info("session waiting: releasing (policy: sleep)", "key", a.Key)
		case activity.Restore:
			log.Info("session resumed: restoring", "key", a.Key)
		case activity.Reacquire:
			log.Info("session resumed after its hold was released: re-acquiring", "key", a.Assertion.Key)
		}
	}
	if m.OnActions != nil {
		m.OnActions(actions)
	}
}
