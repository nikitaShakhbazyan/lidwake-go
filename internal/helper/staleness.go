package helper

import (
	"os"
	"path/filepath"
	"syscall"
)

// A helper launched by launchd keeps running its original image after an update replaces the
// file on disk. With KeepAlive set it adopts the new binary simply by exiting. The check is
// identity-based, not version-based: it records the file's device, inode, size and mtime at
// launch, so any replacement counts, a same-version rebuild included.

type fileSignature struct {
	dev       int64
	ino       uint64
	size      int64
	mtimeSec  int64
	mtimeNsec int64
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

// replacementCheck records path's identity now — call it once at launch, before an update can
// land — and returns a check that reports whether the file at path has changed since. It fails
// safe: when either identity can't be read (no path, or the file observed mid-replace) it
// reports false, because a spurious "replaced" would restart a helper that may be keeping the
// Mac awake.
func replacementCheck(path string) func() bool {
	launch, ok := signatureOf(path)
	return func() bool {
		if !ok {
			return false
		}
		current, ok := signatureOf(path)
		return ok && current != launch
	}
}

// runningExecutable is this process's executable with symlinks resolved, so the same physical
// file always yields the same path; "" when unknown.
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
