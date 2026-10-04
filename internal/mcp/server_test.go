package mcp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nikitaShakhbazyan/lidwake-go/internal/ipc"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/model"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/paths"
)

var testNow = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

type fakeDaemon struct {
	mu       sync.Mutex
	requests []ipc.Request
	reply    func(ipc.Request) (ipc.Response, error)
}

func (f *fakeDaemon) send(req ipc.Request) (ipc.Response, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.mu.Unlock()
	if f.reply == nil {
		return ipc.Response{OK: true}, nil
	}
	return f.reply(req)
}

func reply(resp ipc.Response, err error) func(ipc.Request) (ipc.Response, error) {
	return func(ipc.Request) (ipc.Response, error) { return resp, err }
}

// message is a decoded JSON-RPC response line.
type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type toolResultMsg struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	IsError bool `json:"isError"`
}

// serve feeds input to a server and returns every response line, decoded.
func serve(t *testing.T, daemon *fakeDaemon, input string) []message {
	t.Helper()
	var out bytes.Buffer
	srv := &Server{Tool: "claude-code", Send: daemon.send, Now: func() time.Time { return testNow }}
	if err := srv.Serve(strings.NewReader(input), &out); err != nil {
		t.Fatalf("serve: %v", err)
	}
	var msgs []message
	for _, line := range strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		var m message
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("response %q: %v", line, err)
		}
		if m.JSONRPC != "2.0" {
			t.Fatalf("jsonrpc %q in %q", m.JSONRPC, line)
		}
		msgs = append(msgs, m)
	}
	return msgs
}

func one(t *testing.T, msgs []message) message {
	t.Helper()
	if len(msgs) != 1 {
		t.Fatalf("%d responses, want 1: %+v", len(msgs), msgs)
	}
	return msgs[0]
}

func call(t *testing.T, daemon *fakeDaemon, tool string, args string) toolResultMsg {
	t.Helper()
	m := one(t, serve(t, daemon, `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"`+tool+`","arguments":`+args+`}}`+"\n"))
	if m.Error != nil {
		t.Fatalf("error %+v", m.Error)
	}
	var r toolResultMsg
	if err := json.Unmarshal(m.Result, &r); err != nil {
		t.Fatal(err)
	}
	if len(r.Content) != 1 || r.Content[0].Type != "text" {
		t.Fatalf("content %+v", r.Content)
	}
	return r
}

func expectText(t *testing.T, r toolResultMsg, text string, isError bool) {
	t.Helper()
	if r.Content[0].Text != text || r.IsError != isError {
		t.Fatalf("got %q (isError %v)\nwant %q (isError %v)", r.Content[0].Text, r.IsError, text, isError)
	}
}

func TestMCPProtocol(t *testing.T) {
	t.Run("initialize echoes a supported revision", func(t *testing.T) {
		for requested, want := range map[string]string{
			`"2025-03-26"`: "2025-03-26",
			`"2024-11-05"`: "2024-11-05",
			`"2099-01-01"`: DefaultProtocolVersion,
			`7`:            DefaultProtocolVersion,
		} {
			m := one(t, serve(t, &fakeDaemon{}, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":`+requested+`}}`+"\n"))
			var r struct {
				ProtocolVersion string                     `json:"protocolVersion"`
				Capabilities    map[string]json.RawMessage `json:"capabilities"`
				ServerInfo      map[string]string          `json:"serverInfo"`
			}
			if err := json.Unmarshal(m.Result, &r); err != nil {
				t.Fatal(err)
			}
			if r.ProtocolVersion != want || string(r.Capabilities["tools"]) != "{}" ||
				r.ServerInfo["name"] != "lidwake" || r.ServerInfo["version"] != paths.Version || string(m.ID) != "1" {
				t.Errorf("%s: %s", requested, m.Result)
			}
		}
		m := one(t, serve(t, &fakeDaemon{}, `{"jsonrpc":"2.0","id":1,"method":"initialize"}`+"\n"))
		if !strings.Contains(string(m.Result), `"protocolVersion":"`+DefaultProtocolVersion+`"`) {
			t.Errorf("no params: %s", m.Result)
		}
	})

	t.Run("notifications get no response", func(t *testing.T) {
		msgs := serve(t, &fakeDaemon{}, `{"jsonrpc":"2.0","method":"notifications/initialized"}`+"\n"+
			`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":3}}`+"\n"+
			`{"jsonrpc":"2.0","method":"notifications/unknown"}`+"\n")
		if len(msgs) != 0 {
			t.Fatalf("responses %+v", msgs)
		}
	})

	t.Run("ping", func(t *testing.T) {
		m := one(t, serve(t, &fakeDaemon{}, `{"jsonrpc":"2.0","id":"p-1","method":"ping"}`+"\n"))
		if string(m.Result) != "{}" || string(m.ID) != `"p-1"` {
			t.Fatalf("got %+v", m)
		}
	})

	t.Run("ids are echoed verbatim", func(t *testing.T) {
		for _, id := range []string{`7`, `"abc"`, `1.5`, `null`} {
			m := one(t, serve(t, &fakeDaemon{}, `{"jsonrpc":"2.0","id":`+id+`,"method":"ping"}`+"\n"))
			if string(m.ID) != id {
				t.Errorf("id %s echoed as %s", id, m.ID)
			}
		}
		// A request without an id is still answered for known methods, with a null id.
		m := one(t, serve(t, &fakeDaemon{}, `{"jsonrpc":"2.0","method":"ping"}`+"\n"))
		if string(m.ID) != "null" {
			t.Errorf("id %s", m.ID)
		}
	})

	t.Run("unknown methods", func(t *testing.T) {
		m := one(t, serve(t, &fakeDaemon{}, `{"jsonrpc":"2.0","id":4,"method":"resources/list"}`+"\n"))
		if m.Error == nil || m.Error.Code != -32601 || m.Error.Message != "Method not found: resources/list" || string(m.ID) != "4" {
			t.Fatalf("got %+v", m)
		}
		m = one(t, serve(t, &fakeDaemon{}, `{"jsonrpc":"2.0","id":null,"method":"nope"}`+"\n"))
		if m.Error == nil || m.Error.Code != -32601 || string(m.ID) != "null" {
			t.Fatalf("null id: %+v", m)
		}
		if msgs := serve(t, &fakeDaemon{}, `{"jsonrpc":"2.0","method":"nope"}`+"\n"); len(msgs) != 0 {
			t.Fatalf("notification answered: %+v", msgs)
		}
	})

	t.Run("malformed input", func(t *testing.T) {
		cases := []struct {
			line string
			code int
			msg  string
		}{
			{`{"jsonrpc":`, -32700, "Parse error"},
			{`not json`, -32700, "Parse error"},
			{`42`, -32700, "Parse error"},
			{`"text"`, -32700, "Parse error"},
			{`   `, -32700, "Parse error"},
			{`[{"jsonrpc":"2.0","id":1,"method":"ping"}]`, -32600, "Invalid request: expected a JSON-RPC object"},
		}
		for _, tc := range cases {
			m := one(t, serve(t, &fakeDaemon{}, tc.line+"\n"))
			if m.Error == nil || m.Error.Code != tc.code || m.Error.Message != tc.msg || string(m.ID) != "null" {
				t.Errorf("%q: %+v", tc.line, m)
			}
		}
		// A message without a string method is dropped.
		if msgs := serve(t, &fakeDaemon{}, `{"jsonrpc":"2.0","id":1}`+"\n"+`{"jsonrpc":"2.0","id":1,"method":5}`+"\n"+
			`{"jsonrpc":"2.0","id":1,"method":null}`+"\n"); len(msgs) != 0 {
			t.Fatalf("answered %+v", msgs)
		}
	})

	t.Run("empty lines are skipped and CRLF and a missing final newline are accepted", func(t *testing.T) {
		msgs := serve(t, &fakeDaemon{}, "\n\r\n"+`{"jsonrpc":"2.0","id":1,"method":"ping"}`+"\r\n\n"+`{"jsonrpc":"2.0","id":2,"method":"ping"}`)
		if len(msgs) != 2 || string(msgs[0].ID) != "1" || string(msgs[1].ID) != "2" {
			t.Fatalf("got %+v", msgs)
		}
	})

	t.Run("tools list", func(t *testing.T) {
		m := one(t, serve(t, &fakeDaemon{}, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`+"\n"))
		var r struct {
			Tools []struct {
				Name        string `json:"name"`
				Description string `json:"description"`
				InputSchema struct {
					Type       string                       `json:"type"`
					Properties map[string]map[string]string `json:"properties"`
					Required   []string                     `json:"required"`
				} `json:"inputSchema"`
			} `json:"tools"`
		}
		if err := json.Unmarshal(m.Result, &r); err != nil {
			t.Fatal(err)
		}
		if len(r.Tools) != 3 {
			t.Fatalf("tools %+v", r.Tools)
		}
		want := []struct {
			name     string
			props    map[string]string
			required []string
		}{
			{"keep_awake", map[string]string{"reason": "string", "minutes": "number", "pid": "integer", "keep_display_awake": "boolean"}, []string{"reason"}},
			{"release_awake", map[string]string{"hold_id": "string"}, []string{"hold_id"}},
			{"awake_status", map[string]string{}, nil},
		}
		for i, w := range want {
			tool := r.Tools[i]
			if tool.Name != w.name || tool.Description == "" || tool.InputSchema.Type != "object" {
				t.Errorf("tool %d: %+v", i, tool)
			}
			if len(tool.InputSchema.Properties) != len(w.props) {
				t.Errorf("%s properties %v", w.name, tool.InputSchema.Properties)
			}
			for prop, typ := range w.props {
				if p := tool.InputSchema.Properties[prop]; p["type"] != typ || p["description"] == "" {
					t.Errorf("%s.%s = %v", w.name, prop, p)
				}
			}
			if strings.Join(tool.InputSchema.Required, ",") != strings.Join(w.required, ",") {
				t.Errorf("%s required %v", w.name, tool.InputSchema.Required)
			}
		}
		if !strings.Contains(string(m.Result), `"awake_status","description":"Report whether the Mac is currently being kept awake, and by what (active agents and holds, with their reasons and time remaining).","inputSchema":{"type":"object","properties":{}}}`) {
			t.Errorf("awake_status schema: %s", m.Result)
		}
		if !strings.Contains(string(m.Result), "unlike `caffeinate`, which only prevents idle sleep") {
			t.Errorf("keep_awake description: %s", m.Result)
		}
	})

	t.Run("tools call needs a name", func(t *testing.T) {
		for _, params := range []string{`{}`, `{"name":3}`, `"x"`} {
			m := one(t, serve(t, &fakeDaemon{}, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":`+params+`}`+"\n"))
			if m.Error == nil || m.Error.Code != -32602 || m.Error.Message != "tools/call requires a tool name" {
				t.Errorf("%s: %+v", params, m)
			}
		}
	})

	t.Run("unknown tool", func(t *testing.T) {
		expectText(t, call(t, &fakeDaemon{}, "sleep_now", `{}`), "Unknown tool: sleep_now", true)
	})

	// The server answers each message as it arrives, not at end of input.
	t.Run("answers over a live pipe", func(t *testing.T) {
		inR, inW := io.Pipe()
		outR, outW := io.Pipe()
		srv := &Server{Send: (&fakeDaemon{}).send}
		done := make(chan error, 1)
		go func() { done <- srv.Serve(inR, outW); outW.Close() }()
		lines := bufio.NewReader(outR)
		for i, id := range []string{"1", "2"} {
			if _, err := io.WriteString(inW, `{"jsonrpc":"2.0","id":`+id+`,"method":"ping"}`+"\n"); err != nil {
				t.Fatal(err)
			}
			line, err := lines.ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			if want := `{"jsonrpc":"2.0","id":` + id + `,"result":{}}` + "\n"; line != want {
				t.Fatalf("response %d %q, want %q", i, line, want)
			}
		}
		inW.Close()
		if err := <-done; err != nil {
			t.Fatalf("serve: %v", err)
		}
	})

	t.Run("a closed output does not stop the server", func(t *testing.T) {
		srv := &Server{Send: (&fakeDaemon{}).send}
		input := strings.Repeat(`{"jsonrpc":"2.0","id":1,"method":"ping"}`+"\n", 3)
		if err := srv.Serve(strings.NewReader(input), failingWriter{}); err != nil {
			t.Fatalf("serve: %v", err)
		}
	})

	t.Run("a read error is returned", func(t *testing.T) {
		srv := &Server{Send: (&fakeDaemon{}).send}
		if err := srv.Serve(io.MultiReader(strings.NewReader("{"), errReader{}), io.Discard); err == nil {
			t.Fatal("no error")
		}
	})

	t.Run("logs readiness with the tool label", func(t *testing.T) {
		var log bytes.Buffer
		srv := &Server{Log: &log}
		_ = srv.Serve(strings.NewReader(""), io.Discard)
		if log.String() != "[lidwake mcp] lidwake mcp server ready (tool=manual)\n" {
			t.Fatalf("log %q", log.String())
		}
	})
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }

func TestMCPKeepAwake(t *testing.T) {
	t.Run("places a hold labelled with the tool", func(t *testing.T) {
		d := &fakeDaemon{reply: reply(ipc.Response{OK: true, HoldKey: "hold:ab12cd34", AppliedTTL: ipc.Ptr(1800.0)}, nil)}
		r := call(t, d, "keep_awake", `{"reason":"running migration","minutes":30}`)
		expectText(t, r, "Keeping the Mac awake, including with the lid closed (hold id: hold:ab12cd34). Expires in about 30 min — call release_awake with this id when the task finishes.", false)
		if len(d.requests) != 1 {
			t.Fatalf("requests %+v", d.requests)
		}
		req := d.requests[0]
		if req.Op != ipc.OpHold || req.Tool != "claude-code" || req.ProcessName != "claude-code" || req.Reason != "running migration" ||
			req.TTL == nil || *req.TTL != 1800 || req.PID != 0 || req.Display || req.Key != "" {
			t.Fatalf("request %+v", req)
		}
	})

	t.Run("reports the clamped ttl, then the requested one, then the default", func(t *testing.T) {
		d := &fakeDaemon{reply: reply(ipc.Response{OK: true, HoldKey: "hold:1", AppliedTTL: ipc.Ptr(4 * 3600.0)}, nil)}
		expectText(t, call(t, d, "keep_awake", `{"reason":"train","minutes":600}`),
			"Keeping the Mac awake, including with the lid closed (hold id: hold:1). Expires in about 240 min — call release_awake with this id when the task finishes.", false)
		d = &fakeDaemon{reply: reply(ipc.Response{OK: true, HoldKey: "hold:1"}, nil)}
		expectText(t, call(t, d, "keep_awake", `{"reason":"train","minutes":2.5}`),
			"Keeping the Mac awake, including with the lid closed (hold id: hold:1). Expires in about 3 min — call release_awake with this id when the task finishes.", false)
		d = &fakeDaemon{reply: reply(ipc.Response{OK: true, HoldKey: "hold:1"}, nil)}
		expectText(t, call(t, d, "keep_awake", `{"reason":"train"}`),
			"Keeping the Mac awake, including with the lid closed (hold id: hold:1). Expires in about 60 min — call release_awake with this id when the task finishes.", false)
		if d.requests[0].TTL != nil {
			t.Errorf("ttl %v", *d.requests[0].TTL)
		}
	})

	t.Run("a pid ties the hold to the process", func(t *testing.T) {
		d := &fakeDaemon{reply: reply(ipc.Response{OK: true, HoldKey: "hold:1", AppliedTTL: ipc.Ptr(3600.0)}, nil)}
		expectText(t, call(t, d, "keep_awake", `{"reason":"build","pid":4321}`),
			"Keeping the Mac awake, including with the lid closed (hold id: hold:1). Releases when process 4321 exits, or in about 60 min — call release_awake with this id when the task finishes.", false)
		if d.requests[0].PID != 4321 {
			t.Errorf("pid %d", d.requests[0].PID)
		}
	})

	t.Run("only an exact in-range positive pid is used", func(t *testing.T) {
		for _, pid := range []string{`12.5`, `0`, `-4`, `4294967297`, `"123"`, `true`, `null`} {
			d := &fakeDaemon{reply: reply(ipc.Response{OK: true, HoldKey: "hold:1"}, nil)}
			r := call(t, d, "keep_awake", `{"reason":"build","pid":`+pid+`}`)
			if d.requests[0].PID != 0 || strings.Contains(r.Content[0].Text, "Releases when process") {
				t.Errorf("pid %s: request %+v, text %q", pid, d.requests[0], r.Content[0].Text)
			}
		}
		d := &fakeDaemon{reply: reply(ipc.Response{OK: true, HoldKey: "hold:1"}, nil)}
		call(t, d, "keep_awake", `{"reason":"build","pid":77.0}`)
		if d.requests[0].PID != 77 {
			t.Errorf("integral float pid: %d", d.requests[0].PID)
		}
	})

	t.Run("minutes must be a number", func(t *testing.T) {
		for _, minutes := range []string{`"30"`, `true`, `null`, `1e308`} {
			d := &fakeDaemon{reply: reply(ipc.Response{OK: true, HoldKey: "hold:1"}, nil)}
			call(t, d, "keep_awake", `{"reason":"build","minutes":`+minutes+`}`)
			if d.requests[0].TTL != nil {
				t.Errorf("minutes %s: ttl %v", minutes, *d.requests[0].TTL)
			}
		}
	})

	t.Run("display holds and version skew", func(t *testing.T) {
		d := &fakeDaemon{reply: reply(ipc.Response{OK: true, HoldKey: "hold:1", DisplayApplied: ipc.Ptr(true), AppliedTTL: ipc.Ptr(600.0)}, nil)}
		expectText(t, call(t, d, "keep_awake", `{"reason":"screen","keep_display_awake":true}`),
			"Keeping the Mac awake, including with the lid closed (hold id: hold:1). The display is held awake too. Expires in about 10 min — call release_awake with this id when the task finishes.", false)
		if !d.requests[0].Display {
			t.Error("display not requested")
		}
		d = &fakeDaemon{reply: reply(ipc.Response{OK: true, HoldKey: "hold:1", AppliedTTL: ipc.Ptr(600.0)}, nil)}
		expectText(t, call(t, d, "keep_awake", `{"reason":"screen","keep_display_awake":true}`),
			"Keeping the Mac awake, including with the lid closed (hold id: hold:1). WARNING: the running lidwake daemon predates display holds — the display is NOT being kept awake. Expires in about 10 min — call release_awake with this id when the task finishes.", false)
		d = &fakeDaemon{reply: reply(ipc.Response{OK: true, HoldKey: "hold:1"}, nil)}
		call(t, d, "keep_awake", `{"reason":"screen","keep_display_awake":"yes"}`)
		if d.requests[0].Display {
			t.Error("a non-boolean asked for the display")
		}
	})

	t.Run("needs a reason", func(t *testing.T) {
		for _, args := range []string{`{}`, `{"reason":""}`, `{"reason":5}`, `[]`} {
			d := &fakeDaemon{}
			expectText(t, call(t, d, "keep_awake", args), "keep_awake requires a 'reason'.", true)
			if len(d.requests) != 0 {
				t.Errorf("%s: sent %+v", args, d.requests)
			}
		}
	})

	t.Run("failures", func(t *testing.T) {
		expectText(t, call(t, &fakeDaemon{reply: reply(ipc.Response{OK: false, Error: "agent holds are disabled in settings"}, nil)}, "keep_awake", `{"reason":"x"}`),
			"agent holds are disabled in settings", true)
		expectText(t, call(t, &fakeDaemon{reply: reply(ipc.Response{OK: false}, nil)}, "keep_awake", `{"reason":"x"}`),
			"Could not place the hold.", true)
		expectText(t, call(t, &fakeDaemon{reply: reply(ipc.Response{OK: true}, nil)}, "keep_awake", `{"reason":"x"}`),
			"Could not place the hold.", true)
		expectText(t, call(t, &fakeDaemon{reply: reply(ipc.Response{}, ipc.ErrDaemonUnreachable)}, "keep_awake", `{"reason":"x"}`),
			"The lidwake daemon isn't reachable, so the hold was not placed.", true)
	})
}

func TestMCPReleaseAwake(t *testing.T) {
	t.Run("releases the hold", func(t *testing.T) {
		d := &fakeDaemon{}
		expectText(t, call(t, d, "release_awake", `{"hold_id":"hold:ab12cd34"}`),
			"Released hold:ab12cd34. The Mac can sleep normally once nothing else is holding it awake.", false)
		if req := d.requests[0]; req.Op != ipc.OpRelease || req.Key != "hold:ab12cd34" || req.Tool != "mcp" {
			t.Fatalf("request %+v", req)
		}
	})

	t.Run("nothing matched is not an error", func(t *testing.T) {
		d := &fakeDaemon{reply: reply(ipc.Response{OK: true, Warning: "no assertion with key 'hold:x'"}, nil)}
		expectText(t, call(t, d, "release_awake", `{"hold_id":"hold:x"}`), "Nothing to release — no assertion with key 'hold:x'", false)
	})

	t.Run("needs a hold id", func(t *testing.T) {
		for _, args := range []string{`{}`, `{"hold_id":""}`, `{"hold_id":7}`} {
			d := &fakeDaemon{}
			expectText(t, call(t, d, "release_awake", args), "release_awake requires a 'hold_id'.", true)
			if len(d.requests) != 0 {
				t.Errorf("%s: sent %+v", args, d.requests)
			}
		}
	})

	t.Run("daemon unreachable", func(t *testing.T) {
		expectText(t, call(t, &fakeDaemon{reply: reply(ipc.Response{}, errors.New("boom"))}, "release_awake", `{"hold_id":"hold:1"}`),
			"The lidwake daemon isn't reachable.", true)
	})
}

func TestMCPAwakeStatus(t *testing.T) {
	status := func(s *model.Status) *fakeDaemon {
		return &fakeDaemon{reply: reply(ipc.Response{OK: true, Status: s}, nil)}
	}
	exp := func(d time.Duration) *time.Time { at := testNow.Add(d); return &at }

	t.Run("paused", func(t *testing.T) {
		expectText(t, call(t, status(&model.Status{Paused: true, Assertions: []model.Assertion{{Key: "a:1", Tool: "a"}}}), "awake_status", `{}`),
			"lidwake is paused — the Mac will sleep normally regardless of agents or holds.", false)
	})

	t.Run("nothing held", func(t *testing.T) {
		expectText(t, call(t, status(&model.Status{}), "awake_status", `{}`),
			"The Mac is sleeping normally — nothing is keeping it awake.", false)
	})

	t.Run("lists agents and holds with reasons and time left", func(t *testing.T) {
		d := status(&model.Status{Assertions: []model.Assertion{
			{Key: "claude-code:abc", Tool: "claude-code"},
			{Key: "hold:ab12cd34", Tool: "codex", Reason: "deploy", ExpiresAt: exp(29*time.Minute + 40*time.Second)},
			{Key: "sniffed:aider:7", Tool: "aider", Reason: "", ExpiresAt: exp(-time.Minute)},
		}})
		expectText(t, call(t, d, "awake_status", `{}`), "The Mac is being kept awake by 3 item(s):\n"+
			"• [agent] claude-code\n"+
			"• [hold] codex — deploy (30 min left)\n"+
			"• [agent] aider", false)
		if req := d.requests[0]; req.Op != ipc.OpStatus {
			t.Fatalf("op %q", req.Op)
		}
	})

	t.Run("failures", func(t *testing.T) {
		expectText(t, call(t, &fakeDaemon{}, "awake_status", `{}`), "Could not read status from the daemon.", true)
		expectText(t, call(t, &fakeDaemon{reply: reply(ipc.Response{}, ipc.ErrDaemonUnreachable)}, "awake_status", `{}`),
			"The lidwake daemon isn't reachable.", true)
	})
}
