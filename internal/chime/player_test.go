package chime

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"
)

// call is one recorded afplay invocation, with the WAV bytes the file held at play time.
type call struct {
	name string
	args []string
	wav  []byte
}

// fakeExec records invocations instead of playing anything. block makes each call wait for its
// context, standing in for a wedged afplay.
type fakeExec struct {
	mu    sync.Mutex
	calls []call
	block bool
	err   error
}

func (f *fakeExec) run(ctx context.Context, name string, args ...string) error {
	c := call{name: name, args: slices.Clone(args)}
	if n := len(args); n > 0 && filepath.Ext(args[n-1]) == ".wav" {
		c.wav, _ = os.ReadFile(args[n-1])
	}
	f.mu.Lock()
	f.calls = append(f.calls, c)
	f.mu.Unlock()
	if f.block {
		<-ctx.Done()
		return ctx.Err()
	}
	return f.err
}

func (f *fakeExec) recorded() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

type fixture struct {
	player *Player
	exec   *fakeExec
	sounds string
	tmp    string
	muted  bool
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	fx := &fixture{exec: &fakeExec{}, sounds: t.TempDir(), tmp: t.TempDir()}
	if err := os.WriteFile(filepath.Join(fx.sounds, "Glass.aiff"), []byte("aiff"), 0o644); err != nil {
		t.Fatal(err)
	}
	fx.player = &Player{
		Exec:            fx.exec.run,
		Muted:           func() bool { return fx.muted },
		SoundsDir:       fx.sounds,
		TempDir:         fx.tmp,
		SleepCueTimeout: time.Second,
		Log:             slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return fx
}

func (fx *fixture) tempFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(fx.tmp)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// expectSynth checks that c played a rendered WAV of cue at volume, at unity gain.
func expectSynth(t *testing.T, c call, cue Cue, volume float64) {
	t.Helper()
	if c.name != DefaultAfplay || len(c.args) != 3 || c.args[0] != "-v" || c.args[1] != "1" {
		t.Fatalf("call = %s %v", c.name, c.args)
	}
	want, err := RenderWAV(cue, volume)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(c.wav, want) {
		t.Fatalf("played file is not the %s cue at volume %v (%d bytes)", cue, volume, len(c.wav))
	}
}

func TestChimePlayer(t *testing.T) {
	t.Run("lid-close default renders the two-tone cue and plays it at unity gain", func(t *testing.T) {
		fx := newFixture(t)
		fx.player.PlayLidCloseChime(0.5, "default")
		fx.player.Wait()
		calls := fx.exec.recorded()
		if len(calls) != 1 {
			t.Fatalf("calls = %v", calls)
		}
		expectSynth(t, calls[0], CueLidClose, 0.5)
		if files := fx.tempFiles(t); len(files) != 0 {
			t.Fatalf("temp files left behind: %v", files)
		}
	})

	t.Run("lid-close system sound plays the aiff at the clamped volume", func(t *testing.T) {
		fx := newFixture(t)
		fx.player.PlayLidCloseChime(1.7, "Glass")
		fx.player.Wait()
		calls := fx.exec.recorded()
		want := []string{"-v", "1", filepath.Join(fx.sounds, "Glass.aiff")}
		if len(calls) != 1 || !slices.Equal(calls[0].args, want) {
			t.Fatalf("calls = %v, want args %v", calls, want)
		}
		fx.player.PlayLidCloseChime(0.25, "Glass")
		fx.player.Wait()
		if got := fx.exec.recorded()[1].args[1]; got != "0.25" {
			t.Fatalf("volume arg = %q", got)
		}
	})

	t.Run("lid-close unknown system sound falls back to the synthesized cue", func(t *testing.T) {
		fx := newFixture(t)
		fx.player.PlayLidCloseChime(0.5, "Nonexistent")
		fx.player.Wait()
		calls := fx.exec.recorded()
		if len(calls) != 1 {
			t.Fatalf("calls = %v", calls)
		}
		expectSynth(t, calls[0], CueLidClose, 0.5)
	})

	t.Run("lid-close off and muted play nothing", func(t *testing.T) {
		fx := newFixture(t)
		fx.player.PlayLidCloseChime(0.5, "off")
		fx.muted = true
		fx.player.PlayLidCloseChime(0.5, "default")
		fx.player.PlayLidCloseChime(0.5, "Glass")
		fx.player.Wait()
		if calls := fx.exec.recorded(); len(calls) != 0 {
			t.Fatalf("calls = %v", calls)
		}
	})

	t.Run("sleep cue default plays the given cue and waits for it", func(t *testing.T) {
		fx := newFixture(t)
		for _, cue := range AllCues()[1:] {
			fx.player.PlaySleepCue(context.Background(), "default", cue, 0.8)
		}
		calls := fx.exec.recorded()
		if len(calls) != 4 {
			t.Fatalf("calls = %d", len(calls))
		}
		for i, cue := range AllCues()[1:] {
			expectSynth(t, calls[i], cue, 0.8)
		}
		if files := fx.tempFiles(t); len(files) != 0 {
			t.Fatalf("temp files left behind: %v", files)
		}
	})

	t.Run("sleep cue without a cue defaults to work complete", func(t *testing.T) {
		fx := newFixture(t)
		fx.player.PlaySleepCue(context.Background(), "default", "", 0.5)
		expectSynth(t, fx.exec.recorded()[0], CueSleepWorkComplete, 0.5)
	})

	t.Run("sleep cue system sound plays the aiff", func(t *testing.T) {
		fx := newFixture(t)
		fx.player.PlaySleepCue(context.Background(), "Glass", "", 0.5)
		calls := fx.exec.recorded()
		want := []string{"-v", "0.5", filepath.Join(fx.sounds, "Glass.aiff")}
		if len(calls) != 1 || !slices.Equal(calls[0].args, want) {
			t.Fatalf("calls = %v, want args %v", calls, want)
		}
	})

	t.Run("sleep cue unknown system sound falls back to the synthesized cue", func(t *testing.T) {
		fx := newFixture(t)
		// The upstream decision carries no cue for a named sound, so the fallback is work complete.
		fx.player.PlaySleepCue(context.Background(), "Nonexistent", "", 0.5)
		expectSynth(t, fx.exec.recorded()[0], CueSleepWorkComplete, 0.5)
	})

	t.Run("sleep cue off and muted play nothing", func(t *testing.T) {
		fx := newFixture(t)
		fx.player.PlaySleepCue(context.Background(), "off", CueSleepHoldExpired, 0.5)
		// "" is the upstream decision's silence (no sound name), not a request for the default.
		fx.player.PlaySleepCue(context.Background(), "", "", 0.5)
		fx.muted = true
		fx.player.PlaySleepCue(context.Background(), "default", CueSleepHoldExpired, 0.5)
		if calls := fx.exec.recorded(); len(calls) != 0 {
			t.Fatalf("calls = %v", calls)
		}
	})

	t.Run("a wedged afplay cannot stall the sleep gate past the timeout", func(t *testing.T) {
		fx := newFixture(t)
		fx.exec.block = true
		fx.player.SleepCueTimeout = 50 * time.Millisecond
		start := time.Now()
		fx.player.PlaySleepCue(context.Background(), "default", CueSleepWorkComplete, 0.5)
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("PlaySleepCue took %v", elapsed)
		}
		if len(fx.exec.recorded()) != 1 {
			t.Fatal("expected one afplay call")
		}
		if files := fx.tempFiles(t); len(files) != 0 {
			t.Fatalf("temp files left behind: %v", files)
		}
	})

	t.Run("a cancelled context ends the wait", func(t *testing.T) {
		fx := newFixture(t)
		fx.exec.block = true
		fx.player.SleepCueTimeout = time.Hour
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(20*time.Millisecond, cancel)
		done := make(chan struct{})
		go func() {
			fx.player.PlaySleepCue(ctx, "Glass", "", 0.5)
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("PlaySleepCue ignored the cancelled context")
		}
	})

	t.Run("a hung mute probe cannot stall the sleep gate past the timeout", func(t *testing.T) {
		fx := newFixture(t)
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		fx.player.Muted = func() bool { <-release; return false }
		fx.player.SleepCueTimeout = 50 * time.Millisecond
		done := make(chan struct{})
		go func() {
			fx.player.PlaySleepCue(context.Background(), "default", CueSleepWorkComplete, 0.5)
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("PlaySleepCue waited on the hung mute probe")
		}
		if calls := fx.exec.recorded(); len(calls) != 0 {
			t.Fatalf("calls = %v", calls)
		}
		if files := fx.tempFiles(t); len(files) != 0 {
			t.Fatalf("temp files left behind: %v", files)
		}
	})

	t.Run("a cancelled context ends a hung mute probe", func(t *testing.T) {
		fx := newFixture(t)
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		fx.player.Muted = func() bool { <-release; return false }
		fx.player.SleepCueTimeout = time.Hour
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(20*time.Millisecond, cancel)
		done := make(chan struct{})
		go func() {
			fx.player.PlaySleepCue(ctx, "Glass", "", 0.5)
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("PlaySleepCue ignored the cancelled context during the mute probe")
		}
		if calls := fx.exec.recorded(); len(calls) != 0 {
			t.Fatalf("calls = %v", calls)
		}
	})

	t.Run("an afplay launch failure is logged, not fatal", func(t *testing.T) {
		fx := newFixture(t)
		fx.exec.err = errors.New("exec: no such file")
		fx.player.PlaySleepCue(context.Background(), "default", CueSleepUserAction, 0.5)
		fx.player.PlayLidCloseChime(0.5, "default")
		fx.player.Wait()
		if len(fx.exec.recorded()) != 2 {
			t.Fatal("expected two attempts")
		}
		if files := fx.tempFiles(t); len(files) != 0 {
			t.Fatalf("temp files left behind: %v", files)
		}
	})

	t.Run("a render failure plays nothing", func(t *testing.T) {
		fx := newFixture(t)
		fx.player.TempDir = filepath.Join(fx.tmp, "missing")
		fx.player.PlaySleepCue(context.Background(), "default", CueSleepUserAction, 0.5)
		fx.player.PlaySleepCue(context.Background(), "default", "bogus", 0.5)
		fx.player.PlayLidCloseChime(0.5, "default")
		fx.player.Wait()
		if calls := fx.exec.recorded(); len(calls) != 0 {
			t.Fatalf("calls = %v", calls)
		}
	})

	t.Run("sound names cannot leave the sounds directory", func(t *testing.T) {
		fx := newFixture(t)
		outside := filepath.Join(t.TempDir(), "evil.aiff")
		if err := os.WriteFile(outside, []byte("aiff"), 0o644); err != nil {
			t.Fatal(err)
		}
		rel, err := filepath.Rel(fx.sounds, outside[:len(outside)-len(".aiff")])
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{rel, "", ".", ".."} {
			if _, ok := fx.player.systemSoundPath(name); ok {
				t.Errorf("systemSoundPath(%q) resolved", name)
			}
		}
		if _, ok := fx.player.systemSoundPath("Glass"); !ok {
			t.Error("Glass should resolve")
		}
	})

	t.Run("concurrent lid-close chimes each get their own file", func(t *testing.T) {
		fx := newFixture(t)
		for range 5 {
			fx.player.PlayLidCloseChime(0.5, "default")
		}
		fx.player.Wait()
		calls := fx.exec.recorded()
		if len(calls) != 5 {
			t.Fatalf("calls = %d", len(calls))
		}
		paths := map[string]bool{}
		for _, c := range calls {
			expectSynth(t, c, CueLidClose, 0.5)
			paths[c.args[2]] = true
		}
		if len(paths) != 5 {
			t.Fatalf("paths = %v", paths)
		}
	})

	t.Run("new player carries the production defaults", func(t *testing.T) {
		p := NewPlayer(nil)
		if p.Exec == nil || p.Muted == nil || p.Afplay != "/usr/bin/afplay" || p.SoundsDir != "/System/Library/Sounds" ||
			p.SleepCueTimeout != 6*time.Second || p.TempDir == "" {
			t.Fatalf("NewPlayer = %+v", p)
		}
	})
}
