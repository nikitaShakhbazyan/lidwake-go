package policy

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nikitaShakhbazyan/lidwake-go/internal/model"
)

func TestAwaySummaryBuilder(t *testing.T) {
	closed := time.Unix(1_000_000, 0)
	opened := closed.Add(600 * time.Second)

	held := func(tool, key string, agoBeforeClose time.Duration) HeldAgent {
		if key == "" {
			key = tool + ":session"
		}
		return HeldAgent{Key: key, Tool: tool, DisplayName: strings.ToUpper(tool[:1]) + tool[1:], AcquiredAt: closed.Add(-agoBeforeClose)}
	}
	period := func(h []HeldAgent, active []string, released map[string]time.Time) AwayPeriod {
		keys := map[string]bool{}
		for _, k := range active {
			keys[k] = true
		}
		return AwayPeriod{HeldAtClose: h, ActiveKeys: keys, ReleasedAt: released, ClosedAt: closed, OpenedAt: opened}
	}
	tools := func(agents []model.FinishedAgent) []string {
		var out []string
		for _, a := range agents {
			out = append(out, a.Tool)
		}
		return out
	}
	mustBuild := func(t *testing.T, p AwayPeriod) *model.AwaySummary {
		t.Helper()
		s := BuildAwaySummary(p)
		if s == nil {
			t.Fatal("no summary")
		}
		return s
	}

	t.Run("nothing held at close → no summary", func(t *testing.T) {
		if s := BuildAwaySummary(period(nil, nil, nil)); s != nil {
			t.Fatalf("got %+v", s)
		}
	})

	t.Run("sessions still holding at open land in stillActive; the rest in finished", func(t *testing.T) {
		p := period([]HeldAgent{held("claude-code", "", 100*time.Second), held("codex", "", 200*time.Second)}, []string{"codex:session"}, nil)
		p.PeakTemperature = f64(55)
		s := mustBuild(t, p)
		if !slices.Equal(tools(s.Finished), []string{"claude-code"}) || !slices.Equal(tools(s.StillActive), []string{"codex"}) {
			t.Fatalf("finished=%v stillActive=%v", tools(s.Finished), tools(s.StillActive))
		}
		if s.StillActive[0].Duration != 800*time.Second {
			t.Fatalf("still-active duration %v, want acquisition → lid open", s.StillActive[0].Duration)
		}
	})

	// Partitioning is by KEY: a finished session must not be misfiled as still active just
	// because a different session of the same tool is running at lid open.
	t.Run("a finished session is not masked by another session of the same tool", func(t *testing.T) {
		s := mustBuild(t, period([]HeldAgent{held("claude-code", "claude-code:s1", 100*time.Second)}, []string{"claude-code:s2"}, nil))
		if len(s.Finished) != 1 || s.Finished[0].Key != "claude-code:s1" || len(s.StillActive) != 0 {
			t.Fatalf("finished=%+v stillActive=%+v", s.Finished, s.StillActive)
		}
	})

	t.Run("two sessions of the same tool keep distinct identities", func(t *testing.T) {
		s := mustBuild(t, period([]HeldAgent{
			held("claude-code", "claude-code:s1", 100*time.Second),
			held("claude-code", "claude-code:s2", 50*time.Second),
		}, nil, nil))
		if len(s.Finished) != 2 || s.Finished[0].Key == s.Finished[1].Key {
			t.Fatalf("finished=%+v", s.Finished)
		}
	})

	t.Run("a finished session's duration runs to its recorded release, not lid-open", func(t *testing.T) {
		// Released 120 s after the lid closed; the lid stayed shut another 480 s.
		s := mustBuild(t, period([]HeldAgent{held("claude-code", "", 100*time.Second)}, nil,
			map[string]time.Time{"claude-code:session": closed.Add(120 * time.Second)}))
		if got := s.Finished[0].Duration; got != 220*time.Second {
			t.Fatalf("duration %v: want 100 s before close + 120 s after, NOT the 700 s to lid open", got)
		}
	})

	t.Run("without a recorded release the duration falls back to lid-open", func(t *testing.T) {
		s := mustBuild(t, period([]HeldAgent{held("claude-code", "", 100*time.Second)}, nil, nil))
		if got := s.Finished[0].Duration; got != 700*time.Second {
			t.Fatalf("duration %v", got)
		}
	})

	t.Run("peak temperature and thermal-cutout flag pass through", func(t *testing.T) {
		p := period([]HeldAgent{held("claude-code", "", 0)}, nil, nil)
		p.PeakTemperature, p.ThermalCutout = f64(88.5), true
		s := mustBuild(t, p)
		if s.PeakTemperature == nil || *s.PeakTemperature != 88.5 || !s.ThermalCutout {
			t.Fatalf("peak=%v thermal=%v", s.PeakTemperature, s.ThermalCutout)
		}
		if !s.ClosedAt.Equal(closed) || !s.OpenedAt.Equal(opened) {
			t.Fatalf("closed=%v opened=%v", s.ClosedAt, s.OpenedAt)
		}
	})

	t.Run("low-battery cutout flag passes through", func(t *testing.T) {
		p := period([]HeldAgent{held("claude-code", "", 0)}, nil, nil)
		p.LowBatteryCutout = true
		s := mustBuild(t, p)
		if !s.LowBatteryCutout || s.ThermalCutout {
			t.Fatalf("lowBattery=%v thermal=%v", s.LowBatteryCutout, s.ThermalCutout)
		}
	})

	t.Run("empty partitions are empty lists, not null", func(t *testing.T) {
		s := mustBuild(t, period([]HeldAgent{held("codex", "", 0)}, []string{"codex:session"}, nil))
		if s.Finished == nil || s.StillActive == nil {
			t.Fatalf("finished=%v stillActive=%v", s.Finished, s.StillActive)
		}
	})
}
