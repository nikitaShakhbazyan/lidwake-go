package install

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/nikitaShakhbazyan/lidwake-go/internal/hooks"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/paths"
)

// tempLayout points every location into a fresh temporary root shaped like the real system.
func tempLayout(t *testing.T) (Layout, string) {
	t.Helper()
	root := t.TempDir()
	at := func(p string) string { return filepath.Join(root, p) }
	home := at("Users/me")
	return Layout{
		InstallDir:           at("usr/local/libexec/lidwake"),
		InstalledBin:         at("usr/local/libexec/lidwake/lidwake"),
		BinSymlink:           at("usr/local/bin/lidwake"),
		HelperPlist:          at("Library/LaunchDaemons/" + paths.HelperLabel + ".plist"),
		HelperLog:            at("var/log/lidwake-helper.log"),
		HelperDataDir:        at("var/db/lidwake"),
		OriginalSleepSetting: at("var/db/lidwake/sleep-disabled-before"),
		HelperRunDir:         at("var/run/lidwake"),
		DaemonPlist:          filepath.Join(home, "Library/LaunchAgents", paths.DaemonLabel+".plist"),
		DaemonLog:            filepath.Join(home, "Library/Logs/lidwake/daemon.log"),
		SupportDir:           filepath.Join(home, "Library/Application Support/lidwake"),
	}, root
}

type rig struct {
	*Installer
	out, err  *bytes.Buffer
	commands  []Command
	chowned   map[string]bool
	exe       string
	failOn    func(Command) error
	onCommand func(Command)
}

func newRig(t *testing.T, euid int) *rig {
	t.Helper()
	l, root := tempLayout(t)
	for _, dir := range []string{"Library/LaunchDaemons", "var/log", "var/run", "var/db"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	exe := filepath.Join(root, "Downloads", "lidwake")
	if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, []byte("#!lidwake binary v2"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := &rig{out: &bytes.Buffer{}, err: &bytes.Buffer{}, chowned: map[string]bool{}, exe: exe}
	r.Installer = &Installer{
		Layout:   l,
		Out:      r.out,
		Err:      r.err,
		UID:      501,
		Username: "me",
		Geteuid:  func() int { return euid },
		Executable: func() (string, error) {
			return exe, nil
		},
		Chown: func(path string, uid, gid int) error {
			if uid != 0 || gid != 0 {
				return fmt.Errorf("chown %s to %d:%d", path, uid, gid)
			}
			r.chowned[path] = true
			return nil
		},
		IsRootOnly:          func(string) bool { return true },
		HasInstallSignature: func(string) bool { return false },
		Hooks: &hooks.Installer{
			CLIPath:         l.InstalledBin,
			Home:            filepath.Join(root, "Users/me"),
			ApplicationsDir: filepath.Join(root, "Applications"),
			SearchPath:      []string{},
		},
		Sudo:      "/usr/bin/sudo",
		Launchctl: "/bin/launchctl",
		Codesign:  "/usr/bin/codesign",
		Pmset:     "/usr/bin/pmset",
	}
	r.Run = func(c Command) error {
		r.commands = append(r.commands, c)
		if r.onCommand != nil {
			r.onCommand(c)
		}
		if r.failOn != nil {
			return r.failOn(c)
		}
		return nil
	}
	return r
}

func expectCommands(t *testing.T, got, want []Command) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("commands:\n%s\nwant:\n%s", describeCommands(got), describeCommands(want))
	}
	for i := range want {
		if !reflect.DeepEqual(got[i], want[i]) {
			t.Fatalf("command %d:\n got %+v\nwant %+v", i, got[i], want[i])
		}
	}
}

func describeCommands(cs []Command) string {
	var b strings.Builder
	for _, c := range cs {
		fmt.Fprintf(&b, "  %+v\n", c)
	}
	return b.String()
}

// ---- plists ----------------------------------------------------------------------------------

// plistDict decodes a plist's top-level dict into key → value (string, bool or []string), and
// fails on anything that isn't a well-formed plist of that shape.
func plistDict(t *testing.T, data []byte) map[string]any {
	t.Helper()
	if !bytes.HasPrefix(data, []byte(`<?xml version="1.0" encoding="UTF-8"?>`+"\n"+`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">`)) {
		t.Fatalf("header: %s", data)
	}
	type value struct {
		XMLName xml.Name
		Text    string   `xml:",chardata"`
		Strings []string `xml:"string"`
	}
	var doc struct {
		XMLName xml.Name `xml:"plist"`
		Version string   `xml:"version,attr"`
		Dict    struct {
			Items []value `xml:",any"`
		} `xml:"dict"`
	}
	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.Strict = true
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("not XML: %v\n%s", err, data)
	}
	if doc.Version != "1.0" {
		t.Fatalf("plist version %q", doc.Version)
	}
	items := doc.Dict.Items
	if len(items)%2 != 0 {
		t.Fatalf("odd number of dict items: %+v", items)
	}
	out := map[string]any{}
	for i := 0; i < len(items); i += 2 {
		k, v := items[i], items[i+1]
		if k.XMLName.Local != "key" {
			t.Fatalf("expected <key>, got <%s>", k.XMLName.Local)
		}
		if _, dup := out[k.Text]; dup {
			t.Fatalf("duplicate key %s", k.Text)
		}
		switch v.XMLName.Local {
		case "string":
			out[k.Text] = v.Text
		case "true":
			out[k.Text] = true
		case "false":
			out[k.Text] = false
		case "array":
			out[k.Text] = v.Strings
		default:
			t.Fatalf("unexpected value <%s> for %s", v.XMLName.Local, k.Text)
		}
	}
	return out
}

func lintPlist(t *testing.T, data []byte) {
	t.Helper()
	plutil, err := exec.LookPath("plutil")
	if err != nil {
		return
	}
	path := filepath.Join(t.TempDir(), "job.plist")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(plutil, "-lint", path).CombinedOutput(); err != nil {
		t.Fatalf("plutil -lint: %v: %s", err, out)
	}
}

func TestPlists(t *testing.T) {
	l := DefaultLayout()

	t.Run("helper plist has exactly the launchd keys", func(t *testing.T) {
		data := HelperPlist(l)
		want := map[string]any{
			"Label":             paths.HelperLabel,
			"ProgramArguments":  []string{paths.InstalledBin, "helper"},
			"RunAtLoad":         true,
			"KeepAlive":         true,
			"ProcessType":       "Background",
			"StandardOutPath":   paths.HelperLog,
			"StandardErrorPath": paths.HelperLog,
		}
		if got := plistDict(t, data); !reflect.DeepEqual(got, want) {
			t.Fatalf("got %#v\nwant %#v", got, want)
		}
		lintPlist(t, data)
	})

	t.Run("daemon plist has exactly the launchd keys", func(t *testing.T) {
		data := DaemonPlist(l)
		want := map[string]any{
			"Label":             paths.DaemonLabel,
			"ProgramArguments":  []string{paths.InstalledBin, "daemon"},
			"RunAtLoad":         true,
			"KeepAlive":         true,
			"ProcessType":       "Background",
			"StandardOutPath":   paths.DaemonLog(),
			"StandardErrorPath": paths.DaemonLog(),
		}
		if got := plistDict(t, data); !reflect.DeepEqual(got, want) {
			t.Fatalf("got %#v\nwant %#v", got, want)
		}
		lintPlist(t, data)
	})

	t.Run("paths are escaped", func(t *testing.T) {
		l := l
		l.DaemonLog = "/Users/Tom & Jerry/<logs>/daemon.log"
		data := DaemonPlist(l)
		if got := plistDict(t, data)["StandardOutPath"]; got != l.DaemonLog {
			t.Fatalf("log path %q", got)
		}
		lintPlist(t, data)
	})
}

// ---- setup -----------------------------------------------------------------------------------

func TestSetup(t *testing.T) {
	t.Run("user half: stop the daemon, run the root half, register the daemon", func(t *testing.T) {
		r := newRig(t, 501)
		if err := r.Setup(); err != nil {
			t.Fatal(err)
		}
		expectCommands(t, r.commands, []Command{
			{Path: "/usr/bin/sudo", Args: []string{"-v"}, Interactive: true},
			{Path: "/bin/launchctl", Args: []string{"bootout", "gui/501/" + paths.DaemonLabel}, MayFail: true},
			{Path: "/usr/bin/sudo", Args: []string{r.exe, "setup", "--root", "--uid", "501"}, Interactive: true},
			{Path: "/bin/launchctl", Args: []string{"bootstrap", "gui/501", r.Layout.DaemonPlist}},
		})
		data, err := os.ReadFile(r.Layout.DaemonPlist)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(data, DaemonPlist(r.Layout)) {
			t.Fatalf("daemon plist %s", data)
		}
		if info, err := os.Stat(r.Layout.DaemonPlist); err != nil || info.Mode().Perm() != 0o644 {
			t.Fatalf("plist mode %v, %v", info.Mode(), err)
		}
		if info, err := os.Stat(filepath.Dir(r.Layout.DaemonLog)); err != nil || !info.IsDir() {
			t.Fatalf("log dir: %v", err)
		}
		if !strings.HasSuffix(r.out.String(), "==> done — next: lidwake install-hooks (wire up your agents), then lidwake stats\n") {
			t.Errorf("stdout %q", r.out)
		}
		if len(r.chowned) != 0 {
			t.Errorf("user half chowned %v", r.chowned)
		}
	})

	t.Run("refuses to run as root", func(t *testing.T) {
		r := newRig(t, 0)
		if err := r.Setup(); !errors.Is(err, ErrRunAsUser) {
			t.Fatalf("err %v", err)
		}
		if len(r.commands) != 0 {
			t.Fatalf("ran %v", r.commands)
		}
	})

	failRootHalf := func(c Command) error {
		if c.Path == "/usr/bin/sudo" && slices.Contains(c.Args, "--root") {
			return errors.New("exit status 1")
		}
		return nil
	}

	t.Run("a failed root half stops before the daemon", func(t *testing.T) {
		r := newRig(t, 501)
		r.failOn = failRootHalf
		if err := r.Setup(); err == nil || !strings.Contains(err.Error(), "privileged setup") {
			t.Fatalf("err %v", err)
		}
		if len(r.commands) != 3 {
			t.Fatalf("commands %v", r.commands)
		}
		if _, err := os.Stat(r.Layout.DaemonPlist); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("daemon plist written: %v", err)
		}
	})

	// A cancelled password prompt, three wrong passwords or no terminal (an agent's shell) must not
	// stop a working install: sudo is asked before the daemon is touched.
	t.Run("a refused password leaves the running daemon alone", func(t *testing.T) {
		r := newRig(t, 501)
		r.failOn = func(c Command) error {
			if c.Path == "/usr/bin/sudo" {
				return errors.New("sudo: a terminal is required to read the password")
			}
			return nil
		}
		if err := r.Setup(); err == nil || !strings.Contains(err.Error(), "privileged setup") {
			t.Fatalf("err %v", err)
		}
		expectCommands(t, r.commands, []Command{
			{Path: "/usr/bin/sudo", Args: []string{"-v"}, Interactive: true},
		})
	})

	t.Run("a failed root half starts the earlier daemon again", func(t *testing.T) {
		r := newRig(t, 501)
		if err := os.MkdirAll(filepath.Dir(r.Layout.DaemonPlist), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(r.Layout.DaemonPlist, []byte("earlier plist"), 0o644); err != nil {
			t.Fatal(err)
		}
		r.failOn = failRootHalf
		if err := r.Setup(); err == nil || !strings.Contains(err.Error(), "privileged setup") {
			t.Fatalf("err %v", err)
		}
		expectCommands(t, r.commands, []Command{
			{Path: "/usr/bin/sudo", Args: []string{"-v"}, Interactive: true},
			{Path: "/bin/launchctl", Args: []string{"bootout", "gui/501/" + paths.DaemonLabel}, MayFail: true},
			{Path: "/usr/bin/sudo", Args: []string{r.exe, "setup", "--root", "--uid", "501"}, Interactive: true},
			{Path: "/bin/launchctl", Args: []string{"bootstrap", "gui/501", r.Layout.DaemonPlist}},
		})
		if data, _ := os.ReadFile(r.Layout.DaemonPlist); string(data) != "earlier plist" {
			t.Errorf("earlier plist replaced: %q", data)
		}
		expectOutputHas(t, r.err, "")
	})

	t.Run("an earlier daemon that won't start again is reported", func(t *testing.T) {
		r := newRig(t, 501)
		if err := os.MkdirAll(filepath.Dir(r.Layout.DaemonPlist), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(r.Layout.DaemonPlist, DaemonPlist(r.Layout), 0o644); err != nil {
			t.Fatal(err)
		}
		r.failOn = func(c Command) error {
			if slices.Contains(c.Args, "bootstrap") {
				return errors.New("Bootstrap failed: 5")
			}
			return failRootHalf(c)
		}
		if err := r.Setup(); err == nil || !strings.Contains(err.Error(), "privileged setup") {
			t.Fatalf("err %v", err)
		}
		if !strings.Contains(r.err.String(), "WARNING: could not start the daemon again (Bootstrap failed: 5)") {
			t.Errorf("stderr %q", r.err)
		}
	})

	t.Run("a stopped daemon may already be gone", func(t *testing.T) {
		r := newRig(t, 501)
		r.failOn = func(c Command) error {
			if c.MayFail {
				return errors.New("Boot-out failed: 3: No such process")
			}
			return nil
		}
		if err := r.Setup(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("a failed bootstrap is reported", func(t *testing.T) {
		r := newRig(t, 501)
		r.failOn = func(c Command) error {
			if slices.Contains(c.Args, "bootstrap") {
				return errors.New("Bootstrap failed: 5")
			}
			return nil
		}
		if err := r.Setup(); err == nil || !strings.Contains(err.Error(), "start the daemon") {
			t.Fatalf("err %v", err)
		}
	})

	t.Run("refreshes lidwake hooks that call an older binary and adds none", func(t *testing.T) {
		r := newRig(t, 501)
		home := r.Hooks.Home
		for _, dir := range []string{".claude", ".codex"} {
			if err := os.MkdirAll(filepath.Join(home, dir), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		old := &hooks.Installer{CLIPath: "/Applications/lidwake.app/Contents/MacOS/lidwake", Home: home,
			ApplicationsDir: r.Hooks.ApplicationsDir, SearchPath: []string{}}
		if _, err := old.Install("claude-code", false); err != nil {
			t.Fatal(err)
		}
		if got := r.Hooks.State("claude-code"); got != hooks.StateModifiedExternally {
			t.Fatalf("state before %s", got)
		}
		if err := r.Setup(); err != nil {
			t.Fatal(err)
		}
		if got := r.Hooks.State("claude-code"); got != hooks.StateInstalled {
			t.Errorf("claude-code state after %s", got)
		}
		if got := r.Hooks.State("codex"); got != hooks.StateNotInstalled {
			t.Errorf("codex was installed: %s", got)
		}
		if !strings.Contains(r.out.String(), "==> updating lidwake entries in agent configs that point at an older binary\n[Claude Code] ") {
			t.Errorf("stdout %q", r.out)
		}
		data, _ := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
		if strings.Contains(string(data), "lidwake.app") || !strings.Contains(string(data), r.Layout.InstalledBin) {
			t.Errorf("settings %s", data)
		}
	})

	t.Run("nothing to refresh says nothing", func(t *testing.T) {
		r := newRig(t, 501)
		if err := r.Setup(); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(r.out.String(), "updating lidwake entries") {
			t.Errorf("stdout %q", r.out)
		}
	})
}

func TestSetupRoot(t *testing.T) {
	t.Run("installs the signed binary, the link and the helper", func(t *testing.T) {
		r := newRig(t, 0)
		var staged string
		r.onCommand = func(c Command) {
			if c.Path != "/usr/bin/codesign" {
				return
			}
			// The binary is signed before it reaches the installed path.
			staged = c.Args[len(c.Args)-1]
			if filepath.Dir(staged) != r.Layout.InstallDir || staged == r.Layout.InstalledBin {
				t.Errorf("signed %s, not a staged file in the install dir", staged)
			}
			info, err := os.Stat(staged)
			if err != nil || info.Mode().Perm() != 0o755 || !r.chowned[staged] {
				t.Errorf("staged binary not ready: %v %v chowned=%v", info, err, r.chowned[staged])
			}
			if _, err := os.Stat(r.Layout.InstalledBin); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("installed before signing: %v", err)
			}
		}
		if err := r.SetupRoot(501); err != nil {
			t.Fatal(err)
		}
		expectCommands(t, r.commands, []Command{
			{Path: "/usr/bin/codesign", Args: []string{"--force", "--sign", "-", "--options", "runtime",
				"--identifier", "io.github.nikitashakhbazyan.lidwake", staged}},
			{Path: "/bin/launchctl", Args: []string{"bootout", "system/" + paths.HelperLabel}, MayFail: true},
			{Path: "/bin/launchctl", Args: []string{"bootstrap", "system", r.Layout.HelperPlist}},
		})

		l := r.Layout
		data, err := os.ReadFile(l.InstalledBin)
		if err != nil || string(data) != "#!lidwake binary v2" {
			t.Fatalf("installed binary %q, %v", data, err)
		}
		if info, _ := os.Stat(l.InstalledBin); info.Mode().Perm() != 0o755 {
			t.Errorf("binary mode %v", info.Mode())
		}
		for _, dir := range []string{l.InstallDir, filepath.Dir(l.InstallDir), l.HelperDataDir, filepath.Dir(l.BinSymlink)} {
			info, err := os.Stat(dir)
			if err != nil || !info.IsDir() || info.Mode().Perm() != 0o755 {
				t.Errorf("%s: %v %v", dir, info, err)
			}
			if !r.chowned[dir] {
				t.Errorf("%s not chowned to root:wheel", dir)
			}
		}
		if target, err := os.Readlink(l.BinSymlink); err != nil || target != l.InstalledBin {
			t.Errorf("symlink → %q, %v", target, err)
		}
		plist, err := os.ReadFile(l.HelperPlist)
		if err != nil || !bytes.Equal(plist, HelperPlist(l)) {
			t.Fatalf("helper plist %s, %v", plist, err)
		}
		if info, _ := os.Stat(l.HelperPlist); info.Mode().Perm() != 0o644 {
			t.Errorf("helper plist mode %v", info.Mode())
		}
		chownedPlist := false
		for p := range r.chowned {
			if filepath.Dir(p) == filepath.Dir(l.HelperPlist) {
				chownedPlist = true
			}
		}
		if !chownedPlist {
			t.Error("helper plist not chowned to root:wheel")
		}
		entries, _ := os.ReadDir(l.InstallDir)
		if len(entries) != 1 || entries[0].Name() != "lidwake" {
			t.Errorf("install dir holds %v", entries)
		}
		expectOutputHas(t, r.err, "")
	})

	t.Run("refuses to run without root", func(t *testing.T) {
		r := newRig(t, 501)
		if err := r.SetupRoot(501); !errors.Is(err, ErrNeedRoot) {
			t.Fatalf("err %v", err)
		}
		if len(r.commands) != 0 {
			t.Fatalf("ran %v", r.commands)
		}
	})

	t.Run("running it again upgrades in place", func(t *testing.T) {
		r := newRig(t, 0)
		if err := r.SetupRoot(501); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(r.exe, []byte("#!lidwake binary v3"), 0o755); err != nil {
			t.Fatal(err)
		}
		// A stale link from an older install is repointed.
		if err := os.Remove(r.Layout.BinSymlink); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("/Applications/lidwake.app/Contents/MacOS/lidwake", r.Layout.BinSymlink); err != nil {
			t.Fatal(err)
		}
		if err := r.SetupRoot(501); err != nil {
			t.Fatal(err)
		}
		if data, _ := os.ReadFile(r.Layout.InstalledBin); string(data) != "#!lidwake binary v3" {
			t.Fatalf("binary %q", data)
		}
		if target, _ := os.Readlink(r.Layout.BinSymlink); target != r.Layout.InstalledBin {
			t.Fatalf("symlink → %q", target)
		}
		entries, _ := os.ReadDir(r.Layout.InstallDir)
		if len(entries) != 1 {
			t.Errorf("install dir holds %v", entries)
		}
		linkDir, _ := os.ReadDir(filepath.Dir(r.Layout.BinSymlink))
		if len(linkDir) != 1 {
			t.Errorf("bin dir holds %v", linkDir)
		}
	})

	t.Run("an upgrade removes the separate daemon and helper binaries of older builds", func(t *testing.T) {
		r := newRig(t, 0)
		if err := os.MkdirAll(r.Layout.InstallDir, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"lidwake", "lidwake-daemon", "lidwake-helper"} {
			if err := os.WriteFile(filepath.Join(r.Layout.InstallDir, name), []byte("old"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := r.SetupRoot(501); err != nil {
			t.Fatal(err)
		}
		entries, _ := os.ReadDir(r.Layout.InstallDir)
		if len(entries) != 1 || entries[0].Name() != "lidwake" {
			t.Fatalf("install dir holds %v", entries)
		}
	})

	t.Run("installing from the installed binary works", func(t *testing.T) {
		r := newRig(t, 0)
		if err := r.SetupRoot(501); err != nil {
			t.Fatal(err)
		}
		r.Executable = func() (string, error) { return r.Layout.InstalledBin, nil }
		if err := r.SetupRoot(501); err != nil {
			t.Fatal(err)
		}
		if data, _ := os.ReadFile(r.Layout.InstalledBin); string(data) != "#!lidwake binary v2" {
			t.Fatalf("binary %q", data)
		}
	})

	t.Run("an existing bin directory keeps its owner", func(t *testing.T) {
		r := newRig(t, 0)
		binDir := filepath.Dir(r.Layout.BinSymlink)
		if err := os.MkdirAll(binDir, 0o775); err != nil {
			t.Fatal(err)
		}
		if err := r.SetupRoot(501); err != nil {
			t.Fatal(err)
		}
		if r.chowned[binDir] {
			t.Error("chowned a bin directory that already existed")
		}
	})

	// Go-specific hardening: root must not chmod or chown through a symlink someone planted where
	// an install directory belongs.
	t.Run("a symlink in place of an install directory is refused, not followed", func(t *testing.T) {
		r := newRig(t, 0)
		libexec := filepath.Dir(r.Layout.InstallDir)
		elsewhere := filepath.Join(t.TempDir(), "elsewhere")
		if err := os.MkdirAll(elsewhere, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(libexec), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(elsewhere, libexec); err != nil {
			t.Fatal(err)
		}
		err := r.SetupRoot(501)
		if err == nil || !strings.Contains(err.Error(), libexec+" is a symlink") {
			t.Fatalf("err %v", err)
		}
		if info, err := os.Stat(elsewhere); err != nil || info.Mode().Perm() != 0o700 {
			t.Errorf("symlink target changed: %v, %v", info.Mode(), err)
		}
		if len(r.chowned) != 0 || len(r.commands) != 0 {
			t.Errorf("chowned %v, ran %v", r.chowned, r.commands)
		}
		if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
			t.Errorf("installed through the symlink: %v", entries)
		}
	})

	// A release binary arrives signed for the helper; signing again would need codesign_allocate,
	// which only the Xcode Command Line Tools provide.
	t.Run("a binary already signed for the helper is installed without codesign", func(t *testing.T) {
		r := newRig(t, 0)
		var checked string
		r.HasInstallSignature = func(path string) bool {
			checked = path
			if filepath.Dir(path) != r.Layout.InstallDir || path == r.Layout.InstalledBin {
				t.Errorf("checked %s, not the staged copy in the install dir", path)
			}
			if data, err := os.ReadFile(path); err != nil || string(data) != "#!lidwake binary v2" || !r.chowned[path] {
				t.Errorf("staged copy not ready: %q %v chowned=%v", data, err, r.chowned[path])
			}
			return true
		}
		if err := r.SetupRoot(501); err != nil {
			t.Fatal(err)
		}
		if checked == "" {
			t.Fatal("signature never checked")
		}
		expectCommands(t, r.commands, []Command{
			{Path: "/bin/launchctl", Args: []string{"bootout", "system/" + paths.HelperLabel}, MayFail: true},
			{Path: "/bin/launchctl", Args: []string{"bootstrap", "system", r.Layout.HelperPlist}},
		})
		if data, err := os.ReadFile(r.Layout.InstalledBin); err != nil || string(data) != "#!lidwake binary v2" {
			t.Fatalf("installed binary %q, %v", data, err)
		}
		if !strings.Contains(r.out.String(), "==> keeping its signature (hardened runtime)\n") {
			t.Errorf("stdout %q", r.out)
		}
	})

	t.Run("a failed signature leaves no binary behind", func(t *testing.T) {
		r := newRig(t, 0)
		r.failOn = func(c Command) error {
			if c.Path == "/usr/bin/codesign" {
				return errors.New("codesign failed")
			}
			return nil
		}
		if err := r.SetupRoot(501); err == nil || !strings.Contains(err.Error(), "sign the binary") {
			t.Fatalf("err %v", err)
		}
		entries, _ := os.ReadDir(r.Layout.InstallDir)
		if len(entries) != 0 {
			t.Errorf("install dir holds %v", entries)
		}
		if len(r.commands) != 1 {
			t.Errorf("went on after the signature: %v", r.commands)
		}
	})

	t.Run("warns loudly about every path component root alone can't control", func(t *testing.T) {
		r := newRig(t, 0)
		usrLocal := filepath.Dir(filepath.Dir(r.Layout.InstallDir))
		var checked []string
		r.IsRootOnly = func(p string) bool {
			checked = append(checked, p)
			return p != usrLocal && p != r.Layout.InstallDir
		}
		if err := r.SetupRoot(501); err != nil {
			t.Fatal(err)
		}
		// Every component from the binary up to /.
		var want []string
		for p := r.Layout.InstalledBin; ; p = filepath.Dir(p) {
			want = append(want, p)
			if p == "/" {
				break
			}
		}
		if !slices.Equal(checked, want) {
			t.Errorf("checked %q\nwant %q", checked, want)
		}
		for _, p := range []string{r.Layout.InstallDir, usrLocal} {
			if !strings.Contains(r.err.String(), "WARNING: "+p+" is owned by uid ") ||
				!strings.Contains(r.err.String(), "sudo chown root:wheel "+p+"; sudo chmod go-w "+p+")") {
				t.Errorf("no warning naming %s: %q", p, r.err)
			}
		}
		if strings.Count(r.err.String(), "WARNING: ") != 2 {
			t.Errorf("stderr %q", r.err)
		}
		if !strings.Contains(r.err.String(), "the helper will not trust the daemon") {
			t.Errorf("stderr %q", r.err)
		}
	})
}

const (
	releaseDisplay = "Executable=/tmp/lidwake\nIdentifier=" + CodesignIdentifier + "\n" +
		"Format=Mach-O universal (x86_64 arm64)\n" +
		"CodeDirectory v=20500 size=61702 flags=0x10002(adhoc,runtime) hashes=1922+2 location=embedded\n" +
		"Signature=adhoc\nTeamIdentifier=not set\n"
	linkerSignedDisplay = "Executable=/tmp/lidwake\nIdentifier=a.out\nFormat=Mach-O thin (arm64)\n" +
		"CodeDirectory v=20400 size=61598 flags=0x20002(adhoc,linker-signed) hashes=1922+0 location=embedded\n" +
		"Signature=adhoc\nInfo.plist=not bound\nTeamIdentifier=not set\n"
	teamDisplay = "Executable=/tmp/Chrome\nIdentifier=com.google.Chrome\n" +
		"Format=app bundle with Mach-O universal (x86_64 arm64)\n" +
		"CodeDirectory v=20500 size=765 flags=0x12a00(kill,restrict,library-validation,runtime) hashes=13+7 location=embedded\n" +
		"Signature size=8989\nTeamIdentifier=EQHXZ8M8AV\n"
)

func TestParseCodesignDisplay(t *testing.T) {
	for _, tc := range []struct {
		name, out  string
		identifier string
		flags      uint64
	}{
		{"release build", releaseDisplay, CodesignIdentifier, 0x10002},
		{"go build", linkerSignedDisplay, "a.out", 0x20002},
		{"team-signed", teamDisplay, "com.google.Chrome", 0x12a00},
		{"unsigned", "/tmp/lidwake: code object is not signed at all\n", "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			identifier, flags := parseCodesignDisplay(tc.out)
			if identifier != tc.identifier || flags != tc.flags {
				t.Fatalf("got %q %#x, want %q %#x", identifier, flags, tc.identifier, tc.flags)
			}
		})
	}
}

// fakeCodesign writes a stand-in for /usr/bin/codesign that answers --verify with verifyExit and
// --display with display on stderr and displayExit, and fails on anything else, including a
// file other than path.
func fakeCodesign(t *testing.T, path string, verifyExit int, display string, displayExit int) string {
	t.Helper()
	tool := filepath.Join(t.TempDir(), "codesign")
	script := fmt.Sprintf(`#!/bin/sh
[ "$3" = '%s' ] || exit 65
case "$1 $2" in
"--verify --strict") exit %d ;;
"--display --verbose=2") printf '%%s' '%s' >&2; exit %d ;;
esac
exit 64
`, path, verifyExit, display, displayExit)
	if err := os.WriteFile(tool, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return tool
}

func TestHasInstallSignature(t *testing.T) {
	const binary = "/usr/local/libexec/lidwake/.lidwake-123"
	for _, tc := range []struct {
		name        string
		verifyExit  int
		display     string
		displayExit int
		want        bool
	}{
		{"signed for the helper", 0, releaseDisplay, 0, true},
		{"invalid signature", 1, releaseDisplay, 0, false},
		{"no hardened runtime", 0, linkerSignedDisplay, 0, false},
		{"another identifier", 0, teamDisplay, 0, false},
		{"unreadable signature", 0, releaseDisplay, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tool := fakeCodesign(t, binary, tc.verifyExit, tc.display, tc.displayExit)
			if got := hasInstallSignature(tool, binary); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
	if hasInstallSignature(fakeCodesign(t, "/elsewhere", 0, releaseDisplay, 0), binary) {
		t.Fatal("trusted the signature of another file")
	}
}

func expectOutputHas(t *testing.T, buf *bytes.Buffer, want string) {
	t.Helper()
	if buf.String() != want {
		t.Errorf("got %q, want %q", buf.String(), want)
	}
}

// ---- uninstall -------------------------------------------------------------------------------

func TestUninstall(t *testing.T) {
	t.Run("user half: hooks, daemon, then the root half", func(t *testing.T) {
		r := newRig(t, 501)
		home := r.Hooks.Home
		if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
			t.Fatal(err)
		}
		for _, install := range []func(string, bool) (hooks.Result, error){r.Hooks.Install, r.Hooks.InstallMCP, r.Hooks.InstallBackgroundHold} {
			if _, err := install("claude-code", false); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.MkdirAll(filepath.Dir(r.Layout.DaemonPlist), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(r.Layout.DaemonPlist, DaemonPlist(r.Layout), 0o644); err != nil {
			t.Fatal(err)
		}

		if err := r.Uninstall(); err != nil {
			t.Fatal(err)
		}
		expectCommands(t, r.commands, []Command{
			{Path: "/usr/bin/sudo", Args: []string{"-v"}, Interactive: true},
			{Path: "/bin/launchctl", Args: []string{"bootout", "gui/501/" + paths.DaemonLabel}, MayFail: true},
			{Path: "/usr/bin/sudo", Args: []string{r.exe, "uninstall", "--root"}, Interactive: true},
		})
		if _, err := os.Stat(r.Layout.DaemonPlist); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("daemon plist still there: %v", err)
		}
		for name, state := range map[string]hooks.InstallState{
			"hooks":           r.Hooks.State("claude-code"),
			"MCP":             r.Hooks.MCPState("claude-code"),
			"background hold": r.Hooks.BackgroundHoldState("claude-code"),
		} {
			if state != hooks.StateNotInstalled {
				t.Errorf("%s still %s", name, state)
			}
		}
		out := r.out.String()
		for _, want := range []string{
			"==> removing lidwake from agent configs\n[Claude Code] removed hook entries\n[Claude Code] MCP: ",
			"[Codex] nothing to remove\n",
			"[Pi] nothing to remove\n",
			"==> lidwake removed. Your settings stay in " + r.Layout.SupportDir + " and the logs in " + filepath.Dir(r.Layout.DaemonLog) + ".\n",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("stdout misses %q:\n%s", want, out)
			}
		}
		if strings.Contains(out, "[Codex] MCP") {
			t.Errorf("reported a no-op MCP removal:\n%s", out)
		}
	})

	t.Run("works when nothing is installed", func(t *testing.T) {
		r := newRig(t, 501)
		if err := r.Uninstall(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("refuses to run as root", func(t *testing.T) {
		r := newRig(t, 0)
		if err := r.Uninstall(); !errors.Is(err, ErrRunAsUser) {
			t.Fatalf("err %v", err)
		}
		if len(r.commands) != 0 {
			t.Fatalf("ran %v", r.commands)
		}
	})

	t.Run("a cancelled password prompt changes nothing", func(t *testing.T) {
		r := newRig(t, 501)
		if err := os.MkdirAll(filepath.Join(r.Hooks.Home, ".claude"), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Hooks.Install("claude-code", false); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(r.Layout.DaemonPlist), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(r.Layout.DaemonPlist, DaemonPlist(r.Layout), 0o644); err != nil {
			t.Fatal(err)
		}
		r.failOn = func(c Command) error {
			if c.Path == "/usr/bin/sudo" {
				return errors.New("exit status 1")
			}
			return nil
		}
		if err := r.Uninstall(); err == nil || !strings.Contains(err.Error(), "privileged uninstall") {
			t.Fatalf("err %v", err)
		}
		if r.Hooks.State("claude-code") == hooks.StateNotInstalled {
			t.Error("the hooks were removed although the password prompt failed")
		}
		if _, err := os.Stat(r.Layout.DaemonPlist); err != nil {
			t.Errorf("the daemon plist was removed: %v", err)
		}
		for _, c := range r.commands {
			if c.Path == "/bin/launchctl" {
				t.Errorf("ran %v after the failed prompt", c)
			}
		}
	})

	t.Run("a failed root half is reported", func(t *testing.T) {
		r := newRig(t, 501)
		r.failOn = func(c Command) error {
			if c.Path == "/usr/bin/sudo" {
				return errors.New("exit status 1")
			}
			return nil
		}
		if err := r.Uninstall(); err == nil || !strings.Contains(err.Error(), "privileged uninstall") {
			t.Fatalf("err %v", err)
		}
	})
}

// installed lays out a complete install in r's temporary root.
func installed(t *testing.T, r *rig) {
	t.Helper()
	euid := r.Geteuid
	r.Geteuid = func() int { return 0 }
	if err := r.SetupRoot(501); err != nil {
		t.Fatal(err)
	}
	r.Geteuid = euid
	for _, dir := range []string{r.Layout.HelperRunDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(r.Layout.HelperRunDir, "helper.sock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r.commands = nil
	r.out.Reset()
	r.err.Reset()
}

func TestUninstallRoot(t *testing.T) {
	gone := func(t *testing.T, paths ...string) {
		t.Helper()
		for _, p := range paths {
			if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("%s still there: %v", p, err)
			}
		}
	}

	t.Run("stops the helper first, then removes everything it installed", func(t *testing.T) {
		r := newRig(t, 0)
		installed(t, r)
		var plistAtBootout bool
		r.onCommand = func(c Command) {
			if slices.Contains(c.Args, "bootout") {
				_, err := os.Stat(r.Layout.HelperPlist)
				plistAtBootout = err == nil
			}
		}
		if err := r.UninstallRoot(); err != nil {
			t.Fatal(err)
		}
		expectCommands(t, r.commands, []Command{
			{Path: "/bin/launchctl", Args: []string{"bootout", "system/" + paths.HelperLabel}, MayFail: true},
		})
		if !plistAtBootout {
			t.Error("files removed before the helper was stopped")
		}
		l := r.Layout
		gone(t, l.HelperPlist, l.InstallDir, l.BinSymlink, l.HelperDataDir, l.HelperRunDir)
		if _, err := os.Stat(filepath.Dir(l.BinSymlink)); err != nil {
			t.Errorf("bin directory removed: %v", err)
		}
	})

	t.Run("is idempotent", func(t *testing.T) {
		r := newRig(t, 0)
		if err := r.UninstallRoot(); err != nil {
			t.Fatal(err)
		}
		if err := r.UninstallRoot(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("leaves a foreign command at the link path alone", func(t *testing.T) {
		r := newRig(t, 0)
		installed(t, r)
		if err := os.Remove(r.Layout.BinSymlink); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("/opt/homebrew/Cellar/lidwake/1.0/bin/lidwake", r.Layout.BinSymlink); err != nil {
			t.Fatal(err)
		}
		if err := r.UninstallRoot(); err != nil {
			t.Fatal(err)
		}
		if target, err := os.Readlink(r.Layout.BinSymlink); err != nil || target != "/opt/homebrew/Cellar/lidwake/1.0/bin/lidwake" {
			t.Fatalf("link %q, %v", target, err)
		}
		expectOutputHas(t, r.err, "left "+r.Layout.BinSymlink+" alone: it points at /opt/homebrew/Cellar/lidwake/1.0/bin/lidwake, not at lidwake\n")
	})

	t.Run("restores a sleep setting the helper left behind", func(t *testing.T) {
		for _, value := range []string{"0", "1\n"} {
			r := newRig(t, 0)
			installed(t, r)
			if err := os.WriteFile(r.Layout.OriginalSleepSetting, []byte(value), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := r.UninstallRoot(); err != nil {
				t.Fatal(err)
			}
			expectCommands(t, r.commands, []Command{
				{Path: "/bin/launchctl", Args: []string{"bootout", "system/" + paths.HelperLabel}, MayFail: true},
				{Path: "/usr/bin/pmset", Args: []string{"-a", "disablesleep", strings.TrimSpace(value)}},
			})
			gone(t, r.Layout.HelperDataDir)
		}
	})

	t.Run("keeps the saved sleep setting when it can't be restored", func(t *testing.T) {
		r := newRig(t, 0)
		installed(t, r)
		if err := os.WriteFile(r.Layout.OriginalSleepSetting, []byte("1"), 0o644); err != nil {
			t.Fatal(err)
		}
		r.failOn = func(c Command) error {
			if c.Path == "/usr/bin/pmset" {
				return errors.New("pmset failed")
			}
			return nil
		}
		if err := r.UninstallRoot(); err != nil {
			t.Fatal(err)
		}
		if data, err := os.ReadFile(r.Layout.OriginalSleepSetting); err != nil || string(data) != "1" {
			t.Fatalf("saved setting %q, %v", data, err)
		}
		if !strings.Contains(r.err.String(), "sudo pmset -a disablesleep 1") {
			t.Errorf("stderr %q", r.err)
		}
		gone(t, r.Layout.InstallDir, r.Layout.HelperPlist)

		r = newRig(t, 0)
		installed(t, r)
		if err := os.WriteFile(r.Layout.OriginalSleepSetting, []byte("garbage"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := r.UninstallRoot(); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(r.Layout.OriginalSleepSetting); err != nil {
			t.Fatalf("unreadable setting removed: %v", err)
		}
		if len(r.commands) != 1 {
			t.Errorf("commands %v", r.commands)
		}
	})

	t.Run("refuses to run without root", func(t *testing.T) {
		r := newRig(t, 501)
		installed(t, r)
		if err := r.UninstallRoot(); !errors.Is(err, ErrNeedRoot) {
			t.Fatalf("err %v", err)
		}
		if _, err := os.Stat(r.Layout.InstalledBin); err != nil {
			t.Fatalf("binary removed: %v", err)
		}
	})
}

func TestCommandString(t *testing.T) {
	c := Command{Path: "/bin/launchctl", Args: []string{"bootstrap", "system", "/x.plist"}}
	if got := c.String(); got != "/bin/launchctl bootstrap system /x.plist" {
		t.Fatalf("got %q", got)
	}
}

func TestRunCommand(t *testing.T) {
	// Only harmless programs: the production runner reports output and exit status.
	if err := RunCommand(Command{Path: "/usr/bin/true"}); err != nil {
		t.Fatal(err)
	}
	err := RunCommand(Command{Path: "/bin/sh", Args: []string{"-c", "echo nope >&2; exit 3"}})
	if err == nil || !strings.Contains(err.Error(), "nope") || !strings.Contains(err.Error(), "exit status 3") {
		t.Fatalf("err %v", err)
	}
	if err := RunCommand(Command{Path: "/bin/sh", Args: []string{"-c", "exit 1"}, MayFail: true}); err == nil {
		t.Fatal("MayFail still reports the failure to the caller")
	}
}

// Go-specific: the production Installer has every runner, probe and tool path filled in, and its
// layout and hooks point at the real install locations. Building it touches nothing.
func TestNewIsFullyWired(t *testing.T) {
	in := New(&bytes.Buffer{}, &bytes.Buffer{})
	v := reflect.ValueOf(in).Elem()
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		switch f.Kind() {
		case reflect.Func, reflect.Pointer, reflect.Interface:
			if f.IsNil() {
				t.Errorf("Installer.%s is nil", v.Type().Field(i).Name)
			}
		case reflect.String:
			if f.String() == "" {
				t.Errorf("Installer.%s is empty", v.Type().Field(i).Name)
			}
		}
	}
	if in.Layout != DefaultLayout() || in.Hooks.CLIPath != paths.InstalledBin {
		t.Errorf("layout %+v, hooks cli %q", in.Layout, in.Hooks.CLIPath)
	}
	for _, tool := range []string{in.Sudo, in.Launchctl, in.Codesign, in.Pmset} {
		if !filepath.IsAbs(tool) {
			t.Errorf("tool path %q is not absolute", tool)
		}
	}
}
