package monitor

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/nikitaShakhbazyan/lidwake-go/internal/settings"
)

// fakeSensor is a settable temperature sensor.
type fakeSensor struct {
	mu     sync.Mutex
	temp   float64
	ok     bool
	reads  int
	closed bool
}

func (s *fakeSensor) set(temp float64, ok bool) {
	s.mu.Lock()
	s.temp, s.ok = temp, ok
	s.mu.Unlock()
}

func (s *fakeSensor) CPUTemperature() (float64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	return s.temp, s.ok && !s.closed
}

func (s *fakeSensor) Close() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
}

func (s *fakeSensor) readCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reads
}

func (s *fakeSensor) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// sensorOpener hands out sensor after failing the first `failures` opens.
type sensorOpener struct {
	mu       sync.Mutex
	sensor   *fakeSensor
	failures int
	opens    int
}

func (o *sensorOpener) open() (TemperatureSensor, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.opens++
	if o.opens <= o.failures {
		return nil, errors.New("no AppleSMC")
	}
	return o.sensor, nil
}

func (o *sensorOpener) openCount() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.opens
}

func newTestThermal(s *fakeSensor, interval time.Duration) (*ThermalMonitor, *recorder[float64], *counter) {
	var readings recorder[float64]
	var cuts counter
	o := &sensorOpener{sensor: s}
	m := &ThermalMonitor{Open: o.open, Interval: interval, OnReading: readings.add, OnCutout: cuts.inc, Log: quiet()}
	return m, &readings, &cuts
}

func TestThermalMonitor(t *testing.T) {
	t.Run("does not poll until blocking", func(t *testing.T) {
		s := &fakeSensor{}
		s.set(50, true)
		m, readings, _ := newTestThermal(s, time.Millisecond)
		start(t, m)
		stays(t, "no reads while idle", 30*time.Millisecond, func() bool { return s.readCount() == 0 })
		if readings.len() != 0 {
			t.Error("readings while idle")
		}
	})

	t.Run("blocking begins with a fresh reading at once", func(t *testing.T) {
		s := &fakeSensor{}
		s.set(50, true)
		m, readings, _ := newTestThermal(s, time.Hour)
		start(t, m)
		m.SetBlocking(true)
		eventually(t, "a reading", func() bool { return readings.len() == 1 })
		if c, ok := m.Last(); !ok || c != 50 {
			t.Errorf("Last = %v %v", c, ok)
		}
	})

	t.Run("polls while blocking and stops when blocking ends", func(t *testing.T) {
		s := &fakeSensor{}
		s.set(50, true)
		m, readings, _ := newTestThermal(s, time.Millisecond)
		start(t, m)
		m.SetBlocking(true)
		eventually(t, "polls", func() bool { return readings.len() > 5 })
		m.SetBlocking(false)
		eventually(t, "reads to stop", func() bool {
			n := s.readCount()
			time.Sleep(5 * time.Millisecond)
			return s.readCount() == n
		})
		n := s.readCount()
		stays(t, "no reads after blocking ends", 20*time.Millisecond, func() bool { return s.readCount() == n })
		m.SetBlocking(true)
		eventually(t, "polls again", func() bool { return s.readCount() > n+2 })
	})

	t.Run("cuts out at the threshold with the lid closed while blocking", func(t *testing.T) {
		s := &fakeSensor{}
		s.set(80, true) // the default threshold
		m, readings, cuts := newTestThermal(s, time.Hour)
		m.SetLidClosed(true)
		start(t, m)
		m.SetBlocking(true)
		eventually(t, "cutout", func() bool { return cuts.get() == 1 })
		if readings.len() != 1 {
			t.Errorf("readings = %d", readings.len())
		}
	})

	t.Run("no cutout with the lid open, below the threshold or when disabled", func(t *testing.T) {
		for name, tc := range map[string]struct {
			temp      float64
			lidClosed bool
			enabled   bool
			threshold float64
		}{
			"lid open":            {95, false, true, 80},
			"below the threshold": {79.9, true, true, 80},
			"disabled":            {95, true, false, 80},
			"raised threshold":    {90, true, true, 95},
		} {
			t.Run(name, func(t *testing.T) {
				s := &fakeSensor{}
				s.set(tc.temp, true)
				m, readings, cuts := newTestThermal(s, time.Millisecond)
				st := settings.Defaults()
				st.ThermalCutoutEnabled = tc.enabled
				st.ThermalThresholdCelsius = tc.threshold
				m.ApplySettings(st)
				m.SetLidClosed(tc.lidClosed)
				m.SetBlocking(true)
				start(t, m)
				eventually(t, "readings", func() bool { return readings.len() > 3 })
				if cuts.get() != 0 {
					t.Errorf("cut out %d times", cuts.get())
				}
			})
		}
	})

	t.Run("ReadNow refreshes Last without callbacks and falls back to Last", func(t *testing.T) {
		s := &fakeSensor{}
		s.set(61.5, true)
		m, readings, cuts := newTestThermal(s, time.Hour)
		m.SetLidClosed(true)
		start(t, m)
		if _, ok := m.Last(); ok {
			t.Fatal("a reading before any read")
		}
		if c, ok := m.ReadNow(); !ok || c != 61.5 {
			t.Fatalf("ReadNow = %v %v", c, ok)
		}
		s.set(0, false)
		if c, ok := m.ReadNow(); !ok || c != 61.5 {
			t.Errorf("ReadNow on a failed read = %v %v, want the last reading", c, ok)
		}
		if readings.len() != 0 || cuts.get() != 0 {
			t.Error("ReadNow fired callbacks")
		}
	})

	t.Run("Current serves the polled reading while blocking and reads fresh otherwise", func(t *testing.T) {
		s := &fakeSensor{}
		s.set(60, true)
		m, readings, _ := newTestThermal(s, time.Hour)
		start(t, m)
		if c, ok := m.Current(); !ok || c != 60 || s.readCount() != 1 {
			t.Fatalf("idle Current = %v %v after %d reads, want a fresh 60", c, ok, s.readCount())
		}
		m.SetBlocking(true)
		eventually(t, "the blocking reading", func() bool { return readings.len() == 1 })
		s.set(70, true)
		n := s.readCount()
		if c, ok := m.Current(); !ok || c != 60 || s.readCount() != n {
			t.Errorf("blocking Current = %v %v with %d extra reads, want the polled 60 and no read", c, ok, s.readCount()-n)
		}
		m.SetBlocking(false)
		eventually(t, "a fresh read once idle", func() bool { c, ok := m.Current(); return ok && c == 70 })
	})

	t.Run("a failed open is retried on later reads", func(t *testing.T) {
		s := &fakeSensor{}
		s.set(70, true)
		o := &sensorOpener{sensor: s, failures: 3}
		var readings recorder[float64]
		m := &ThermalMonitor{Open: o.open, Interval: time.Millisecond, OnReading: readings.add, Log: quiet()}
		start(t, m) // open #1 fails
		if _, ok := m.ReadNow(); ok {
			t.Fatal("read without a sensor")
		}
		m.SetBlocking(true)
		eventually(t, "a reading once the SMC opens", func() bool { return readings.len() > 0 })
		opens := o.openCount()
		eventually(t, "more readings", func() bool { return readings.len() > 3 })
		if o.openCount() != opens {
			t.Error("reopened an open sensor")
		}
	})

	t.Run("stop closes the sensor and ReadNow keeps the last reading", func(t *testing.T) {
		s := &fakeSensor{}
		s.set(55, true)
		o := &sensorOpener{sensor: s}
		m := &ThermalMonitor{Open: o.open, Interval: time.Hour, Log: quiet()}
		start(t, m)
		if _, ok := m.ReadNow(); !ok {
			t.Fatal("no reading")
		}
		m.Stop()
		if !s.isClosed() {
			t.Error("sensor left open")
		}
		if c, ok := m.ReadNow(); !ok || c != 55 {
			t.Errorf("ReadNow after Stop = %v %v", c, ok)
		}
		if o.openCount() != 1 {
			t.Errorf("opened %d times; Stop must not reopen", o.openCount())
		}
	})
}
