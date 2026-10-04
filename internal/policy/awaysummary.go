package policy

import (
	"time"

	"github.com/nikitaShakhbazyan/lidwake-go/internal/model"
)

// HeldAgent is one assertion that was held at the moment the lid closed.
type HeldAgent struct {
	Key         string
	Tool        string
	DisplayName string
	AcquiredAt  time.Time
}

// AwayPeriod is what the daemon recorded between lid close and lid open.
type AwayPeriod struct {
	HeldAtClose []HeldAgent
	// ActiveKeys are the assertion keys still held at lid open.
	ActiveKeys map[string]bool
	// ReleasedAt are the release times recorded while the lid was closed, by key.
	ReleasedAt       map[string]time.Time
	ClosedAt         time.Time
	OpenedAt         time.Time
	PeakTemperature  *float64
	ThermalCutout    bool
	LowBatteryCutout bool
}

// BuildAwaySummary assembles the "while the lid was closed" summary when the lid opens after a
// period that was closed with at least one active assertion; nil when nothing was held at close.
//
// Each assertion held at lid close is partitioned by whether its key is still active at lid open
// (tool-level matching would misfile a finished session as still active whenever another session
// of the same tool is running). Finished durations run to the recorded release time, not to lid
// open — an agent that finished in ten minutes must not be reported as having run all night.
func BuildAwaySummary(p AwayPeriod) *model.AwaySummary {
	if len(p.HeldAtClose) == 0 {
		return nil
	}
	summary := &model.AwaySummary{
		ClosedAt:         p.ClosedAt,
		OpenedAt:         p.OpenedAt,
		Finished:         []model.FinishedAgent{},
		StillActive:      []model.FinishedAgent{},
		ThermalCutout:    p.ThermalCutout,
		LowBatteryCutout: p.LowBatteryCutout,
	}
	if p.PeakTemperature != nil {
		peak := *p.PeakTemperature
		summary.PeakTemperature = &peak
	}
	for _, held := range p.HeldAtClose {
		agent := model.FinishedAgent{Key: held.Key, Tool: held.Tool, DisplayName: held.DisplayName}
		if p.ActiveKeys[held.Key] {
			agent.Duration = p.OpenedAt.Sub(held.AcquiredAt)
			summary.StillActive = append(summary.StillActive, agent)
			continue
		}
		end, ok := p.ReleasedAt[held.Key]
		if !ok {
			end = p.OpenedAt
		}
		agent.Duration = end.Sub(held.AcquiredAt)
		summary.Finished = append(summary.Finished, agent)
	}
	return summary
}
