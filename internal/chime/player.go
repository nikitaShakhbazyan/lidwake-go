package chime

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultAfplay plays the cues. Not an in-process audio engine: a LaunchAgent can drive one
	// without error yet produce no audible output, while afplay routes to hardware from any
	// context.
	DefaultAfplay = "/usr/bin/afplay"
	// DefaultSoundsDir holds the named macOS system sounds (`<Name>.aiff`).
	DefaultSoundsDir = "/System/Library/Sounds"
	// DefaultSleepCueTimeout bounds how long PlaySleepCue waits, so a wedged afplay or mute probe
	// cannot stall the sleep gate.
	DefaultSleepCueTimeout = 6 * time.Second

	// SoundDefault selects the synthesized cue; SoundOff plays nothing. Any other sound name is a
	// macOS system sound.
	SoundDefault = "default"
	SoundOff     = "off"

	// lidCloseTimeout reaps a fire-and-forget afplay that never exits.
	lidCloseTimeout = 30 * time.Second
	// mutedProbeTimeout bounds the osascript mute probe.
	mutedProbeTimeout = 2 * time.Second
)

// ExecFunc runs name with args and waits for it to exit; cancelling ctx kills it.
type ExecFunc func(ctx context.Context, name string, args ...string) error

// Player plays the daemon's audio cues: the lid-close confirmation chime (fire and forget) and the
// pre-sleep cue (awaited, so the caller can sequence "cue audible, then allow sleep").
//
// The default cues are synthesized by Samples/RenderWAV, written to a temp file and played with
// afplay; the user may instead pick a named macOS system sound. Cues are skipped while the system
// output is muted (the lid-open summary covers that case) and respect the configured volume.
//
// The zero value works with the defaults; NewPlayer fills them in explicitly. Fields must not be
// changed while a cue is playing.
type Player struct {
	// Exec runs afplay. Defaults to exec.CommandContext with stdio discarded; tests inject a fake
	// so nothing is ever played.
	Exec ExecFunc
	// Muted reports whether the default output device is muted. Nil reads as not muted.
	Muted func() bool
	// Afplay is the player binary (DefaultAfplay when empty).
	Afplay string
	// SoundsDir holds the system sounds (DefaultSoundsDir when empty).
	SoundsDir string
	// TempDir receives the rendered WAV files (os.TempDir() when empty). Each play writes its own
	// file and removes it once afplay exits.
	TempDir string
	// SleepCueTimeout bounds PlaySleepCue (DefaultSleepCueTimeout when zero).
	SleepCueTimeout time.Duration
	// Log receives the player's notices (slog.Default() when nil).
	Log *slog.Logger

	wg sync.WaitGroup
}

// NewPlayer returns a player with the production defaults, including the osascript mute probe.
func NewPlayer(log *slog.Logger) *Player {
	return &Player{
		Exec:            RunCommand,
		Muted:           OSAOutputMuted,
		Afplay:          DefaultAfplay,
		SoundsDir:       DefaultSoundsDir,
		TempDir:         os.TempDir(),
		SleepCueTimeout: DefaultSleepCueTimeout,
		Log:             log,
	}
}

// PlayLidCloseChime plays the lid-close chime without waiting for it. chimeName is SoundDefault
// for the synthesized two-tone cue, SoundOff for silence, or a macOS system sound name (an unknown
// one falls back to the synthesized cue).
func (p *Player) PlayLidCloseChime(volume float64, chimeName string) {
	if chimeName == SoundOff {
		return
	}
	// Bounded: the lid-close handler (and the screen lock after it) waits for this call.
	probe, cancel := context.WithTimeout(context.Background(), mutedProbeTimeout)
	muted, _ := p.mutedWithin(probe)
	cancel()
	if muted {
		p.log().Info("lid-close chime skipped — system output muted")
		return
	}
	v := clampVolume(volume)
	path, playVolume, cleanup, err := p.resolve(chimeName, CueLidClose, v)
	if err != nil {
		p.log().Error("lid-close chime — failed to render two-tone", "err", err)
		return
	}
	if chimeName != SoundDefault && cleanup == nil {
		p.log().Info("playing lid-close system sound", "sound", chimeName, "volume", v)
	} else {
		p.log().Info("playing lid-close two-tone chime", "volume", v)
	}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), lidCloseTimeout)
		defer cancel()
		p.afplay(ctx, path, playVolume)
		if cleanup != nil {
			cleanup()
		}
	}()
}

// PlaySleepCue plays the pre-sleep cue and waits for playback to finish, bounded by
// SleepCueTimeout and ctx, so the daemon clears the sleep block only after the cue has been heard
// — with the lid closed, that clear is what lets the Mac sleep. The bound covers the mute probe
// too. On timeout a wedged afplay is killed rather than left running.
//
// soundName is SoundDefault for the synthesized cue (CueSleepWorkComplete when cue is empty),
// SoundOff or "" for silence, or a macOS system sound name. An unknown system sound falls back to
// the synthesized cue. Whether to play at all, and which cue, is decided upstream per release
// cause; "" is that decision's silence, so passing a silent decision through plays nothing.
func (p *Player) PlaySleepCue(ctx context.Context, soundName string, cue Cue, volume float64) {
	if soundName == SoundOff || soundName == "" {
		return
	}
	timeout := p.SleepCueTimeout
	if timeout <= 0 {
		timeout = DefaultSleepCueTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	muted, answered := p.mutedWithin(ctx)
	if !answered {
		p.log().Warn("pre-sleep cue skipped — mute probe did not answer in time")
		return
	}
	if muted {
		p.log().Info("pre-sleep cue skipped — system output muted")
		return
	}
	if cue == "" {
		cue = CueSleepWorkComplete
	}
	v := clampVolume(volume)
	path, playVolume, cleanup, err := p.resolve(soundName, cue, v)
	if err != nil {
		p.log().Error("pre-sleep cue — failed to render", "sound", soundName, "cue", string(cue), "err", err)
		return
	}
	if cleanup != nil {
		defer cleanup()
	}
	p.log().Info("playing pre-sleep cue", "sound", soundName, "volume", v)
	p.afplay(ctx, path, playVolume)
}

// Wait blocks until every fire-and-forget chime has finished (or been reaped).
func (p *Player) Wait() {
	p.wg.Wait()
}

// resolve picks what to play: the named system sound when it exists (at volume v), otherwise the
// synthesized cue rendered to a fresh temp file (volume baked in, played at unity gain). cleanup
// removes that temp file and is nil for a system sound.
func (p *Player) resolve(soundName string, cue Cue, v float64) (path string, playVolume float64, cleanup func(), err error) {
	if soundName != SoundDefault {
		if sound, ok := p.systemSoundPath(soundName); ok {
			return sound, v, nil, nil
		}
	}
	path, err = p.render(cue, v)
	if err != nil {
		return "", 0, nil, err
	}
	return path, 1, func() { _ = os.Remove(path) }, nil
}

// systemSoundPath is `<SoundsDir>/<name>.aiff` when it exists. A name that is not a plain file
// name never resolves, so a config value cannot point afplay outside the sounds directory.
func (p *Player) systemSoundPath(name string) (string, bool) {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\x00") {
		return "", false
	}
	dir := p.SoundsDir
	if dir == "" {
		dir = DefaultSoundsDir
	}
	path := filepath.Join(dir, name+".aiff")
	if _, err := os.Stat(path); err != nil {
		return "", false
	}
	return path, true
}

// render writes cue at volume v to a new temp file and returns its path.
func (p *Player) render(cue Cue, v float64) (string, error) {
	data, err := RenderWAV(cue, v)
	if err != nil {
		return "", err
	}
	dir := p.TempDir
	if dir == "" {
		dir = os.TempDir()
	}
	f, err := os.CreateTemp(dir, "lidwake-"+string(cue)+"-*.wav")
	if err != nil {
		return "", fmt.Errorf("create cue file: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", fmt.Errorf("write cue file: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", fmt.Errorf("write cue file: %w", err)
	}
	return f.Name(), nil
}

func (p *Player) afplay(ctx context.Context, path string, volume float64) {
	run := p.Exec
	if run == nil {
		run = RunCommand
	}
	bin := p.Afplay
	if bin == "" {
		bin = DefaultAfplay
	}
	err := run(ctx, bin, "-v", strconv.FormatFloat(volume, 'f', -1, 64), path)
	switch {
	case ctx.Err() != nil:
		p.log().Warn("afplay did not finish in time", "path", path)
	case err != nil:
		p.log().Error("afplay failed", "path", path, "err", err)
	}
}

func (p *Player) muted() bool {
	return p.Muted != nil && p.Muted()
}

// mutedWithin is muted bounded by ctx; answered is false when ctx ends first. Muted may be an
// in-process CoreAudio call that a wedged coreaudiod never returns from, and a cgo call cannot be
// interrupted, so the probe runs on its own goroutine, left behind if it never answers.
func (p *Player) mutedWithin(ctx context.Context) (muted, answered bool) {
	if p.Muted == nil {
		return false, true
	}
	result := make(chan bool, 1)
	go func() { result <- p.Muted() }()
	select {
	case muted = <-result:
		return muted, true
	case <-ctx.Done():
		return false, false
	}
}

func (p *Player) log() *slog.Logger {
	if p.Log != nil {
		return p.Log
	}
	return slog.Default()
}

// RunCommand runs name with args, discarding its output, and waits for it; cancelling ctx kills
// it.
func RunCommand(ctx context.Context, name string, args ...string) error {
	return exec.CommandContext(ctx, name, args...).Run()
}

// OSAOutputMuted reports whether the default output device is muted, asking
// `osascript -e "output muted of (get volume settings)"`. Best effort: any failure, or a device
// without a mute control ("missing value"), reads as not muted.
func OSAOutputMuted() bool {
	ctx, cancel := context.WithTimeout(context.Background(), mutedProbeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/bin/osascript", "-e", "output muted of (get volume settings)").Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "true"
}
