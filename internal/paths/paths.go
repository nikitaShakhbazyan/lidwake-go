// Package paths holds the identifiers, install locations and per-user file paths every lidwake
// component agrees on.
package paths

import (
	"os"
	"path/filepath"
	"runtime/debug"
)

// devVersion is what a build reports when neither -X nor the Go toolchain supplied a version.
const devVersion = "0.1.0-dev"

// Version is the version every component reports. The Makefile and the release build set it with
// -ldflags "-X github.com/nikitaShakhbazyan/lidwake-go/internal/paths.Version=…"; a go install
// build has no ldflags, so init falls back to the module version the toolchain stamped.
var Version = devVersion

func init() { Version = resolveVersion(Version, debug.ReadBuildInfo) }

// resolveVersion keeps v unless it is still the dev default, in which case it takes the main
// module's version from the build info ("(devel)" or empty means there is none).
func resolveVersion(v string, readBuildInfo func() (*debug.BuildInfo, bool)) string {
	if v != devVersion {
		return v
	}
	if bi, ok := readBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return v
}

const (
	// Label prefixes the launchd labels.
	Label       = "io.github.nikitashakhbazyan.lidwake"
	HelperLabel = Label + ".helper"
	DaemonLabel = Label + ".daemon"

	// InstallDir holds the one binary that runs as CLI, daemon and helper. The helper trusts a
	// caller only when its executable is InstallDir/lidwake and every path component up to "/"
	// is writable by root alone.
	InstallDir    = "/usr/local/libexec/lidwake"
	InstalledBin  = InstallDir + "/lidwake"
	BinSymlink    = "/usr/local/bin/lidwake"
	HelperPlist   = "/Library/LaunchDaemons/" + HelperLabel + ".plist"
	HelperRunDir  = "/var/run/lidwake"
	HelperSocket  = HelperRunDir + "/helper.sock"
	HelperLog     = "/var/log/lidwake-helper.log"
	HelperDataDir = "/var/db/lidwake"
	// OriginalSleepSetting stores the disablesleep value from before the first block.
	OriginalSleepSetting = HelperDataDir + "/sleep-disabled-before"
)

// Home is the current user's home directory ($HOME, falling back to os.UserHomeDir).
func Home() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	h, _ := os.UserHomeDir()
	return h
}

// SupportDir is ~/Library/Application Support/lidwake, created on demand by the callers that
// write into it.
func SupportDir() string {
	return filepath.Join(Home(), "Library", "Application Support", "lidwake")
}

func ConfigFile() string { return filepath.Join(SupportDir(), "config.json") }
func StateFile() string  { return filepath.Join(SupportDir(), "state.json") }
func EventLog() string   { return filepath.Join(SupportDir(), "events.log") }
func CLISocket() string  { return filepath.Join(SupportDir(), "cli.sock") }

// DaemonPlist is the per-user LaunchAgent.
func DaemonPlist() string {
	return filepath.Join(Home(), "Library", "LaunchAgents", DaemonLabel+".plist")
}

// DaemonLog is where launchd sends the daemon's stdout/stderr.
func DaemonLog() string {
	return filepath.Join(Home(), "Library", "Logs", "lidwake", "daemon.log")
}
