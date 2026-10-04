package chime

import (
	"bytes"
	"encoding/binary"
	"math"
	"testing"
)

// wavFile is a decoded mono 16-bit PCM WAV.
type wavFile struct {
	sampleRate int
	samples    []float64 // normalized to [-1, 1]
}

func decodeWAV(t *testing.T, data []byte) wavFile {
	t.Helper()
	le := binary.LittleEndian
	if len(data) < 44 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		t.Fatalf("not a RIFF/WAVE file")
	}
	if got := le.Uint32(data[4:8]); int(got) != len(data)-8 {
		t.Fatalf("RIFF size = %d, want %d", got, len(data)-8)
	}
	if string(data[12:16]) != "fmt " || le.Uint32(data[16:20]) != 16 {
		t.Fatalf("missing 16-byte fmt chunk")
	}
	format, channels := le.Uint16(data[20:22]), le.Uint16(data[22:24])
	rate, byteRate := le.Uint32(data[24:28]), le.Uint32(data[28:32])
	blockAlign, bits := le.Uint16(data[32:34]), le.Uint16(data[34:36])
	if format != 1 || channels != 1 || bits != 16 || blockAlign != 2 || byteRate != rate*2 {
		t.Fatalf("format=%d channels=%d bits=%d blockAlign=%d byteRate=%d", format, channels, bits, blockAlign, byteRate)
	}
	if string(data[36:40]) != "data" {
		t.Fatalf("missing data chunk")
	}
	size := int(le.Uint32(data[40:44]))
	if size != len(data)-44 || size%2 != 0 {
		t.Fatalf("data size = %d, file has %d bytes after the header", size, len(data)-44)
	}
	pcm := make([]int16, size/2)
	if err := binary.Read(bytes.NewReader(data[44:]), le, pcm); err != nil {
		t.Fatal(err)
	}
	out := make([]float64, len(pcm))
	for i, s := range pcm {
		out[i] = float64(s) / math.MaxInt16
	}
	return wavFile{sampleRate: int(rate), samples: out}
}

func peak(samples []float64) float64 {
	p := 0.0
	for _, s := range samples {
		p = math.Max(p, math.Abs(s))
	}
	return p
}

func renderPeak(t *testing.T, c Cue, volume float64) float64 {
	t.Helper()
	data, err := RenderWAV(c, volume)
	if err != nil {
		t.Fatal(err)
	}
	return peak(decodeWAV(t, data).samples)
}

func TestChimeSynth(t *testing.T) {
	// Every cue must render a readable, non-silent file of the expected length — a broken motif
	// table (zero-duration segment, all-rest cue) would otherwise fail silently at the moment the
	// cue matters most.
	t.Run("renders a non-silent file of the expected duration", func(t *testing.T) {
		for _, cue := range AllCues() {
			t.Run(string(cue), func(t *testing.T) {
				data, err := RenderWAV(cue, 1)
				if err != nil {
					t.Fatal(err)
				}
				wav := decodeWAV(t, data)
				seconds := float64(len(wav.samples)) / float64(wav.sampleRate)
				// Frame counts truncate per segment, so allow a small tolerance under the sum.
				if math.Abs(seconds-Duration(cue).Seconds()) >= 0.01 {
					t.Errorf("rendered %.4fs, motif is %v", seconds, Duration(cue))
				}
				if seconds <= 0.3 {
					t.Errorf("a cue shorter than ~0.3s won't register across a room (%.3fs)", seconds)
				}
				p := peak(wav.samples)
				if p <= 0.3 {
					t.Errorf("cue %s rendered near-silent (peak %v)", cue, p)
				}
				if p > 1.0 {
					t.Errorf("cue %s clips (peak %v)", cue, p)
				}
			})
		}
	})

	// Volume is baked into the samples (the daemon plays the file at unity gain).
	t.Run("volume scales the rendered samples", func(t *testing.T) {
		full := renderPeak(t, CueSleepWorkComplete, 1)
		half := renderPeak(t, CueSleepWorkComplete, 0.5)
		zero := renderPeak(t, CueSleepWorkComplete, 0)
		if math.Abs(half-full/2) >= 0.01 {
			t.Errorf("half = %v, full = %v", half, full)
		}
		if zero != 0 {
			t.Errorf("zero = %v", zero)
		}
	})
}

// Go-specific behavior.
func TestChimeSynthGo(t *testing.T) {
	t.Run("cue names are the wire values the sleep-cue decision carries", func(t *testing.T) {
		// The daemon converts the decision's cue by value, so these strings are a contract.
		want := []Cue{"lidClose", "sleepWorkComplete", "sleepHoldExpired", "sleepSafetyCutout", "sleepUserAction"}
		if got := AllCues(); len(got) != len(want) {
			t.Fatalf("AllCues() = %v", got)
		}
		for i, c := range AllCues() {
			if c != want[i] {
				t.Errorf("AllCues()[%d] = %q, want %q", i, c, want[i])
			}
		}
	})

	t.Run("an unknown cue is an error, not a panic", func(t *testing.T) {
		if _, err := RenderWAV("nope", 1); err == nil {
			t.Fatal("expected an error")
		}
		if Duration("nope") != 0 {
			t.Fatal("unknown cue has no duration")
		}
	})

	t.Run("volume is clamped and NaN reads as silence", func(t *testing.T) {
		if got, want := renderPeak(t, CueLidClose, 7), renderPeak(t, CueLidClose, 1); got != want {
			t.Errorf("volume 7 peak = %v, want %v", got, want)
		}
		if got := renderPeak(t, CueLidClose, -1); got != 0 {
			t.Errorf("volume -1 peak = %v", got)
		}
		if got := renderPeak(t, CueLidClose, math.NaN()); got != 0 {
			t.Errorf("volume NaN peak = %v", got)
		}
	})

	t.Run("frame counts sum per segment", func(t *testing.T) {
		f := func(seconds ...float64) int {
			n := 0
			for _, s := range seconds {
				n += int(s * SampleRate)
			}
			return n
		}
		want := map[Cue]int{
			CueLidClose:          f(0.18, 0.22),
			CueSleepWorkComplete: f(0.16, 0.16, 0.38),
			CueSleepHoldExpired:  f(0.15, 0.08, 0.28),
			CueSleepSafetyCutout: f(0.14, 0.20, 0.10, 0.14, 0.26),
			CueSleepUserAction:   f(0.14, 0.32),
		}
		for cue, n := range want {
			s, err := Samples(cue, 1)
			if err != nil || len(s) != n {
				t.Errorf("%s: %d samples (err %v), want %d", cue, len(s), err, n)
			}
		}
	})

	t.Run("cues start and end at silence (no clicks)", func(t *testing.T) {
		for _, cue := range AllCues() {
			s, _ := Samples(cue, 1)
			if s[0] != 0 || math.Abs(s[len(s)-1]) > 1e-3 {
				t.Errorf("%s: first %v last %v", cue, s[0], s[len(s)-1])
			}
		}
	})

	t.Run("lid-close runs cooler than the pre-sleep cues", func(t *testing.T) {
		lid := renderPeak(t, CueLidClose, 1)
		for _, cue := range AllCues()[1:] {
			if p := renderPeak(t, cue, 1); p <= lid {
				t.Errorf("%s peak %v <= lid-close peak %v", cue, p, lid)
			}
		}
	})
}
