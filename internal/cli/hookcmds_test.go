package cli

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/nikitaShakhbazyan/lidwake-go/internal/ipc"
)

// A hook's process tree: the agent (claude, pid 300) runs /bin/sh (pid 200), which runs lidwake.
func claudeTree() map[int]proc {
	return map[int]proc{
		200: {path: "/bin/sh", ppid: 300},
		300: {path: "/Users/me/.local/share/claude/versions/2.1.156", ppid: 1},
	}
}

func TestAcquire(t *testing.T) {
	t.Run("stdin session id wins over the positional and the owning agent pid is resolved", func(t *testing.T) {
		h := newHarness(t).withPayload(`{"session_id":"sess-1","hook_event_name":"UserPromptSubmit"}`)
		h.resolver = fakeResolver(200, claudeTree())
		expectCode(t, h.do(t, "acquire", "positional-id", "--tool", "claude-code", "--reason", "turn"), 0)
		req := h.daemon.only(t)
		want := ipc.Request{Op: ipc.OpAcquire, Key: "claude-code:sess-1", Tool: "claude-code", Reason: "turn", PID: 300, ProcessName: "claude-code"}
		if req.Op != want.Op || req.Key != want.Key || req.Tool != want.Tool || req.Reason != want.Reason ||
			req.PID != want.PID || req.ProcessName != want.ProcessName || req.TTL != nil || req.Display {
			t.Fatalf("request %+v, want %+v", req, want)
		}
		expectOutput(t, "stderr", h.errOut, "")
		expectOutput(t, "stdout", h.out, "")
	})

	t.Run("positional is the fallback and is trimmed", func(t *testing.T) {
		h := newHarness(t)
		expectCode(t, h.do(t, "acquire", "  abc \n", "--tool", "codex"), 0)
		if req := h.daemon.only(t); req.Key != "codex:abc" || req.PID != 0 {
			t.Fatalf("request %+v", req)
		}
	})

	t.Run("already prefixed keys pass through verbatim", func(t *testing.T) {
		h := newHarness(t)
		h.do(t, "acquire", "codex:abc", "--tool", "codex")
		if req := h.daemon.only(t); req.Key != "codex:abc" {
			t.Fatalf("key %q", req.Key)
		}
	})

	t.Run("no session key fails soft for hooks", func(t *testing.T) {
		h := newHarness(t)
		expectCode(t, h.do(t, "acquire", "   ", "--tool", "claude-code"), 0)
		h.daemon.none(t)
		expectOutput(t, "stderr", h.errOut, "lidwake: acquire: no session key (stdin payload or positional) — ignored\n")
	})

	t.Run("no session key exits 2 at a terminal", func(t *testing.T) {
		h := newHarness(t).atTTY()
		expectCode(t, h.do(t, "acquire", "--tool", "claude-code"), 2)
		h.daemon.none(t)
	})

	t.Run("ttl and pid are parsed and bad values warned about", func(t *testing.T) {
		h := newHarness(t)
		h.resolver = fakeResolver(200, map[int]proc{777: {path: "/opt/node", ppid: 1}})
		h.do(t, "acquire", "k", "--tool", "pi", "--ttl", "90", "--pid", "777")
		req := h.daemon.only(t)
		if req.TTL == nil || *req.TTL != 90 || req.PID != 777 {
			t.Fatalf("request %+v", req)
		}
		expectOutput(t, "stderr", h.errOut, "")

		for _, bad := range [][]string{{"--ttl", "soon"}, {"--ttl", "0"}, {"--ttl", "-5"}, {"--ttl", "inf"}, {"--ttl", "NaN"}} {
			h := newHarness(t)
			h.do(t, append([]string{"acquire", "k", "--tool", "codex"}, bad...)...)
			if req := h.daemon.only(t); req.TTL != nil {
				t.Errorf("%v: ttl %v", bad, *req.TTL)
			}
			expectOutput(t, "stderr", h.errOut, "lidwake: ignoring invalid --ttl '"+bad[1]+"'\n")
		}
		for _, bad := range []string{"0", "x", "99999999999", "1.5"} {
			h := newHarness(t)
			h.do(t, "acquire", "k", "--tool", "codex", "--pid="+bad)
			if req := h.daemon.only(t); req.PID != 0 {
				t.Errorf("%s: pid %d", bad, req.PID)
			}
			expectOutput(t, "stderr", h.errOut, "lidwake: ignoring invalid --pid '"+bad+"'\n")
		}
	})

	t.Run("a dead hook pid falls back to the process walk", func(t *testing.T) {
		h := newHarness(t)
		h.resolver = fakeResolver(200, claudeTree())
		h.do(t, "acquire", "k", "--tool", "claude-code", "--pid", "999")
		if req := h.daemon.only(t); req.PID != 300 {
			t.Fatalf("pid %d", req.PID)
		}
		expectOutput(t, "stderr", h.errOut, "lidwake: --pid 999 is not alive — falling back to process-tree resolution\n")
	})

	t.Run("unresolved agent sends no pid", func(t *testing.T) {
		h := newHarness(t)
		h.resolver = fakeResolver(200, map[int]proc{200: {path: "/bin/sh", ppid: 1}})
		h.do(t, "acquire", "k", "--tool", "claude-code")
		if req := h.daemon.only(t); req.PID != 0 {
			t.Fatalf("pid %d", req.PID)
		}
	})

	// A Node-hosted Pi: the executable walk sees only node, argv reveals Pi.
	piTree := map[int]proc{
		200: {path: "/bin/sh", ppid: 300},
		300: {path: "/opt/homebrew/bin/node", ppid: 1, argv: []string{"node", "/opt/homebrew/bin/pi", "--continue"}},
	}

	t.Run("pi environment markers authorize the argv walk", func(t *testing.T) {
		for _, env := range []string{"AI_AGENT=pi", "PI_CODING_AGENT=true"} {
			h := newHarness(t)
			h.resolver = fakeResolver(200, piTree)
			h.env = []string{"PATH=/usr/bin", env}
			h.do(t, "acquire", "k", "--tool", "pi")
			if req := h.daemon.only(t); req.PID != 300 {
				t.Errorf("%s: pid %d", env, req.PID)
			}
		}
	})

	t.Run("pi argv walk needs the markers and the pi tool", func(t *testing.T) {
		h := newHarness(t)
		h.resolver = fakeResolver(200, piTree)
		h.do(t, "acquire", "k", "--tool", "pi")
		if req := h.daemon.only(t); req.PID != 0 {
			t.Errorf("without markers: pid %d", req.PID)
		}
		h = newHarness(t)
		h.resolver = fakeResolver(200, piTree)
		h.env = []string{"AI_AGENT=pi"}
		h.do(t, "acquire", "k", "--tool", "codex")
		if req := h.daemon.only(t); req.PID != 0 {
			t.Errorf("other tool: pid %d", req.PID)
		}
	})

	t.Run("subagent keys on agent_id", func(t *testing.T) {
		h := newHarness(t).withPayload(`{"session_id":"parent","agent_id":"sub-9"}`)
		h.resolver = fakeResolver(200, claudeTree())
		h.do(t, "acquire", "parent", "--tool", "claude-code", "--subagent")
		if req := h.daemon.only(t); req.Key != "claude-code:sub-9" || req.PID != 300 {
			t.Fatalf("request %+v", req)
		}
	})

	t.Run("subagent without agent_id never falls back to the session", func(t *testing.T) {
		h := newHarness(t).withPayload(`{"session_id":"parent"}`)
		expectCode(t, h.do(t, "acquire", "parent", "--tool", "claude-code", "--subagent"), 0)
		h.daemon.none(t)
		expectOutput(t, "stderr", h.errOut, "lidwake: acquire --subagent: no agent_id on stdin — ignored\n")
	})

	t.Run("if-background places nothing for a foreground command", func(t *testing.T) {
		h := newHarness(t).withPayload(`{"session_id":"s","tool_input":{"command":"ls"}}`)
		expectCode(t, h.do(t, "acquire", "--tool", "claude-code", "--if-background"), 0)
		h.daemon.none(t)
		expectOutput(t, "stderr", h.errOut, "")
	})

	t.Run("if-background holds a background command under a fresh key", func(t *testing.T) {
		h := newHarness(t).withPayload(`{"session_id":"s","tool_input":{"command":"make","run_in_background":true}}`)
		h.resolver = fakeResolver(200, claudeTree())
		h.do(t, "acquire", "--tool", "claude-code", "--if-background")
		req := h.daemon.only(t)
		if !regexp.MustCompile(`^claude-code:bg-[0-9a-f]{8}$`).MatchString(req.Key) {
			t.Errorf("key %q", req.Key)
		}
		if req.TTL == nil || *req.TTL != 86400 || req.PID != 300 {
			t.Errorf("request %+v", req)
		}

		h = newHarness(t).withPayload(`{"tool_input":{"run_in_background":true}}`)
		h.do(t, "acquire", "--tool", "claude-code", "--if-background", "--ttl", "3600")
		if req := h.daemon.only(t); req.TTL == nil || *req.TTL != 3600 {
			t.Errorf("ttl %v", req.TTL)
		}
	})

	t.Run("gateway agents share one key and watch the gateway pid", func(t *testing.T) {
		h := newHarness(t).withPayload(`{"session_id":"s"}`)
		if err := os.MkdirAll(filepath.Join(h.home, ".hermes"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(h.home, ".hermes", "gateway.pid"), []byte(`{"pid": 555, "kind": "hermes-gateway"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		h.resolver = fakeResolver(200, map[int]proc{555: {path: "/usr/bin/python3", ppid: 1}})
		h.do(t, "acquire", "s", "--tool", "hermes")
		if req := h.daemon.only(t); req.Key != "hermes:gateway" || req.PID != 555 {
			t.Fatalf("request %+v", req)
		}
	})

	t.Run("display is requested and version skew is reported", func(t *testing.T) {
		h := newHarness(t)
		h.do(t, "acquire", "k", "--tool", "codex", "--display")
		if req := h.daemon.only(t); !req.Display {
			t.Fatal("display not requested")
		}
		expectOutput(t, "stderr", h.errOut, "lidwake: the running daemon predates --display — the display is NOT being kept awake. Update lidwake and retry.\n")

		h = newHarness(t)
		h.daemon.reply = replyWith(ipc.Response{OK: true, DisplayApplied: ipc.Ptr(true)}, nil)
		h.do(t, "acquire", "k", "--tool", "codex", "--display")
		expectOutput(t, "stderr", h.errOut, "")
	})

	t.Run("daemon failures never fail the hook", func(t *testing.T) {
		cases := []struct {
			name   string
			resp   ipc.Response
			err    error
			stderr string
		}{
			{"unreachable", ipc.Response{}, ipc.ErrDaemonUnreachable, "lidwake: daemon not running (acquire ignored)\n"},
			{"transport", ipc.Response{}, errors.New("i/o timeout"), "lidwake: acquire failed (i/o timeout) — ignored\n"},
			{"refused", ipc.Response{OK: false, Error: "lidwake is off"}, nil, "lidwake: acquire refused: lidwake is off\n"},
			{"refused silently", ipc.Response{OK: false}, nil, "lidwake: acquire refused: ?\n"},
		}
		for _, tc := range cases {
			for _, tty := range []bool{false, true} {
				h := newHarness(t)
				h.tty = tty
				h.daemon.reply = replyWith(tc.resp, tc.err)
				expectCode(t, h.do(t, "acquire", "k", "--tool", "codex"), 0)
				expectOutput(t, tc.name+" stderr", h.errOut, tc.stderr)
			}
		}
	})
}

func TestRelease(t *testing.T) {
	t.Run("key resolution accepts every key form", func(t *testing.T) {
		cases := []struct {
			args []string
			key  string
		}{
			{[]string{"abc", "--tool", "claude-code"}, "claude-code:abc"},
			{[]string{"claude-code:abc", "--tool", "claude-code"}, "claude-code:abc"},
			{[]string{"hold:ab12cd34", "--tool", "claude-code"}, "hold:ab12cd34"},
			{[]string{"sniffed:codex:4242", "--tool", "claude-code"}, "sniffed:codex:4242"},
			{[]string{"sniffed:codex:4242"}, "sniffed:codex:4242"},
			{[]string{"hold:ab12cd34"}, "hold:ab12cd34"},
			{[]string{"codex:abc"}, "codex:abc"},
			{[]string{"abc"}, "unknown:abc"},
			{[]string{" abc\t", "--tool", "codex"}, "codex:abc"},
		}
		for _, tc := range cases {
			h := newHarness(t)
			expectCode(t, h.do(t, append([]string{"release"}, tc.args...)...), 0)
			req := h.daemon.only(t)
			if req.Op != ipc.OpRelease || req.Key != tc.key {
				t.Errorf("%q: request %+v, want key %q", tc.args, req, tc.key)
			}
		}
	})

	t.Run("stdin session id wins", func(t *testing.T) {
		h := newHarness(t).withPayload(`{"session_id":"sess-2"}`)
		h.do(t, "release", "stale", "--tool", "claude-code")
		if req := h.daemon.only(t); req.Key != "claude-code:sess-2" || req.Tool != "claude-code" {
			t.Fatalf("request %+v", req)
		}
	})

	t.Run("subagent releases the agent_id key", func(t *testing.T) {
		h := newHarness(t).withPayload(`{"session_id":"parent","agent_id":"sub-9"}`)
		h.do(t, "release", "--tool", "claude-code", "--subagent")
		if req := h.daemon.only(t); req.Key != "claude-code:sub-9" {
			t.Fatalf("key %q", req.Key)
		}

		h = newHarness(t).withPayload(`{"session_id":"parent"}`)
		expectCode(t, h.do(t, "release", "--tool", "claude-code", "--subagent"), 0)
		h.daemon.none(t)
		expectOutput(t, "stderr", h.errOut, "lidwake: release --subagent: no agent_id on stdin — ignored\n")
	})

	t.Run("gateway agents release the shared key", func(t *testing.T) {
		h := newHarness(t)
		h.do(t, "release", "whatever", "--tool", "hermes")
		if req := h.daemon.only(t); req.Key != "hermes:gateway" {
			t.Fatalf("key %q", req.Key)
		}
	})

	t.Run("no key fails soft, and exits 2 at a terminal", func(t *testing.T) {
		h := newHarness(t)
		expectCode(t, h.do(t, "release", "--tool", "codex"), 0)
		expectOutput(t, "stderr", h.errOut, "lidwake: release: no session key (stdin payload or positional) — ignored\n")
		h = newHarness(t).atTTY()
		expectCode(t, h.do(t, "release", "--tool", "codex"), 2)
		h.daemon.none(t)
	})

	t.Run("a release that matched nothing exits 1 only for humans and scripts", func(t *testing.T) {
		cases := []struct {
			name string
			args []string
			tty  bool
			code int
		}{
			{"hook", []string{"abc", "--tool", "codex"}, false, 0},
			{"hook at a terminal", []string{"abc", "--tool", "codex"}, true, 1},
			{"script without a tool", []string{"hold:ab12cd34"}, false, 1},
			{"human without a tool", []string{"hold:ab12cd34"}, true, 1},
		}
		for _, tc := range cases {
			h := newHarness(t)
			h.tty = tc.tty
			h.daemon.reply = replyWith(ipc.Response{OK: true, Warning: "no assertion with key 'codex:abc'"}, nil)
			if code := h.do(t, append([]string{"release"}, tc.args...)...); code != tc.code {
				t.Errorf("%s: exit %d, want %d", tc.name, code, tc.code)
			}
			expectOutput(t, tc.name+" stderr", h.errOut, "lidwake: no assertion with key 'codex:abc'\n")
		}
	})

	t.Run("transport failures are ignored", func(t *testing.T) {
		h := newHarness(t)
		h.daemon.reply = replyWith(ipc.Response{}, ipc.ErrDaemonUnreachable)
		expectCode(t, h.do(t, "release", "abc"), 0)
		expectOutput(t, "stderr", h.errOut, "lidwake: release failed (lidwake daemon is not running.) — ignored\n")
	})

	t.Run("release all reports what it released", func(t *testing.T) {
		cases := []struct {
			count *int
			out   string
		}{
			{ipc.Ptr(3), "Released 3 assertions — your Mac can sleep.\n"},
			{ipc.Ptr(1), "Released 1 assertion — your Mac can sleep.\n"},
			{ipc.Ptr(0), "Nothing was held — released nothing.\n"},
			{nil, "Nothing was held — released nothing.\n"},
		}
		for _, tc := range cases {
			h := newHarness(t)
			h.daemon.reply = replyWith(ipc.Response{OK: true, ReleasedCount: tc.count}, nil)
			expectCode(t, h.do(t, "release", "--all"), 0)
			if req := h.daemon.only(t); req.Op != ipc.OpReleaseAll {
				t.Fatalf("op %q", req.Op)
			}
			expectOutput(t, "stdout", h.out, tc.out)
		}
		h := newHarness(t)
		h.daemon.reply = replyWith(ipc.Response{}, ipc.ErrDaemonUnreachable)
		expectCode(t, h.do(t, "release", "--all"), 1)
		expectOutput(t, "stderr", h.errOut, "lidwake: release --all failed (lidwake daemon is not running.)\n")
	})
}
