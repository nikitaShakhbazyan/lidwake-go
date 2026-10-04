// Package chime is lidwake's audio: cues synthesized at runtime (there are no bundled audio
// assets) and a player that hands them, or a named macOS system sound, to /usr/bin/afplay.
package chime

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"time"
)

// Cue is a distinct synthesized cue. Each is a short sine-tone motif whose contour carries the
// meaning, so they stay tellable apart from across a room.
type Cue string

const (
	// CueLidClose: two descending tones (G5 → D5, ~0.4 s) — "your Mac is staying awake".
	CueLidClose Cue = "lidClose"
	// CueSleepWorkComplete: a descending G-major arpeggio (D6 → B5 → G5, ~0.7 s) — "the agents
	// finished; goodnight". The happy path, warm and resolved.
	CueSleepWorkComplete Cue = "sleepWorkComplete"
	// CueSleepHoldExpired: the same note twice (A5, ~0.5 s) — neutral, timer-like; the hold ran
	// out, the work may not be done.
	CueSleepHoldExpired Cue = "sleepHoldExpired"
	// CueSleepSafetyCutout: a tritone drop played twice (C5 → F#4, ~0.85 s) — deliberately
	// unresolved; a safety cutout stopped the work mid-task.
	CueSleepSafetyCutout Cue = "sleepSafetyCutout"
	// CueSleepUserAction: a rising fourth (E5 → A5, ~0.45 s) — a short "acknowledged" for a
	// release the user commanded (a force release over SSH against a closed lid).
	CueSleepUserAction Cue = "sleepUserAction"
)

// AllCues lists every cue.
func AllCues() []Cue {
	return []Cue{CueLidClose, CueSleepWorkComplete, CueSleepHoldExpired, CueSleepSafetyCutout, CueSleepUserAction}
}

// SampleRate is the rate every cue is rendered at.
const SampleRate = 44_100

// segment is one note (or rest) of a cue; frequency 0 renders silence. Durations are seconds.
type segment struct {
	frequency float64
	duration  float64
}

// Note frequencies (Hz, equal temperament).
const (
	g5      = 783.99
	d5      = 587.33
	d6      = 1_174.66
	b5      = 987.77
	a5      = 880.0
	c5      = 523.25
	fSharp4 = 369.99
	e5      = 659.26
)

func segments(c Cue) []segment {
	switch c {
	case CueLidClose:
		return []segment{{g5, 0.18}, {d5, 0.22}}
	case CueSleepWorkComplete:
		return []segment{{d6, 0.16}, {b5, 0.16}, {g5, 0.38}}
	case CueSleepHoldExpired:
		return []segment{{a5, 0.15}, {0, 0.08}, {a5, 0.28}}
	case CueSleepSafetyCutout:
		return []segment{{c5, 0.14}, {fSharp4, 0.20}, {0, 0.10}, {c5, 0.14}, {fSharp4, 0.26}}
	case CueSleepUserAction:
		return []segment{{e5, 0.14}, {a5, 0.32}}
	}
	return nil
}

// gain is the peak gain at volume 1. The pre-sleep cues run hotter than the lid-close chime: they
// play to a user who may be across the room, while the lid-close chime plays under the user's
// hands.
func gain(c Cue) float64 {
	if c == CueLidClose {
		return 0.6
	}
	return 0.85
}

// Duration is the nominal length of c's motif (0 for an unknown cue). The rendered audio can be a
// few samples shorter: frame counts truncate per segment.
func Duration(c Cue) time.Duration {
	total := 0.0
	for _, s := range segments(c) {
		total += s.duration
	}
	return time.Duration(math.Round(total * float64(time.Second)))
}

// Samples renders c as normalized samples in [-1, 1]: concatenated sine segments with short
// attack/release envelopes so they do not click. volume (0…1, clamped; NaN reads as 0) is baked
// into the samples, so the file plays at unity gain.
func Samples(c Cue, volume float64) ([]float64, error) {
	segs := segments(c)
	if segs == nil {
		return nil, fmt.Errorf("chime: unknown cue %q", c)
	}
	// Sum per-segment integer frame counts (rather than truncating the summed duration once) so
	// every frame is written below.
	counts := make([]int, len(segs))
	frames := 0
	for i, s := range segs {
		counts[i] = int(s.duration * SampleRate)
		frames += counts[i]
	}
	if frames <= 0 {
		return nil, fmt.Errorf("chime: cue %q has no frames", c)
	}
	g := clampVolume(volume) * gain(c)
	out := make([]float64, 0, frames)
	for i, s := range segs {
		count := counts[i]
		for n := 0; n < count; n++ {
			if s.frequency <= 0 {
				out = append(out, 0)
				continue
			}
			t := float64(n) / SampleRate
			out = append(out, math.Sin(2.0*math.Pi*s.frequency*t)*envelope(n, count)*g)
		}
	}
	return out, nil
}

// Envelope ramps: 8 ms attack, 40 ms release (whole frames).
var (
	attackFrames  = max(1, int(math.Floor(0.008*SampleRate)))
	releaseFrames = max(1, int(math.Floor(0.04*SampleRate)))
)

func envelope(i, count int) float64 {
	if i < attackFrames {
		return float64(i) / float64(attackFrames)
	}
	if i > count-releaseFrames {
		return float64(count-i) / float64(releaseFrames)
	}
	return 1.0
}

// RenderWAV renders c at volume as a mono 16-bit PCM WAV file at SampleRate.
func RenderWAV(c Cue, volume float64) ([]byte, error) {
	samples, err := Samples(c, volume)
	if err != nil {
		return nil, err
	}
	pcm := make([]int16, len(samples))
	for i, s := range samples {
		pcm[i] = int16(math.Round(math.Max(-1, math.Min(1, s)) * math.MaxInt16))
	}
	return encodeWAV(pcm, SampleRate), nil
}

// encodeWAV writes a canonical 44-byte-header RIFF/WAVE file: one fmt chunk (PCM, mono, 16-bit)
// and one data chunk.
func encodeWAV(pcm []int16, rate int) []byte {
	const channels, bitsPerSample = 1, 16
	dataSize := len(pcm) * 2
	var b bytes.Buffer
	b.Grow(44 + dataSize)
	le := binary.LittleEndian
	b.WriteString("RIFF")
	_ = binary.Write(&b, le, uint32(36+dataSize))
	b.WriteString("WAVE")
	b.WriteString("fmt ")
	_ = binary.Write(&b, le, uint32(16))                            // fmt chunk size
	_ = binary.Write(&b, le, uint16(1))                             // PCM
	_ = binary.Write(&b, le, uint16(channels))                      // channels
	_ = binary.Write(&b, le, uint32(rate))                          // sample rate
	_ = binary.Write(&b, le, uint32(rate*channels*bitsPerSample/8)) // byte rate
	_ = binary.Write(&b, le, uint16(channels*bitsPerSample/8))      // block align
	_ = binary.Write(&b, le, uint16(bitsPerSample))                 // bits per sample
	b.WriteString("data")
	_ = binary.Write(&b, le, uint32(dataSize))
	_ = binary.Write(&b, le, pcm)
	return b.Bytes()
}

func clampVolume(v float64) float64 {
	if math.IsNaN(v) {
		return 0
	}
	return math.Max(0, math.Min(1, v))
}
