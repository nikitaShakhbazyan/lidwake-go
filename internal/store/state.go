// Package store is the daemon's persistence: state.json (live assertions, the paused bit and the
// off timer, so a restart neither drops live agent sessions nor silently un-pauses lidwake) and
// events.log (an append-only JSON-lines event log).
package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/nikitaShakhbazyan/lidwake-go/internal/model"
)

const (
	// StateFileName is the state file's name inside the support directory.
	StateFileName = "state.json"
	// maxStateFileSize bounds a state read. A full registry is a few hundred kilobytes; anything
	// larger is not a state file.
	maxStateFileSize = 16 << 20
)

// StateStore reads and writes state.json. Safe for concurrent use.
type StateStore struct {
	// Path is the state file. NewStateStore puts it in the support directory.
	Path string

	mu sync.Mutex // serializes saves
}

// NewStateStore returns a store for dir/state.json; the daemon passes paths.SupportDir().
func NewStateStore(dir string) *StateStore {
	return &StateStore{Path: filepath.Join(dir, StateFileName)}
}

// Load reads the persisted state. ok is false, with the zero state, when there is nothing usable:
// no file, an unreadable one or garbage. Persistence is best effort, and a daemon that cannot
// restore must still start.
func (s *StateStore) Load() (state model.PersistedState, ok bool) {
	f, err := os.Open(s.Path)
	if err != nil {
		return model.PersistedState{}, false
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxStateFileSize+1))
	if err != nil || len(data) > maxStateFileSize {
		return model.PersistedState{}, false
	}
	return DecodeState(data)
}

// Save writes the state atomically: a temporary file in the same directory, flushed, then renamed
// over state.json, so a crash mid-write leaves the previous state rather than a torn file. The
// file is readable by the user only (it names the user's agents and what they work on); the
// directory is created 0700 if missing.
func (s *StateStore) Save(state model.PersistedState) error {
	if state.Assertions == nil {
		state.Assertions = []model.Assertion{}
	}
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("store: encode state: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return writeFileAtomic(s.Path, data)
}

func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("store: create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("store: create a temporary file in %s: %w", dir, err)
	}
	name := tmp.Name()
	if err := writeAndClose(tmp, data); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return fmt.Errorf("store: replace %s: %w", path, err)
	}
	return nil
}

// writeAndClose writes data to f with owner-only permissions and flushes it to disk before
// closing, so the rename that follows never exposes a partly written file.
func writeAndClose(f *os.File, data []byte) error {
	err := f.Chmod(0o600)
	if err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("store: write %s: %w", f.Name(), err)
	}
	return nil
}

// DecodeState decodes a state file: the {assertions, paused, offAt} envelope, or the bare
// assertion array older builds wrote (paused false). Files written by the previous build, whose
// times are seconds since 2001-01-01 UTC rather than RFC 3339 strings, decode too, so an upgrade
// keeps the paused bit, the off timer and live holds. An object is an envelope only when it has
// both the assertions and the paused key, non-null; ok is false for anything else. Assertions
// without a key are dropped: nothing could ever release them.
func DecodeState(data []byte) (state model.PersistedState, ok bool) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return model.PersistedState{}, false
	}
	switch trimmed[0] {
	case '{':
		if !isEnvelope(trimmed) {
			break
		}
		if json.Unmarshal(trimmed, &state) == nil {
			return withKeyedAssertions(state), true
		}
		var legacy legacyState
		if json.Unmarshal(trimmed, &legacy) == nil {
			if st, ok := legacy.state(); ok {
				return withKeyedAssertions(st), true
			}
		}
	case '[':
		var assertions []model.Assertion
		if json.Unmarshal(trimmed, &assertions) == nil {
			return withKeyedAssertions(model.PersistedState{Assertions: assertions}), true
		}
		var legacy []legacyAssertion
		if json.Unmarshal(trimmed, &legacy) == nil {
			if as, ok := convertLegacy(legacy); ok {
				return withKeyedAssertions(model.PersistedState{Assertions: as}), true
			}
		}
	}
	return model.PersistedState{}, false
}

// isEnvelope reports whether data is an object carrying the envelope's required keys. Field
// matching in encoding/json is lenient (any object decodes, keys match case-insensitively), so
// without this check {} or an unrelated object would read as an empty, unpaused state.
func isEnvelope(data []byte) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil {
		return false
	}
	for _, key := range []string{"assertions", "paused"} {
		v, ok := fields[key]
		if !ok || string(v) == "null" {
			return false
		}
	}
	return true
}

func withKeyedAssertions(st model.PersistedState) model.PersistedState {
	kept := make([]model.Assertion, 0, len(st.Assertions))
	for _, a := range st.Assertions {
		if a.Key != "" {
			kept = append(kept, a)
		}
	}
	st.Assertions = kept
	return st
}

// referenceDate is the epoch of the previous build's timestamps.
var referenceDate = time.Date(2001, time.January, 1, 0, 0, 0, 0, time.UTC)

// legacyTime is a timestamp as the previous build wrote it: seconds since referenceDate.
type legacyTime float64

func (t legacyTime) time() (time.Time, bool) {
	s := float64(t)
	// ±1e10 s is about ±317 years around 2001: far beyond any real timestamp. That is past
	// time.Duration's ±292 years, so whole seconds and the fraction are added separately.
	if math.IsNaN(s) || math.IsInf(s, 0) || math.Abs(s) > 1e10 {
		return time.Time{}, false
	}
	whole := math.Floor(s)
	nanos := math.Round((s - whole) * float64(time.Second))
	return time.Unix(referenceDate.Unix()+int64(whole), int64(nanos)).UTC(), true
}

type legacyAssertion struct {
	Key            string       `json:"key"`
	Tool           string       `json:"tool"`
	Reason         string       `json:"reason"`
	PID            int          `json:"pid"`
	ProcessName    string       `json:"processName"`
	AcquiredAt     *legacyTime  `json:"acquiredAt"`
	LastActivityAt *legacyTime  `json:"lastActivityAt"`
	ExpiresAt      *legacyTime  `json:"expiresAt"`
	Origin         model.Origin `json:"origin"`
	HoldsDisplay   bool         `json:"holdsDisplay"`
	WaitingFor     string       `json:"waitingFor"`
}

type legacyState struct {
	Assertions []legacyAssertion `json:"assertions"`
	Paused     bool              `json:"paused"`
	OffAt      *legacyTime       `json:"offAt"`
}

func (l legacyState) state() (model.PersistedState, bool) {
	as, ok := convertLegacy(l.Assertions)
	if !ok {
		return model.PersistedState{}, false
	}
	st := model.PersistedState{Assertions: as, Paused: l.Paused}
	if l.OffAt != nil {
		t, ok := l.OffAt.time()
		if !ok {
			return model.PersistedState{}, false
		}
		st.OffAt = &t
	}
	return st, true
}

// convertLegacy converts the previous build's assertions; like that build's decoder, it rejects
// the whole file when an assertion lacks its acquisition times.
func convertLegacy(in []legacyAssertion) ([]model.Assertion, bool) {
	out := make([]model.Assertion, 0, len(in))
	for _, l := range in {
		if l.AcquiredAt == nil || l.LastActivityAt == nil {
			return nil, false
		}
		acquired, ok1 := l.AcquiredAt.time()
		active, ok2 := l.LastActivityAt.time()
		if !ok1 || !ok2 {
			return nil, false
		}
		a := model.Assertion{
			Key: l.Key, Tool: l.Tool, Reason: l.Reason, PID: l.PID, ProcessName: l.ProcessName,
			AcquiredAt: acquired, LastActivityAt: active, Origin: l.Origin,
			HoldsDisplay: l.HoldsDisplay, WaitingFor: l.WaitingFor,
		}
		if a.Origin == "" {
			a.Origin = model.OriginHook
		}
		if l.ExpiresAt != nil {
			t, ok := l.ExpiresAt.time()
			if !ok {
				return nil, false
			}
			a.ExpiresAt = &t
		}
		out = append(out, a)
	}
	return out, true
}
