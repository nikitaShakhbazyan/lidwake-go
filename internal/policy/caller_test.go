package policy

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nikitaShakhbazyan/lidwake-go/internal/darwin"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/paths"
)

// The full socket authorization path needs a live peer, so it can't be unit-tested here. These
// cover the decisions: the identifier allow-list (team-signed builds) and the install-path trust
// that is the entire gate for an unsigned build.
func TestCallerVerifier(t *testing.T) {
	t.Run("reverse-DNS identifiers under our prefix are accepted", func(t *testing.T) {
		for _, id := range []string{
			"io.github.nikitashakhbazyan.lidwake",
			"io.github.nikitashakhbazyan.lidwake.daemon",
			"io.github.nikitashakhbazyan.lidwake.helper",
		} {
			if !IsLidwakeComponent(id) {
				t.Errorf("%q rejected", id)
			}
		}
	})

	// One binary is CLI, daemon and helper, so there are no per-tool identifiers any more: a
	// team-signed build is signed with the reverse-DNS identifier, and the old tool names (or the
	// bare binary name, which an unsigned identifier defaults to) are not ours.
	t.Run("the tool names are accepted exactly", func(t *testing.T) {
		for _, id := range []string{"lidwake-daemon", "lidwake-helper", "lidwake"} {
			if IsLidwakeComponent(id) {
				t.Errorf("%q accepted", id)
			}
		}
	})

	t.Run("look-alikes are rejected", func(t *testing.T) {
		for _, id := range []string{
			"lidwake-daemon-55554944572a111aa4e631978f328488fa7c4992",
			"io.github.nikitashakhbazyan.lidwakeevil",
			"com.evil.lidwake",
			"evil.io.github.nikitashakhbazyan.lidwake",
			"",
		} {
			if IsLidwakeComponent(id) {
				t.Errorf("%q accepted", id)
			}
		}
	})
}

func TestCallerVerifierDecision(t *testing.T) {
	const team = "52K336H235"
	installed := paths.InstalledBin
	rootOnly := func(string) bool { return true }
	peer := func(mod func(*darwin.PeerCode)) darwin.PeerCode {
		p := darwin.PeerCode{
			PID: 4242, UID: 501, Path: installed, Identifier: paths.Label,
			HardenedRuntime: true, Valid: true,
		}
		if mod != nil {
			mod(&p)
		}
		return p
	}

	t.Run("team-signed self rejects a caller with no team or another team", func(t *testing.T) {
		if AuthorizePeer(team, peer(nil), rootOnly) {
			t.Error("no team accepted")
		}
		if AuthorizePeer(team, peer(func(p *darwin.PeerCode) { p.Team = "EVILTEAM00" }), rootOnly) {
			t.Error("another team accepted")
		}
	})

	t.Run("team-signed self accepts its own component and rejects a foreign identifier", func(t *testing.T) {
		if !AuthorizePeer(team, peer(func(p *darwin.PeerCode) { p.Team = team }), rootOnly) {
			t.Error("own component rejected")
		}
		if AuthorizePeer(team, peer(func(p *darwin.PeerCode) { p.Team = team; p.Identifier = "com.example.other" }), rootOnly) {
			t.Error("foreign identifier accepted")
		}
	})

	t.Run("team-signed self still requires valid code", func(t *testing.T) {
		if AuthorizePeer(team, peer(func(p *darwin.PeerCode) { p.Team = team; p.Valid = false }), rootOnly) {
			t.Error("invalid code accepted")
		}
	})

	t.Run("unsigned self trusts a hardened binary from the root-only install directory", func(t *testing.T) {
		if !AuthorizePeer("", peer(nil), rootOnly) {
			t.Error("installed binary rejected")
		}
	})

	t.Run("unsigned self ignores the claimed identifier and checks the location", func(t *testing.T) {
		// Any ad-hoc binary can call itself anything; where it runs from is what counts.
		if AuthorizePeer("", peer(func(p *darwin.PeerCode) { p.Path = "/Users/me/src/lidwake/lidwake" }), rootOnly) {
			t.Error("binary outside the install directory accepted")
		}
		if AuthorizePeer("", peer(func(p *darwin.PeerCode) { p.Path = "" }), rootOnly) {
			t.Error("unknown path accepted")
		}
		if !AuthorizePeer("", peer(func(p *darwin.PeerCode) { p.Identifier = "a.out" }), rootOnly) {
			t.Error("the identifier should not matter")
		}
		if AuthorizePeer("", peer(func(p *darwin.PeerCode) { p.Path = paths.InstallDir + "/other" }), rootOnly) {
			t.Error("a sibling of the installed binary accepted")
		}
	})

	t.Run("unsigned self requires the hardened runtime", func(t *testing.T) {
		if AuthorizePeer("", peer(func(p *darwin.PeerCode) { p.HardenedRuntime = false }), rootOnly) {
			t.Error("non-hardened binary accepted")
		}
	})

	t.Run("unsigned self requires valid code", func(t *testing.T) {
		if AuthorizePeer("", peer(func(p *darwin.PeerCode) { p.Valid = false }), rootOnly) {
			t.Error("invalid code accepted")
		}
	})

	t.Run("a writable component anywhere on the path breaks trust", func(t *testing.T) {
		chain := []string{installed, "/usr/local/libexec/lidwake", "/usr/local/libexec", "/usr/local", "/usr", "/"}
		var checked []string
		if !IsTrustedInstallPath(installed, func(p string) bool { checked = append(checked, p); return true }) {
			t.Fatal("all-root chain rejected")
		}
		if len(checked) != len(chain) {
			t.Fatalf("checked %v, want %v", checked, chain)
		}
		for i := range chain {
			if checked[i] != chain[i] {
				t.Fatalf("checked %v, want %v", checked, chain)
			}
		}
		for _, writable := range chain {
			if IsTrustedInstallPath(installed, func(p string) bool { return p != writable }) {
				t.Errorf("writable %s did not break trust", writable)
			}
			if AuthorizePeer("", peer(nil), func(p string) bool { return p != writable }) {
				t.Errorf("writable %s did not break the decision", writable)
			}
		}
	})

	t.Run("dot segments and look-alike directories are rejected", func(t *testing.T) {
		for _, p := range []string{
			"/usr/local/libexec/lidwake/../evil/lidwake",
			"/usr/local/libexec/lidwake/./lidwake",
			"/usr/local/libexec/lidwake-evil/lidwake",
			"/usr/local/libexec/lidwake",
			"/usr/local/libexec/lidwake/lidwake/",
		} {
			if IsTrustedInstallPath(p, rootOnly) {
				t.Errorf("%s trusted", p)
			}
		}
	})

	t.Run("the real file system check refuses user-writable paths", func(t *testing.T) {
		if !IsWritableByRootOnly("/usr/bin/true") {
			t.Error("/usr/bin/true refused")
		}
		dir := t.TempDir()
		if IsWritableByRootOnly(dir) {
			t.Error("a user-owned temp directory accepted")
		}
		if IsWritableByRootOnly("/no/such/file") {
			t.Error("a missing file accepted")
		}
		// A symlink is refused even when root owns it and its target: lstat sees the link.
		if fi, err := os.Lstat("/etc"); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			if IsWritableByRootOnly("/etc") {
				t.Error("the root-owned /etc symlink accepted")
			}
			if !IsWritableByRootOnly("/private/etc") {
				t.Error("/private/etc refused")
			}
		}
		link := filepath.Join(dir, "true")
		if err := os.Symlink("/usr/bin/true", link); err != nil {
			t.Fatal(err)
		}
		if IsWritableByRootOnly(link) {
			t.Error("a symlink accepted")
		}
	})
}
