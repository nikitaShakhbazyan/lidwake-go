package hooks

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nikitaShakhbazyan/lidwake-go/internal/policy"
)

// A faithful mock of how Codex parses and dispatches hooks.json:
//
//   - The file is {"hooks": {"<Event>": [<MatcherGroup>]}}. The top level is strict (only
//     "description" and "hooks", like serde's deny_unknown_fields); a MatcherGroup
//     ({matcher?, hooks}) is lenient; a command handler is {"type": "command", "command": …} with
//     extra keys tolerated.
//   - Event keys are CamelCase; every hook gets a stdin payload carrying session_id.
//   - UserPromptSubmit fires on each prompt; Stop when the turn completes.
//
// The mock routes an event to our installed command handlers and derives the registry key the CLI
// would (session_id from stdin → policy.SessionKey), so the assertions exercise the real shared key
// rule rather than a reimplementation of it.

type codexHooksFile struct {
	hooks map[string][]codexMatcherGroup
}

type codexMatcherGroup struct {
	matcher string
	hooks   []mockCodexHandler
}

type mockCodexHandler struct {
	typ     string
	command string
}

func (f codexHooksFile) groups(event string) []codexMatcherGroup { return f.hooks[event] }

func decodeCodexHooksFile(data []byte) (codexHooksFile, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return codexHooksFile{}, err
	}
	for k := range top {
		if k != "description" && k != "hooks" {
			return codexHooksFile{}, fmt.Errorf("unknown field `%s` (HooksFile denies unknown fields)", k)
		}
	}
	file := codexHooksFile{hooks: map[string][]codexMatcherGroup{}}
	raw, ok := top["hooks"]
	if !ok || bytes.Equal(raw, []byte("null")) {
		return file, nil
	}
	var events map[string][]json.RawMessage
	if err := json.Unmarshal(raw, &events); err != nil {
		return codexHooksFile{}, err
	}
	for event, groups := range events {
		for _, g := range groups {
			group, err := decodeCodexMatcherGroup(g)
			if err != nil {
				return codexHooksFile{}, err
			}
			file.hooks[event] = append(file.hooks[event], group)
		}
	}
	return file, nil
}

// decodeCodexMatcherGroup is lenient: matcher and hooks both default, any other key is ignored.
// That is why a flat [{type, command}] element contributes no handlers.
func decodeCodexMatcherGroup(data []byte) (codexMatcherGroup, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return codexMatcherGroup{}, err
	}
	var g codexMatcherGroup
	if raw, ok := m["matcher"]; ok && !bytes.Equal(raw, []byte("null")) {
		if err := json.Unmarshal(raw, &g.matcher); err != nil {
			return codexMatcherGroup{}, err
		}
	}
	if raw, ok := m["hooks"]; ok && !bytes.Equal(raw, []byte("null")) {
		var handlers []map[string]json.RawMessage
		if err := json.Unmarshal(raw, &handlers); err != nil {
			return codexMatcherGroup{}, err
		}
		for _, h := range handlers {
			var handler mockCodexHandler
			rawType, ok := h["type"]
			if !ok {
				return codexMatcherGroup{}, fmt.Errorf("handler missing `type`")
			}
			if err := json.Unmarshal(rawType, &handler.typ); err != nil {
				return codexMatcherGroup{}, err
			}
			if rawCmd, ok := h["command"]; ok && !bytes.Equal(rawCmd, []byte("null")) {
				if err := json.Unmarshal(rawCmd, &handler.command); err != nil {
					return codexMatcherGroup{}, err
				}
			}
			g.hooks = append(g.hooks, handler)
		}
	}
	return g, nil
}

// codexOp is one registry operation our hooks drive.
type codexOp struct{ op, key string }

// invocation parses `… lidwake <op> --tool <tool>` out of a hook command.
func invocation(command string) (op, tool string, ok bool) {
	toks := strings.Split(command, " ")
	i := slices.IndexFunc(toks, func(s string) bool { return s == "acquire" || s == "release" })
	j := slices.Index(toks, "--tool")
	if i < 0 || j < 0 || j+1 >= len(toks) {
		return "", "", false
	}
	return toks[i], toks[j+1], true
}

// dispatch fires event with a stdin payload carrying session_id == sessionID and returns the ops
// our command handlers perform, keyed by the CLI's own rule (policy.SessionKey) so the test can't
// drift from production key derivation.
func dispatch(f codexHooksFile, event, sessionID string) []codexOp {
	var ops []codexOp
	for _, g := range f.groups(event) {
		for _, h := range g.hooks {
			if h.typ != "command" || !commandInvokesLidwakeCLI(h.command) {
				continue
			}
			if op, tool, ok := invocation(h.command); ok {
				ops = append(ops, codexOp{op, policy.SessionKey(tool, sessionID)})
			}
		}
	}
	return ops
}

// installCodex installs our real Codex hooks into a fake home and parses the result through the
// mock, so output Codex couldn't parse fails here.
func installCodex(t *testing.T) codexHooksFile {
	t.Helper()
	home := fakeHome(t, ".codex")
	mustInstall(t, testInstaller(testCLI, home), agentCodex)
	f, err := decodeCodexHooksFile([]byte(readFile(t, filepath.Join(home, ".codex", "hooks.json"))))
	if err != nil {
		t.Fatalf("our output violates Codex's model: %v", err)
	}
	return f
}

func TestCodexHookModel(t *testing.T) {
	t.Run("our installed config parses under codex's strict model and routes both events", func(t *testing.T) {
		f := installCodex(t)
		if len(f.groups("UserPromptSubmit")) == 0 {
			t.Error("acquire must route on prompt submit")
		}
		if len(f.groups("Stop")) == 0 {
			t.Error("release must route on turn end")
		}
	})

	t.Run("a turn brackets acquire and release on the same key", func(t *testing.T) {
		f := installCodex(t)
		sid := "0199abcd-thread-id"
		acquire := dispatch(f, "UserPromptSubmit", sid)
		release := dispatch(f, "Stop", sid)
		if !slices.Equal(acquire, []codexOp{{"acquire", "codex:" + sid}}) {
			t.Errorf("acquire = %v", acquire)
		}
		if !slices.Equal(release, []codexOp{{"release", "codex:" + sid}}) {
			t.Errorf("release = %v", release)
		}
		if len(acquire) == 0 || len(release) == 0 || acquire[0].key != release[0].key {
			t.Error("release must target exactly the acquired key")
		}
	})

	t.Run("a multi-turn session cycles one idempotent key", func(t *testing.T) {
		f := installCodex(t)
		sid := "session-A"
		keys := map[string]bool{}
		for range 3 {
			for _, op := range dispatch(f, "UserPromptSubmit", sid) {
				keys[op.key] = true
			}
			for _, op := range dispatch(f, "Stop", sid) {
				keys[op.key] = true
			}
		}
		if len(keys) != 1 || !keys["codex:"+sid] {
			t.Errorf("one stable key for the whole session: %v", keys)
		}
	})

	t.Run("concurrent sessions get distinct keys", func(t *testing.T) {
		f := installCodex(t)
		a := dispatch(f, "UserPromptSubmit", "A")
		b := dispatch(f, "UserPromptSubmit", "B")
		if !slices.Equal(a, []codexOp{{"acquire", "codex:A"}}) || !slices.Equal(b, []codexOp{{"acquire", "codex:B"}}) {
			t.Errorf("a = %v, b = %v", a, b)
		}
	})

	t.Run("the flat no-wrapper shape contributes no handlers", func(t *testing.T) {
		js := `{"hooks":{"UserPromptSubmit":[{"type":"command","command":"/usr/local/bin/lidwake acquire --tool codex"}]}}`
		f, err := decodeCodexHooksFile([]byte(js))
		if err != nil {
			t.Fatal(err)
		}
		if ops := dispatch(f, "UserPromptSubmit", "x"); len(ops) != 0 {
			t.Errorf("flat handlers have no hooks wrapper, so nothing routes: %v", ops)
		}
	})

	t.Run("a stray top-level key is rejected like codex's strict HooksFile", func(t *testing.T) {
		if _, err := decodeCodexHooksFile([]byte(`{"hooks":{},"bogus":1}`)); err == nil {
			t.Error("a stray top-level key must fail the decode")
		}
	})

	t.Run("an interrupted turn leaks no extra op and the next turn still releases", func(t *testing.T) {
		f := installCodex(t)
		sid := "interrupted"
		// An Esc-interrupt fires no Stop: acquire, (no Stop), acquire, Stop must end with a clean
		// release of the single key.
		live := map[string]bool{}
		for _, op := range dispatch(f, "UserPromptSubmit", sid) {
			live[op.key] = true
		}
		for _, op := range dispatch(f, "UserPromptSubmit", sid) {
			live[op.key] = true
		}
		if len(live) != 1 || !live["codex:"+sid] {
			t.Errorf("re-acquire is idempotent on the one key: %v", live)
		}
		for _, op := range dispatch(f, "Stop", sid) {
			delete(live, op.key)
		}
		if len(live) != 0 {
			t.Errorf("the next turn's Stop releases the key the interrupted turn left held: %v", live)
		}
	})
}
