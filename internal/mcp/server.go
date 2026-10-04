// Package mcp is `lidwake mcp`: a Model Context Protocol server (JSON-RPC 2.0 over stdio) that
// exposes agent holds as native, agent-callable tools. An agent configured with this server can
// keep the Mac awake for a background job itself, with a reason and a timeout, without knowing
// the CLI.
//
// Transport per the MCP spec: each line of input is one JSON-RPC message and each response is one
// line of output. Diagnostics go to the log writer (stderr) so they never corrupt the protocol
// stream. Tool calls are synchronous round trips to the daemon over the CLI socket.
package mcp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	"github.com/nikitaShakhbazyan/lidwake-go/internal/ipc"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/model"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/paths"
	"github.com/nikitaShakhbazyan/lidwake-go/internal/policy"
)

// DefaultProtocolVersion is the MCP revision this server implements.
const DefaultProtocolVersion = "2024-11-05"

// supportedProtocolVersions are the revisions close enough to ours that echoing them is honest
// (newline-delimited JSON-RPC over stdio, text tool results).
var supportedProtocolVersions = map[string]bool{"2024-11-05": true, "2025-03-26": true}

// JSON-RPC error codes.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
)

// Server answers MCP requests. The zero value talks to the real daemon under the default tool
// label; tests substitute Send and Now.
type Server struct {
	// Tool labels the holds this server places (`lidwake mcp --tool claude-code`), so they
	// carry the agent's name in status output. Empty means policy.DefaultHoldTool.
	Tool string
	// Send makes one daemon round trip; nil means ipc.Send.
	Send func(ipc.Request) (ipc.Response, error)
	// Now reads the clock for "min left" in awake_status; nil means time.Now.
	Now func() time.Time
	// Log receives diagnostics; nil discards them.
	Log io.Writer
}

// Serve reads one JSON-RPC message per line from in and writes each response as one line to
// out, until in is exhausted. Write errors are ignored: a client that closed its end of the
// pipe while still sending must not crash the server.
func (s *Server) Serve(in io.Reader, out io.Writer) error {
	s.logf("lidwake mcp server ready (tool=%s)", s.tool())
	w := &writer{out: out}
	r := bufio.NewReader(in)
	for {
		line, err := r.ReadBytes('\n')
		line = bytes.TrimSuffix(bytes.TrimSuffix(line, []byte("\n")), []byte("\r"))
		if len(line) > 0 {
			s.handleLine(line, w)
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("mcp: read: %w", err)
		}
	}
}

func (s *Server) handleLine(line []byte, w *writer) {
	if !json.Valid(line) {
		// A client awaiting a reply to a request id would otherwise sit on its own timeout;
		// JSON-RPC defines -32700 for exactly this.
		w.error(nil, codeParseError, "Parse error")
		return
	}
	switch bytes.TrimLeft(line, " \t\r\n")[0] {
	case '{':
	case '[':
		w.error(nil, codeInvalidRequest, "Invalid request: expected a JSON-RPC object")
		return
	default:
		// A bare scalar is not a JSON-RPC message at all.
		w.error(nil, codeParseError, "Parse error")
		return
	}
	var msg map[string]json.RawMessage
	if err := json.Unmarshal(line, &msg); err != nil {
		w.error(nil, codeParseError, "Parse error")
		return
	}
	s.handle(msg, w)
}

func (s *Server) handle(msg map[string]json.RawMessage, w *writer) {
	// id is absent for notifications; a present id (even null) is echoed verbatim.
	id, hasID := msg["id"]
	// A message without a string method (absent, null, a number) is not a request; drop it.
	method, ok := stringValue(msg["method"])
	if !ok {
		return
	}
	params := object(msg["params"])

	switch method {
	case "initialize":
		// Echo a requested revision only if this server implements it; for an unknown revision
		// the spec says to answer with our own, never to claim support for semantics
		// (structured tool results, batches) we don't have.
		version := DefaultProtocolVersion
		if requested, ok := stringValue(params["protocolVersion"]); ok && supportedProtocolVersions[requested] {
			version = requested
		}
		w.result(id, initializeResult{
			ProtocolVersion: version,
			Capabilities:    capabilities{Tools: struct{}{}},
			ServerInfo:      serverInfo{Name: "lidwake", Version: paths.Version},
		})
	case "notifications/initialized", "notifications/cancelled":
		// Notifications take no response.
	case "ping":
		w.result(id, struct{}{})
	case "tools/list":
		w.result(id, toolsList{Tools: toolDefinitions()})
	case "tools/call":
		s.handleToolCall(id, params, w)
	default:
		if hasID {
			w.error(id, codeMethodNotFound, "Method not found: "+method)
		}
	}
}

// ---- Tools -----------------------------------------------------------------------------------

type property struct {
	Type        string `json:"type"`
	Description string `json:"description"`
}

type inputSchema struct {
	Type       string              `json:"type"`
	Properties map[string]property `json:"properties"`
	Required   []string            `json:"required,omitempty"`
}

type toolDefinition struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	InputSchema inputSchema `json:"inputSchema"`
}

type toolsList struct {
	Tools []toolDefinition `json:"tools"`
}

func toolDefinitions() []toolDefinition {
	return []toolDefinition{
		{
			Name:        "keep_awake",
			Description: "Keep this Mac fully awake for a background task that will outlive the current turn — e.g. a build, deploy, migration, or training run you just started. This blocks sleep COMPLETELY, including the closed-lid (clamshell) case with the display off and on battery — unlike `caffeinate`, which only prevents idle sleep and still lets the Mac sleep when the user shuts the lid. So the task keeps running even after the user closes the laptop and walks away. Returns a hold id. The hold ends when you call release_awake, when the named process exits, or when its time runs out. Use this when work continues after you finish responding; you do NOT need it for work done within your turn.",
			InputSchema: inputSchema{
				Type: "object",
				Properties: map[string]property{
					"reason": {
						Type:        "string",
						Description: "Short human-readable reason, shown to the user (e.g. 'running database migration').",
					},
					"minutes": {
						Type:        "number",
						Description: "How long to hold, in minutes. Optional; defaults to 60 and is capped by the user's setting. Prefer a realistic estimate over a large value.",
					},
					"pid": {
						Type:        "integer",
						Description: "Optional process id of the background job. When given, the hold releases automatically the moment that process exits — the most precise option.",
					},
					"keep_display_awake": {
						Type:        "boolean",
						Description: "Also keep the DISPLAY awake (and wake it if it is dark). Set this ONLY when the task reads the screen — vision or accessibility-tree work: a sleeping display collapses every app's accessibility tree, so a normal (system-only) hold would keep the machine running while blinding the agent. Leave unset for headless work; the display then sleeps as usual.",
					},
				},
				Required: []string{"reason"},
			},
		},
		{
			Name:        "release_awake",
			Description: "Release a hold placed by keep_awake, letting the Mac sleep normally again once nothing else is holding it awake. Call this as soon as the background task finishes.",
			InputSchema: inputSchema{
				Type: "object",
				Properties: map[string]property{
					"hold_id": {
						Type:        "string",
						Description: "The hold id returned by keep_awake (looks like 'hold:abc12345').",
					},
				},
				Required: []string{"hold_id"},
			},
		},
		{
			Name:        "awake_status",
			Description: "Report whether the Mac is currently being kept awake, and by what (active agents and holds, with their reasons and time remaining).",
			InputSchema: inputSchema{Type: "object", Properties: map[string]property{}},
		},
	}
}

func (s *Server) handleToolCall(id json.RawMessage, params map[string]json.RawMessage, w *writer) {
	name, ok := stringValue(params["name"])
	if !ok {
		w.error(id, codeInvalidParams, "tools/call requires a tool name")
		return
	}
	arguments := object(params["arguments"])

	switch name {
	case "keep_awake":
		s.keepAwake(id, arguments, w)
	case "release_awake":
		s.releaseAwake(id, arguments, w)
	case "awake_status":
		s.awakeStatus(id, w)
	default:
		w.toolResult(id, "Unknown tool: "+name, true)
	}
}

func (s *Server) keepAwake(id json.RawMessage, arguments map[string]json.RawMessage, w *writer) {
	reason, ok := stringValue(arguments["reason"])
	if !ok || reason == "" {
		w.toolResult(id, "keep_awake requires a 'reason'.", true)
		return
	}
	var ttl *float64
	if minutes, ok := numberValue(arguments["minutes"]); ok {
		if seconds := minutes * 60; !math.IsInf(seconds, 0) {
			ttl = &seconds
		}
	}
	// Only an exact, in-range integer: a wrapped or truncated pid would tie the hold's lifetime
	// to whatever unrelated process that value happens to name.
	pid := 0
	if n, ok := numberValue(arguments["pid"]); ok && n == math.Trunc(n) && n > 0 && n <= math.MaxInt32 {
		pid = int(n)
	}
	wantsDisplay := boolValue(arguments["keep_display_awake"])

	tool := s.tool()
	resp, err := s.send(ipc.Request{
		Op:          ipc.OpHold,
		Tool:        tool,
		Reason:      reason,
		PID:         pid,
		ProcessName: tool,
		TTL:         ttl,
		Display:     wantsDisplay,
	})
	if err != nil {
		w.toolResult(id, "The lidwake daemon isn't reachable, so the hold was not placed.", true)
		return
	}
	if !resp.OK || resp.HoldKey == "" {
		msg := resp.Error
		if msg == "" {
			msg = "Could not place the hold."
		}
		w.toolResult(id, msg, true)
		return
	}
	var text strings.Builder
	fmt.Fprintf(&text, "Keeping the Mac awake, including with the lid closed (hold id: %s).", resp.HoldKey)
	if wantsDisplay {
		// Version skew: an old daemon ignores the display field and omits the echo — the agent
		// must know its screen-reading plan is not actually protected.
		if resp.DisplayApplied == nil {
			text.WriteString(" WARNING: the running lidwake daemon predates display holds — the display is NOT being kept awake.")
		} else {
			text.WriteString(" The display is held awake too.")
		}
	}
	// The daemon clamps the TTL to the user's cap: report what was applied, not what was asked
	// for, so the agent doesn't plan around time it won't get.
	applied := policy.DefaultHoldTTL.Seconds()
	switch {
	case resp.AppliedTTL != nil:
		applied = *resp.AppliedTTL
	case ttl != nil:
		applied = *ttl
	}
	minutes := int(math.Round(applied / 60))
	if pid > 0 {
		fmt.Fprintf(&text, " Releases when process %d exits, or in about %d min", pid, minutes)
	} else {
		fmt.Fprintf(&text, " Expires in about %d min", minutes)
	}
	text.WriteString(" — call release_awake with this id when the task finishes.")
	w.toolResult(id, text.String(), false)
}

func (s *Server) releaseAwake(id json.RawMessage, arguments map[string]json.RawMessage, w *writer) {
	holdID, ok := stringValue(arguments["hold_id"])
	if !ok || holdID == "" {
		w.toolResult(id, "release_awake requires a 'hold_id'.", true)
		return
	}
	resp, err := s.send(ipc.Request{Op: ipc.OpRelease, Key: holdID, Tool: "mcp"})
	if err != nil {
		w.toolResult(id, "The lidwake daemon isn't reachable.", true)
		return
	}
	if resp.Warning != "" {
		w.toolResult(id, "Nothing to release — "+resp.Warning, false)
		return
	}
	w.toolResult(id, "Released "+holdID+". The Mac can sleep normally once nothing else is holding it awake.", false)
}

func (s *Server) awakeStatus(id json.RawMessage, w *writer) {
	resp, err := s.send(ipc.Request{Op: ipc.OpStatus})
	if err != nil {
		w.toolResult(id, "The lidwake daemon isn't reachable.", true)
		return
	}
	if resp.Status == nil {
		w.toolResult(id, "Could not read status from the daemon.", true)
		return
	}
	w.toolResult(id, describe(resp.Status, s.now()), false)
}

// describe is awake_status's answer: what keeps the Mac awake, with reasons and time left.
func describe(status *model.Status, now time.Time) string {
	if status.Paused {
		return "lidwake is paused — the Mac will sleep normally regardless of agents or holds."
	}
	if len(status.Assertions) == 0 {
		return "The Mac is sleeping normally — nothing is keeping it awake."
	}
	lines := []string{fmt.Sprintf("The Mac is being kept awake by %d item(s):", len(status.Assertions))}
	for _, a := range status.Assertions {
		kind := "agent"
		if policy.IsHoldKey(a.Key) {
			kind = "hold"
		}
		line := fmt.Sprintf("• [%s] %s", kind, a.Tool)
		if a.Reason != "" {
			line += " — " + a.Reason
		}
		if a.ExpiresAt != nil {
			if remaining := a.ExpiresAt.Sub(now); remaining > 0 {
				line += fmt.Sprintf(" (%d min left)", int(math.Round(remaining.Minutes())))
			}
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// ---- Plumbing --------------------------------------------------------------------------------

func (s *Server) tool() string {
	if s.Tool == "" {
		return policy.DefaultHoldTool
	}
	return s.Tool
}

func (s *Server) send(req ipc.Request) (ipc.Response, error) {
	if s.Send == nil {
		return ipc.Send(req)
	}
	return s.Send(req)
}

func (s *Server) now() time.Time {
	if s.Now == nil {
		return time.Now()
	}
	return s.Now()
}

func (s *Server) logf(format string, args ...any) {
	if s.Log != nil {
		fmt.Fprintf(s.Log, "[lidwake mcp] "+format+"\n", args...)
	}
}

// object decodes a JSON object, or yields an empty one for anything else (absent, null, an
// array, a scalar): a malformed params or arguments value reads as "nothing given".
func object(raw json.RawMessage) map[string]json.RawMessage {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil || m == nil {
		return map[string]json.RawMessage{}
	}
	return m
}

func stringValue(raw json.RawMessage) (string, bool) {
	var s string
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil || bytes.Equal(raw, []byte("null")) {
		return "", false
	}
	return s, true
}

func numberValue(raw json.RawMessage) (float64, bool) {
	var f float64
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) || json.Unmarshal(raw, &f) != nil {
		return 0, false
	}
	return f, true
}

func boolValue(raw json.RawMessage) bool {
	var b bool
	return len(raw) > 0 && json.Unmarshal(raw, &b) == nil && b
}

type initializeResult struct {
	ProtocolVersion string       `json:"protocolVersion"`
	Capabilities    capabilities `json:"capabilities"`
	ServerInfo      serverInfo   `json:"serverInfo"`
}

type capabilities struct {
	Tools struct{} `json:"tools"`
}

type serverInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type textContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type toolResult struct {
	Content []textContent `json:"content"`
	IsError bool          `json:"isError"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type resultMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result"`
}

type errorMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Error   rpcError        `json:"error"`
}

// writer emits one JSON-RPC message per line. A nil id is written as null.
type writer struct{ out io.Writer }

func (w *writer) result(id json.RawMessage, result any) {
	w.write(resultMessage{JSONRPC: "2.0", ID: nullable(id), Result: result})
}

func (w *writer) error(id json.RawMessage, code int, message string) {
	w.write(errorMessage{JSONRPC: "2.0", ID: nullable(id), Error: rpcError{Code: code, Message: message}})
}

func (w *writer) toolResult(id json.RawMessage, text string, isError bool) {
	w.result(id, toolResult{Content: []textContent{{Type: "text", Text: text}}, IsError: isError})
}

func (w *writer) write(v any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if enc.Encode(v) != nil { // Encode ends the message with the newline the transport needs
		return
	}
	_, _ = w.out.Write(buf.Bytes())
}

func nullable(id json.RawMessage) json.RawMessage {
	if len(id) == 0 {
		return json.RawMessage("null")
	}
	return id
}
