// Package install is `lidwake setup` and `lidwake uninstall`: putting the one binary where the
// root helper trusts it, registering the helper (a LaunchDaemon) and the per-user daemon (a
// LaunchAgent), and taking all of it away again.
//
// Both commands run as the user. The privileged half (`setup --root`, `uninstall --root`) is the
// same binary run through sudo, so the password is asked for once and nothing else runs as root.
//
//	/usr/local/libexec/lidwake/lidwake         root-owned binary; the helper trusts only a daemon
//	                                           running from here, so every directory up to / must
//	                                           stay writable by root alone
//	/usr/local/bin/lidwake                     symlink to it
//	/Library/LaunchDaemons/<label>.helper.plist  root helper (pmset disablesleep)
//	~/Library/LaunchAgents/<label>.daemon.plist  policy daemon for the installing user
package install

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/nikitaShakhbazyan/lidwake-go/internal/agents"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/hooks"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/paths"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/policy"
)

// Layout is every file location setup and uninstall touch. Production uses DefaultLayout; tests
// point it into a temporary directory.
type Layout struct {
	InstallDir   string
	InstalledBin string
	BinSymlink   string
	HelperPlist  string
	HelperLog    string
	// HelperDataDir holds OriginalSleepSetting, the disablesleep value from before the first block.
	HelperDataDir        string
	OriginalSleepSetting string
	HelperRunDir         string
	DaemonPlist          string
	DaemonLog            string
	// SupportDir holds the user's settings, which uninstall keeps.
	SupportDir string
}

// DefaultLayout is the real install locations.
func DefaultLayout() Layout {
	return Layout{
		InstallDir:           paths.InstallDir,
		InstalledBin:         paths.InstalledBin,
		BinSymlink:           paths.BinSymlink,
		HelperPlist:          paths.HelperPlist,
		HelperLog:            paths.HelperLog,
		HelperDataDir:        paths.HelperDataDir,
		OriginalSleepSetting: paths.OriginalSleepSetting,
		HelperRunDir:         paths.HelperRunDir,
		DaemonPlist:          paths.DaemonPlist(),
		DaemonLog:            paths.DaemonLog(),
		SupportDir:           paths.SupportDir(),
	}
}

// Command is one external program invocation.
type Command struct {
	Path string
	Args []string
	// Interactive runs the command on the terminal: sudo asks for the password there, and the
	// privileged half prints its own progress.
	Interactive bool
	// MayFail marks a command whose failure is expected and ignored, such as stopping a service
	// that isn't loaded. Its output is discarded.
	MayFail bool
}

func (c Command) String() string {
	return strings.Join(append([]string{c.Path}, c.Args...), " ")
}

// Runner runs one external command.
type Runner func(Command) error

// RunCommand is the production Runner.
func RunCommand(c Command) error {
	cmd := exec.Command(c.Path, c.Args...)
	switch {
	case c.Interactive:
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%s: %w", c, err)
		}
	case c.MayFail:
		return cmd.Run() // stdout and stderr go to /dev/null
	default:
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("%s: %w: %s", c, err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

// Installer performs setup and uninstall. New fills every field for production; tests replace
// the runner, the layout and the identity checks.
type Installer struct {
	Layout Layout
	Run    Runner
	// Out receives progress, Err warnings.
	Out, Err io.Writer
	// UID and Username identify the installing user (the user half of setup and uninstall).
	UID      int
	Username string
	Geteuid  func() int
	// Executable is this binary's path with symlinks resolved.
	Executable func() (string, error)
	// Chown changes the owner of a path without following a final symlink.
	Chown func(path string, uid, gid int) error
	// IsRootOnly reports whether a path is root-owned, not group/world-writable and no symlink:
	// the helper's test for every component of the daemon's path.
	IsRootOnly func(path string) bool
	// HasInstallSignature reports whether a file is already validly signed the way setup signs it:
	// under CodesignIdentifier, with the hardened runtime. A release build ships signed so and is
	// installed as it is: reading a signature needs only /usr/bin/codesign, but making one also
	// runs codesign_allocate, which comes with the Xcode Command Line Tools.
	HasInstallSignature func(path string) bool
	// Hooks edits the agents' configs; it embeds Layout.InstalledBin in hook commands.
	Hooks *hooks.Installer

	Sudo, Launchctl, Codesign, Pmset string
}

// New returns an Installer for the real system that reports to out and errOut.
func New(out, errOut io.Writer) *Installer {
	in := &Installer{
		Layout:     DefaultLayout(),
		Run:        RunCommand,
		Out:        out,
		Err:        errOut,
		UID:        os.Getuid(),
		Username:   currentUsername(),
		Geteuid:    os.Geteuid,
		Executable: ownExecutable,
		Chown:      os.Lchown,
		IsRootOnly: policy.IsWritableByRootOnly,
		Hooks:      hooks.New(paths.InstalledBin, paths.Home()),
		Sudo:       "/usr/bin/sudo",
		Launchctl:  "/bin/launchctl",
		Codesign:   "/usr/bin/codesign",
		Pmset:      "/usr/bin/pmset",
	}
	in.HasInstallSignature = func(path string) bool { return hasInstallSignature(in.Codesign, path) }
	return in
}

func ownExecutable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("find this executable: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", exe, err)
	}
	return resolved, nil
}

func currentUsername() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	if name := os.Getenv("USER"); name != "" {
		return name
	}
	return "this user"
}

// ErrRunAsUser refuses a user-half command run as root: the daemon is a per-user LaunchAgent
// and would be installed for root.
var ErrRunAsUser = errors.New("run as your own user, not root: the daemon is installed for whoever runs this")

// ErrNeedRoot refuses a privileged-half command run without root.
var ErrNeedRoot = errors.New("this part must run as root (lidwake runs it through sudo)")

// CodesignIdentifier is the signing identifier setup gives the binary. The helper accepts a
// team-signed peer only under this identifier (or a dotted child of it).
const CodesignIdentifier = paths.Label

func (in *Installer) say(format string, args ...any) {
	fmt.Fprintf(in.Out, "==> "+format+"\n", args...)
}

func (in *Installer) warn(format string, args ...any) {
	fmt.Fprintf(in.Err, format+"\n", args...)
}

func (in *Installer) daemonService() string {
	return "gui/" + strconv.Itoa(in.UID) + "/" + paths.DaemonLabel
}

// ---- setup -----------------------------------------------------------------------------------

// Setup installs or upgrades lidwake for the current user. It stops a running daemon first (on
// SIGTERM the daemon has the helper restore the sleep setting), runs the privileged half through
// sudo, then registers the daemon. Running it again is safe: every step replaces what is there.
//
// The password is asked for before anything stops, so a cancelled prompt or a missing terminal
// leaves a working install running; if the privileged half fails after that, the daemon of the
// earlier install is started again rather than left stopped until the next login.
func (in *Installer) Setup() error {
	if in.Geteuid() == 0 {
		return ErrRunAsUser
	}
	exe, err := in.Executable()
	if err != nil {
		return err
	}

	in.say("sudo asks for your password to install the binary and the root helper")
	if err := in.Run(Command{Path: in.Sudo, Args: []string{"-v"}, Interactive: true}); err != nil {
		return fmt.Errorf("privileged setup: %w", err)
	}

	in.say("stopping a running lidwake, if any")
	_ = in.Run(Command{Path: in.Launchctl, Args: []string{"bootout", in.daemonService()}, MayFail: true})

	in.say("installing the binary and the root helper")
	if err := in.Run(Command{
		Path:        in.Sudo,
		Args:        []string{exe, "setup", "--root", "--uid", strconv.Itoa(in.UID)},
		Interactive: true,
	}); err != nil {
		in.restartPreviousDaemon()
		return fmt.Errorf("privileged setup: %w", err)
	}

	in.say("registering the daemon for %s", in.Username)
	if err := os.MkdirAll(filepath.Dir(in.Layout.DaemonLog), 0o755); err != nil {
		return fmt.Errorf("create the log directory: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(in.Layout.DaemonPlist), 0o755); err != nil {
		return fmt.Errorf("create the LaunchAgents directory: %w", err)
	}
	if err := writeFileAtomic(in.Layout.DaemonPlist, DaemonPlist(in.Layout), 0o644, nil); err != nil {
		return fmt.Errorf("write the daemon plist: %w", err)
	}
	if err := in.bootstrapDaemon(); err != nil {
		return fmt.Errorf("start the daemon: %w", err)
	}

	in.refreshHooks()

	in.say("done — next: lidwake install-hooks (wire up your agents), then lidwake stats")
	return nil
}

func (in *Installer) bootstrapDaemon() error {
	return in.Run(Command{
		Path: in.Launchctl,
		Args: []string{"bootstrap", "gui/" + strconv.Itoa(in.UID), in.Layout.DaemonPlist},
	})
}

// restartPreviousDaemon starts the daemon Setup stopped, from the earlier install's plist, after
// the privileged half failed: until it runs, nothing blocks sleep. A fresh install has no plist
// and nothing to restart.
func (in *Installer) restartPreviousDaemon() {
	if _, err := os.Stat(in.Layout.DaemonPlist); err != nil {
		return
	}
	in.say("starting the daemon of the earlier install again")
	if err := in.bootstrapDaemon(); err != nil {
		in.warn("WARNING: could not start the daemon again (%v); it starts at your next login.", err)
	}
}

// refreshHooks repairs lidwake entries that already exist in agent configs but no longer match
// what this build writes — typically hooks that still call an older binary at another path. It
// never adds an integration the user hasn't installed.
func (in *Installer) refreshHooks() {
	if in.Hooks == nil {
		return
	}
	announced := false
	refresh := func(agent, what string, install func(string, bool) (hooks.Result, error)) {
		if !announced {
			in.say("updating lidwake entries in agent configs that point at an older binary")
			announced = true
		}
		name := agents.Kind(agent).DisplayName()
		result, err := install(agent, false)
		switch {
		case err == nil:
			fmt.Fprintf(in.Out, "[%s] %s%s\n", name, what, result.Summary)
		case hooks.IsSkip(err, hooks.SkipNotInstalled):
		default:
			in.warn("[%s] error: %v", name, err)
		}
	}
	for _, agent := range hooks.Agents() {
		if in.Hooks.State(agent) == hooks.StateModifiedExternally {
			refresh(agent, "", in.Hooks.Install)
		}
		if in.Hooks.MCPState(agent) == hooks.StateModifiedExternally {
			refresh(agent, "MCP: ", in.Hooks.InstallMCP)
		}
		if in.Hooks.BackgroundHoldState(agent) == hooks.StateModifiedExternally {
			refresh(agent, "background hold: ", in.Hooks.InstallBackgroundHold)
		}
	}
}

// SetupRoot is the privileged half of Setup, run as `sudo lidwake setup --root --uid <uid>`:
// install this binary, signed, where the helper trusts it, link it onto PATH and (re)start the
// helper.
func (in *Installer) SetupRoot(uid int) error {
	if in.Geteuid() != 0 {
		return ErrNeedRoot
	}
	exe, err := in.Executable()
	if err != nil {
		return err
	}
	l := in.Layout

	in.say("installing %s (for uid %d)", l.InstalledBin, uid)
	for _, dir := range []string{filepath.Dir(l.InstallDir), l.InstallDir} {
		if err := in.rootDir(dir); err != nil {
			return err
		}
	}
	if err := in.installBinary(exe); err != nil {
		return err
	}
	// Older builds shipped the daemon and the helper as separate binaries next to the CLI. One
	// binary plays every role now, so they go rather than linger in the trusted directory.
	for _, name := range []string{"lidwake-daemon", "lidwake-helper"} {
		if err := os.Remove(filepath.Join(l.InstallDir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove the old %s: %w", name, err)
		}
	}
	if err := in.linkBinary(); err != nil {
		return err
	}
	in.checkTrustChain()

	in.say("registering the root helper")
	if err := in.rootDir(l.HelperDataDir); err != nil {
		return err
	}
	if err := writeFileAtomic(l.HelperPlist, HelperPlist(l), 0o644, in.Chown); err != nil {
		return fmt.Errorf("write the helper plist: %w", err)
	}
	_ = in.Run(Command{Path: in.Launchctl, Args: []string{"bootout", "system/" + paths.HelperLabel}, MayFail: true})
	if err := in.Run(Command{Path: in.Launchctl, Args: []string{"bootstrap", "system", l.HelperPlist}}); err != nil {
		return fmt.Errorf("start the helper: %w", err)
	}
	return nil
}

// rootDir makes dir exist as root:wheel 0755, whatever it was before: the helper's trust in the
// daemon rests on these directories being writable by root alone. A symlink in dir's place is
// refused rather than followed: whoever planted it would choose what root chmods, and the helper
// never trusts a path through a symlink anyway.
func (in *Installer) rootDir(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	if info, err := os.Lstat(dir); err != nil {
		return fmt.Errorf("check %s: %w", dir, err)
	} else if !info.IsDir() {
		return fmt.Errorf("%s is a symlink or not a directory: lidwake installs only into real, root-owned directories", dir)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		return fmt.Errorf("chmod %s: %w", dir, err)
	}
	if err := in.Chown(dir, 0, 0); err != nil {
		return fmt.Errorf("chown %s: %w", dir, err)
	}
	return nil
}

// installBinary copies exe next to the installed binary, gives it its owner, mode and an ad-hoc
// signature with the hardened runtime, and only then renames it into place: the installed path
// never holds a partial or unsigned binary, and a fresh inode keeps the kernel's cached code
// signature of the old one out of the picture. The hardened runtime also keeps
// DYLD_INSERT_LIBRARIES out of the process the helper trusts. A copy already signed that way (a
// release build) keeps its signature; the copy is checked, not exe, because only root can change
// it before it is renamed into place.
func (in *Installer) installBinary(exe string) error {
	l := in.Layout
	src, err := os.Open(exe)
	if err != nil {
		return fmt.Errorf("open %s: %w", exe, err)
	}
	defer src.Close()
	tmp, err := os.CreateTemp(l.InstallDir, ".lidwake-*")
	if err != nil {
		return fmt.Errorf("stage the binary: %w", err)
	}
	staged := tmp.Name()
	done := false
	defer func() {
		if !done {
			os.Remove(staged)
		}
	}()
	if _, err := io.Copy(tmp, src); err != nil {
		tmp.Close()
		return fmt.Errorf("copy %s: %w", exe, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("copy %s: %w", exe, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("copy %s: %w", exe, err)
	}
	if err := os.Chmod(staged, 0o755); err != nil {
		return fmt.Errorf("chmod the binary: %w", err)
	}
	if err := in.Chown(staged, 0, 0); err != nil {
		return fmt.Errorf("chown the binary: %w", err)
	}
	if in.HasInstallSignature(staged) {
		in.say("keeping its signature (hardened runtime)")
	} else {
		in.say("signing (ad-hoc, hardened runtime)")
		if err := in.Run(Command{Path: in.Codesign, Args: []string{
			"--force", "--sign", "-", "--options", "runtime", "--identifier", CodesignIdentifier, staged,
		}}); err != nil {
			return fmt.Errorf("sign the binary: %w", err)
		}
	}
	if err := os.Rename(staged, l.InstalledBin); err != nil {
		return fmt.Errorf("install the binary: %w", err)
	}
	done = true
	return nil
}

// hardenedRuntimeFlag is kSecCodeSignatureRuntime among a CodeDirectory's flags.
const hardenedRuntimeFlag = 0x10000

// hasInstallSignature asks codesign, which only reads the file here, whether path is validly
// signed under CodesignIdentifier with the hardened runtime.
func hasInstallSignature(codesign, path string) bool {
	if exec.Command(codesign, "--verify", "--strict", path).Run() != nil {
		return false
	}
	out, err := exec.Command(codesign, "--display", "--verbose=2", path).CombinedOutput()
	if err != nil {
		return false
	}
	identifier, flags := parseCodesignDisplay(string(out))
	return identifier == CodesignIdentifier && flags&hardenedRuntimeFlag != 0
}

// parseCodesignDisplay reads the signing identifier and the CodeDirectory flags from the output of
// `codesign --display --verbose=2`: "Identifier=…" and
// "CodeDirectory v=20500 size=… flags=0x10002(adhoc,runtime) hashes=…".
func parseCodesignDisplay(out string) (identifier string, flags uint64) {
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(line, "Identifier="); ok {
			identifier = strings.TrimSpace(v)
			continue
		}
		if !strings.HasPrefix(line, "CodeDirectory ") {
			continue
		}
		for _, field := range strings.Fields(line) {
			if v, ok := strings.CutPrefix(field, "flags=0x"); ok {
				hex, _, _ := strings.Cut(v, "(")
				flags, _ = strconv.ParseUint(hex, 16, 64)
			}
		}
	}
	return identifier, flags
}

// linkBinary points BinSymlink at the installed binary, creating its directory only when it is
// missing: /usr/local/bin may belong to the user (Homebrew), and its owner is not ours to change.
func (in *Installer) linkBinary() error {
	l := in.Layout
	if target, err := os.Readlink(l.BinSymlink); err == nil && target == l.InstalledBin {
		return nil
	}
	dir := filepath.Dir(l.BinSymlink)
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		if err := in.rootDir(dir); err != nil {
			return err
		}
	}
	tmp := filepath.Join(dir, fmt.Sprintf(".lidwake-link-%d", os.Getpid()))
	_ = os.Remove(tmp)
	if err := os.Symlink(l.InstalledBin, tmp); err != nil {
		return fmt.Errorf("link %s: %w", l.BinSymlink, err)
	}
	if err := os.Rename(tmp, l.BinSymlink); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("link %s: %w", l.BinSymlink, err)
	}
	return nil
}

// checkTrustChain warns loudly about every component of the installed binary's path that anyone
// but root could change: the helper refuses a daemon running from such a path, so lid-close
// sleep could never be blocked.
func (in *Installer) checkTrustChain() {
	for p := in.Layout.InstalledBin; ; p = filepath.Dir(p) {
		if !in.IsRootOnly(p) {
			in.warn("WARNING: %s %s — the helper will not trust the daemon.", p, describeOwner(p))
			in.warn("         Make it root-owned and not group/world-writable (e.g. sudo chown root:wheel %s; sudo chmod go-w %s).", p, p)
		}
		if parent := filepath.Dir(p); parent == p {
			return
		}
	}
}

func describeOwner(p string) string {
	info, err := os.Lstat(p)
	switch {
	case err != nil:
		return fmt.Sprintf("can't be checked (%v)", err)
	case info.Mode()&fs.ModeSymlink != 0:
		return "is a symlink"
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return fmt.Sprintf("is owned by uid %d with mode %o", st.Uid, info.Mode().Perm())
	}
	return fmt.Sprintf("has mode %o", info.Mode().Perm())
}

// ---- uninstall -------------------------------------------------------------------------------

// Uninstall removes lidwake for the current user: the agent integrations, the daemon, then
// through sudo the helper and the binary. The user's settings and logs stay.
func (in *Installer) Uninstall() error {
	if in.Geteuid() == 0 {
		return ErrRunAsUser
	}
	exe, err := in.Executable()
	if err != nil {
		return err
	}

	// Ask for the password before touching anything: a cancelled prompt then changes nothing,
	// instead of leaving the hooks and the daemon gone with the helper still installed.
	in.say("sudo asks for your password to remove the binary and the root helper")
	if err := in.Run(Command{Path: in.Sudo, Args: []string{"-v"}, Interactive: true}); err != nil {
		return fmt.Errorf("privileged uninstall: %w", err)
	}

	in.say("removing lidwake from agent configs")
	in.removeHooks()

	in.say("stopping the daemon")
	_ = in.Run(Command{Path: in.Launchctl, Args: []string{"bootout", in.daemonService()}, MayFail: true})
	if err := os.Remove(in.Layout.DaemonPlist); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove the daemon plist: %w", err)
	}

	in.say("removing the root helper and the binary")
	if err := in.Run(Command{Path: in.Sudo, Args: []string{exe, "uninstall", "--root"}, Interactive: true}); err != nil {
		return fmt.Errorf("privileged uninstall: %w", err)
	}

	in.say("lidwake removed. Your settings stay in %s and the logs in %s.",
		in.Layout.SupportDir, filepath.Dir(in.Layout.DaemonLog))
	return nil
}

// removeHooks takes lidwake out of every agent's config: the hooks, and the MCP server and
// background-shell hook where present, all of which call the binary about to be removed.
func (in *Installer) removeHooks() {
	if in.Hooks == nil {
		return
	}
	for _, agent := range hooks.Agents() {
		name := agents.Kind(agent).DisplayName()
		if result, err := in.Hooks.Uninstall(agent, false); err != nil {
			in.warn("[%s] error: %v", name, err)
		} else {
			fmt.Fprintf(in.Out, "[%s] %s\n", name, result.Summary)
		}
		extras := []struct {
			what      string
			uninstall func(string, bool) (hooks.Result, error)
		}{
			{"MCP", in.Hooks.UninstallMCP},
			{"background hold", in.Hooks.UninstallBackgroundHold},
		}
		for _, extra := range extras {
			result, err := extra.uninstall(agent, false)
			switch {
			case err != nil:
				in.warn("[%s] %s error: %v", name, extra.what, err)
			case result.Diff != "(unchanged)":
				fmt.Fprintf(in.Out, "[%s] %s: %s\n", name, extra.what, result.Summary)
			}
		}
	}
}

// UninstallRoot is the privileged half of Uninstall, run as `sudo lidwake uninstall --root`.
// Stopping the helper comes first: on SIGTERM it restores the sleep setting from before lidwake.
func (in *Installer) UninstallRoot() error {
	if in.Geteuid() != 0 {
		return ErrNeedRoot
	}
	l := in.Layout
	in.say("stopping the root helper")
	_ = in.Run(Command{Path: in.Launchctl, Args: []string{"bootout", "system/" + paths.HelperLabel}, MayFail: true})
	keepData := in.restoreLeftoverSleepSetting()

	in.say("removing %s", l.InstallDir)
	if err := os.Remove(l.HelperPlist); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove the helper plist: %w", err)
	}
	if err := os.RemoveAll(l.InstallDir); err != nil {
		return fmt.Errorf("remove %s: %w", l.InstallDir, err)
	}
	if target, err := os.Readlink(l.BinSymlink); err == nil {
		if target == l.InstalledBin {
			if err := os.Remove(l.BinSymlink); err != nil {
				return fmt.Errorf("remove %s: %w", l.BinSymlink, err)
			}
		} else {
			in.warn("left %s alone: it points at %s, not at lidwake", l.BinSymlink, target)
		}
	}
	if !keepData {
		if err := os.RemoveAll(l.HelperDataDir); err != nil {
			return fmt.Errorf("remove %s: %w", l.HelperDataDir, err)
		}
	}
	if err := os.RemoveAll(l.HelperRunDir); err != nil {
		return fmt.Errorf("remove %s: %w", l.HelperRunDir, err)
	}
	return nil
}

// restoreLeftoverSleepSetting is the safety net for a helper that wasn't running to restore the
// sleep setting itself: a saved pre-lidwake value still on disk is applied here, before its
// directory goes. It reports whether the data directory must be kept because the value could
// not be applied — deleting it would lose the only record of the user's setting.
func (in *Installer) restoreLeftoverSleepSetting() (keep bool) {
	path := in.Layout.OriginalSleepSetting
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false
	}
	value := strings.TrimSpace(string(data))
	if err != nil || (value != "0" && value != "1") {
		in.warn("WARNING: %s holds no readable sleep setting; left it in place. Check `pmset -g` for SleepDisabled.", path)
		return true
	}
	if err := in.Run(Command{Path: in.Pmset, Args: []string{"-a", "disablesleep", value}}); err != nil {
		in.warn("WARNING: could not restore the sleep setting (disablesleep %s): %v. Left %s in place; run `sudo pmset -a disablesleep %s` yourself.", value, err, path, value)
		return true
	}
	in.say("restored the sleep setting from before lidwake (disablesleep %s)", value)
	return false
}

// ---- launchd job definitions -----------------------------------------------------------------

// HelperPlist is the root helper's LaunchDaemon definition.
func HelperPlist(l Layout) []byte {
	return launchdPlist(paths.HelperLabel, []string{l.InstalledBin, "helper"}, l.HelperLog)
}

// DaemonPlist is the per-user daemon's LaunchAgent definition.
func DaemonPlist(l Layout) []byte {
	return launchdPlist(paths.DaemonLabel, []string{l.InstalledBin, "daemon"}, l.DaemonLog)
}

func launchdPlist(label string, program []string, logPath string) []byte {
	var b bytes.Buffer
	str := func(indent, s string) {
		b.WriteString(indent + "<string>")
		_ = xml.EscapeText(&b, []byte(s))
		b.WriteString("</string>\n")
	}
	key := func(k string) { b.WriteString("\t<key>" + k + "</key>\n") }

	b.WriteString(xml.Header)
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString("<plist version=\"1.0\">\n<dict>\n")
	key("Label")
	str("\t", label)
	key("ProgramArguments")
	b.WriteString("\t<array>\n")
	for _, arg := range program {
		str("\t\t", arg)
	}
	b.WriteString("\t</array>\n")
	key("RunAtLoad")
	b.WriteString("\t<true/>\n")
	key("KeepAlive")
	b.WriteString("\t<true/>\n")
	key("ProcessType")
	str("\t", "Background")
	key("StandardOutPath")
	str("\t", logPath)
	key("StandardErrorPath")
	str("\t", logPath)
	b.WriteString("</dict>\n</plist>\n")
	return b.Bytes()
}

// writeFileAtomic writes data to a temporary file next to path, gives it its mode (and, with
// chown, root:wheel), then renames it over path.
func writeFileAtomic(path string, data []byte, perm fs.FileMode, chown func(string, int, int) error) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, werr := tmp.Write(data)
	if werr == nil {
		werr = tmp.Sync()
	}
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(name, perm)
	}
	if werr == nil && chown != nil {
		werr = chown(name, 0, 0)
	}
	if werr == nil {
		werr = os.Rename(name, path)
	}
	if werr != nil {
		os.Remove(name)
		return werr
	}
	return nil
}
