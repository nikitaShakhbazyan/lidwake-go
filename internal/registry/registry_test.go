package registry

import (
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/nikitaShakhbazyan/lidwake-go/internal/model"
)

// fakeClock is a manually advanced clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newRegistry(t *testing.T) (*Registry, *fakeClock) {
	t.Helper()
	c := newClock()
	r := New(c.now)
	t.Cleanup(r.Close)
	return r, c
}

func mk(c *fakeClock, key string, pid int, tool string) model.Assertion {
	return model.New(key, tool, "", pid, tool, c.now(), nil, model.OriginHook)
}

func mkDisplay(c *fakeClock, key string, pid int) model.Assertion {
	a := model.New(key, "rocuronium", "", pid, "rocuronium", c.now(), nil, model.OriginHook)
	a.HoldsDisplay = true
	return a
}

// recv reads n edges from ch, failing if any takes longer than a generous timeout.
func recv(t *testing.T, ch <-chan bool, n int) []bool {
	t.Helper()
	var got []bool
	for len(got) < n {
		select {
		case v, ok := <-ch:
			if !ok {
				t.Fatalf("channel closed after %v, wanted %d edges", got, n)
			}
			got = append(got, v)
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out after %v, wanted %d edges", got, n)
		}
	}
	return got
}

// expectNoEdge fails if ch delivers anything within a short window.
func expectNoEdge(t *testing.T, ch <-chan bool) {
	t.Helper()
	select {
	case v, ok := <-ch:
		if ok {
			t.Fatalf("unexpected extra edge %v", v)
		}
	case <-time.After(50 * time.Millisecond):
	}
}

func keys(as []model.Assertion) []string {
	out := make([]string, len(as))
	for i, a := range as {
		out[i] = a.Key
	}
	return out
}

func TestAssertionRegistry(t *testing.T) {
	t.Run("starts empty and not blocking", func(t *testing.T) {
		r, _ := newRegistry(t)
		if r.IsBlocking() {
			t.Fatal("IsBlocking = true")
		}
		if len(r.Snapshot()) != 0 {
			t.Fatal("snapshot not empty")
		}
	})

	t.Run("acquire adds and flips blocking", func(t *testing.T) {
		r, c := newRegistry(t)
		r.Acquire(mk(c, "a", 100, "claude-code"))
		if !r.IsBlocking() {
			t.Fatal("IsBlocking = false")
		}
		if n := len(r.Snapshot()); n != 1 {
			t.Fatalf("count = %d", n)
		}
	})

	t.Run("acquire is idempotent by key", func(t *testing.T) {
		r, c := newRegistry(t)
		for range 3 {
			r.Acquire(mk(c, "a", 100, "claude-code"))
		}
		if n := len(r.Snapshot()); n != 1 {
			t.Fatalf("count = %d", n)
		}
	})

	t.Run("release removes assertion", func(t *testing.T) {
		r, c := newRegistry(t)
		r.Acquire(mk(c, "a", 100, "claude-code"))
		r.Release("a")
		if r.IsBlocking() || len(r.Snapshot()) != 0 {
			t.Fatal("not released")
		}
	})

	t.Run("release unknown key is noop", func(t *testing.T) {
		r, c := newRegistry(t)
		r.Acquire(mk(c, "a", 100, "claude-code"))
		r.Release("does-not-exist")
		if n := len(r.Snapshot()); n != 1 {
			t.Fatalf("count = %d", n)
		}
	})

	t.Run("release does not flip blocking while others held", func(t *testing.T) {
		r, c := newRegistry(t)
		r.Acquire(mk(c, "a", 100, "claude-code"))
		r.Acquire(mk(c, "b", 100, "claude-code"))
		r.Release("a")
		if !r.IsBlocking() {
			t.Fatal("IsBlocking = false")
		}
	})

	t.Run("release all matching pid removes only matches", func(t *testing.T) {
		r, c := newRegistry(t)
		r.Acquire(mk(c, "a", 100, "claude-code"))
		r.Acquire(mk(c, "b", 100, "claude-code"))
		r.Acquire(mk(c, "c", 200, "claude-code"))
		if removed := r.ReleaseAllMatchingPID(100); removed != 2 {
			t.Fatalf("removed = %d", removed)
		}
		if got := keys(r.Snapshot()); !slices.Equal(got, []string{"c"}) {
			t.Fatalf("keys = %v", got)
		}
	})

	t.Run("remove all clears everything", func(t *testing.T) {
		r, c := newRegistry(t)
		r.Acquire(mk(c, "a", 100, "claude-code"))
		r.Acquire(mk(c, "b", 100, "claude-code"))
		r.RemoveAll()
		if r.IsBlocking() {
			t.Fatal("IsBlocking = true")
		}
	})

	t.Run("replace all overwrites everything", func(t *testing.T) {
		r, c := newRegistry(t)
		r.Acquire(mk(c, "a", 100, "claude-code"))
		r.ReplaceAll([]model.Assertion{mk(c, "x", 100, "claude-code"), mk(c, "y", 100, "claude-code")})
		got := keys(r.Snapshot())
		slices.Sort(got)
		if !slices.Equal(got, []string{"x", "y"}) {
			t.Fatalf("keys = %v", got)
		}
	})

	t.Run("snapshot is sorted by acquired at", func(t *testing.T) {
		r, c := newRegistry(t)
		now := c.now()
		older := model.New("old", "t", "", 1, "t", now.Add(-100*time.Second), nil, model.OriginHook)
		newer := model.New("new", "t", "", 2, "t", now, nil, model.OriginHook)
		r.Acquire(newer)
		r.Acquire(older)
		if got := keys(r.Snapshot()); !slices.Equal(got, []string{"old", "new"}) {
			t.Fatalf("keys = %v", got)
		}
	})

	t.Run("blocking state change emits on flip only", func(t *testing.T) {
		r, c := newRegistry(t)
		r.Acquire(mk(c, "a", 100, "claude-code"))
		r.Acquire(mk(c, "b", 100, "claude-code")) // already blocking — no emission
		r.Release("a")                            // still blocking — no emission
		r.Release("b")                            // now idle — emits false
		ch := r.BlockingChanges()
		if got := recv(t, ch, 2); !slices.Equal(got, []bool{true, false}) {
			t.Fatalf("edges = %v", got)
		}
		expectNoEdge(t, ch)
	})

	t.Run("acquire returns true when new false when duplicate", func(t *testing.T) {
		r, c := newRegistry(t)
		first := r.Acquire(mk(c, "a", 100, "claude-code"))
		dup := r.Acquire(mk(c, "a", 100, "claude-code"))
		if !first || dup {
			t.Fatalf("first = %v, dup = %v", first, dup)
		}
	})

	t.Run("release reports whether key existed", func(t *testing.T) {
		r, c := newRegistry(t)
		r.Acquire(mk(c, "a", 100, "claude-code"))
		hit := r.Release("a")
		miss := r.Release("a")
		if !hit || miss {
			t.Fatalf("hit = %v, miss = %v", hit, miss)
		}
	})

	t.Run("touch updates last activity at", func(t *testing.T) {
		r, c := newRegistry(t)
		a := mk(c, "a", 100, "claude-code")
		r.Acquire(a)
		c.advance(10 * time.Millisecond)
		r.Touch("a")
		if snap := r.Snapshot(); !snap[0].LastActivityAt.After(a.LastActivityAt) {
			t.Fatalf("LastActivityAt = %v, not after %v", snap[0].LastActivityAt, a.LastActivityAt)
		}
	})

	t.Run("replace all with duplicate keys does not crash", func(t *testing.T) {
		// A corrupted/hand-edited state.json could carry repeated keys; restore must not fail.
		r, c := newRegistry(t)
		a1 := model.New("dup", "t", "", 1, "t", c.now(), nil, model.OriginHook)
		a2 := model.New("dup", "t", "", 2, "t", c.now(), nil, model.OriginHook)
		r.ReplaceAll([]model.Assertion{a1, a2})
		snap := r.Snapshot()
		if len(snap) != 1 {
			t.Fatalf("count = %d", len(snap))
		}
		if snap[0].PID != 2 {
			t.Fatalf("PID = %d, want last-wins 2", snap[0].PID)
		}
	})

	t.Run("duplicate acquire preserves original acquired at", func(t *testing.T) {
		r, c := newRegistry(t)
		original := model.New("a", "t", "", 1, "t", c.now().Add(-100*time.Second), nil, model.OriginHook)
		r.Acquire(original)
		c.advance(5 * time.Millisecond)
		r.Acquire(mk(c, "a", 100, "claude-code")) // re-acquire with a fresh AcquiredAt
		snap := r.Snapshot()
		if len(snap) != 1 {
			t.Fatalf("count = %d", len(snap))
		}
		if !snap[0].AcquiredAt.Equal(original.AcquiredAt) {
			t.Fatalf("AcquiredAt = %v, want original %v", snap[0].AcquiredAt, original.AcquiredAt)
		}
		if !snap[0].LastActivityAt.After(original.AcquiredAt) {
			t.Fatal("activity not refreshed")
		}
	})

	t.Run("remove all when empty emits nothing", func(t *testing.T) {
		r, c := newRegistry(t)
		r.RemoveAll()                             // empty → must NOT emit
		r.Acquire(mk(c, "a", 100, "claude-code")) // emits true
		ch := r.BlockingChanges()
		if got := recv(t, ch, 1); got[0] != true {
			t.Fatalf("first edge = %v, want the acquire's true", got[0])
		}
		expectNoEdge(t, ch)
	})

	t.Run("release all matching nonexistent pid returns zero", func(t *testing.T) {
		r, c := newRegistry(t)
		r.Acquire(mk(c, "a", 100, "claude-code"))
		if n := r.ReleaseAllMatchingPID(999); n != 0 {
			t.Fatalf("removed = %d", n)
		}
		if n := r.Count(); n != 1 {
			t.Fatalf("count = %d", n)
		}
	})

	t.Run("release all ignores non positive pid", func(t *testing.T) {
		// Sentinel (-1) PIDs are PID-less assertions; a process exit must never group them.
		r, c := newRegistry(t)
		r.Acquire(mk(c, "a", -1, "claude-code"))
		r.Acquire(mk(c, "b", -1, "claude-code"))
		r.Acquire(mk(c, "z", 0, "claude-code"))
		if n := r.ReleaseAllMatchingPID(-1); n != 0 {
			t.Fatalf("removed = %d", n)
		}
		if n := r.ReleaseAllMatchingPID(0); n != 0 {
			t.Fatalf("removed = %d", n)
		}
		if n := r.Count(); n != 3 {
			t.Fatalf("count = %d", n)
		}
	})

	t.Run("replace all to empty flips blocking off", func(t *testing.T) {
		r, c := newRegistry(t)
		r.Acquire(mk(c, "a", 100, "claude-code")) // emits true
		r.ReplaceAll(nil)                         // flips → emits false
		if r.IsBlocking() {
			t.Fatal("IsBlocking = true")
		}
		if got := recv(t, r.BlockingChanges(), 2); !slices.Equal(got, []bool{true, false}) {
			t.Fatalf("edges = %v", got)
		}
	})

	t.Run("version increases on every content mutation", func(t *testing.T) {
		r, c := newRegistry(t)
		v0 := r.Version()
		r.Acquire(mk(c, "a", 100, "claude-code"))
		v1 := r.Version()
		if v1 <= v0 {
			t.Fatal("acquire did not bump version")
		}
		r.Acquire(mk(c, "a", 100, "claude-code")) // duplicate refresh mutates LastActivityAt/PID
		v2 := r.Version()
		if v2 <= v1 {
			t.Fatal("duplicate acquire did not bump version")
		}
		r.Touch("a")
		v3 := r.Version()
		if v3 <= v2 {
			t.Fatal("touch did not bump version")
		}
		r.Release("a")
		if v4 := r.Version(); v4 <= v3 {
			t.Fatal("release did not bump version")
		}
	})

	t.Run("version is untouched by no-op release", func(t *testing.T) {
		r, c := newRegistry(t)
		r.Acquire(mk(c, "a", 100, "claude-code"))
		v1 := r.Version()
		r.Release("nope")
		if r.Version() != v1 {
			t.Fatal("no-op release bumped version")
		}
		r.ReleaseAllMatchingPID(999)
		if r.Version() != v1 {
			t.Fatal("no-match releaseAll bumped version")
		}
	})

	t.Run("wants display tracks display-class assertions only", func(t *testing.T) {
		r, c := newRegistry(t)
		r.Acquire(mk(c, "plain", 100, "claude-code"))
		if r.WantsDisplay() {
			t.Fatal("plain hold wants display")
		}
		r.Acquire(mkDisplay(c, "seeing", 100))
		if !r.WantsDisplay() {
			t.Fatal("display hold does not want display")
		}
		r.Release("seeing")
		if r.WantsDisplay() {
			t.Fatal("system-only holds must not keep the display awake")
		}
		if !r.IsBlocking() {
			t.Fatal("the plain hold still blocks system sleep")
		}
	})

	// Every release path drops the display want — this is what makes pause and the
	// thermal/battery cutouts (which remove everything) outrank a display hold for free.
	t.Run("remove all drops the display want", func(t *testing.T) {
		r, c := newRegistry(t)
		r.Acquire(mkDisplay(c, "seeing", 100))
		if !r.WantsDisplay() {
			t.Fatal("WantsDisplay = false")
		}
		r.RemoveAll()
		if r.WantsDisplay() {
			t.Fatal("WantsDisplay = true after RemoveAll")
		}
	})

	t.Run("display class is sticky across re-acquires", func(t *testing.T) {
		r, c := newRegistry(t)
		r.Acquire(mkDisplay(c, "k", 100))
		// A later per-turn re-acquire without the flag (same key) must not downgrade the hold —
		// dropping the display mid-work would blind the agent it exists for.
		r.Acquire(mk(c, "k", 100, "rocuronium"))
		if !r.WantsDisplay() {
			t.Fatal("re-acquire downgraded the display class")
		}
		// And the upgrade direction works: a plain hold re-acquired with the flag gains it.
		r.Acquire(mk(c, "up", 100, "claude-code"))
		r.Acquire(mkDisplay(c, "up", 100))
		for _, a := range r.Snapshot() {
			if a.Key == "up" && !a.HoldsDisplay {
				t.Fatal("re-acquire with the flag did not upgrade")
			}
		}
	})

	t.Run("display state changes emits on flips including duplicate-acquire upgrades", func(t *testing.T) {
		r, c := newRegistry(t)
		r.Acquire(mk(c, "plain", 100, "claude-code")) // no display flip
		r.Acquire(mk(c, "up", 100, "claude-code"))
		r.Acquire(mkDisplay(c, "up", 100)) // duplicate upgrade → flips true
		r.Release("up")                    // → flips false
		ch := r.DisplayChanges()
		if got := recv(t, ch, 2); !slices.Equal(got, []bool{true, false}) {
			t.Fatalf("edges = %v", got)
		}
		expectNoEdge(t, ch)
	})

	// The pair returned by VersionedSnapshot is what the daemon stamps into its status: a
	// later-versioned snapshot must reflect a later content state, so a receiver comparing
	// versions can safely drop the lower one.
	t.Run("versioned snapshot orders content states", func(t *testing.T) {
		r, c := newRegistry(t)
		r.Acquire(mk(c, "a", 100, "claude-code"))
		before, vBefore := r.VersionedSnapshot()
		r.Acquire(mk(c, "b", 100, "claude-code"))
		after, vAfter := r.VersionedSnapshot()
		if len(before) != 1 || len(after) != 2 {
			t.Fatalf("before = %d, after = %d", len(before), len(after))
		}
		if vAfter <= vBefore {
			t.Fatalf("vAfter %d <= vBefore %d", vAfter, vBefore)
		}
	})
}

// Behavior the tests above leave implicit, plus what the Go port adds (copies, Close, the
// non-blocking edge queue).
func TestAssertionRegistryRefresh(t *testing.T) {
	t.Run("duplicate acquire adopts a positive pid with its process name", func(t *testing.T) {
		r, c := newRegistry(t)
		r.Acquire(model.New("k", "codex", "", 100, "codex", c.now(), nil, model.OriginHook))
		r.Acquire(model.New("k", "codex", "", 200, "codex-aarch64-apple-darwin", c.now(), nil, model.OriginHook))
		a := r.Snapshot()[0]
		if a.PID != 200 || a.ProcessName != "codex-aarch64-apple-darwin" {
			t.Fatalf("pid/name = %d/%q", a.PID, a.ProcessName)
		}
	})

	t.Run("duplicate acquire keeps the pid when the incoming one is not positive", func(t *testing.T) {
		r, c := newRegistry(t)
		r.Acquire(model.New("k", "codex", "", 100, "codex", c.now(), nil, model.OriginHook))
		r.Acquire(model.New("k", "codex", "", -1, "other", c.now(), nil, model.OriginHook))
		r.Acquire(model.New("k", "codex", "", 0, "other", c.now(), nil, model.OriginHook))
		a := r.Snapshot()[0]
		if a.PID != 100 || a.ProcessName != "codex" {
			t.Fatalf("pid/name = %d/%q", a.PID, a.ProcessName)
		}
	})

	t.Run("duplicate acquire adopts reason and expiry only when given", func(t *testing.T) {
		r, c := newRegistry(t)
		ttl := time.Hour
		r.Acquire(model.New("k", "t", "first", 1, "t", c.now(), &ttl, model.OriginManual))
		wantExp := c.now().Add(time.Hour)

		r.Acquire(model.New("k", "other-tool", "", 1, "t", c.now(), nil, model.OriginHook))
		a := r.Snapshot()[0]
		if a.Reason != "first" || a.ExpiresAt == nil || !a.ExpiresAt.Equal(wantExp) {
			t.Fatalf("reason/expiry not kept: %q %v", a.Reason, a.ExpiresAt)
		}
		if a.Tool != "t" || a.Origin != model.OriginManual {
			t.Fatalf("tool/origin not kept: %q %q", a.Tool, a.Origin)
		}

		c.advance(time.Minute)
		ttl2 := 2 * time.Hour
		r.Acquire(model.New("k", "t", "second", 1, "t", c.now(), &ttl2, model.OriginHook))
		a = r.Snapshot()[0]
		if a.Reason != "second" || !a.ExpiresAt.Equal(c.now().Add(2*time.Hour)) {
			t.Fatalf("reason/expiry not adopted: %q %v", a.Reason, a.ExpiresAt)
		}
		if !a.LastActivityAt.Equal(c.now()) {
			t.Fatalf("LastActivityAt = %v, want injected now %v", a.LastActivityAt, c.now())
		}
	})

	t.Run("duplicate acquire clears the waiting label", func(t *testing.T) {
		r, c := newRegistry(t)
		r.Acquire(mk(c, "k", 1, "claude-code"))
		r.SetWaitingFor("k", "approve Bash")
		r.Acquire(mk(c, "k", 1, "claude-code"))
		if w := r.Snapshot()[0].WaitingFor; w != "" {
			t.Fatalf("WaitingFor = %q", w)
		}
	})

	t.Run("set expiry overwrites and clears", func(t *testing.T) {
		r, c := newRegistry(t)
		ttl := time.Hour
		r.Acquire(model.New("k", "t", "", 1, "t", c.now(), &ttl, model.OriginHook))
		grace := c.now().Add(5 * time.Minute)
		v := r.Version()
		r.SetExpiry("k", &grace)
		if got := r.Snapshot()[0].ExpiresAt; got == nil || !got.Equal(grace) {
			t.Fatalf("ExpiresAt = %v", got)
		}
		if r.Version() <= v {
			t.Fatal("SetExpiry did not bump version")
		}
		r.SetExpiry("k", nil)
		if got := r.Snapshot()[0].ExpiresAt; got != nil {
			t.Fatalf("ExpiresAt = %v, want cleared", got)
		}
	})

	t.Run("set waiting for stamps and clears", func(t *testing.T) {
		r, c := newRegistry(t)
		r.Acquire(mk(c, "k", 1, "claude-code"))
		v := r.Version()
		r.SetWaitingFor("k", "input needed")
		if w := r.Snapshot()[0].WaitingFor; w != "input needed" {
			t.Fatalf("WaitingFor = %q", w)
		}
		if r.Version() <= v {
			t.Fatal("SetWaitingFor did not bump version")
		}
		r.SetWaitingFor("k", "")
		if w := r.Snapshot()[0].WaitingFor; w != "" {
			t.Fatalf("WaitingFor = %q", w)
		}
	})

	t.Run("per-key setters ignore unknown keys", func(t *testing.T) {
		r, c := newRegistry(t)
		r.Acquire(mk(c, "k", 1, "claude-code"))
		v := r.Version()
		at := c.now()
		r.Touch("nope")
		r.SetExpiry("nope", &at)
		r.SetWaitingFor("nope", "x")
		if r.Version() != v || r.Count() != 1 {
			t.Fatal("unknown key mutated the store")
		}
	})

	t.Run("snapshot returns copies", func(t *testing.T) {
		r, c := newRegistry(t)
		ttl := time.Hour
		in := model.New("k", "t", "", 1, "t", c.now(), &ttl, model.OriginHook)
		want := *in.ExpiresAt
		r.Acquire(in)
		*in.ExpiresAt = want.Add(time.Hour) // caller mutates its own value after acquire
		snap := r.Snapshot()
		*snap[0].ExpiresAt = want.Add(2 * time.Hour)
		snap[0].Reason = "changed"
		got := r.Snapshot()[0]
		if !got.ExpiresAt.Equal(want) || got.Reason != "" {
			t.Fatalf("store shares memory with callers: %v %q", got.ExpiresAt, got.Reason)
		}
	})

	t.Run("snapshot breaks acquired at ties by key", func(t *testing.T) {
		r, c := newRegistry(t)
		for _, k := range []string{"c", "a", "b"} {
			r.Acquire(mk(c, k, 1, "t"))
		}
		if got := keys(r.Snapshot()); !slices.Equal(got, []string{"a", "b", "c"}) {
			t.Fatalf("keys = %v", got)
		}
	})

	t.Run("any holds display mirrors wants display", func(t *testing.T) {
		c := newClock()
		plain := mk(c, "a", 1, "t")
		display := mkDisplay(c, "b", 1)
		if AnyHoldsDisplay([]model.Assertion{plain}) {
			t.Fatal("plain list holds display")
		}
		if !AnyHoldsDisplay([]model.Assertion{plain, display}) {
			t.Fatal("display list does not hold display")
		}
		if AnyHoldsDisplay(nil) {
			t.Fatal("empty list holds display")
		}
	})
}

func TestAssertionRegistryEdges(t *testing.T) {
	t.Run("mutations never wait for an absent consumer", func(t *testing.T) {
		r, c := newRegistry(t)
		const flips = 2000
		done := make(chan struct{})
		go func() {
			defer close(done)
			for range flips / 2 {
				r.Acquire(mk(c, "k", 1, "t"))
				r.Release("k")
			}
		}()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("mutations blocked with nobody reading the edges")
		}
		got := recv(t, r.BlockingChanges(), flips)
		for i, v := range got {
			if v != (i%2 == 0) {
				t.Fatalf("edge %d = %v: out of order", i, v)
			}
		}
		expectNoEdge(t, r.BlockingChanges())
	})

	t.Run("a slow consumer does not stall mutations", func(t *testing.T) {
		r, c := newRegistry(t)
		ch := r.BlockingChanges()
		r.Acquire(mk(c, "k", 1, "t"))
		<-ch // the consumer is now "busy" and stops reading
		done := make(chan struct{})
		go func() {
			defer close(done)
			for range 100 {
				r.Release("k")
				r.Acquire(mk(c, "k", 1, "t"))
			}
		}()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("mutations blocked behind a slow consumer")
		}
		got := recv(t, ch, 200)
		for i, v := range got {
			if v != (i%2 == 1) {
				t.Fatalf("edge %d = %v: out of order", i, v)
			}
		}
	})

	t.Run("concurrent mutations deliver alternating edges ending in the final state", func(t *testing.T) {
		r, c := newRegistry(t)
		ch := r.BlockingChanges()
		var edges []bool
		finished := make(chan struct{}) // mutations are over
		consumed := make(chan struct{})
		go func() {
			defer close(consumed)
			// Read concurrently with the mutations; once they are over, stop after a quiet spell.
			over := finished
			for {
				var quiet <-chan time.Time
				if over == nil {
					quiet = time.After(200 * time.Millisecond)
				}
				select {
				case v := <-ch:
					edges = append(edges, v)
				case <-over:
					over = nil
				case <-quiet:
					return
				}
			}
		}()
		var wg sync.WaitGroup
		for g := range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				key := fmt.Sprintf("k%d", g)
				for range 200 {
					r.Acquire(mk(c, key, g+1, "t"))
					r.Release(key)
				}
			}()
		}
		wg.Wait()
		r.Acquire(mk(c, "final", 1, "t"))
		close(finished)
		<-consumed
		if len(edges) == 0 {
			t.Fatal("no edges delivered")
		}
		for i, v := range edges {
			if v != (i%2 == 0) {
				t.Fatalf("edge %d = %v: edges must alternate starting with true", i, v)
			}
		}
		if !edges[len(edges)-1] || !r.IsBlocking() {
			t.Fatal("last edge disagrees with the final state")
		}
	})

	t.Run("a consumer may call back into the registry", func(t *testing.T) {
		r, c := newRegistry(t)
		ch := r.BlockingChanges()
		results := make(chan int, 1)
		go func() {
			<-ch
			results <- len(r.Snapshot()) // would deadlock if delivery held the registry lock
		}()
		r.Acquire(mk(c, "k", 1, "t"))
		select {
		case n := <-results:
			if n != 1 {
				t.Fatalf("snapshot len = %d", n)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("consumer deadlocked calling back into the registry")
		}
	})

	t.Run("close ends both channels and later mutations still work", func(t *testing.T) {
		c := newClock()
		r := New(c.now)
		blocking := r.BlockingChanges()
		r.Acquire(mk(c, "k", 1, "t"))
		r.Close()
		r.Close() // idempotent
		drain(t, blocking)
		drain(t, r.DisplayChanges()) // never started before Close
		r.Release("k")
		r.Acquire(mkDisplay(c, "d", 1))
		if !r.IsBlocking() || !r.WantsDisplay() {
			t.Fatal("store stopped working after Close")
		}
	})

	t.Run("nil now defaults to the wall clock", func(t *testing.T) {
		r := New(nil)
		t.Cleanup(r.Close)
		before := time.Now()
		r.Acquire(model.New("k", "t", "", 1, "t", before.Add(-time.Hour), nil, model.OriginHook))
		r.Touch("k")
		if got := r.Snapshot()[0].LastActivityAt; got.Before(before) {
			t.Fatalf("LastActivityAt = %v, before %v", got, before)
		}
	})
}

// drain reads ch until it is closed, failing if that takes too long.
func drain(t *testing.T, ch <-chan bool) {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-timeout:
			t.Fatal("channel not closed")
		}
	}
}
