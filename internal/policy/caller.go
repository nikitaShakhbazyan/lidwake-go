package policy

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/nikitaShakhbazyan/lidwake-go/internal/darwin"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/paths"
)

// AuthorizePeer decides whether the root helper trusts the process on the other end of its
// socket; only the lidwake daemon may drive it. ownTeam is the helper's own team identifier ("" when
// ad-hoc signed); isRootOnly is IsWritableByRootOnly in production.
//
//   - Team-signed build (someone shipping with a Developer ID): the peer must share our team
//     identifier and be one of our components by signing identifier. A peer with no team is
//     rejected: its identifier would be unanchored.
//   - Unsigned build (the default — built from source, ad-hoc signed): there is no certificate to
//     anchor trust in, and an ad-hoc binary can claim any identifier. Trust comes from where the
//     peer runs instead: its executable must be exactly paths.InstalledBin with every path
//     component writable by root alone, so only root could have put it there. It must also run
//     with the hardened runtime, which keeps a user-editable LaunchAgent plist from injecting a
//     library through DYLD_INSERT_LIBRARIES.
//
// Either way the peer's code must have passed the validity check: a peer that can't be
// validated is an unknown caller.
func AuthorizePeer(ownTeam string, peer darwin.PeerCode, isRootOnly func(path string) bool) bool {
	if !peer.Valid {
		return false
	}
	if ownTeam != "" {
		return peer.Team == ownTeam && IsLidwakeComponent(peer.Identifier)
	}
	if !peer.HardenedRuntime || peer.Path != paths.InstalledBin {
		return false
	}
	return IsTrustedInstallPath(peer.Path, isRootOnly)
}

// IsLidwakeComponent reports whether a signing identifier is ours: the reverse-DNS label itself
// or a dotted child of it. A bare prefix test would also admit "…lidwakeevil".
func IsLidwakeComponent(identifier string) bool {
	return identifier == paths.Label || strings.HasPrefix(identifier, paths.Label+".")
}

// IsTrustedInstallPath reports whether path is the installed binary and every path component,
// up to "/", is one only root can modify.
func IsTrustedInstallPath(path string, isRootOnly func(path string) bool) bool {
	if path != paths.InstalledBin {
		return false
	}
	current := path
	for {
		if !isRootOnly(current) {
			return false
		}
		if current == "/" {
			return true
		}
		current = filepath.Dir(current)
	}
}

// IsWritableByRootOnly reports whether path is owned by root, not writable by group or others,
// and not a symlink (checked with lstat, so a link can't stand in for a root-owned target).
func IsWritableByRootOnly(path string) bool {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&fs.ModeSymlink != 0 {
		return false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	return st.Uid == 0 && info.Mode().Perm()&0o022 == 0
}
