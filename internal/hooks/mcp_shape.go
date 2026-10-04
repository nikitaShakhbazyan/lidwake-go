package hooks

import "reflect"

// mcpServerShape registers (or removes) lidwake's MCP server inside an agent's JSON config — the
// agent-facing side of `lidwake mcp`, exposing keep_awake / release_awake / awake_status so the
// agent can hold sleep on its own.
//
// Simpler than the hook shapes: an MCP server is a single named entry under a container object
// (mcpServers), so the server name is its own idempotency handle and needs no tag. Install upserts
// our entry, uninstall removes just that key, and the user's other servers are always preserved.
//
//	{ "mcpServers": { "lidwake": { "type": "stdio", "command": "…/lidwake", "args": ["mcp", "--tool", "claude-code"] } } }
type mcpServerShape struct {
	configPath string
	// containerKey is the top-level object holding named servers ("mcpServers").
	containerKey string
	serverName   string
	// entry is the canonical server entry, also the yardstick for state.
	entry map[string]any
}

func (s mcpServerShape) container() string {
	if s.containerKey == "" {
		return "mcpServers"
	}
	return s.containerKey
}

func (s mcpServerShape) install(dryRun bool) (Result, error) {
	existing, err := readJSONForUpdate(s.configPath)
	if err != nil {
		return Result{}, err
	}
	before := orEmpty(existing)
	after := deepCopyObject(before)
	servers := objectField(after, s.container())
	servers[s.serverName] = deepCopyObject(s.entry)
	after[s.container()] = servers

	diff := makeDiff(before, after)
	if !dryRun {
		if err := saveJSON(after, s.configPath, existing, diff); err != nil {
			return Result{}, err
		}
	}
	return Result{Summary: "registered " + s.serverName + " MCP server", Diff: diff}, nil
}

func (s mcpServerShape) uninstall(dryRun bool) (Result, error) {
	existing, err := readJSONForUpdate(s.configPath)
	if err != nil {
		return Result{}, err
	}
	servers, ok := existing[s.container()].(map[string]any)
	if !ok {
		return nothingRemoved(), nil
	}
	if _, ok := servers[s.serverName]; !ok {
		return nothingRemoved(), nil
	}
	after := deepCopyObject(existing)
	servers = objectField(after, s.container())
	delete(servers, s.serverName)
	// An emptied container stays in place: non-destructive, and the agent treats an empty
	// mcpServers the same as none.
	after[s.container()] = servers
	diff := makeDiff(existing, after)
	if !dryRun {
		if err := saveJSON(after, s.configPath, existing, diff); err != nil {
			return Result{}, err
		}
	}
	return Result{Summary: "removed " + s.serverName + " MCP server", Diff: diff}, nil
}

func (s mcpServerShape) state() InstallState {
	obj, status := readConfig(s.configPath)
	switch status {
	case readMissing:
		return StateNotInstalled
	case readUnparseable:
		return StateConfigUnreadable
	}
	servers, ok := obj[s.container()].(map[string]any)
	if !ok {
		return StateNotInstalled
	}
	installed, ok := servers[s.serverName].(map[string]any)
	if !ok {
		return StateNotInstalled
	}
	// Deep-compare the on-disk entry with what we'd write, nested args array included.
	if reflect.DeepEqual(installed, s.entry) {
		return StateInstalled
	}
	return StateModifiedExternally
}
