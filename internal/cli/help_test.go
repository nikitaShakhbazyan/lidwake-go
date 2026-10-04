package cli

import (
	"os"
	"strings"
	"testing"
)

// Every command answers --help with its usage and does nothing else: no daemon request, no exec,
// no file written into the (test) home.
func TestHelpNeverActs(t *testing.T) {
	commands := []string{
		"stats", "on", "off", "timer", "run", "config", "setup", "uninstall", "hold", "release",
		"acquire", "status", "install-hooks", "uninstall-hooks", "daemon-status", "mcp",
	}
	for _, command := range commands {
		for _, flag := range []string{"--help", "-h"} {
			t.Run(command+" "+flag, func(t *testing.T) {
				h := newHarness(t)
				if code := h.do(t, command, flag); code != 0 {
					t.Fatalf("exit %d, stderr %q", code, h.errOut.String())
				}
				if !strings.HasPrefix(h.out.String(), "usage: lidwake "+command) && !strings.Contains(h.out.String(), "| "+command) {
					t.Fatalf("stdout %q", h.out.String())
				}
				if len(h.daemon.requests) != 0 || len(h.execs) != 0 || len(h.commands) != 0 || len(h.statsCalls) != 0 {
					t.Fatalf("acted: requests %v execs %v commands %v stats %v", h.daemon.requests, h.execs, h.commands, h.statsCalls)
				}
				entries, err := os.ReadDir(h.hookHome)
				if err != nil || len(entries) != 0 {
					t.Fatalf("wrote into the home: %v %v", entries, err)
				}
			})
		}
	}

	t.Run("run passes --help after the command to the command", func(t *testing.T) {
		h := newHarness(t)
		h.do(t, "run", "--", "true", "--help")
		if strings.HasPrefix(h.out.String(), "usage:") {
			t.Fatal("run treated the command's --help as its own")
		}
	})
}

func TestHookCommandsRefuseUnknownArguments(t *testing.T) {
	for _, command := range []string{"install-hooks", "uninstall-hooks"} {
		for _, args := range [][]string{{"--hepl"}, {"claude-code"}, {"--tool", "claude-code", "--force"}} {
			t.Run(command+" "+strings.Join(args, " "), func(t *testing.T) {
				h := newHarness(t)
				if code := h.do(t, append([]string{command}, args...)...); code != 2 {
					t.Fatalf("exit %d, want 2", code)
				}
				if !strings.Contains(h.errOut.String(), "unexpected") {
					t.Fatalf("stderr %q", h.errOut.String())
				}
				entries, err := os.ReadDir(h.hookHome)
				if err != nil || len(entries) != 0 {
					t.Fatalf("wrote into the home: %v %v", entries, err)
				}
			})
		}
	}
}
