package darwin

import (
	"encoding/binary"
	"math"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestDeviceCapabilities(t *testing.T) {
	t.Run("laptop is not desktop", func(t *testing.T) {
		if (DeviceCapabilities{HasLid: true, HasBattery: true}).IsDesktop() {
			t.Fatal("a Mac with a lid and a battery was labeled a desktop")
		}
	})
	t.Run("both absent is desktop", func(t *testing.T) {
		if !(DeviceCapabilities{HasLid: false, HasBattery: false}).IsDesktop() {
			t.Fatal("a Mac with neither a lid nor a battery was not labeled a desktop")
		}
	})
	t.Run("either signal present is not desktop", func(t *testing.T) {
		// A laptop whose lid probe transiently misses still has a battery, so it must not be
		// mislabeled a desktop (and vice versa).
		if (DeviceCapabilities{HasLid: false, HasBattery: true}).IsDesktop() {
			t.Error("battery only was labeled a desktop")
		}
		if (DeviceCapabilities{HasLid: true, HasBattery: false}).IsDesktop() {
			t.Error("lid only was labeled a desktop")
		}
	})
}

// countingProbe answers from results in order, repeating the last one, and counts the calls.
type countingProbe struct {
	results []bool
	calls   int
}

func (p *countingProbe) probe() bool {
	r := p.results[min(p.calls, len(p.results)-1)]
	p.calls++
	return r
}

func TestCapabilityProbe(t *testing.T) {
	t.Run("a missed battery probe is retried", func(t *testing.T) {
		// powerd restarting while the daemon starts: the first probe finds no battery.
		battery := &countingProbe{results: []bool{false, true}}
		p := &capabilityProbe{hasLid: func() bool { return true }, hasBattery: battery.probe}
		if p.get().HasBattery {
			t.Fatal("first probe: HasBattery true, want the missed probe's false")
		}
		if !p.get().HasBattery {
			t.Fatal("a missed battery probe was cached: HasBattery still false after the battery appeared")
		}
	})
	t.Run("a missed lid probe is retried", func(t *testing.T) {
		lid := &countingProbe{results: []bool{false, true}}
		p := &capabilityProbe{hasLid: lid.probe, hasBattery: func() bool { return true }}
		p.get()
		if !p.get().HasLid {
			t.Fatal("a missed lid probe was cached")
		}
	})
	t.Run("a found capability is kept without probing again", func(t *testing.T) {
		lid := &countingProbe{results: []bool{true, false}}
		battery := &countingProbe{results: []bool{true, false}}
		p := &capabilityProbe{hasLid: lid.probe, hasBattery: battery.probe}
		want := DeviceCapabilities{HasLid: true, HasBattery: true}
		for i := range 3 {
			if got := p.get(); got != want {
				t.Fatalf("call %d: %+v, want %+v", i, got, want)
			}
		}
		if lid.calls != 1 || battery.calls != 1 {
			t.Fatalf("probed lid %d and battery %d times, want once each", lid.calls, battery.calls)
		}
	})
	t.Run("a desktop is probed on every call", func(t *testing.T) {
		lid := &countingProbe{results: []bool{false}}
		battery := &countingProbe{results: []bool{false}}
		p := &capabilityProbe{hasLid: lid.probe, hasBattery: battery.probe}
		for range 2 {
			if !p.get().IsDesktop() {
				t.Fatal("a Mac with neither a lid nor a battery was not labeled a desktop")
			}
		}
		if lid.calls != 2 || battery.calls != 2 {
			t.Fatalf("probed lid %d and battery %d times, want twice each", lid.calls, battery.calls)
		}
	})
}

func TestAwaitSleepDisabled(t *testing.T) {
	t.Run("an applied value returns at once", func(t *testing.T) {
		kernel := &countingProbe{results: []bool{true}}
		if err := awaitSleepDisabled(true, kernel.probe, time.Second, time.Millisecond); err != nil {
			t.Fatal(err)
		}
		if kernel.calls != 1 {
			t.Fatalf("read the flag %d times, want once", kernel.calls)
		}
	})
	t.Run("waits for powerd to apply the value pmset wrote", func(t *testing.T) {
		// pmset -a disablesleep 0 has exited but powerd has not applied it yet: the flag still
		// reads 1. Returning now would let the next block save that 1 as the original setting.
		kernel := &countingProbe{results: []bool{true, true, true, false}}
		if err := awaitSleepDisabled(false, kernel.probe, 5*time.Second, time.Millisecond); err != nil {
			t.Fatal(err)
		}
		if kernel.calls != 4 {
			t.Fatalf("returned after %d reads, want after the flag changed on the 4th", kernel.calls)
		}
	})
	t.Run("a flag that never changes is an error", func(t *testing.T) {
		start := time.Now()
		err := awaitSleepDisabled(true, func() bool { return false }, 50*time.Millisecond, time.Millisecond)
		if err == nil || !strings.Contains(err.Error(), "still reads false") {
			t.Fatalf("err = %v, want the flag that did not change", err)
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("took %v to give up", elapsed)
		}
	})
}

func TestDecodeTemperature(t *testing.T) {
	flt := func(v float32) [4]byte {
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], math.Float32bits(v))
		return b
	}
	tests := []struct {
		name   string
		bytes  [4]byte
		typ    string
		want   float64
		wantOK bool
	}{
		{"sp78 positive", [4]byte{0x3c, 0x80}, "sp78", 60.5, true},
		{"sp78 negative", [4]byte{0xff, 0x00}, "sp78", -1, true},
		{"flt little-endian", flt(45.25), "flt ", 45.25, true},
		{"unknown type", [4]byte{1, 2, 3, 4}, "ui32", 0, false},
		{"empty type", [4]byte{}, "", 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := decodeTemperature(tc.bytes, tc.typ)
			if ok != tc.wantOK || got != tc.want {
				t.Fatalf("decodeTemperature = %v, %v; want %v, %v", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestFourCC(t *testing.T) {
	code, ok := fourCharCode("TC0P")
	if !ok || code != 0x54433050 {
		t.Fatalf("fourCharCode(TC0P) = %#x, %v", code, ok)
	}
	if got := decodeFourCC(code); got != "TC0P" {
		t.Fatalf("decodeFourCC round trip = %q", got)
	}
	if got := decodeFourCC(0x666c7420); got != "flt " {
		t.Fatalf("decodeFourCC(flt ) = %q", got)
	}
	if _, ok := fourCharCode("TC0"); ok {
		t.Error("a three-character key was accepted")
	}
	if _, ok := fourCharCode("TC0PX"); ok {
		t.Error("a five-character key was accepted")
	}
	if got := decodeFourCC(0xff414243); got != "" {
		t.Errorf("non-ASCII code decoded to %q, want empty", got)
	}
}

type fakeSensor struct {
	typ   string
	bytes [4]byte
}

// fakeSMC is an in-memory SMC: keys in index order, plus their types and first data bytes.
type fakeSMC struct {
	order   []string
	sensors map[string]fakeSensor
}

func (f *fakeSMC) keyInfo(key string) (uint32, string, bool) {
	if key == "#KEY" {
		return 4, "ui32", true
	}
	s, ok := f.sensors[key]
	if !ok {
		return 0, "", false
	}
	return 4, s.typ, true
}

func (f *fakeSMC) readKey(key string, size uint32) ([4]byte, bool) {
	if key == "#KEY" {
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], uint32(len(f.order)))
		return b, true
	}
	s, ok := f.sensors[key]
	return s.bytes, ok
}

func (f *fakeSMC) keyAt(index uint32) (string, bool) {
	if int(index) >= len(f.order) {
		return "", false
	}
	return f.order[index], true
}

func fltSensor(v float32) fakeSensor {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], math.Float32bits(v))
	return fakeSensor{typ: "flt ", bytes: b}
}

func sp78Sensor(v float64) fakeSensor {
	raw := uint16(int16(v * 256))
	return fakeSensor{typ: "sp78", bytes: [4]byte{byte(raw >> 8), byte(raw)}}
}

func TestSMCSensorDiscovery(t *testing.T) {
	t.Run("apple silicon averages plausible core sensors", func(t *testing.T) {
		f := &fakeSMC{
			order: []string{"#KEY", "TaLP", "Te05", "Tp01", "Tp09", "Tp0X", "TC0P"},
			sensors: map[string]fakeSensor{
				"TaLP": fltSensor(60),  // not a CPU core sensor
				"Te05": fltSensor(40),  // efficiency core
				"Tp01": fltSensor(50),  // performance core
				"Tp09": fltSensor(200), // implausible: dead sensor
				"Tp0X": sp78Sensor(55), // wrong format for Apple Silicon
				"TC0P": sp78Sensor(70), // Intel key, unused once core sensors exist
			},
		}
		keys := discoverCPUSensors(f)
		if want := []string{"Te05", "Tp01"}; !slices.Equal(keys, want) {
			t.Fatalf("discovered %v, want %v", keys, want)
		}
		got, ok := averageTemperature(f, keys)
		if !ok || got != 45 {
			t.Fatalf("average = %v, %v; want 45", got, ok)
		}
	})
	t.Run("intel falls back to the proximity keys", func(t *testing.T) {
		f := &fakeSMC{
			order: []string{"#KEY", "TC0P", "TC0E"},
			sensors: map[string]fakeSensor{
				"TC0P": sp78Sensor(50),
				"TC0E": sp78Sensor(54),
			},
		}
		keys := discoverCPUSensors(f)
		if want := []string{"TC0P", "TC0E"}; !slices.Equal(keys, want) {
			t.Fatalf("discovered %v, want %v", keys, want)
		}
		got, ok := averageTemperature(f, keys)
		if !ok || got != 52 {
			t.Fatalf("average = %v, %v; want 52", got, ok)
		}
	})
	t.Run("implausible readings are left out of the average", func(t *testing.T) {
		f := &fakeSMC{sensors: map[string]fakeSensor{"Tp01": fltSensor(50), "Tp02": fltSensor(2)}}
		if got, ok := averageTemperature(f, []string{"Tp01", "Tp02", "Tp03"}); !ok || got != 50 {
			t.Fatalf("average = %v, %v; want 50", got, ok)
		}
		if _, ok := averageTemperature(f, []string{"Tp02"}); ok {
			t.Fatal("an implausible-only reading was reported")
		}
		if _, ok := averageTemperature(f, nil); ok {
			t.Fatal("no sensors reported a temperature")
		}
	})
	t.Run("empty discovery is retried, a successful one is cached", func(t *testing.T) {
		f := &fakeSMC{order: []string{"#KEY"}, sensors: map[string]fakeSensor{}}
		s := &SMC{}
		if _, ok := s.cpuTemperatureVia(f); ok {
			t.Fatal("reported a temperature with no sensors")
		}
		if s.keys != nil {
			t.Fatalf("cached an empty discovery: %v", s.keys)
		}
		f.order = append(f.order, "Tp01")
		f.sensors["Tp01"] = fltSensor(48)
		if got, ok := s.cpuTemperatureVia(f); !ok || got != 48 {
			t.Fatalf("after the sensor appeared: %v, %v", got, ok)
		}
		if !slices.Equal(s.keys, []string{"Tp01"}) {
			t.Fatalf("cached keys = %v", s.keys)
		}
		// The cache stands: a new sensor is not picked up without a rediscovery.
		f.order = append(f.order, "Tp02")
		f.sensors["Tp02"] = fltSensor(52)
		if got, _ := s.cpuTemperatureVia(f); got != 48 {
			t.Fatalf("rediscovered on a cached read: %v", got)
		}
	})
}

// procArgs2 builds a KERN_PROCARGS2 blob.
func procArgs2(argc int32, exe string, pad int, rest ...string) []byte {
	b := binary.NativeEndian.AppendUint32(nil, uint32(argc))
	b = append(b, exe...)
	b = append(b, make([]byte, 1+pad)...)
	for _, s := range rest {
		b = append(b, s...)
		b = append(b, 0)
	}
	return b
}

func TestParseProcArgs2(t *testing.T) {
	tests := []struct {
		name    string
		buf     []byte
		want    []string
		wantErr bool
	}{
		{"argv without the environment", procArgs2(2, "/bin/sleep", 5, "sleep", "5", "PATH=/usr/bin", "HOME=/x"), []string{"sleep", "5"}, false},
		{"no padding", procArgs2(1, "/bin/ls", 0, "ls", "TERM=x"), []string{"ls"}, false},
		{"empty argument kept", procArgs2(3, "/bin/sh", 2, "sh", "", "x"), []string{"sh", "", "x"}, false},
		{"truncated blob returns what it has", procArgs2(4, "/bin/sh", 1, "sh", "-c"), []string{"sh", "-c"}, false},
		{"zero argc", procArgs2(0, "/bin/sh", 1, "sh"), nil, true},
		{"negative argc", procArgs2(-1, "/bin/sh", 1, "sh"), nil, true},
		{"short buffer", []byte{1, 0, 0, 0}, nil, true},
		{"unterminated argument", append(procArgs2(1, "/bin/sh", 1), "sh"...), nil, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseProcArgs2(tc.buf)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("args = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTicksToDuration(t *testing.T) {
	tests := []struct {
		name         string
		ticks        uint64
		numer, denom uint32
		want         time.Duration
	}{
		{"apple silicon 24 MHz timebase", 24_000_000, 125, 3, time.Second},
		{"intel 1:1 timebase", 1_500_000_000, 1, 1, 1500 * time.Millisecond},
		{"zero", 0, 125, 3, 0},
		{"large values do not overflow the product", 1 << 62, 125, 3, time.Duration(math.MaxInt64)},
		{"product past 64 bits still divides", 1 << 62, 8, 16, time.Duration(1 << 61)},
		{"zero denominator falls back to 1:1", 42, 0, 0, 42},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ticksToDuration(tc.ticks, tc.numer, tc.denom); got != tc.want {
				t.Fatalf("ticksToDuration = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRunWithWatchdog(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		if err := runWithWatchdog("/usr/bin/true", nil, 5*time.Second); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("stderr becomes the error", func(t *testing.T) {
		err := runWithWatchdog("/bin/sh", []string{"-c", "echo boom >&2; exit 3"}, 5*time.Second)
		if err == nil || !strings.Contains(err.Error(), "boom") {
			t.Fatalf("err = %v, want the stderr text", err)
		}
	})
	t.Run("silent failure reports the exit status", func(t *testing.T) {
		err := runWithWatchdog("/bin/sh", []string{"-c", "exit 3"}, 5*time.Second)
		if err == nil || !strings.Contains(err.Error(), "exited 3") {
			t.Fatalf("err = %v, want the exit status", err)
		}
	})
	t.Run("a wedged tool is killed at the deadline", func(t *testing.T) {
		start := time.Now()
		err := runWithWatchdog("/bin/sleep", []string{"30"}, 200*time.Millisecond)
		if err == nil || !strings.Contains(err.Error(), "did not exit") {
			t.Fatalf("err = %v, want a timeout", err)
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("took %v to give up", elapsed)
		}
	})
	t.Run("a tool ignoring SIGTERM is killed after the grace period", func(t *testing.T) {
		start := time.Now()
		err := runWithWatchdog("/bin/sh", []string{"-c", "trap '' TERM; exec sleep 30"}, 200*time.Millisecond)
		if err == nil {
			t.Fatal("no error for a wedged tool")
		}
		if elapsed := time.Since(start); elapsed > 8*time.Second {
			t.Fatalf("took %v to give up", elapsed)
		}
	})
	t.Run("missing executable", func(t *testing.T) {
		if err := runWithWatchdog("/nonexistent/pmset", nil, time.Second); err == nil {
			t.Fatal("no error for a missing executable")
		}
	})
}
