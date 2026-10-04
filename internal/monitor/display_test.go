package monitor

import (
	"errors"
	"sync"
	"testing"
)

type fakeAssertion struct {
	mu       sync.Mutex
	releases int
}

func (a *fakeAssertion) Release() {
	a.mu.Lock()
	a.releases++
	a.mu.Unlock()
}

func (a *fakeAssertion) releaseCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.releases
}

type fakeDisplay struct {
	mu         sync.Mutex
	created    []*fakeAssertion
	names      []string
	failCreate int // fail this many creates first
	wakes      int
	onlyClosed bool
}

func (d *fakeDisplay) create(name string) (AssertionHandle, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.names = append(d.names, name)
	if d.failCreate > 0 {
		d.failCreate--
		return nil, errors.New("IOReturn 0xe00002c2")
	}
	a := &fakeAssertion{}
	d.created = append(d.created, a)
	return a, nil
}

func (d *fakeDisplay) declare(string) error {
	d.mu.Lock()
	d.wakes++
	d.mu.Unlock()
	return nil
}

func (d *fakeDisplay) counts() (created, wakes int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.created), d.wakes
}

func (d *fakeDisplay) hold() *DisplayHold {
	return &DisplayHold{
		Create:              d.create,
		DeclareUserActivity: d.declare,
		OnlyDisplayIsClosedBuiltIn: func(lidClosed bool) bool {
			d.mu.Lock()
			defer d.mu.Unlock()
			return lidClosed && d.onlyClosed
		},
		Log: quiet(),
	}
}

func TestDisplayHold(t *testing.T) {
	t.Run("raises once and relights the display on every set", func(t *testing.T) {
		d := &fakeDisplay{}
		h := d.hold()
		h.Set(true)
		h.Set(true)
		h.Set(true) // the reconcile
		if created, wakes := d.counts(); created != 1 || wakes != 3 {
			t.Fatalf("created %d, woke %d; want 1 and 3", created, wakes)
		}
		if !h.Held() {
			t.Error("not held")
		}
	})

	t.Run("drops once and can raise again", func(t *testing.T) {
		d := &fakeDisplay{}
		h := d.hold()
		h.Set(false) // nothing held: nothing to do
		h.Set(true)
		h.Set(false)
		h.Set(false)
		if h.Held() || d.created[0].releaseCount() != 1 {
			t.Fatalf("held=%v releases=%d", h.Held(), d.created[0].releaseCount())
		}
		h.Set(true)
		if created, _ := d.counts(); created != 2 || !h.Held() {
			t.Errorf("created %d, held %v", created, h.Held())
		}
	})

	t.Run("a failed raise is retried on the next set", func(t *testing.T) {
		d := &fakeDisplay{failCreate: 1}
		h := d.hold()
		h.Set(true)
		if created, wakes := d.counts(); h.Held() || created != 0 || wakes != 0 {
			t.Fatalf("held=%v created=%d wakes=%d after a failed raise", h.Held(), created, wakes)
		}
		h.Set(true)
		if !h.Held() {
			t.Error("not retried")
		}
	})

	t.Run("a failed relight keeps the hold", func(t *testing.T) {
		d := &fakeDisplay{}
		h := d.hold()
		var wakes counter
		h.DeclareUserActivity = func(string) error { wakes.inc(); return errors.New("IOReturn 0xe00002bc") }
		h.Set(true)
		h.Set(true)
		if !h.Held() || wakes.get() != 2 {
			t.Errorf("held=%v wakes=%d", h.Held(), wakes.get())
		}
	})

	t.Run("stop releases and ignores later sets", func(t *testing.T) {
		d := &fakeDisplay{}
		h := d.hold()
		h.Set(true)
		h.Stop()
		if h.Held() || d.created[0].releaseCount() != 1 {
			t.Fatal("not released by Stop")
		}
		h.Set(true)
		if created, _ := d.counts(); created != 1 || h.Held() {
			t.Error("raised after Stop")
		}
		h.Stop()
	})

	t.Run("the assertion name is ASCII", func(t *testing.T) {
		d := &fakeDisplay{}
		d.hold().Set(true)
		for _, name := range append(d.names, displayWakeName) {
			for i := 0; i < len(name); i++ {
				if name[i] >= 0x80 {
					t.Errorf("non-ASCII name %q (pmset shows it as ?)", name)
					break
				}
			}
		}
	})

	t.Run("warns only for a wanted hold on a closed built-in panel with no other display", func(t *testing.T) {
		for _, tc := range []struct {
			wanted, lidClosed, onlyClosed bool
			warn                          bool
		}{
			{true, true, true, true},
			{true, true, false, false}, // an external display is a real target
			{true, false, true, false},
			{false, true, true, false},
		} {
			d := &fakeDisplay{onlyClosed: tc.onlyClosed}
			got := d.hold().Warning(tc.wanted, tc.lidClosed)
			if (got != "") != tc.warn {
				t.Errorf("wanted=%v lid=%v only=%v: %q", tc.wanted, tc.lidClosed, tc.onlyClosed, got)
			}
			if tc.warn && got != DisplayBlindWarning {
				t.Errorf("warning = %q", got)
			}
		}
	})

	t.Run("safe for concurrent use", func(t *testing.T) {
		d := &fakeDisplay{}
		h := d.hold()
		var wg sync.WaitGroup
		for i := range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := range 50 {
					h.Set((i+j)%2 == 0)
					h.Held()
				}
			}()
		}
		wg.Wait()
		h.Set(false)
		d.mu.Lock()
		defer d.mu.Unlock()
		for _, a := range d.created {
			if a.releaseCount() != 1 {
				t.Fatalf("an assertion released %d times", a.releaseCount())
			}
		}
	})
}
