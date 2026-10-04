package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nikitaShakhbazyan/lidwake-go/internal/model"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/paths"
)

func sampleAssertion() model.Assertion {
	return model.New("k", "t", "", 1, "p", time.Date(2026, 10, 4, 10, 0, 0, 0, time.UTC), nil, model.OriginHook)
}

func TestPersistedDaemonState(t *testing.T) {
	t.Run("envelope round-trips assertions and paused", func(t *testing.T) {
		state := model.PersistedState{Assertions: []model.Assertion{sampleAssertion()}, Paused: true}
		data, err := json.Marshal(state)
		if err != nil {
			t.Fatal(err)
		}
		decoded, ok := DecodeState(data)
		if !ok {
			t.Fatal("envelope did not decode")
		}
		if !decoded.Paused {
			t.Error("paused lost")
		}
		if len(decoded.Assertions) == 0 || decoded.Assertions[0].Key != "k" {
			t.Errorf("assertions = %+v, want key k first", decoded.Assertions)
		}
	})

	t.Run("legacy bare-array state files decode with paused false", func(t *testing.T) {
		legacy, err := json.Marshal([]model.Assertion{sampleAssertion()})
		if err != nil {
			t.Fatal(err)
		}
		decoded, ok := DecodeState(legacy)
		if !ok {
			t.Fatal("bare array did not decode")
		}
		if decoded.Paused {
			t.Error("a bare array must decode as not paused")
		}
		if len(decoded.Assertions) != 1 {
			t.Errorf("got %d assertions, want 1", len(decoded.Assertions))
		}
	})

	t.Run("garbage state files decode as nil", func(t *testing.T) {
		if _, ok := DecodeState([]byte("not json")); ok {
			t.Error("garbage decoded")
		}
	})
}

func TestDecodeStateRejectsNonState(t *testing.T) {
	for _, in := range []string{"", "   ", "null", "true", "42", `"state"`, `{"assertions": 5}`, `[1, 2]`, `{"paused": true} trailing`, `[{"key": "k", "pid": "one"}]`,
		`{}`, `{"foo": 1}`, `{"assertions": []}`, `{"paused": false}`, `{"assertions": null, "paused": false}`,
		`{"assertions": [], "paused": null}`, `{"Assertions": [], "Paused": true}`} {
		if st, ok := DecodeState([]byte(in)); ok {
			t.Errorf("DecodeState(%q) = %+v, want not ok", in, st)
		}
	}
}

func TestDecodeStateEnvelopeNeedsOnlyItsRequiredKeys(t *testing.T) {
	st, ok := DecodeState([]byte(`{"assertions": [], "paused": true, "future": {"x": 1}}`))
	if !ok || !st.Paused || st.OffAt != nil || len(st.Assertions) != 0 {
		t.Fatalf("got %+v ok=%v", st, ok)
	}
}

func TestDecodeStateDropsKeylessAssertions(t *testing.T) {
	st, ok := DecodeState([]byte(`{"assertions":[{"key":"","tool":"t"},{"key":"k","tool":"t"}],"paused":false}`))
	if !ok {
		t.Fatal("did not decode")
	}
	if len(st.Assertions) != 1 || st.Assertions[0].Key != "k" {
		t.Fatalf("assertions = %+v, want only k", st.Assertions)
	}
	if st.Assertions[0].Origin != model.OriginHook {
		t.Errorf("origin = %q, want the hook default", st.Assertions[0].Origin)
	}
}

// The previous build stored times as seconds since 2001-01-01 UTC. An upgrade must keep the
// paused bit, the off timer and live holds.
func TestDecodeStatePreviousBuildFormat(t *testing.T) {
	ref := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	acquired := time.Date(2026, 10, 4, 9, 30, 0, 0, time.UTC)
	secs := acquired.Sub(ref).Seconds()

	t.Run("envelope", func(t *testing.T) {
		data := []byte(`{"assertions":[{"key":"claude-code:s1","tool":"claude-code","reason":"why","pid":4242,` +
			`"processName":"claude","acquiredAt":` + jsonFloat(secs) + `,"lastActivityAt":` + jsonFloat(secs+60) +
			`,"expiresAt":` + jsonFloat(secs+3600.5) + `,"origin":"manual","holdsDisplay":true,"waitingFor":"input needed"}],` +
			`"paused":true,"offAt":` + jsonFloat(secs+7200) + `}`)
		st, ok := DecodeState(data)
		if !ok {
			t.Fatal("previous build's envelope did not decode")
		}
		if !st.Paused {
			t.Error("paused lost")
		}
		if st.OffAt == nil || !st.OffAt.Equal(acquired.Add(2*time.Hour)) {
			t.Errorf("offAt = %v, want %v", st.OffAt, acquired.Add(2*time.Hour))
		}
		if len(st.Assertions) != 1 {
			t.Fatalf("got %d assertions", len(st.Assertions))
		}
		a := st.Assertions[0]
		if a.Key != "claude-code:s1" || a.Tool != "claude-code" || a.Reason != "why" || a.PID != 4242 ||
			a.ProcessName != "claude" || a.Origin != model.OriginManual || !a.HoldsDisplay || a.WaitingFor != "input needed" {
			t.Errorf("fields lost: %+v", a)
		}
		if !a.AcquiredAt.Equal(acquired) || !a.LastActivityAt.Equal(acquired.Add(time.Minute)) {
			t.Errorf("times = %v / %v", a.AcquiredAt, a.LastActivityAt)
		}
		if a.ExpiresAt == nil || !a.ExpiresAt.Equal(acquired.Add(time.Hour+500*time.Millisecond)) {
			t.Errorf("expiresAt = %v", a.ExpiresAt)
		}
	})

	t.Run("bare array without origin", func(t *testing.T) {
		data := []byte(`[{"key":"k","tool":"t","pid":1,"processName":"p","acquiredAt":` + jsonFloat(secs) +
			`,"lastActivityAt":` + jsonFloat(secs) + `}]`)
		st, ok := DecodeState(data)
		if !ok || st.Paused || len(st.Assertions) != 1 {
			t.Fatalf("got %+v ok=%v", st, ok)
		}
		if st.Assertions[0].Origin != model.OriginHook {
			t.Errorf("origin = %q, want hook", st.Assertions[0].Origin)
		}
		if st.Assertions[0].ExpiresAt != nil {
			t.Error("expiresAt invented")
		}
	})

	t.Run("an assertion without its times is garbage", func(t *testing.T) {
		if _, ok := DecodeState([]byte(`{"assertions":[{"key":"k","acquiredAt":1}],"paused":true}`)); ok {
			t.Error("decoded an assertion with no lastActivityAt")
		}
	})

	t.Run("times beyond time.Duration's range convert exactly", func(t *testing.T) {
		for _, tc := range []struct {
			secs float64
			want time.Time
		}{
			{9.5e9, time.Unix(ref.Unix()+9_500_000_000, 0).UTC()},
			{-9.5e9, time.Unix(ref.Unix()-9_500_000_000, 0).UTC()},
			{secs + 0.25, acquired.Add(250 * time.Millisecond)},
			{-0.5, ref.Add(-500 * time.Millisecond)},
		} {
			if got, ok := legacyTime(tc.secs).time(); !ok || !got.Equal(tc.want) {
				t.Errorf("%v s = %v ok=%v, want %v", tc.secs, got, ok, tc.want)
			}
		}
	})

	t.Run("absurd times are garbage", func(t *testing.T) {
		if _, ok := DecodeState([]byte(`{"assertions":[],"paused":true,"offAt":1e300}`)); ok {
			t.Error("decoded an off timer 1e300 s away")
		}
	})
}

func jsonFloat(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}

// The daemon passes paths.SupportDir(); the files must land where the rest of lidwake looks.
func TestStorePathsMatchPaths(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if got := NewStateStore(paths.SupportDir()).Path; got != paths.StateFile() {
		t.Errorf("state path = %s, want %s", got, paths.StateFile())
	}
	if got := NewEventLog(paths.SupportDir()).Path; got != paths.EventLog() {
		t.Errorf("event log path = %s, want %s", got, paths.EventLog())
	}
}

func TestStateStoreSaveAndLoad(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "support")
	s := NewStateStore(dir)
	if s.Path != filepath.Join(dir, "state.json") {
		t.Fatalf("path = %s", s.Path)
	}

	if _, ok := s.Load(); ok {
		t.Fatal("a missing file loaded")
	}

	off := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	exp := time.Date(2026, 10, 4, 11, 0, 0, 0, time.UTC)
	a := sampleAssertion()
	a.ExpiresAt = &exp
	a.HoldsDisplay = true
	a.WaitingFor = "approve Bash"
	a.Origin = model.OriginSniffed
	want := model.PersistedState{Assertions: []model.Assertion{a}, Paused: true, OffAt: &off}
	if err := s.Save(want); err != nil {
		t.Fatal(err)
	}
	got, ok := s.Load()
	if !ok {
		t.Fatal("saved state did not load")
	}
	if !got.Paused || got.OffAt == nil || !got.OffAt.Equal(off) || len(got.Assertions) != 1 {
		t.Fatalf("got %+v", got)
	}
	g := got.Assertions[0]
	if g.Key != a.Key || g.Origin != model.OriginSniffed || !g.HoldsDisplay || g.WaitingFor != "approve Bash" ||
		g.ExpiresAt == nil || !g.ExpiresAt.Equal(exp) || !g.AcquiredAt.Equal(a.AcquiredAt) {
		t.Errorf("assertion = %+v", g)
	}

	info, err := os.Stat(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("state.json mode = %o, want 600", perm)
	}
	dinfo, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := dinfo.Mode().Perm(); perm != 0o700 {
		t.Errorf("support dir mode = %o, want 700", perm)
	}
	assertNoTempFiles(t, dir)
}

func TestStateStoreSaveReplacesWithOwnerOnlyFile(t *testing.T) {
	dir := t.TempDir()
	s := NewStateStore(dir)
	if err := os.WriteFile(s.Path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(model.PersistedState{}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600", perm)
	}
	data, err := os.ReadFile(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	// An empty registry is written as an empty list, never null.
	if !strings.Contains(string(data), `"assertions":[]`) {
		t.Errorf("state.json = %s, want an empty assertions list", data)
	}
	st, ok := s.Load()
	if !ok || st.Paused || len(st.Assertions) != 0 {
		t.Errorf("got %+v ok=%v", st, ok)
	}
}

func TestStateStoreLoadIsTolerant(t *testing.T) {
	dir := t.TempDir()
	s := NewStateStore(dir)
	for _, content := range []string{"", "not json", "null", `{"assertions": [`} {
		if err := os.WriteFile(s.Path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if st, ok := s.Load(); ok {
			t.Errorf("Load of %q = %+v, want not ok", content, st)
		}
	}
	// An oversized file is not a state file.
	big := make([]byte, maxStateFileSize+1)
	for i := range big {
		big[i] = ' '
	}
	big[0] = '['
	if err := os.WriteFile(s.Path, big, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Load(); ok {
		t.Error("an oversized file loaded")
	}
	// A directory in its place reads as nothing.
	if err := os.Remove(s.Path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(s.Path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Load(); ok {
		t.Error("a directory loaded")
	}
}

func TestStateStoreSaveFailureLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	s := NewStateStore(dir)
	// state.json is a non-empty directory, so the rename over it fails.
	if err := os.MkdirAll(filepath.Join(s.Path, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(model.PersistedState{Paused: true}); err == nil {
		t.Fatal("save over a directory succeeded")
	}
	assertNoTempFiles(t, dir)

	// The support directory can't be created under a file.
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := NewStateStore(filepath.Join(blocker, "sub")).Save(model.PersistedState{}); err == nil {
		t.Error("save under a file succeeded")
	}
}

func TestStateStoreConcurrentSaves(t *testing.T) {
	dir := t.TempDir()
	s := NewStateStore(dir)
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Save(model.PersistedState{Paused: i%2 == 0}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if _, ok := s.Load(); !ok {
		t.Error("state unreadable after concurrent saves")
	}
	assertNoTempFiles(t, dir)
}

func assertNoTempFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("temporary file left behind: %s", e.Name())
		}
	}
}
