package tui

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/nikitaShakhbazyan/lidwake-go/internal/ipc"
)

// recorder answers every request with resp (or err) and remembers what was asked.
type recorder struct {
	reqs []ipc.Request
	resp ipc.Response
	err  error
}

func (r *recorder) send(req ipc.Request) (ipc.Response, error) {
	r.reqs = append(r.reqs, req)
	return r.resp, r.err
}

func TestDaemonActions(t *testing.T) {
	t.Run("status returns the daemon's status", func(t *testing.T) {
		r := &recorder{resp: ipc.Response{OK: true, Status: testStatus()}}
		s, err := DaemonActions{Send: r.send}.Status()
		if err != nil || s == nil || !s.LidClosed || r.reqs[0].Op != ipc.OpStatus {
			t.Errorf("status %v, err %v, reqs %v", s, err, r.reqs)
		}
	})

	t.Run("a status reply without a status is an error", func(t *testing.T) {
		r := &recorder{resp: ipc.Response{OK: true}}
		if _, err := (DaemonActions{Send: r.send}).Status(); err == nil || err.Error() != "the daemon sent no status" {
			t.Errorf("err %v", err)
		}
	})

	t.Run("transport errors come back as they are", func(t *testing.T) {
		r := &recorder{err: ipc.ErrDaemonUnreachable}
		if _, err := (DaemonActions{Send: r.send}).Status(); !errors.Is(err, ipc.ErrDaemonUnreachable) || err.Error() != ipc.ErrDaemonUnreachable.Error() {
			t.Errorf("err %v", err)
		}
	})

	t.Run("a refusal is a RefusedError", func(t *testing.T) {
		r := &recorder{resp: ipc.Response{OK: false, Error: "nope"}}
		_, err := DaemonActions{Send: r.send}.ReleaseAll()
		if re, ok := errors.AsType[*RefusedError](err); !ok || re.Message != "nope" || re.Op != ipc.OpReleaseAll {
			t.Errorf("err %v", err)
		}
	})

	t.Run("on and off send pause and resume", func(t *testing.T) {
		r := &recorder{resp: ipc.Response{OK: true, ReleasedCount: ipc.Ptr(3)}}
		a := DaemonActions{Send: r.send}
		n, err := a.SetPaused(true)
		if err != nil || n != 3 {
			t.Errorf("pause: %d, %v", n, err)
		}
		r.resp.ReleasedCount = nil
		if n, err := a.SetPaused(false); err != nil || n != 0 {
			t.Errorf("resume: %d, %v", n, err)
		}
		if r.reqs[0].Op != ipc.OpPause || r.reqs[1].Op != ipc.OpResume {
			t.Errorf("reqs %v", r.reqs)
		}
	})

	t.Run("the timer sends seconds and reads back what was applied", func(t *testing.T) {
		r := &recorder{resp: ipc.Response{OK: true, AppliedTTL: ipc.Ptr(900.0)}}
		a := DaemonActions{Send: r.send}
		left, armed, err := a.SetTimer(15 * time.Minute)
		if err != nil || !armed || left != 15*time.Minute {
			t.Errorf("set: %v, %v, %v", left, armed, err)
		}
		if r.reqs[0].Op != ipc.OpTimer || r.reqs[0].TTL == nil || *r.reqs[0].TTL != 900 {
			t.Errorf("req %+v", r.reqs[0])
		}
		r.resp.AppliedTTL = nil
		left, armed, err = a.SetTimer(0)
		if err != nil || armed || left != 0 {
			t.Errorf("cancel: %v, %v, %v", left, armed, err)
		}
		if r.reqs[1].TTL != nil {
			t.Errorf("a cancel carried a TTL: %v", *r.reqs[1].TTL)
		}
	})

	t.Run("release all reports the count", func(t *testing.T) {
		r := &recorder{resp: ipc.Response{OK: true, ReleasedCount: ipc.Ptr(2)}}
		if n, err := (DaemonActions{Send: r.send}).ReleaseAll(); err != nil || n != 2 || r.reqs[0].Op != ipc.OpReleaseAll {
			t.Errorf("%d, %v, %v", n, err, r.reqs)
		}
	})
}

func TestRenderOnce(t *testing.T) {
	opts := Options{Width: 80, Now: func() time.Time { return testNow }}

	t.Run("prints one plain frame", func(t *testing.T) {
		var b bytes.Buffer
		if err := RenderOnce(&b, &fakeActions{status: testStatus()}, opts); err != nil {
			t.Fatal(err)
		}
		out := b.String()
		if !strings.HasSuffix(out, "\n") || !strings.Contains(out, "● ON · idle") || strings.Contains(out, "\x1b[") {
			t.Errorf("frame %q", out)
		}
		if want := strings.Join(render(testStatus()), "\n") + "\n"; out != want {
			t.Errorf("frame differs from Render:\n%s\nwant:\n%s", out, want)
		}
	})

	t.Run("a missing daemon prints its screen and returns why", func(t *testing.T) {
		var b bytes.Buffer
		err := RenderOnce(&b, &fakeActions{statusErr: ipc.ErrDaemonUnreachable}, opts)
		if !errors.Is(err, ipc.ErrDaemonUnreachable) {
			t.Errorf("err %v", err)
		}
		if !strings.Contains(b.String(), "daemon not running") || !strings.Contains(b.String(), ipc.ErrDaemonUnreachable.Error()) {
			t.Errorf("frame %q", b.String())
		}
	})

	t.Run("stats prints one frame when the output is not a terminal", func(t *testing.T) {
		dir := t.TempDir()
		in, err := os.Create(filepath.Join(dir, "in"))
		if err != nil {
			t.Fatal(err)
		}
		defer in.Close()
		for _, c := range []struct {
			actions Actions
			code    int
			want    string
		}{
			{&fakeActions{status: testStatus()}, 0, "AGENTS"},
			{&fakeActions{statusErr: ipc.ErrDaemonUnreachable}, 1, "daemon not running"},
		} {
			out, err := os.Create(filepath.Join(dir, "out"))
			if err != nil {
				t.Fatal(err)
			}
			if code := Stats(c.actions, false, in, out); code != c.code {
				t.Errorf("exit code %d, want %d", code, c.code)
			}
			out.Close()
			data, _ := os.ReadFile(out.Name())
			if !strings.Contains(string(data), c.want) || strings.Contains(string(data), "\x1b[") {
				t.Errorf("output %q", data)
			}
		}
		if IsTerminal(in.Fd()) || TerminalWidth(in.Fd()) != 80 {
			t.Error("a regular file taken for a terminal")
		}
	})
}

func TestRun(t *testing.T) {
	t.Run("a hangup quits and leaves the alternate screen", func(t *testing.T) {
		hup := make(chan os.Signal, 1)
		var out bytes.Buffer
		done := make(chan error, 1)
		go func() {
			done <- run(&fakeActions{status: testStatus()}, Options{Width: 80}, hup,
				tea.WithInput(nil), tea.WithOutput(&out), tea.WithoutSignalHandler())
		}()
		hup <- syscall.SIGHUP
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("run: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the dashboard kept running after a hangup")
		}
		if !strings.Contains(out.String(), "\x1b[?1049l") {
			t.Errorf("the alternate screen was not left: %q", out.String())
		}
	})
}
