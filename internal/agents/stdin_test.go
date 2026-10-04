package agents

import (
	"bytes"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// pipe returns a reader the payload reader consumes and a writer the test drives; both are
// closed at cleanup so no reading goroutine outlives the test.
func pipe(t *testing.T) (*io.PipeReader, *io.PipeWriter) {
	t.Helper()
	pr, pw := io.Pipe()
	t.Cleanup(func() {
		pw.Close()
		pr.Close()
	})
	return pr, pw
}

// readAsync runs Read on its own goroutine and returns the result and how long it took.
func readAsync(p PayloadReader) <-chan struct {
	data    []byte
	elapsed time.Duration
} {
	out := make(chan struct {
		data    []byte
		elapsed time.Duration
	}, 1)
	go func() {
		start := time.Now()
		data := p.Read()
		out <- struct {
			data    []byte
			elapsed time.Duration
		}{data, time.Since(start)}
	}()
	return out
}

// failingReader fails the test if anything reads it.
type failingReader struct{ t *testing.T }

func (f failingReader) Read([]byte) (int, error) {
	f.t.Error("stdin was read")
	return 0, io.EOF
}

// onceReader hands out its whole payload together with EOF on the first Read and counts calls.
type onceReader struct {
	data  string
	reads atomic.Int32
}

func (o *onceReader) Read(p []byte) (int, error) {
	if o.reads.Add(1) > 1 {
		return 0, io.EOF
	}
	return copy(p, o.data), io.EOF
}

func TestCLIStdin(t *testing.T) {
	t.Run("a terminal carries no payload and is not read", func(t *testing.T) {
		p := PayloadReader{In: failingReader{t}, IsTerminal: func() bool { return true }}
		if data := p.Read(); data != nil {
			t.Fatalf("got %q", data)
		}
	})

	t.Run("a missing reader carries no payload", func(t *testing.T) {
		if data := (PayloadReader{}).Read(); data != nil {
			t.Fatalf("got %q", data)
		}
	})

	t.Run("an open pipe with nothing on it does not hang", func(t *testing.T) {
		pr, _ := pipe(t)
		res := <-readAsync(PayloadReader{In: pr, FirstByteWait: 30 * time.Millisecond, Deadline: 10 * time.Second})
		if res.data != nil {
			t.Fatalf("got %q", res.data)
		}
		if res.elapsed > 5*time.Second {
			t.Fatalf("waited %v for the first byte", res.elapsed)
		}
	})

	t.Run("immediate EOF carries no payload", func(t *testing.T) {
		if data := (PayloadReader{In: strings.NewReader("")}).Read(); data != nil {
			t.Fatalf("got %q", data)
		}
	})

	t.Run("a complete object returns without waiting for EOF", func(t *testing.T) {
		pr, pw := pipe(t)
		res := readAsync(PayloadReader{In: pr, Deadline: 20 * time.Second})
		payload := `{"session_id":"s1"}`
		go pw.Write([]byte(payload)) // the writer keeps the pipe open afterwards
		select {
		case r := <-res:
			if string(r.data) != payload {
				t.Fatalf("got %q", r.data)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("waited for EOF after a complete payload")
		}
	})

	t.Run("a payload split across writes is reassembled", func(t *testing.T) {
		pr, pw := pipe(t)
		res := readAsync(PayloadReader{In: pr, FirstByteWait: 5 * time.Second, Deadline: 20 * time.Second})
		go func() {
			pw.Write([]byte(`{"session_id":`))
			time.Sleep(20 * time.Millisecond)
			pw.Write([]byte(`"split"}`))
		}()
		r := <-res
		if got, ok := SessionID(r.data); !ok || got != "split" {
			t.Fatalf("got %q from %q", got, r.data)
		}
	})

	t.Run("a payload larger than one read is reassembled", func(t *testing.T) {
		// A UserPromptSubmit payload carries the whole prompt and easily exceeds 64 KB.
		prompt := strings.Repeat("x", 300<<10)
		payload := `{"session_id":"big","prompt":"` + prompt + `"}`
		pr, pw := pipe(t)
		res := readAsync(PayloadReader{In: pr, FirstByteWait: 5 * time.Second, Deadline: 20 * time.Second})
		go func() {
			for chunk := range chunks(payload, 10000) {
				pw.Write([]byte(chunk))
			}
		}()
		r := <-res
		if string(r.data) != payload {
			t.Fatalf("got %d bytes, want %d", len(r.data), len(payload))
		}
	})

	t.Run("non-JSON is returned at EOF and rejected by the accessors", func(t *testing.T) {
		data := PayloadReader{In: strings.NewReader("hello")}.Read()
		if string(data) != "hello" {
			t.Fatalf("got %q", data)
		}
		if _, ok := SessionID(data); ok {
			t.Fatal("non-JSON accepted")
		}
	})

	t.Run("a trickling writer is cut off at the deadline", func(t *testing.T) {
		pr, pw := pipe(t)
		stop := make(chan struct{})
		defer close(stop)
		go func() {
			pw.Write([]byte(`{"session_id":"`))
			for {
				select {
				case <-stop:
					return
				case <-time.After(5 * time.Millisecond):
					if _, err := pw.Write([]byte("a")); err != nil {
						return
					}
				}
			}
		}()
		res := <-readAsync(PayloadReader{In: pr, FirstByteWait: 5 * time.Second, Deadline: 100 * time.Millisecond})
		if !bytes.HasPrefix(res.data, []byte(`{"session_id":"`)) {
			t.Fatalf("got %q", res.data)
		}
		if res.elapsed > 5*time.Second {
			t.Fatalf("read for %v past a 100ms deadline", res.elapsed)
		}
	})

	t.Run("the size is bounded", func(t *testing.T) {
		pr, pw := pipe(t)
		go func() {
			for {
				if _, err := pw.Write(bytes.Repeat([]byte("["), 1024)); err != nil {
					return
				}
			}
		}()
		res := <-readAsync(PayloadReader{In: pr, FirstByteWait: 5 * time.Second, Deadline: 20 * time.Second, MaxBytes: 8 << 10})
		if len(res.data) <= 8<<10 || len(res.data) > 8<<10+64<<10 {
			t.Fatalf("read %d bytes with an 8 KiB bound", len(res.data))
		}
	})

	t.Run("a JSON array also completes the read", func(t *testing.T) {
		pr, pw := pipe(t)
		res := readAsync(PayloadReader{In: pr, Deadline: 20 * time.Second})
		go pw.Write([]byte(` [1, 2] `))
		select {
		case r := <-res:
			if string(r.data) != ` [1, 2] ` {
				t.Fatalf("got %q", r.data)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("array did not complete the read")
		}
	})

	t.Run("stdin is read once and shared by every accessor", func(t *testing.T) {
		in := &onceReader{data: `{"session_id":"parent","agent_id":"child"}`}
		s := NewStdin(PayloadReader{In: in})
		if got, ok := s.SessionID(); !ok || got != "parent" {
			t.Fatalf("session: got %q, %v", got, ok)
		}
		if got, ok := s.AgentID(); !ok || got != "child" {
			t.Fatalf("agent: got %q, %v", got, ok)
		}
		if s.Payload() == nil {
			t.Fatal("payload lost")
		}
		if n := in.reads.Load(); n != 1 {
			t.Fatalf("stdin read %d times", n)
		}
	})

	t.Run("no payload yields no ids", func(t *testing.T) {
		s := NewStdin(PayloadReader{In: failingReader{t}, IsTerminal: func() bool { return true }})
		if _, ok := s.SessionID(); ok {
			t.Fatal("session id from a terminal")
		}
		if _, ok := s.AgentID(); ok {
			t.Fatal("agent id from a terminal")
		}
		if s.Payload() != nil {
			t.Fatal("payload from a terminal")
		}
	})
}

// chunks yields s in pieces of n bytes.
func chunks(s string, n int) func(func(string) bool) {
	return func(yield func(string) bool) {
		for len(s) > 0 {
			k := min(n, len(s))
			if !yield(s[:k]) {
				return
			}
			s = s[k:]
		}
	}
}
