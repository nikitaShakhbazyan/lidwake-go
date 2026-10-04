package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nikitaShakhbazyan/lidwake-go/internal/install"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/ipc"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/model"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/paths"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/settings"
)

func TestDispatch(t *testing.T) {
	t.Run("no arguments prints the short usage and exits 1", func(t *testing.T) {
		h := newHarness(t)
		expectCode(t, h.do(t), 1)
		expectOutput(t, "stdout", h.out, shortUsage)
	})

	t.Run("unknown command", func(t *testing.T) {
		h := newHarness(t)
		expectCode(t, h.do(t, "frobnicate"), 2)
		expectOutput(t, "stderr", h.errOut, "Unknown command: frobnicate\n")
		expectOutput(t, "stdout", h.out, shortUsage)
	})

	t.Run("version", func(t *testing.T) {
		for _, arg := range []string{"version", "--version", "-v"} {
			h := newHarness(t)
			expectCode(t, h.do(t, arg), 0)
			expectOutput(t, "stdout", h.out, "lidwake "+paths.Version+"\n")
		}
	})

	t.Run("help", func(t *testing.T) {
		for _, arg := range []string{"help", "--help", "-h"} {
			h := newHarness(t)
			expectCode(t, h.do(t, arg), 0)
			expectOutput(t, "stdout", h.out, fullUsage)
		}
	})

	// The everyday commands lead both usage texts, setup and uninstall among them.
	t.Run("usage lists the everyday commands first", func(t *testing.T) {
		first := []string{"stats", "on", "off", "timer", "run", "config", "setup", "uninstall"}
		line := strings.Split(shortUsage, "\n")[1]
		listed := strings.Split(strings.TrimPrefix(line, "commands: "), " | ")
		if !slices.Equal(listed[:len(first)], first) {
			t.Errorf("short usage order %q", listed)
		}
		for _, cmd := range []string{"hold", "release", "status", "acquire", "install-hooks", "uninstall-hooks", "daemon-status", "mcp", "version"} {
			if !slices.Contains(listed, cmd) {
				t.Errorf("short usage misses %s", cmd)
			}
		}
		var order []string
		for _, l := range strings.Split(fullUsage, "\n") {
			if rest, ok := strings.CutPrefix(l, "  lidwake "); ok {
				fields := strings.Fields(rest)
				order = append(order, fields[0])
				if len(fields) > 2 && fields[1] == "|" && fields[2] == "off" {
					order = append(order, "off")
				}
			}
		}
		if !slices.Equal(order[:len(first)], first) {
			t.Errorf("full usage order %q", order)
		}
	})

	t.Run("stats and its aliases pass --once and talk to the daemon", func(t *testing.T) {
		for _, args := range [][]string{{"stats"}, {"--stats", "--once"}, {"top", "--once"}} {
			h := newHarness(t)
			h.daemon.reply = replyWith(ipc.Response{OK: true, Status: &model.Status{Paused: true}}, nil)
			expectCode(t, h.do(t, args...), 0)
			wantOnce := len(args) == 2
			if !slices.Equal(h.statsCalls, []bool{wantOnce}) {
				t.Fatalf("%q: stats calls %v", args, h.statsCalls)
			}
			s, err := h.statsAction.Status()
			if err != nil || s == nil || !s.Paused {
				t.Fatalf("%q: status %+v, %v", args, s, err)
			}
			if req := h.daemon.only(t); req.Op != ipc.OpStatus {
				t.Fatalf("op %q", req.Op)
			}
		}
	})

	t.Run("mcp serves with the named tool", func(t *testing.T) {
		h := newHarness(t)
		h.mcpIn = strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"keep_awake","arguments":{"reason":"deploy"}}}` + "\n")
		h.daemon.reply = replyWith(ipc.Response{OK: true, HoldKey: "hold:ab12cd34", AppliedTTL: f64(3600)}, nil)
		expectCode(t, h.do(t, "mcp", "--tool", "claude-code"), 0)
		if req := h.daemon.only(t); req.Op != ipc.OpHold || req.Tool != "claude-code" || req.Reason != "deploy" {
			t.Fatalf("request %+v", req)
		}
		expectContains(t, "stdout", h.out, "hold:ab12cd34")
		expectContains(t, "stderr", h.errOut, "[lidwake mcp] lidwake mcp server ready (tool=claude-code)")

		h = newHarness(t)
		h.do(t, "mcp")
		expectContains(t, "stderr", h.errOut, "(tool=manual)")
	})
}

func TestHold(t *testing.T) {
	t.Run("prints the key on stdout and the summary on stderr", func(t *testing.T) {
		h := newHarness(t)
		h.daemon.reply = replyWith(ipc.Response{OK: true, HoldKey: "hold:ab12cd34", AppliedTTL: f64(1800)}, nil)
		expectCode(t, h.do(t, "hold", "--reason", "deploy", "--for", "30m", "--tool", "codex"), 0)
		req := h.daemon.only(t)
		if req.Op != ipc.OpHold || req.Reason != "deploy" || req.Tool != "codex" || req.ProcessName != "codex" ||
			req.TTL == nil || *req.TTL != 1800 || req.PID != 0 || req.Key != "" {
			t.Fatalf("request %+v", req)
		}
		expectOutput(t, "stdout", h.out, "hold:ab12cd34\n")
		expectOutput(t, "stderr", h.errOut, "Keeping your Mac awake for up to 30m · release with: lidwake release hold:ab12cd34\n")
	})

	t.Run("summary reports the applied ttl and the pid", func(t *testing.T) {
		h := newHarness(t)
		h.daemon.reply = replyWith(ipc.Response{OK: true, HoldKey: "hold:1", AppliedTTL: f64(4 * 3600)}, nil)
		h.do(t, "hold", "--for", "10h", "--pid", "321")
		if req := h.daemon.only(t); req.PID != 321 || *req.TTL != 36000 {
			t.Fatalf("request %+v", req)
		}
		expectOutput(t, "stderr", h.errOut, "Keeping your Mac awake until process 321 exits for up to 4h · release with: lidwake release hold:1\n")
	})

	t.Run("ttl seconds and the default", func(t *testing.T) {
		h := newHarness(t)
		h.daemon.reply = replyWith(ipc.Response{OK: true, HoldKey: "hold:1"}, nil)
		h.do(t, "hold", "--ttl", "90")
		if req := h.daemon.only(t); *req.TTL != 90 {
			t.Fatalf("ttl %v", *req.TTL)
		}
		expectOutput(t, "stderr", h.errOut, "Keeping your Mac awake for up to 1m30s · release with: lidwake release hold:1\n")

		h = newHarness(t)
		h.daemon.reply = replyWith(ipc.Response{OK: true, HoldKey: "hold:1"}, nil)
		h.do(t, "hold")
		if req := h.daemon.only(t); req.TTL != nil {
			t.Fatalf("ttl %v", *req.TTL)
		}
		expectOutput(t, "stderr", h.errOut, "Keeping your Mac awake for up to 1h · release with: lidwake release hold:1\n")
	})

	t.Run("usage errors exit 2", func(t *testing.T) {
		h := newHarness(t)
		expectCode(t, h.do(t, "hold", "--for", "soonish"), 2)
		expectOutput(t, "stderr", h.errOut, "hold: could not understand duration 'soonish' (try 30m, 2h, 1h30m)\n")
		h.daemon.none(t)
		for _, pid := range []string{"0", "-3", "abc"} {
			h := newHarness(t)
			expectCode(t, h.do(t, "hold", "--pid="+pid), 2)
			expectOutput(t, "stderr", h.errOut, "hold: --pid must be a positive process id\n")
		}
	})

	t.Run("failures exit 1", func(t *testing.T) {
		cases := []struct {
			resp   ipc.Response
			err    error
			stderr string
		}{
			{ipc.Response{}, ipc.ErrDaemonUnreachable, "hold failed: lidwake daemon is not running.\n"},
			{ipc.Response{}, errors.New("i/o timeout"), "hold failed: i/o timeout\n"},
			{ipc.Response{OK: false, Error: "agent holds are disabled"}, nil, "hold failed: agent holds are disabled\n"},
			{ipc.Response{OK: true}, nil, "hold failed: unknown error\n"},
		}
		for _, tc := range cases {
			h := newHarness(t)
			h.daemon.reply = replyWith(tc.resp, tc.err)
			expectCode(t, h.do(t, "hold"), 1)
			expectOutput(t, "stderr", h.errOut, tc.stderr)
			expectOutput(t, "stdout", h.out, "")
		}
	})

	t.Run("display skew warns but keeps the hold", func(t *testing.T) {
		h := newHarness(t)
		h.daemon.reply = replyWith(ipc.Response{OK: true, HoldKey: "hold:1"}, nil)
		expectCode(t, h.do(t, "hold", "--display"), 0)
		expectContains(t, "stderr", h.errOut, "hold: the running daemon predates --display — the system stays awake but the display is NOT held. Update lidwake.\n")
		expectOutput(t, "stdout", h.out, "hold:1\n")
	})

	t.Run("compact durations", func(t *testing.T) {
		for in, want := range map[float64]string{45: "45s", 300: "5m", 330: "5m30s", 3600: "1h", 5400: "1h30m", 7260: "2h1m", 59.6: "1m"} {
			if got := compactDuration(in); got != want {
				t.Errorf("%v: %q, want %q", in, got, want)
			}
		}
	})
}

func TestStatus(t *testing.T) {
	t.Run("daemon not running", func(t *testing.T) {
		h := newHarness(t)
		h.daemon.reply = replyWith(ipc.Response{}, ipc.ErrDaemonUnreachable)
		expectCode(t, h.do(t, "status"), 1)
		expectOutput(t, "stdout", h.out, "lidwake daemon is not running.\n")

		h = newHarness(t)
		h.daemon.reply = replyWith(ipc.Response{}, ipc.ErrDaemonUnreachable)
		expectCode(t, h.do(t, "status", "--json"), 1)
		expectOutput(t, "stdout", h.out, `{"daemonRunning":false}`+"\n")
	})

	t.Run("other transport errors", func(t *testing.T) {
		h := newHarness(t)
		h.daemon.reply = replyWith(ipc.Response{}, errors.New("i/o timeout"))
		expectCode(t, h.do(t, "status"), 1)
		expectOutput(t, "stderr", h.errOut, "Error: i/o timeout\n")
	})

	acquired := testNow.Add(-(2*time.Minute + 5*time.Second))
	status := &model.Status{
		Blocking: true,
		Assertions: []model.Assertion{
			{Key: "claude-code:abc", Tool: "claude-code", AcquiredAt: acquired, Origin: model.OriginHook},
			{Key: "hold:ab12cd34", Tool: "manual", Reason: "deploy\x1b[2J", AcquiredAt: testNow.Add(-90 * time.Minute), Origin: model.OriginManual},
		},
		LidClosed:       true,
		HelperConnected: true,
		CPUTemperature:  f64(61.25),
		ActiveCutouts:   []string{},
		Warnings:        []string{},
		Settings:        settings.Defaults(),
		Version:         "1.2.3",
	}

	t.Run("json prints the daemon status", func(t *testing.T) {
		h := newHarness(t)
		h.daemon.reply = replyWith(ipc.Response{OK: true, Status: status}, nil)
		expectCode(t, h.do(t, "status", "--json"), 0)
		var got model.Status
		if err := json.Unmarshal(h.out.Bytes(), &got); err != nil {
			t.Fatalf("%v: %s", err, h.out)
		}
		if len(got.Assertions) != 2 || got.Assertions[1].Key != "hold:ab12cd34" || got.Version != "1.2.3" || !got.Blocking {
			t.Fatalf("got %+v", got)
		}
		if !strings.HasSuffix(h.out.String(), "}\n") || strings.Count(h.out.String(), "\n") != 1 {
			t.Errorf("not one line: %q", h.out)
		}

		h = newHarness(t)
		expectCode(t, h.do(t, "status", "--json"), 0)
		expectOutput(t, "stdout", h.out, `{"daemonRunning":true,"statusUnavailable":true}`+"\n")
	})

	t.Run("text", func(t *testing.T) {
		h := newHarness(t)
		h.daemon.reply = replyWith(ipc.Response{OK: true, Status: status}, nil)
		expectCode(t, h.do(t, "status"), 0)
		expectOutput(t, "stdout", h.out, "lidwake — blocking sleep\n"+
			"  Assertions: 2\n"+
			"    • claude-code [claude-code:abc] — 2m 5s\n"+
			"    • manual [hold:ab12cd34] — 90m 0s — deploy [2J\n"+
			"  Lid: closed\n"+
			"  CPU temp: 61.2°C\n"+
			"  Helper: connected\n")

		h = newHarness(t)
		h.daemon.reply = replyWith(ipc.Response{OK: true, Status: &model.Status{}}, nil)
		h.do(t, "status")
		expectOutput(t, "stdout", h.out, "lidwake — idle\n  Assertions: 0\n  Lid: open\n  Helper: disconnected\n")

		h = newHarness(t)
		h.do(t, "status")
		expectOutput(t, "stdout", h.out, "Daemon responded but status could not be decoded.\n")
	})

	t.Run("text shows what happened while the lid was closed", func(t *testing.T) {
		closed := time.Date(2026, 10, 4, 9, 2, 0, 0, time.UTC)
		s := &model.Status{AwaySummary: &model.AwaySummary{
			ClosedAt: closed,
			OpenedAt: closed.Add(65 * time.Minute),
			Finished: []model.FinishedAgent{
				{Key: "claude-code:a", Tool: "claude-code", DisplayName: "Claude Code", Duration: 45 * time.Minute},
				{Key: "codex:b", Tool: "codex", Duration: 12 * time.Minute},
			},
			StillActive:     []model.FinishedAgent{{Key: "hold:1", Tool: "manual", DisplayName: "manual", Duration: time.Hour}},
			PeakTemperature: f64(72),
			ThermalCutout:   true,
		}}
		h := newHarness(t)
		h.daemon.reply = replyWith(ipc.Response{OK: true, Status: s}, nil)
		h.do(t, "status")
		expectContains(t, "stdout", h.out, "  While the lid was closed: 1h 05m · 09:02 → 10:07\n"+
			"    Finished: 2 agents — Claude Code 45m, codex 12m\n"+
			"    Still working: 1 agent — manual 1h 00m\n"+
			"    Peak CPU temp: 72.0°C\n"+
			"    Safety cutouts: overheating fired\n")

		s.AwaySummary = &model.AwaySummary{ClosedAt: closed, OpenedAt: closed.Add(time.Minute)}
		h = newHarness(t)
		h.daemon.reply = replyWith(ipc.Response{OK: true, Status: s}, nil)
		h.do(t, "status")
		expectContains(t, "stdout", h.out, "    Finished: none\n    Safety cutouts: none fired\n")
	})
}

func TestDaemonStatus(t *testing.T) {
	cases := []struct {
		name string
		resp ipc.Response
		err  error
		out  string
	}{
		{"running", ipc.Response{OK: true}, nil, "daemon: running\n"},
		{"not ok", ipc.Response{OK: false, Error: "busy"}, nil, "daemon: running (reported not-ok: busy)\n"},
		{"not ok silently", ipc.Response{OK: false}, nil, "daemon: running (reported not-ok: unknown)\n"},
		{"not running", ipc.Response{}, ipc.ErrDaemonUnreachable, "daemon: not running (lidwake setup installs and starts it)\n"},
		{"unreachable", ipc.Response{}, errors.New("i/o timeout"), "daemon: unreachable (i/o timeout)\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.daemon.reply = replyWith(tc.resp, tc.err)
			expectCode(t, h.do(t, "daemon-status"), 0)
			if req := h.daemon.only(t); req.Op != ipc.OpPing {
				t.Fatalf("op %q", req.Op)
			}
			expectOutput(t, "stdout", h.out, tc.out)
		})
	}
}

func TestControlCommands(t *testing.T) {
	t.Run("on and off", func(t *testing.T) {
		h := newHarness(t)
		expectCode(t, h.do(t, "on"), 0)
		if req := h.daemon.only(t); req.Op != ipc.OpResume {
			t.Fatalf("op %q", req.Op)
		}
		expectOutput(t, "stdout", h.out, "lidwake is on — working agents keep the Mac awake, lid closed or not.\n")

		for count, want := range map[int]string{
			0: "lidwake is off · the Mac sleeps normally.\n",
			1: "lidwake is off — released 1 hold · the Mac sleeps normally.\n",
			3: "lidwake is off — released 3 holds · the Mac sleeps normally.\n",
		} {
			h := newHarness(t)
			h.daemon.reply = replyWith(ipc.Response{OK: true, ReleasedCount: ipc.Ptr(count)}, nil)
			expectCode(t, h.do(t, "off"), 0)
			if req := h.daemon.only(t); req.Op != ipc.OpPause {
				t.Fatalf("op %q", req.Op)
			}
			expectOutput(t, "stdout", h.out, want)
		}
	})

	t.Run("failures exit 1", func(t *testing.T) {
		cases := []struct {
			resp   ipc.Response
			err    error
			stderr string
		}{
			{ipc.Response{}, ipc.ErrDaemonUnreachable, "lidwake: lidwake daemon is not running.\n"},
			{ipc.Response{OK: false, Error: "nope"}, nil, "lidwake: nope\n"},
			{ipc.Response{OK: false}, nil, "lidwake: the daemon refused the request\n"},
		}
		for _, tc := range cases {
			for _, args := range [][]string{{"on"}, {"off"}, {"timer", "1h"}} {
				h := newHarness(t)
				h.daemon.reply = replyWith(tc.resp, tc.err)
				expectCode(t, h.do(t, args...), 1)
				expectOutput(t, "stderr", h.errOut, tc.stderr)
				expectOutput(t, "stdout", h.out, "")
			}
		}
	})

	t.Run("timer arms with a duration", func(t *testing.T) {
		cases := []struct {
			arg     string
			seconds float64
			out     string
		}{
			{"1h", 3600, "lidwake turns off at 13:00 (in 1h 00m).\n"},
			{"30m", 1800, "lidwake turns off at 12:30 (in 30m).\n"},
			{"1h30m", 5400, "lidwake turns off at 13:30 (in 1h 30m).\n"},
		}
		for _, tc := range cases {
			h := newHarness(t)
			h.daemon.reply = func(req ipc.Request) (ipc.Response, error) {
				return ipc.Response{OK: true, AppliedTTL: req.TTL}, nil
			}
			expectCode(t, h.do(t, "timer", tc.arg), 0)
			req := h.daemon.only(t)
			if req.Op != ipc.OpTimer || req.TTL == nil || *req.TTL != tc.seconds {
				t.Fatalf("%s: request %+v", tc.arg, req)
			}
			expectOutput(t, "stdout", h.out, tc.out)
		}
	})

	t.Run("timer off cancels", func(t *testing.T) {
		for _, arg := range []string{"off", "cancel", "none"} {
			h := newHarness(t)
			expectCode(t, h.do(t, "timer", arg), 0)
			if req := h.daemon.only(t); req.Op != ipc.OpTimer || req.TTL != nil {
				t.Fatalf("%s: request %+v", arg, req)
			}
			expectOutput(t, "stdout", h.out, "Off timer cancelled.\n")
		}
	})

	t.Run("timer usage errors", func(t *testing.T) {
		for _, args := range [][]string{{"timer"}, {"timer", "1h", "2h"}} {
			h := newHarness(t)
			expectCode(t, h.do(t, args...), 1)
			expectOutput(t, "stderr", h.errOut, "lidwake: usage: lidwake timer <duration, e.g. 30m, 1h, 1h30m> | off\n")
			h.daemon.none(t)
		}
		for _, arg := range []string{"0", "soon", "OFF", "1h30"} {
			h := newHarness(t)
			expectCode(t, h.do(t, "timer", arg), 1)
			expectOutput(t, "stderr", h.errOut, "lidwake: could not understand duration '"+arg+"' (try 30m, 1h, 1h30m)\n")
			h.daemon.none(t)
		}
	})
}

func TestRun(t *testing.T) {
	t.Run("parsing", func(t *testing.T) {
		cases := []struct {
			name     string
			args     []string
			duration *float64
			reason   string
			display  bool
			command  []string
			errMsg   string
			help     bool
		}{
			{name: "command after --", args: []string{"--", "make", "test"}, command: []string{"make", "test"}},
			{name: "first non-option starts the command", args: []string{"make", "-j4", "--for", "1h"}, command: []string{"make", "-j4", "--for", "1h"}},
			{name: "all options", args: []string{"--for", "1h30m", "--reason", "nightly build", "--display", "--", "./build.sh"},
				duration: f64(5400), reason: "nightly build", display: true, command: []string{"./build.sh"}},
			{name: "dashes after -- belong to the command", args: []string{"--display", "--", "--weird", "-x"}, display: true, command: []string{"--weird", "-x"}},
			{name: "for without a value", args: []string{"--for"}, errMsg: "--for needs a duration such as 2h or 1h30m"},
			{name: "for garbage", args: []string{"--for", "later", "--", "x"}, errMsg: "--for needs a duration such as 2h or 1h30m"},
			{name: "for zero", args: []string{"--for", "0", "--", "x"}, errMsg: "--for needs a duration such as 2h or 1h30m"},
			{name: "reason without a value", args: []string{"--reason"}, errMsg: "--reason needs a value"},
			{name: "unknown option", args: []string{"-x", "make"}, errMsg: "unknown option -x (put the command after --)"},
			{name: "no command", args: []string{"--for", "1h", "--"}, errMsg: "usage: lidwake run [--for <duration>] [--reason <text>] -- <command> [args...]"},
			{name: "nothing", args: nil, errMsg: "usage: lidwake run [--for <duration>] [--reason <text>] -- <command> [args...]"},
			{name: "help", args: []string{"--reason", "x", "--help"}, help: true, reason: "x"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				plan, errMsg := parseRun(tc.args)
				if errMsg != tc.errMsg || plan.help != tc.help {
					t.Fatalf("err %q help %v, want %q %v", errMsg, plan.help, tc.errMsg, tc.help)
				}
				if errMsg != "" {
					return
				}
				if (plan.duration == nil) != (tc.duration == nil) || (plan.duration != nil && *plan.duration != *tc.duration) {
					t.Errorf("duration %v", plan.duration)
				}
				if plan.reason != tc.reason || plan.display != tc.display || !slices.Equal(plan.command, tc.command) {
					t.Errorf("plan %+v", plan)
				}
			})
		}
	})

	t.Run("hold request", func(t *testing.T) {
		plan, _ := parseRun([]string{"--", "/usr/local/bin/make", "all"})
		req := plan.holdRequest(4242)
		if req.Op != ipc.OpHold || req.Tool != "make" || req.ProcessName != "make" || req.Reason != "lidwake run make" ||
			req.PID != 4242 || req.TTL == nil || *req.TTL != 86400 || req.Display || req.Key != "" {
			t.Fatalf("request %+v", req)
		}
		plan, _ = parseRun([]string{"--for", "2h", "--reason", "train", "--display", "python3"})
		req = plan.holdRequest(1)
		if req.Reason != "train" || *req.TTL != 7200 || !req.Display {
			t.Fatalf("request %+v", req)
		}
	})

	t.Run("holds its own pid then execs the command", func(t *testing.T) {
		h := newHarness(t)
		h.env = []string{"PATH=/usr/bin", "HOME=/Users/me"}
		h.daemon.reply = replyWith(ipc.Response{OK: true, HoldKey: "hold:1", AppliedTTL: f64(4 * 3600)}, nil)
		expectCode(t, h.do(t, "run", "--", "make", "test"), 1) // the stubbed exec returns
		if req := h.daemon.only(t); req.PID != 4242 || req.Tool != "make" {
			t.Fatalf("request %+v", req)
		}
		if len(h.execs) != 1 || h.execs[0].path != "/usr/bin/make" || !slices.Equal(h.execs[0].argv, []string{"make", "test"}) ||
			!slices.Equal(h.execs[0].env, h.env) {
			t.Fatalf("exec %+v", h.execs)
		}
		expectOutput(t, "stderr", h.errOut, "lidwake: keeping the Mac awake while make runs (at most 4h 00m)\n"+
			"lidwake: make: exec stubbed\n")
	})

	t.Run("runs the command anyway when the hold fails", func(t *testing.T) {
		cases := []struct {
			resp ipc.Response
			err  error
			note string
		}{
			{ipc.Response{}, ipc.ErrDaemonUnreachable, "lidwake: lidwake daemon is not running. Running make without keeping the Mac awake.\n"},
			{ipc.Response{OK: false, Error: "lidwake is off"}, nil, "lidwake: could not keep the Mac awake: lidwake is off — running make anyway\n"},
			{ipc.Response{OK: true}, nil, "lidwake: could not keep the Mac awake: unknown error — running make anyway\n"},
		}
		for _, tc := range cases {
			h := newHarness(t)
			h.daemon.reply = replyWith(tc.resp, tc.err)
			h.do(t, "run", "make")
			if len(h.execs) != 1 {
				t.Fatalf("exec %+v", h.execs)
			}
			if !strings.HasPrefix(h.errOut.String(), tc.note) {
				t.Errorf("stderr %q, want prefix %q", h.errOut, tc.note)
			}
		}
	})

	t.Run("a missing command is reported and nothing runs", func(t *testing.T) {
		h := newHarness(t)
		h.lookPath = func(string) (string, error) {
			return "", &exec.Error{Name: "nosuch", Err: exec.ErrNotFound}
		}
		expectCode(t, h.do(t, "run", "nosuch"), 1)
		if len(h.execs) != 0 {
			t.Fatal("exec ran")
		}
		expectContains(t, "stderr", h.errOut, "lidwake: nosuch: executable file not found in $PATH\n")
	})

	// Go-specific: exec.LookPath refuses a match in a relative PATH entry, which execvp runs.
	t.Run("a command found through a relative PATH entry runs", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, "bin"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "bin", "mytool"), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Chdir(dir)
		t.Setenv("PATH", "./bin:/nonexistent")
		h := newHarness(t)
		h.lookPath = exec.LookPath
		h.do(t, "run", "--", "mytool", "x")
		if len(h.execs) != 1 || h.execs[0].path != filepath.Join("bin", "mytool") || !slices.Equal(h.execs[0].argv, []string{"mytool", "x"}) {
			t.Fatalf("exec %+v", h.execs)
		}
		expectContains(t, "stderr", h.errOut, "lidwake: mytool: exec stubbed\n")
	})

	// Go-specific: syscall.Exec has no execvp fallback for a script without a #! line.
	t.Run("a script the kernel can't exec runs through /bin/sh", func(t *testing.T) {
		h := newHarness(t)
		h.env = []string{"PATH=/usr/bin"}
		h.lookPath = func(file string) (string, error) { return file, nil }
		h.exec = func(path string, argv, env []string) error {
			h.execs = append(h.execs, execCall{path, argv, env})
			if len(h.execs) == 1 {
				return syscall.ENOEXEC
			}
			return errors.New("exec stubbed")
		}
		expectCode(t, h.do(t, "run", "--", "./long-job.sh", "a", "b"), 1)
		want := []execCall{
			{"./long-job.sh", []string{"./long-job.sh", "a", "b"}, h.env},
			{"/bin/sh", []string{"sh", "./long-job.sh", "a", "b"}, h.env},
		}
		if !reflect.DeepEqual(h.execs, want) {
			t.Fatalf("exec %+v, want %+v", h.execs, want)
		}
		expectContains(t, "stderr", h.errOut, "lidwake: ./long-job.sh: exec stubbed\n")
	})

	t.Run("help and usage errors place no hold", func(t *testing.T) {
		h := newHarness(t)
		expectCode(t, h.do(t, "run", "-h"), 0)
		expectOutput(t, "stdout", h.out, runUsage+"\n")
		h.daemon.none(t)
		h = newHarness(t)
		expectCode(t, h.do(t, "run", "--for", "x"), 1)
		h.daemon.none(t)
	})
}

func TestConfig(t *testing.T) {
	t.Run("lists every setting sorted", func(t *testing.T) {
		h := newHarness(t)
		expectCode(t, h.do(t, "config"), 0)
		lines := strings.Split(strings.TrimSuffix(h.out.String(), "\n"), "\n")
		if len(lines) != len(settings.Keys()) {
			t.Fatalf("%d lines for %d keys", len(lines), len(settings.Keys()))
		}
		if !slices.IsSorted(lines) {
			t.Errorf("not sorted: %q", lines)
		}
		for _, want := range []string{"soundOnLidClose = true", "soundVolume = 0.5", "thermalThresholdCelsius = 80",
			"lowBatteryThresholdPercent = 20", "agentWaitingPolicy = grace", "manualHoldMaxHours = 4", "chimeName = default"} {
			if !slices.Contains(lines, want) {
				t.Errorf("missing %q in %q", want, lines)
			}
		}
		h.daemon.none(t)
	})

	t.Run("gets one setting", func(t *testing.T) {
		h := newHarness(t)
		expectCode(t, h.do(t, "config", "idleReleaseSeconds"), 0)
		expectOutput(t, "stdout", h.out, "90\n")
	})

	t.Run("unknown key lists the known ones", func(t *testing.T) {
		h := newHarness(t)
		expectCode(t, h.do(t, "config", "volume", "1"), 1)
		known := slices.Clone(settings.Keys())
		slices.Sort(known)
		expectOutput(t, "stderr", h.errOut, "lidwake: unknown setting 'volume'. Known: "+strings.Join(known, ", ")+"\n")
		if _, err := os.Stat(h.configPath); err == nil {
			t.Error("config written")
		}
	})

	t.Run("sets values typed after the current one, clamped, then reloads", func(t *testing.T) {
		cases := []struct {
			key, value, shown string
			check             func(settings.Settings) bool
		}{
			{"lockOnLidClose", "off", "false", func(s settings.Settings) bool { return !s.LockOnLidClose }},
			{"lockOnLidClose", "YES", "true", func(s settings.Settings) bool { return s.LockOnLidClose }},
			{"requireACPower", "1", "true", func(s settings.Settings) bool { return s.RequireACPower }},
			{"soundVolume", "0.25", "0.25", func(s settings.Settings) bool { return s.SoundVolume == 0.25 }},
			{"soundVolume", "1", "1", func(s settings.Settings) bool { return s.SoundVolume == 1 }},
			{"lowBatteryThresholdPercent", "150", "99", func(s settings.Settings) bool { return s.LowBatteryThresholdPercent == 99 }},
			{"lowBatteryThresholdPercent", "30.0", "30", func(s settings.Settings) bool { return s.LowBatteryThresholdPercent == 30 }},
			{"lowBatteryThresholdPercent", "30.5", "20", func(s settings.Settings) bool { return s.LowBatteryThresholdPercent == 20 }},
			{"idleReleaseSeconds", "1e3", "1000", func(s settings.Settings) bool { return s.IdleReleaseSeconds == 1000 }},
			{"manualHoldMaxHours", "100", "24", func(s settings.Settings) bool { return s.ManualHoldMaxHours == 24 }},
			{"agentWaitingPolicy", "keepAwake", "keepAwake", func(s settings.Settings) bool { return s.AgentWaitingPolicy == settings.WaitKeepAwake }},
			{"agentWaitingPolicy", "bogus", "grace", func(s settings.Settings) bool { return s.AgentWaitingPolicy == settings.WaitGrace }},
			{"chimeName", "Glass", "Glass", func(s settings.Settings) bool { return s.ChimeName == "Glass" }},
		}
		for _, tc := range cases {
			h := newHarness(t)
			expectCode(t, h.do(t, "config", tc.key, tc.value), 0)
			expectOutput(t, tc.key+" stdout", h.out, tc.key+" = "+tc.shown+"\n")
			if !tc.check(settings.Load(h.configPath)) {
				t.Errorf("%s %s: saved %+v", tc.key, tc.value, settings.Load(h.configPath))
			}
			if req := h.daemon.only(t); req.Op != ipc.OpReloadSettings {
				t.Errorf("op %q", req.Op)
			}
			expectOutput(t, "stderr", h.errOut, "")
		}
	})

	t.Run("other settings survive a change", func(t *testing.T) {
		h := newHarness(t)
		h.do(t, "config", "chimeName", "Glass")
		h.do(t, "config", "soundVolume", "0.75")
		got := settings.Load(h.configPath)
		if got.ChimeName != "Glass" || got.SoundVolume != 0.75 || got.ThermalThresholdCelsius != 80 {
			t.Fatalf("saved %+v", got)
		}
		data, err := os.ReadFile(h.configPath)
		if err != nil || !json.Valid(data) {
			t.Fatalf("config %q, %v", data, err)
		}
	})

	t.Run("wrong types are refused", func(t *testing.T) {
		cases := map[[2]string]string{
			{"lockOnLidClose", "maybe"}: "lidwake: lockOnLidClose expects true or false, got 'maybe'\n",
			{"soundVolume", "loud"}:     "lidwake: soundVolume expects a number, got 'loud'\n",
			{"soundVolume", "inf"}:      "lidwake: soundVolume expects a number, got 'inf'\n",
		}
		for args, want := range cases {
			h := newHarness(t)
			expectCode(t, h.do(t, "config", args[0], args[1]), 1)
			expectOutput(t, "stderr", h.errOut, want)
			h.daemon.none(t)
		}
	})

	t.Run("saved even when the daemon is down", func(t *testing.T) {
		h := newHarness(t)
		h.daemon.reply = replyWith(ipc.Response{}, ipc.ErrDaemonUnreachable)
		expectCode(t, h.do(t, "config", "soundOnLidClose", "false"), 0)
		expectOutput(t, "stderr", h.errOut, "lidwake: saved; the daemon is not running, so it applies on next start\n")
		if settings.Load(h.configPath).SoundOnLidClose {
			t.Error("not saved")
		}
	})

	t.Run("reads the existing file", func(t *testing.T) {
		h := newHarness(t)
		if err := os.WriteFile(h.configPath, []byte(`{"chimeName":"Hero"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		h.do(t, "config", "chimeName")
		expectOutput(t, "stdout", h.out, "Hero\n")
	})

	t.Run("too many arguments", func(t *testing.T) {
		h := newHarness(t)
		expectCode(t, h.do(t, "config", "a", "b", "c"), 1)
		expectOutput(t, "stderr", h.errOut, "lidwake: usage: lidwake config [<key> [<value>]]\n")
	})
}

func TestHookCommands(t *testing.T) {
	t.Run("an unknown tool never fans out", func(t *testing.T) {
		for _, cmd := range []string{"install-hooks", "uninstall-hooks"} {
			h := newHarness(t)
			expectCode(t, h.do(t, cmd, "--tool", "claude"), 2)
			expectOutput(t, "stderr", h.errOut, "unknown tool 'claude' — valid: claude-code, codex, cursor, gemini-cli, aider, hermes, opencode, cline, pi\n")
			expectOutput(t, "stdout", h.out, "")
		}
	})

	t.Run("install reports per agent", func(t *testing.T) {
		h := newHarness(t)
		if err := os.MkdirAll(filepath.Join(h.hookHome, ".claude"), 0o755); err != nil {
			t.Fatal(err)
		}
		expectCode(t, h.do(t, "install-hooks"), 0)
		out := h.out.String()
		if !strings.HasPrefix(out, "[Claude Code] ") || strings.HasPrefix(out, "[Claude Code] not detected") {
			t.Errorf("stdout %q", out)
		}
		for _, name := range []string{"Codex", "Cursor", "Gemini CLI", "Aider", "Hermes", "OpenCode", "Cline", "Pi"} {
			if !strings.Contains(out, "["+name+"] not detected, skipping\n") {
				t.Errorf("%s not reported as skipped: %q", name, out)
			}
		}
		data, err := os.ReadFile(filepath.Join(h.hookHome, ".claude", "settings.json"))
		if err != nil || !strings.Contains(string(data), testCLIPath+"' acquire") && !strings.Contains(string(data), testCLIPath+" acquire") {
			t.Errorf("settings %s, %v", data, err)
		}
	})

	t.Run("dry runs write nothing", func(t *testing.T) {
		h := newHarness(t)
		if err := os.MkdirAll(filepath.Join(h.hookHome, ".claude"), 0o755); err != nil {
			t.Fatal(err)
		}
		expectCode(t, h.do(t, "install-hooks", "--tool", "claude-code", "--dry-run"), 0)
		expectContains(t, "stdout", h.out, "[Claude Code] would write:\n")
		if _, err := os.Stat(filepath.Join(h.hookHome, ".claude", "settings.json")); err == nil {
			t.Error("dry run wrote the config")
		}

		h.out.Reset()
		expectCode(t, h.do(t, "uninstall-hooks", "--tool", "claude-code", "--dry-run"), 0)
		expectOutput(t, "stdout", h.out, "[Claude Code] would remove:\n(unchanged)\n")
	})

	t.Run("uninstall removes what install wrote", func(t *testing.T) {
		h := newHarness(t)
		if err := os.MkdirAll(filepath.Join(h.hookHome, ".claude"), 0o755); err != nil {
			t.Fatal(err)
		}
		h.do(t, "install-hooks", "--tool", "claude-code")
		h.out.Reset()
		expectCode(t, h.do(t, "uninstall-hooks", "--tool", "claude-code"), 0)
		expectOutput(t, "stdout", h.out, "[Claude Code] removed hook entries\n")
		if h.hooks().State("claude-code") != "notInstalled" {
			t.Error("hooks still installed")
		}
	})

	t.Run("errors go to stderr and the command still exits 0", func(t *testing.T) {
		h := newHarness(t)
		dir := filepath.Join(h.hookHome, ".claude")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte("{not json"), 0o644); err != nil {
			t.Fatal(err)
		}
		expectCode(t, h.do(t, "install-hooks", "--tool", "claude-code"), 0)
		expectContains(t, "stderr", h.errOut, "[Claude Code] error: ")
	})
}

func TestSetupDispatch(t *testing.T) {
	t.Run("setup runs the user half", func(t *testing.T) {
		h := newHarness(t)
		h.euid = 0
		expectCode(t, h.do(t, "setup"), 1)
		expectOutput(t, "stderr", h.errOut, "lidwake: setup failed: "+install.ErrRunAsUser.Error()+"\n")
		if len(h.commands) != 0 {
			t.Errorf("commands %v", h.commands)
		}
	})

	t.Run("setup --root runs the privileged half", func(t *testing.T) {
		h := newHarness(t)
		expectCode(t, h.do(t, "setup", "--root", "--uid", "501"), 1)
		expectOutput(t, "stderr", h.errOut, "lidwake: setup failed: "+install.ErrNeedRoot.Error()+"\n")

		for _, args := range [][]string{{"setup", "--root"}, {"setup", "--root", "--uid"}, {"setup", "--root", "--uid", "0"}, {"setup", "--root", "--uid", "me"}} {
			h := newHarness(t)
			expectCode(t, h.do(t, args...), 1)
			expectOutput(t, "stderr", h.errOut, "lidwake: setup --root needs --uid <the installing user's uid>\n")
		}

		h = newHarness(t)
		expectCode(t, h.do(t, "setup", "--root", "--uid=501"), 1)
		expectOutput(t, "stderr", h.errOut, "lidwake: setup failed: "+install.ErrNeedRoot.Error()+"\n")
	})

	t.Run("uninstall and uninstall --root", func(t *testing.T) {
		h := newHarness(t)
		h.euid = 0
		expectCode(t, h.do(t, "uninstall"), 1)
		expectOutput(t, "stderr", h.errOut, "lidwake: uninstall failed: "+install.ErrRunAsUser.Error()+"\n")

		h = newHarness(t)
		expectCode(t, h.do(t, "uninstall", "--root"), 1)
		expectOutput(t, "stderr", h.errOut, "lidwake: uninstall failed: "+install.ErrNeedRoot.Error()+"\n")
	})

	// Go-specific: both act at once on the whole machine, so an argument they don't take — a
	// flag borrowed from uninstall-hooks, a typo — stops them before anything is touched.
	t.Run("unknown arguments stop setup and uninstall before they act", func(t *testing.T) {
		cases := []struct {
			args []string
			want string
		}{
			{[]string{"uninstall", "--dry-run"}, "lidwake: uninstall takes no arguments (got --dry-run)\n"},
			{[]string{"uninstall", "--tool", "claude-code"}, "lidwake: uninstall takes no arguments (got --tool)\n"},
			{[]string{"uninstall", "now"}, "lidwake: uninstall takes no arguments (got now)\n"},
			{[]string{"uninstall", "--root", "--uid", "501"}, "lidwake: uninstall takes no arguments (got --uid)\n"},
			{[]string{"uninstall", "--root=yes"}, "lidwake: uninstall takes no arguments (got --root)\n"},
			{[]string{"setup", "--dry-run", "--force"}, "lidwake: setup takes no arguments (got --dry-run --force)\n"},
			{[]string{"setup", "--uid", "501"}, "lidwake: setup takes no arguments (got --uid)\n"},
			{[]string{"setup", "--root", "--uid", "501", "extra"}, "lidwake: setup takes no arguments (got extra)\n"},
		}
		for _, tc := range cases {
			h := newHarness(t)
			expectCode(t, h.do(t, tc.args...), 2)
			expectOutput(t, "stderr "+strings.Join(tc.args, " "), h.errOut, tc.want)
			if len(h.commands) != 0 || h.out.Len() != 0 {
				t.Errorf("%v ran %v, printed %q", tc.args, h.commands, h.out)
			}
		}
	})

	t.Run("help prints the usage and does nothing", func(t *testing.T) {
		for _, tc := range []struct{ command, usage string }{{"setup", setupUsage}, {"uninstall", uninstallUsage}} {
			for _, flag := range []string{"-h", "--help"} {
				h := newHarness(t)
				expectCode(t, h.do(t, tc.command, flag), 0)
				expectOutput(t, "stdout", h.out, tc.usage+"\n")
				if len(h.commands) != 0 {
					t.Errorf("%s %s ran %v", tc.command, flag, h.commands)
				}
			}
		}
	})
}

// Go-specific: every outside-world hook of the production app is wired, so no command can hit a
// nil function. Building the app reads nothing and touches no file.
func TestNewAppIsFullyWired(t *testing.T) {
	a := newApp()
	v := reflect.ValueOf(a).Elem()
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		switch f.Kind() {
		case reflect.Func, reflect.Pointer, reflect.Interface:
			if f.IsNil() {
				t.Errorf("app.%s is nil", v.Type().Field(i).Name)
			}
		case reflect.String:
			if f.String() == "" {
				t.Errorf("app.%s is empty", v.Type().Field(i).Name)
			}
		}
	}
}

// Go-specific: the daemon-not-running error reads as the full sentence users have always seen;
// other errors pass through unchanged.
func TestErrText(t *testing.T) {
	if got := errText(ipc.ErrDaemonUnreachable); got != "lidwake daemon is not running." {
		t.Errorf("unreachable: %q", got)
	}
	if got := errText(fmt.Errorf("send: %w", ipc.ErrDaemonUnreachable)); got != "lidwake daemon is not running." {
		t.Errorf("wrapped: %q", got)
	}
	if got := errText(errors.New("i/o timeout")); got != "i/o timeout" {
		t.Errorf("other: %q", got)
	}
}
