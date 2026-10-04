package daemon

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/nikitaShakhbazyan/lidwake-go/internal/ipc"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/model"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/settings"
)

func TestCLISocket(t *testing.T) {
	t.Run("the socket and its directory are the user's alone", func(t *testing.T) {
		h := newHarness(t)
		dir := filepath.Join(h.dir, "support")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		h.cfg.Socket = filepath.Join(dir, "cli.sock")
		h.start()
		info, err := os.Lstat(h.cfg.Socket)
		if err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0o600 {
			t.Fatalf("socket mode = %v, %v", info.Mode(), err)
		}
		if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("directory mode = %v, %v", info.Mode(), err)
		}
	})

	t.Run("a stale socket from a dead daemon is replaced", func(t *testing.T) {
		h := newHarness(t)
		ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: h.cfg.Socket, Net: "unix"})
		if err != nil {
			t.Fatal(err)
		}
		ln.SetUnlinkOnClose(false)
		ln.Close()
		if _, err := os.Lstat(h.cfg.Socket); err != nil {
			t.Fatalf("no stale socket to test with: %v", err)
		}
		h.start()
		if resp := h.send(ipc.Request{Op: ipc.OpPing}); !resp.OK {
			t.Fatalf("ping = %+v", resp)
		}
	})

	t.Run("ping and unknown ops", func(t *testing.T) {
		h := newHarness(t)
		h.start()
		if resp := h.send(ipc.Request{Op: ipc.OpPing}); !resp.OK || resp.Error != "" {
			t.Fatalf("ping = %+v", resp)
		}
		if resp := h.send(ipc.Request{Op: "selfDestruct"}); resp.OK || resp.Error != `unknown op "selfDestruct"` {
			t.Fatalf("unknown op = %+v", resp)
		}
	})

	t.Run("status answers with the status and its summary fields", func(t *testing.T) {
		h := newHarness(t)
		h.start()
		h.acquire("claude-code:s1")
		resp := h.send(ipc.Request{Op: ipc.OpStatus})
		if !resp.OK || resp.Status == nil || !*resp.Blocking || *resp.AssertionCount != 1 {
			t.Fatalf("status = %+v", resp)
		}
	})

	t.Run("acquire validates every field", func(t *testing.T) {
		h := newHarness(t)
		h.start()
		cases := []struct {
			name string
			req  ipc.Request
			want string
		}{
			{"missing key", ipc.Request{Tool: "claude-code"}, "acquire requires key and tool"},
			{"missing tool", ipc.Request{Key: "claude-code:s1"}, "acquire requires key and tool"},
			{"blank key", ipc.Request{Key: "   ", Tool: "claude-code"}, "acquire requires a non-empty key"},
			{"long key", ipc.Request{Key: strings.Repeat("k", 257), Tool: "claude-code"}, "key exceeds 256 characters"},
			{"long tool", ipc.Request{Key: "x:s1", Tool: strings.Repeat("t", 65)}, "tool exceeds 64 characters"},
			{"hold namespace", ipc.Request{Key: "hold:abcd", Tool: "claude-code"}, "the 'hold:' key namespace is reserved"},
			{"sniffed namespace", ipc.Request{Key: "sniffed:claude-code:1", Tool: "claude-code"}, "the 'sniffed:' key namespace is reserved"},
		}
		for _, c := range cases {
			c.req.Op = ipc.OpAcquire
			if resp := h.send(c.req); resp.OK || resp.Error != c.want {
				t.Errorf("%s: %+v, want error %q", c.name, resp, c.want)
			}
		}
		if n := len(h.status().Assertions); n != 0 {
			t.Fatalf("a rejected acquire was stored (%d)", n)
		}
	})

	t.Run("acquire clamps the reason, the ttl and the expiry", func(t *testing.T) {
		h := newHarness(t)
		h.start()
		long := strings.Repeat("é", 2000)
		h.send(ipc.Request{Op: ipc.OpAcquire, Key: "claude-code:a", Tool: "claude-code", Reason: long, TTL: ipc.Ptr(10 * 86400.0), PID: 77, ProcessName: "claude"})
		h.send(ipc.Request{Op: ipc.OpAcquire, Key: "claude-code:b", Tool: "claude-code", TTL: ipc.Ptr(-5.0)})
		as := h.status().Assertions
		a, b := as[0], as[1]
		if got := len([]rune(a.Reason)); got != 1024 {
			t.Errorf("reason kept %d characters", got)
		}
		// 24 h by the validator, then down to the 4 h max hold.
		if a.ExpiresAt == nil || !a.ExpiresAt.Equal(harnessEpoch.Add(4*time.Hour)) {
			t.Errorf("expiry = %v", a.ExpiresAt)
		}
		if a.PID != 77 || a.ProcessName != "claude" || a.Origin != model.OriginHook {
			t.Errorf("a = %+v", a)
		}
		if b.ExpiresAt != nil || b.PID != -1 || b.ProcessName != "claude-code" {
			t.Errorf("b = %+v (a negative ttl is no ttl; no pid is -1; the tool names the process)", b)
		}
	})

	t.Run("a duplicate acquire refreshes without a new event", func(t *testing.T) {
		h := newHarness(t)
		h.start()
		h.acquire("claude-code:s1")
		h.clock.Advance(time.Minute)
		resp := h.acquire("claude-code:s1")
		if !resp.OK || *resp.AssertionCount != 1 {
			t.Fatalf("duplicate = %+v", resp)
		}
		if n := h.countEvents(model.EventAcquired); n != 1 {
			t.Fatalf("acquired logged %d times", n)
		}
		if a := h.status().Assertions[0]; !a.LastActivityAt.Equal(harnessEpoch.Add(time.Minute)) {
			t.Fatalf("lastActivityAt = %v", a.LastActivityAt)
		}
	})

	t.Run("the registry has a hard size limit, overall and per process", func(t *testing.T) {
		h := newHarness(t)
		h.start()
		for i := range maxAssertionsPerPID {
			if resp := h.send(ipc.Request{Op: ipc.OpAcquire, Key: fmt.Sprintf("x:p%d", i), Tool: "x", PID: 9}); !resp.OK {
				t.Fatalf("acquire %d = %+v", i, resp)
			}
		}
		resp := h.send(ipc.Request{Op: ipc.OpAcquire, Key: "x:p-over", Tool: "x", PID: 9})
		if resp.OK || resp.Error != "too many active assertions" || !*resp.Blocking || *resp.AssertionCount != maxAssertionsPerPID {
			t.Fatalf("over the per-process limit = %+v", resp)
		}
		// A duplicate of a held key is a refresh, never refused.
		if resp := h.send(ipc.Request{Op: ipc.OpAcquire, Key: "x:p0", Tool: "x", PID: 9}); !resp.OK {
			t.Fatalf("refresh at the limit = %+v", resp)
		}
		for i := maxAssertionsPerPID; i < maxAssertions; i++ {
			if resp := h.send(ipc.Request{Op: ipc.OpAcquire, Key: fmt.Sprintf("x:%d", i), Tool: "x"}); !resp.OK {
				t.Fatalf("acquire %d = %+v", i, resp)
			}
		}
		if resp := h.send(ipc.Request{Op: ipc.OpAcquire, Key: "x:over", Tool: "x"}); resp.OK || resp.Error != "too many active assertions" {
			t.Fatalf("over the limit = %+v", resp)
		}
		if resp := h.send(ipc.Request{Op: ipc.OpHold}); resp.OK || resp.Error != "Too many active assertions — hold not placed." {
			t.Fatalf("hold over the limit = %+v", resp)
		}
	})

	t.Run("release takes the key verbatim and warns on an unknown one", func(t *testing.T) {
		h := newHarness(t)
		h.start()
		if resp := h.send(ipc.Request{Op: ipc.OpRelease}); resp.OK || resp.Error != "release requires key" {
			t.Fatalf("no key = %+v", resp)
		}
		resp := h.send(ipc.Request{Op: ipc.OpRelease, Key: "claude-code:nope"})
		if !resp.OK || resp.Warning != "no assertion for key 'claude-code:nope' — released nothing" || *resp.Blocking || *resp.AssertionCount != 0 {
			t.Fatalf("unknown key = %+v", resp)
		}
		hold := h.send(ipc.Request{Op: ipc.OpHold})
		if resp := h.send(ipc.Request{Op: ipc.OpRelease, Key: hold.HoldKey, Tool: "mcp"}); !resp.OK || resp.Warning != "" {
			t.Fatalf("release of a hold key = %+v", resp)
		}
	})

	t.Run("hold places a manual, TTL-bounded assertion", func(t *testing.T) {
		h := newHarness(t)
		h.start()
		resp := h.send(ipc.Request{Op: ipc.OpHold, Reason: "deploy", TTL: ipc.Ptr(10 * 3600.0)})
		if !resp.OK || !strings.HasPrefix(resp.HoldKey, "hold:") || len(resp.HoldKey) != len("hold:")+8 {
			t.Fatalf("hold = %+v", resp)
		}
		if resp.AppliedTTL == nil || *resp.AppliedTTL != 4*3600 || !*resp.Blocking || *resp.AssertionCount != 1 {
			t.Fatalf("hold = %+v (capped by the 4 h max)", resp)
		}
		if resp.DisplayApplied == nil || *resp.DisplayApplied {
			t.Fatalf("displayApplied = %v", resp.DisplayApplied)
		}
		a := h.status().Assertions[0]
		if a.Key != resp.HoldKey || a.Tool != "manual" || a.ProcessName != "manual" || a.Origin != model.OriginManual ||
			a.PID != -1 || a.Reason != "deploy" || a.ExpiresAt == nil || !a.ExpiresAt.Equal(harnessEpoch.Add(4*time.Hour)) {
			t.Fatalf("hold assertion = %+v", a)
		}
		resp = h.send(ipc.Request{Op: ipc.OpHold, Tool: "claude-code", PID: 55, Display: true})
		if *resp.AppliedTTL != 3600 || !*resp.DisplayApplied {
			t.Fatalf("default hold = %+v", resp)
		}
		for _, a := range h.status().Assertions {
			if a.Key == resp.HoldKey && (a.Tool != "claude-code" || a.PID != 55 || !a.HoldsDisplay) {
				t.Fatalf("named hold = %+v", a)
			}
		}
		if !h.exits.isWatched(55) {
			t.Fatal("the hold's process is not watched")
		}
	})

	t.Run("hold is refused when agent holds are off", func(t *testing.T) {
		h := newHarness(t)
		h.writeSettings(func(s *settings.Settings) { s.AgentHoldsEnabled = false })
		h.start()
		if resp := h.send(ipc.Request{Op: ipc.OpHold}); resp.OK || resp.Error != "Agent holds are turned off in lidwake settings." {
			t.Fatalf("disabled = %+v", resp)
		}
	})

	t.Run("a hold with an over-long tool is placed with the label clamped", func(t *testing.T) {
		h := newHarness(t)
		h.start()
		// `run` sends the command's basename, which can be any length.
		resp := h.send(ipc.Request{Op: ipc.OpHold, Tool: strings.Repeat("ü", 65) + ".sh"})
		if !resp.OK || resp.HoldKey == "" {
			t.Fatalf("long tool = %+v", resp)
		}
		a := h.status().Assertions[0]
		if want := strings.Repeat("ü", 64); a.Key != resp.HoldKey || a.Tool != want || a.ProcessName != want {
			t.Fatalf("hold assertion = %+v", a)
		}
	})

	t.Run("releaseAll reports what it released", func(t *testing.T) {
		h := newHarness(t)
		h.start()
		resp := h.send(ipc.Request{Op: ipc.OpReleaseAll})
		if !resp.OK || *resp.ReleasedCount != 0 || resp.Warning != "nothing was held — released nothing" || *resp.Blocking || *resp.AssertionCount != 0 {
			t.Fatalf("nothing held = %+v", resp)
		}
		h.acquire("claude-code:a")
		h.acquire("claude-code:b")
		resp = h.send(ipc.Request{Op: ipc.OpReleaseAll})
		if !resp.OK || *resp.ReleasedCount != 2 || resp.Warning != "" {
			t.Fatalf("two held = %+v", resp)
		}
	})

	t.Run("reloadSettings applies config.json", func(t *testing.T) {
		h := newHarness(t)
		h.start()
		h.writeSettings(func(s *settings.Settings) { s.ManualHoldMaxHours = 1 })
		if resp := h.send(ipc.Request{Op: ipc.OpReloadSettings}); !resp.OK {
			t.Fatalf("reload = %+v", resp)
		}
		if got := h.status().Settings.ManualHoldMaxHours; got != 1 {
			t.Fatalf("max hold = %v", got)
		}
		if resp := h.send(ipc.Request{Op: ipc.OpHold, TTL: ipc.Ptr(7200.0)}); *resp.AppliedTTL != 3600 {
			t.Fatalf("hold under the new cap = %+v", resp)
		}
	})

	t.Run("a client that never sends is dropped", func(t *testing.T) {
		h := newHarness(t)
		h.cfg.ConnTimeout = 30 * time.Millisecond
		h.start()
		conn, err := net.Dial("unix", h.cfg.Socket)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		_ = conn.SetReadDeadline(time.Now().Add(waitLimit))
		if _, err := conn.Read(make([]byte, 1)); err != io.EOF {
			t.Fatalf("read = %v, want EOF from the server hanging up", err)
		}
	})

	t.Run("a malformed or oversized frame gets no reply", func(t *testing.T) {
		h := newHarness(t)
		h.start()
		for _, frame := range [][]byte{
			append(binary.BigEndian.AppendUint32(nil, 3), []byte("{x}")...),
			binary.BigEndian.AppendUint32(nil, ipc.MaxFrame+1),
		} {
			conn, err := net.Dial("unix", h.cfg.Socket)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := conn.Write(frame); err != nil {
				t.Fatal(err)
			}
			_ = conn.SetReadDeadline(time.Now().Add(waitLimit))
			if _, err := conn.Read(make([]byte, 1)); err != io.EOF {
				t.Fatalf("read = %v, want EOF", err)
			}
			conn.Close()
		}
		if resp := h.send(ipc.Request{Op: ipc.OpPing}); !resp.OK {
			t.Fatal("the daemon stopped answering")
		}
	})
}

func TestMainThread(t *testing.T) {
	t.Run("posted work runs on the goroutine that calls run, in order", func(t *testing.T) {
		m := newMainThread()
		caller := make(chan uint64, 1)
		var ran []uint64
		var order []int
		release := make(chan struct{})
		err := func() error {
			caller <- goroutineID()
			return m.run(func() error {
				for i := range 3 {
					if !m.post(func() { ran = append(ran, goroutineID()); order = append(order, i) }) {
						return fmt.Errorf("post %d refused", i)
					}
				}
				m.post(func() { close(release) })
				<-release
				return io.ErrUnexpectedEOF
			})
		}()
		if err != io.ErrUnexpectedEOF {
			t.Fatalf("run = %v, want serve's error", err)
		}
		want := <-caller
		if len(ran) != 3 {
			t.Fatalf("ran %d tasks", len(ran))
		}
		for i, id := range ran {
			if id != want || order[i] != i {
				t.Fatalf("task %d ran on goroutine %d (want %d), order %v", i, id, want, order)
			}
		}
	})

	t.Run("post never blocks; a full queue drops", func(t *testing.T) {
		m := newMainThread()
		accepted := 0
		for range 10 {
			if m.post(func() {}) {
				accepted++
			}
		}
		if accepted != cap(m.tasks) {
			t.Fatalf("accepted %d, want %d", accepted, cap(m.tasks))
		}
	})
}

// goroutineID parses the current goroutine's id from its stack header.
func goroutineID() uint64 {
	buf := make([]byte, 64)
	buf = buf[:runtime.Stack(buf, false)]
	var id uint64
	fmt.Sscanf(strings.TrimPrefix(string(buf), "goroutine "), "%d", &id)
	return id
}
