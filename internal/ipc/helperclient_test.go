package ipc

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// shortDir is a temp dir short enough for a Unix socket path (104 bytes on macOS).
func shortDir(t *testing.T) string {
	dir, err := os.MkdirTemp("/tmp", "lw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// A helper that accepts but never answers: Connected and Close must not wait behind the call.
func TestHelperClientCloseDoesNotWaitForACallInFlight(t *testing.T) {
	sock := filepath.Join(shortDir(t), "h.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close() // hold it open, never reply
		}
	}()

	c := NewHelperClient(sock)
	done := make(chan error, 1)
	go func() {
		_, err := c.SetBlocked(true)
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)

	start := time.Now()
	if !c.Connected() {
		t.Fatal("Connected = false while a call is in flight")
	}
	c.Close()
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Connected+Close took %v behind the call in flight", d)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the interrupted call reported success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not fail the call in flight")
	}
}

func TestHelperClientUnreachable(t *testing.T) {
	c := NewHelperClient(filepath.Join(t.TempDir(), "missing.sock"))
	if _, err := c.SetBlocked(true); err != ErrHelperUnreachable {
		t.Fatalf("err = %v, want ErrHelperUnreachable", err)
	}
	if c.Connected() {
		t.Fatal("Connected after a failed dial")
	}
}
