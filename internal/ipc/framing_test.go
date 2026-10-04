package ipc

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/nikitaShakhbazyan/lidwake-go/internal/model"
)

// roundtrip writes v as a frame and decodes it back into out.
func roundtrip(t *testing.T, v, out any) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := WriteFrame(&buf, v); err != nil {
		t.Fatalf("write: %v", err)
	}
	frame := append([]byte(nil), buf.Bytes()...)
	if err := ReadFrame(&buf, out); err != nil {
		t.Fatalf("read: %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("%d bytes left after one frame", buf.Len())
	}
	return frame
}

func header(n uint32) []byte {
	var h [4]byte
	binary.BigEndian.PutUint32(h[:], n)
	return h[:]
}

func marshal(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestCLIFraming(t *testing.T) {
	t.Run("roundtrip request", func(t *testing.T) {
		req := Request{Op: OpAcquire, Key: "claude-code:abc", Tool: "claude-code", Reason: "tests", PID: 1234, ProcessName: "claude", TTL: Ptr(60.0)}
		var got Request
		roundtrip(t, req, &got)
		if got.Op != OpAcquire || got.Key != req.Key || got.Tool != req.Tool || got.Reason != req.Reason ||
			got.PID != req.PID || got.ProcessName != req.ProcessName || got.TTL == nil || *got.TTL != 60 {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("roundtrip response", func(t *testing.T) {
		resp := Response{OK: true, Blocking: Ptr(true), AssertionCount: Ptr(3), Status: &model.Status{Version: "1.2.3", Paused: true}}
		var got Response
		roundtrip(t, resp, &got)
		if !got.OK || got.AssertionCount == nil || *got.AssertionCount != 3 || got.Blocking == nil || !*got.Blocking {
			t.Fatalf("got %+v", got)
		}
		if got.Status == nil || got.Status.Version != "1.2.3" || !got.Status.Paused {
			t.Fatalf("status %+v", got.Status)
		}
	})

	// The Go wire keys: the TTL travels as "ttl" (seconds), the blocking state as "blocking".
	t.Run("wire keys match spec", func(t *testing.T) {
		reqJSON := marshal(t, Request{Op: OpAcquire, Key: "k", Tool: "t", PID: 1, TTL: Ptr(30.0)})
		if !strings.Contains(reqJSON, `"ttl":30`) || strings.Contains(reqJSON, "ttlSeconds") {
			t.Errorf("request %s", reqJSON)
		}
		respJSON := marshal(t, Response{OK: true, Blocking: Ptr(true), AssertionCount: Ptr(1), Warning: "w"})
		if !strings.Contains(respJSON, `"blocking":true`) || !strings.Contains(respJSON, `"warning":"w"`) {
			t.Errorf("response %s", respJSON)
		}
		if strings.Contains(respJSON, "blockingState") {
			t.Errorf("response %s", respJSON)
		}
	})

	// Display-class version skew, both directions. Old CLI → new daemon: a frame without the
	// display field decodes as false (system only, nothing changes). New CLI → old daemon: a
	// reply without displayApplied decodes as nil — the CLI's cue to warn that the display is NOT
	// protected instead of trusting a bare ok.
	t.Run("display field skews tolerantly in both directions", func(t *testing.T) {
		var old Request
		if err := json.Unmarshal([]byte(`{"op": "hold", "reason": "screen work"}`), &old); err != nil {
			t.Fatal(err)
		}
		if old.Display {
			t.Error("absent display decoded as true")
		}
		var decodedReq Request
		roundtrip(t, Request{Op: OpHold, Tool: "rocuronium", Reason: "screen work", Display: true}, &decodedReq)
		if !decodedReq.Display {
			t.Error("display lost in transit")
		}
		var oldReply Response
		if err := json.Unmarshal([]byte(`{"ok": true, "holdKey": "hold:ab12cd34"}`), &oldReply); err != nil {
			t.Fatal(err)
		}
		if oldReply.DisplayApplied != nil {
			t.Error("absent displayApplied decoded as set")
		}
		var decoded Response
		roundtrip(t, Response{OK: true, Blocking: Ptr(true), AssertionCount: Ptr(1), HoldKey: "hold:ab12cd34", DisplayApplied: Ptr(true)}, &decoded)
		if decoded.DisplayApplied == nil || !*decoded.DisplayApplied || decoded.HoldKey != "hold:ab12cd34" {
			t.Errorf("got %+v", decoded)
		}
	})

	t.Run("release all roundtrips with released count", func(t *testing.T) {
		var req Request
		roundtrip(t, Request{Op: OpReleaseAll}, &req)
		if req.Op != OpReleaseAll {
			t.Fatalf("op %q", req.Op)
		}
		var resp Response
		roundtrip(t, Response{OK: true, Blocking: Ptr(false), AssertionCount: Ptr(0), ReleasedCount: Ptr(4)}, &resp)
		if resp.ReleasedCount == nil || *resp.ReleasedCount != 4 {
			t.Fatalf("released %v", resp.ReleasedCount)
		}
		// assertionCount stays "active now", not "released" — the wire meaning is invariant.
		if resp.AssertionCount == nil || *resp.AssertionCount != 0 {
			t.Fatalf("assertionCount %v", resp.AssertionCount)
		}
	})

	t.Run("response warning roundtrips", func(t *testing.T) {
		var got Response
		roundtrip(t, Response{OK: true, Blocking: Ptr(false), AssertionCount: Ptr(0), Warning: "unknown key"}, &got)
		if got.Warning != "unknown key" {
			t.Errorf("warning %q", got.Warning)
		}
		// A false blocking state is sent, not omitted.
		if got.Blocking == nil || *got.Blocking {
			t.Errorf("blocking %v", got.Blocking)
		}
	})

	t.Run("frame prefix is big endian length", func(t *testing.T) {
		var buf bytes.Buffer
		if err := WriteFrame(&buf, Request{Op: OpPing}); err != nil {
			t.Fatal(err)
		}
		frame := buf.Bytes()
		if len(frame) < 4 {
			t.Fatalf("frame of %d bytes", len(frame))
		}
		if n := binary.BigEndian.Uint32(frame[:4]); int(n) != len(frame)-4 {
			t.Fatalf("prefix %d, body %d", n, len(frame)-4)
		}
		if !json.Valid(frame[4:]) {
			t.Fatalf("body %q is not JSON", frame[4:])
		}
	})

	t.Run("read frame rejects oversize", func(t *testing.T) {
		// A frame claiming 32 MB must be refused before anything is allocated or read.
		var v Request
		if err := ReadFrame(bytes.NewReader(header(32<<20)), &v); !errors.Is(err, ErrFrameTooLarge) {
			t.Fatalf("err %v", err)
		}
	})

	t.Run("read frame rejects short length", func(t *testing.T) {
		var v Request
		err := ReadFrame(bytes.NewReader([]byte{0, 0}), &v) // only 2 bytes; the length needs 4
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("err %v", err)
		}
		if err := ReadFrame(bytes.NewReader(nil), &v); !errors.Is(err, io.EOF) {
			t.Fatalf("empty stream: err %v", err)
		}
	})

	t.Run("read frame rejects short body", func(t *testing.T) {
		var v Request
		frame := append(header(10), []byte(`{"op"`)...)
		if err := ReadFrame(bytes.NewReader(frame), &v); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("err %v", err)
		}
	})

	t.Run("read frame rejects a body that is not JSON", func(t *testing.T) {
		var v Request
		body := []byte("not json")
		if err := ReadFrame(bytes.NewReader(append(header(uint32(len(body))), body...)), &v); err == nil {
			t.Fatal("decoded garbage")
		}
	})

	t.Run("decodes ttl from external JSON", func(t *testing.T) {
		// An external tool writing the documented wire shape uses "ttl" in seconds.
		var req Request
		if err := json.Unmarshal([]byte(`{"op":"acquire","key":"k","tool":"t","ttl":45}`), &req); err != nil {
			t.Fatal(err)
		}
		if req.TTL == nil || *req.TTL != 45 || req.Op != OpAcquire {
			t.Fatalf("got %+v", req)
		}
	})

	t.Run("response decodes when warning absent", func(t *testing.T) {
		// A response from an older daemon has no "warning" key.
		var resp Response
		if err := json.Unmarshal([]byte(`{"ok":true,"blocking":true,"assertionCount":2}`), &resp); err != nil {
			t.Fatal(err)
		}
		if !resp.OK || resp.Blocking == nil || !*resp.Blocking || resp.Warning != "" {
			t.Fatalf("got %+v", resp)
		}
	})

	// The Go limit is MaxFrame inclusive: a length prefix of exactly MaxFrame is allowed (the
	// writer produces such frames), and the first length past it is refused.
	t.Run("read frame rejects exactly at size limit", func(t *testing.T) {
		var v Request
		if err := ReadFrame(bytes.NewReader(header(MaxFrame+1)), &v); !errors.Is(err, ErrFrameTooLarge) {
			t.Fatalf("MaxFrame+1: err %v", err)
		}
		body := append([]byte(`{"op":"ping","reason":"`), bytes.Repeat([]byte("r"), MaxFrame)...)
		body = append(body[:MaxFrame-2], '"', '}')
		if err := ReadFrame(bytes.NewReader(append(header(MaxFrame), body...)), &v); err != nil {
			t.Fatalf("MaxFrame: err %v", err)
		}
		if v.Op != OpPing {
			t.Fatalf("op %q", v.Op)
		}
	})

	t.Run("write frame refuses a body over the limit", func(t *testing.T) {
		var buf bytes.Buffer
		err := WriteFrame(&buf, Request{Op: OpAcquire, Reason: strings.Repeat("r", MaxFrame)})
		if !errors.Is(err, ErrFrameTooLarge) {
			t.Fatalf("err %v", err)
		}
		if buf.Len() != 0 {
			t.Fatalf("wrote %d bytes of a refused frame", buf.Len())
		}
	})

	t.Run("request omits nil fields on wire", func(t *testing.T) {
		js := marshal(t, Request{Op: OpRelease, Key: "k"})
		if !strings.Contains(js, `"key"`) || strings.Contains(js, `"tool"`) || strings.Contains(js, `"ttl"`) {
			t.Fatalf("got %s", js)
		}
	})

	t.Run("frames on one stream are read back in order", func(t *testing.T) {
		var buf bytes.Buffer
		for _, op := range []Op{OpPing, OpStatus, OpPause} {
			if err := WriteFrame(&buf, Request{Op: op}); err != nil {
				t.Fatal(err)
			}
		}
		for _, want := range []Op{OpPing, OpStatus, OpPause} {
			var got Request
			if err := ReadFrame(&buf, &got); err != nil || got.Op != want {
				t.Fatalf("got %q (%v), want %q", got.Op, err, want)
			}
		}
	})

	t.Run("helper messages roundtrip", func(t *testing.T) {
		var req HelperRequest
		roundtrip(t, HelperRequest{Op: HelperSet, Blocked: true}, &req)
		if req.Op != HelperSet || !req.Blocked {
			t.Fatalf("got %+v", req)
		}
		// blocked=false is always on the wire in a response: the helper's state is never implied.
		js := marshal(t, HelperResponse{OK: true, Blocked: false})
		if !strings.Contains(js, `"blocked":false`) {
			t.Fatalf("got %s", js)
		}
		var resp HelperResponse
		roundtrip(t, HelperResponse{OK: false, Error: "pmset failed", Blocked: true, Version: "1.0"}, &resp)
		if resp.OK || resp.Error != "pmset failed" || !resp.Blocked || resp.Version != "1.0" {
			t.Fatalf("got %+v", resp)
		}
	})
}
