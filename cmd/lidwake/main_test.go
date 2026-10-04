package main

import (
	"context"
	"errors"
	"syscall"
	"testing"
	"time"
)

func TestServe(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT} {
		t.Run("stops cleanly on "+sig.String(), func(t *testing.T) {
			code := serve("test", func(ctx context.Context) error {
				// The handler is installed before run starts, so the signal reaches the context
				// instead of killing the test process.
				if err := syscall.Kill(syscall.Getpid(), sig); err != nil {
					return err
				}
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(5 * time.Second):
					return errors.New("context not cancelled")
				}
			})
			if code != 0 {
				t.Fatalf("exit code %d", code)
			}
		})
	}

	t.Run("a failed job exits 1", func(t *testing.T) {
		if code := serve("test", func(context.Context) error { return errors.New("boom") }); code != 1 {
			t.Fatalf("exit code %d", code)
		}
	})
}
