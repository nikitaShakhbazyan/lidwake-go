package daemon

import (
	"os"
	"path/filepath"
	"syscall"
)

// ExecutableStaleness detects that the running executable's file on disk has been replaced — the
// sign that an update swapped the binary out from under this long-lived process.
//
// A daemon launched by launchd keeps running the image it was started from after the file on disk
// is replaced. With KeepAlive set it adopts the new binary simply by exiting: launchd relaunches it
// from disk. This type answers "should I exit?".
//
// The check is identity-based, not version-based: it records the file's device, inode, size and
// mtime at launch and compares them with the live file on demand. That needs no compiled-in version
// string and catches any replacement, a same-version rebuild included. It fails safe: when either
// identity cannot be read (no path, or the file observed mid-replace), HasBeenReplaced reports
// false, so a transient stat error never forces a needless restart of a service that may be keeping
// the Mac awake.
type ExecutableStaleness struct {
	path   string
	launch fileSignature
	ok     bool
}

type fileSignature struct {
	dev       int64
	ino       uint64
	size      int64
	mtimeSec  int64
	mtimeNsec int64
}

// NewExecutableStaleness records path's identity now. Construct it once, at process launch, so the
// recorded identity is the binary the process actually runs: built later, it would record whatever
// is on disk by then, which after an update is the new file. An empty path never reports a
// replacement.
func NewExecutableStaleness(path string) *ExecutableStaleness {
	sig, ok := signatureOf(path)
	return &ExecutableStaleness{path: path, launch: sig, ok: ok}
}

// RunningExecutableStaleness records the identity of this process's own executable.
func RunningExecutableStaleness() *ExecutableStaleness {
	return NewExecutableStaleness(runningExecutable())
}

// HasBeenReplaced reports whether the file at the recorded path now differs from what it was at
// launch. It is false whenever either identity is unreadable, and on a nil receiver.
func (s *ExecutableStaleness) HasBeenReplaced() bool {
	if s == nil || !s.ok {
		return false
	}
	current, ok := signatureOf(s.path)
	return ok && current != s.launch
}

func signatureOf(path string) (fileSignature, bool) {
	if path == "" {
		return fileSignature{}, false
	}
	info, err := os.Stat(path)
	if err != nil {
		return fileSignature{}, false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fileSignature{}, false
	}
	return fileSignature{
		dev:       int64(st.Dev),
		ino:       st.Ino,
		size:      st.Size,
		mtimeSec:  st.Mtimespec.Sec,
		mtimeNsec: st.Mtimespec.Nsec,
	}, true
}

// runningExecutable is this process's executable with symlinks resolved, so the same physical file
// always yields the same path; "" when unknown.
func runningExecutable() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		return resolved
	}
	return exe
}
