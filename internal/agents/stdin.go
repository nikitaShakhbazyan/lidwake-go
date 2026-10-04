package agents

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// Default limits for reading a hook payload.
const (
	DefaultFirstByteWait = 100 * time.Millisecond
	DefaultReadDeadline  = 300 * time.Millisecond
	DefaultMaxPayload    = 4 << 20
)

// PayloadReader reads a hook's JSON payload without ever stalling the agent that ran the hook.
//
// The payload can arrive split across several pipe writes (a UserPromptSubmit payload carries
// the full prompt text, which easily exceeds one write or even the 64 KB pipe capacity), so one
// read isn't enough — but stdin attached to an open pipe with no hook payload (an SSH channel, a
// manual invocation) must not hang either. So: wait briefly for the first bytes, then keep
// reading until EOF, until the accumulated bytes parse as a JSON object or array (the writer may
// keep the pipe open), or until a hard deadline; the size is bounded too.
type PayloadReader struct {
	// In is the stream to read (os.Stdin in production).
	In io.Reader
	// IsTerminal reports whether In is an interactive terminal; then there is no payload and
	// nothing is read. nil means "not a terminal".
	IsTerminal func() bool
	// FirstByteWait is how long to wait for the first bytes (0: DefaultFirstByteWait).
	FirstByteWait time.Duration
	// Deadline bounds the whole read, measured from its start (0: DefaultReadDeadline).
	Deadline time.Duration
	// MaxBytes stops reading once more than this many bytes arrived (0: DefaultMaxPayload).
	MaxBytes int
}

// Read returns the payload bytes, or nil when In is a terminal or nothing arrived in time. The
// bytes are returned as read even when they are not valid JSON; the field accessors reject them.
//
// The read itself runs on a goroutine so it can be abandoned at the deadline. A reader that
// never returns (a writer holding the pipe open without sending) leaves that goroutine blocked
// until the process exits — acceptable for the short-lived CLI that uses this.
func (p PayloadReader) Read() []byte {
	if p.In == nil || (p.IsTerminal != nil && p.IsTerminal()) {
		return nil
	}
	firstWait := orDefault(p.FirstByteWait, DefaultFirstByteWait)
	deadline := time.Now().Add(orDefault(p.Deadline, DefaultReadDeadline))
	maxBytes := p.MaxBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxPayload
	}

	chunks := make(chan []byte)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		defer close(chunks)
		buf := make([]byte, 64<<10)
		for {
			n, err := p.In.Read(buf)
			if n > 0 {
				select {
				case chunks <- bytes.Clone(buf[:n]):
				case <-stop:
					return
				}
			}
			if err != nil { // EOF or a read error both end the payload
				return
			}
		}
	}()

	var data []byte
	for {
		wait := firstWait
		if len(data) > 0 {
			wait = time.Until(deadline)
		}
		chunk, ok := receive(chunks, wait)
		if !ok {
			return nilIfEmpty(data)
		}
		data = append(data, chunk...)
		if isJSONContainer(data) {
			return data
		}
		if !time.Now().Before(deadline) || len(data) > maxBytes {
			return data
		}
	}
}

// receive waits up to wait for the next chunk; ok is false on timeout or end of stream. A
// non-positive wait still takes a chunk that is already available.
func receive(ch <-chan []byte, wait time.Duration) ([]byte, bool) {
	if wait <= 0 {
		select {
		case c, ok := <-ch:
			return c, ok
		default:
			return nil, false
		}
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case c, ok := <-ch:
		return c, ok
	case <-t.C:
		return nil, false
	}
}

// isJSONContainer reports whether data is complete JSON whose top level is an object or array.
func isJSONContainer(data []byte) bool {
	trimmed := bytes.TrimLeft(data, " \t\r\n")
	if len(trimmed) == 0 || (trimmed[0] != '{' && trimmed[0] != '[') {
		return false
	}
	return json.Valid(trimmed)
}

func nilIfEmpty(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	return b
}

func orDefault(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}

// Stdin is the hook payload on the CLI's stdin, read at most once and shared by every accessor
// (stdin can only be consumed once).
//
// Reading the identity a hold is keyed on from the payload, rather than from a shell env-var
// substitution in the hook command, makes the integration immune to per-agent env-var naming
// differences — the class of bug that left agents acquiring with an empty key. The env-var
// positional argument remains the CLI's fallback for SessionID. Sub-agent hooks read AgentID:
// keying on session_id there would collide with the parent turn's hold.
type Stdin struct {
	reader PayloadReader
	once   sync.Once
	data   []byte
}

// NewStdin wraps a PayloadReader.
func NewStdin(r PayloadReader) *Stdin { return &Stdin{reader: r} }

// OSStdin reads the process's real stdin, treating a terminal as "no payload".
func OSStdin() *Stdin {
	return NewStdin(PayloadReader{
		In: os.Stdin,
		// Probe fd 0 directly: os.Stdin.Fd() would switch a non-blocking descriptor to
		// blocking mode, a flag shared with whoever else holds that pipe end.
		IsTerminal: func() bool {
			_, err := unix.IoctlGetTermios(unix.Stdin, unix.TIOCGETA)
			return err == nil
		},
	})
}

// Payload is the raw payload bytes, or nil when stdin is a terminal or carries nothing. Used by
// `acquire --if-background`, which inspects tool_input rather than an id field.
func (s *Stdin) Payload() []byte {
	s.once.Do(func() { s.data = s.reader.Read() })
	return s.data
}

// SessionID is the parent session_id from the payload; ok is false when stdin is a terminal,
// carries nothing, or isn't the expected JSON.
func (s *Stdin) SessionID() (string, bool) { return SessionID(s.Payload()) }

// AgentID is a sub-agent's own agent_id from a SubagentStart/SubagentStop payload; ok is false
// when it is absent or empty or stdin carries no JSON. No agent exposes the sub-agent id as an
// env var, so `--subagent` hooks depend entirely on this.
func (s *Stdin) AgentID() (string, bool) { return AgentID(s.Payload()) }
