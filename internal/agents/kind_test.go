package agents

import (
	"slices"
	"strings"
	"testing"
)

func TestAgentKind(t *testing.T) {
	t.Run("every agent has display name", func(t *testing.T) {
		for _, k := range All() {
			if k.DisplayName() == "" {
				t.Errorf("%s missing display name", k)
			}
		}
	})

	t.Run("every agent has at least one binary name", func(t *testing.T) {
		for _, k := range All() {
			if len(k.BinaryNames()) == 0 {
				t.Errorf("%s has no binary names", k)
			}
		}
	})

	t.Run("tier classification covers all agents", func(t *testing.T) {
		tier1 := []Kind{ClaudeCode, Codex, Cursor, GeminiCLI}
		tier2 := []Kind{Aider, Hermes, OpenCode, Cline, Pi}
		union := append(slices.Clone(tier1), tier2...)
		all := All()
		slices.Sort(union)
		slices.Sort(all)
		if !slices.Equal(union, all) {
			t.Fatalf("tiers %v do not cover %v", union, all)
		}
		for _, k := range tier1 {
			if k.Tier() != 1 {
				t.Errorf("%s should be tier 1", k)
			}
		}
		for _, k := range tier2 {
			if k.Tier() != 2 {
				t.Errorf("%s should be tier 2", k)
			}
		}
	})

	t.Run("raw values are kebab case and unique", func(t *testing.T) {
		seen := map[Kind]bool{}
		for _, k := range All() {
			if seen[k] {
				t.Errorf("duplicate raw value %q", k)
			}
			seen[k] = true
			raw := string(k)
			if raw != strings.ToLower(raw) || strings.Contains(raw, " ") {
				t.Errorf("raw value %q is not kebab case", raw)
			}
		}
	})

	t.Run("pi env markers authorize the argv walk", func(t *testing.T) {
		// Pi sets both in its own env at startup, so the hook-spawned CLI inherits both; either
		// alone is proof enough.
		cases := []struct {
			env  map[string]string
			want bool
		}{
			{map[string]string{"PI_CODING_AGENT": "true"}, true},
			{map[string]string{"AI_AGENT": "pi"}, true},
			{map[string]string{"AI_AGENT": "claude"}, false},
			{map[string]string{"PI_CODING_AGENT": "false"}, false},
			{map[string]string{}, false},
			{nil, false},
		}
		for _, c := range cases {
			if got := EnvironmentMarksPi(c.env); got != c.want {
				t.Errorf("EnvironmentMarksPi(%v) = %v", c.env, got)
			}
		}
	})

	t.Run("argv identifies node-hosted pi launchers", func(t *testing.T) {
		for _, argv := range [][]string{
			// npm/Homebrew: the shebang passes the launcher symlink through, basename `pi`.
			{"node", "/opt/homebrew/bin/pi"},
			{"node", "/opt/homebrew/bin/pi", "--continue"},
			// Nix-style wrappers exec the real entry point inside the package directory.
			{"node", "/nix/store/abc-pi-0.84.2/lib/node_modules/@earendil-works/pi-coding-agent/dist/bundle/cli.js"},
		} {
			if !ArgvIsPi(argv) {
				t.Errorf("ArgvIsPi(%q) = false", argv)
			}
		}
	})

	t.Run("argv rejects non-pi node processes", func(t *testing.T) {
		for _, argv := range [][]string{
			// A directory merely named `pi` must not match — only a script basename of `pi` does.
			{"node", "/Users/u/src/pi/serve.js"},
			{"node", "/usr/local/bin/pinch"},
			// argv[0] is the interpreter and never consulted; a real `pi` binary is matched by the
			// executable-path walk instead.
			{"pi"},
			{"node"},
			{},
			nil,
		} {
			if ArgvIsPi(argv) {
				t.Errorf("ArgvIsPi(%q) = true", argv)
			}
		}
	})

	t.Run("aider and cline are tier 2", func(t *testing.T) {
		if Aider.Tier() != 2 || Cline.Tier() != 2 {
			t.Fatalf("tiers = %d, %d", Aider.Tier(), Cline.Tier())
		}
	})

	t.Run("all binary names are non empty", func(t *testing.T) {
		for _, k := range All() {
			for _, n := range k.BinaryNames() {
				if n == "" {
					t.Errorf("%s has an empty binary name", k)
				}
			}
		}
	})

	t.Run("all binary names union covers all known agents", func(t *testing.T) {
		all := AllBinaryNames()
		for _, n := range []string{"claude", "codex", "aider", "pi", "opencode"} {
			if !all[n] {
				t.Errorf("AllBinaryNames lacks %q", n)
			}
		}
	})

	t.Run("binary names are globally unique", func(t *testing.T) {
		// The daemon's sniff sweep builds a binary-name → Kind map; a collision would silently
		// mis-attribute an auto-acquired assertion to the wrong agent.
		var all []string
		for _, k := range All() {
			all = append(all, k.BinaryNames()...)
		}
		if len(AllBinaryNames()) != len(all) {
			t.Fatalf("binary names collide across agents: %v", all)
		}
	})

	t.Run("unknown raw value is nil", func(t *testing.T) {
		if k, ok := Parse("not-an-agent"); ok {
			t.Fatalf("Parse = %q", k)
		}
		if Kind("not-an-agent").Known() {
			t.Fatal("unknown kind reports Known")
		}
	})

	t.Run("raw value round trips", func(t *testing.T) {
		for _, k := range All() {
			if got, ok := Parse(string(k)); !ok || got != k {
				t.Errorf("Parse(%q) = %q, %v", k, got, ok)
			}
		}
	})

	t.Run("for running process matches basename", func(t *testing.T) {
		if k, ok := ForRunningProcess("codex", "/usr/local/bin/codex"); !ok || k != Codex {
			t.Fatalf("got %q, %v", k, ok)
		}
	})

	// Homebrew's cask symlinks `codex` → a triple-suffixed real binary, and proc_pidpath resolves
	// the symlink, so the daemon sees `codex-aarch64-apple-darwin` (or the x86_64 variant) as the
	// process basename. Both must resolve to Codex, or every Homebrew install is unwatchable and
	// its hold never releases until the 24h backstop.
	t.Run("for running process matches homebrew triple-suffixed codex", func(t *testing.T) {
		arm := "/opt/homebrew/Caskroom/codex/0.136.0/codex-aarch64-apple-darwin"
		if k, ok := ForRunningProcess("codex-aarch64-apple-darwin", arm); !ok || k != Codex {
			t.Fatalf("arm: got %q, %v", k, ok)
		}
		intel := "/usr/local/Caskroom/codex/0.136.0/codex-x86_64-apple-darwin"
		if k, ok := ForRunningProcess("codex-x86_64-apple-darwin", intel); !ok || k != Codex {
			t.Fatalf("intel: got %q, %v", k, ok)
		}
		// The owning-PID walk uses the same name set; the suffixed basename must satisfy it too.
		if !PathMatchesAgent(arm, AllBinaryNames()) {
			t.Fatal("PathMatchesAgent rejects the suffixed codex")
		}
	})

	// npm's `opencode-ai` maps its bin entry to `bin/opencode.exe` even on macOS, and Homebrew
	// symlinks `opencode` → that file, so proc_pidpath reports basename `opencode.exe`.
	t.Run("for running process matches homebrew opencode exe", func(t *testing.T) {
		p := "/opt/homebrew/Cellar/opencode/1.18.0/libexec/lib/node_modules/opencode-ai/bin/opencode.exe"
		if k, ok := ForRunningProcess("opencode.exe", p); !ok || k != OpenCode {
			t.Fatalf("got %q, %v", k, ok)
		}
		if !PathMatchesAgent(p, AllBinaryNames()) {
			t.Fatal("PathMatchesAgent rejects opencode.exe")
		}
	})

	t.Run("for running process matches versioned path component", func(t *testing.T) {
		k, ok := ForRunningProcess("2.1.156", "/Users/u/.local/share/claude/versions/2.1.156")
		if !ok || k != ClaudeCode {
			t.Fatalf("got %q, %v", k, ok)
		}
	})

	t.Run("for running process returns nil for unknown", func(t *testing.T) {
		if k, ok := ForRunningProcess("python3", "/usr/bin/python3"); ok {
			t.Fatalf("got %q", k)
		}
	})

	t.Run("only hermes is gateway scoped", func(t *testing.T) {
		// Hermes runs as one shared 24/7 gateway process; everything else is one process per
		// session.
		for _, k := range All() {
			if k.IsGatewayScoped() != (k == Hermes) {
				t.Errorf("%s gateway scoping is wrong", k)
			}
		}
		if p := Hermes.GatewayPIDFileRelativePath(); p != ".hermes/gateway.pid" {
			t.Fatalf("pid file = %q", p)
		}
	})

	t.Run("argv matches hermes gateway", func(t *testing.T) {
		argv := []string{"python", "-m", "hermes_cli.main", "gateway", "run", "--replace"}
		if k, ok := ForArgv(argv); !ok || k != Hermes {
			t.Fatalf("got %q, %v", k, ok)
		}
	})

	t.Run("argv matches hermes desktop dashboard", func(t *testing.T) {
		argv := []string{
			"python", "-m", "hermes_cli.main", "dashboard", "--no-open", "--tui",
			"--host", "127.0.0.1", "--port", "9120",
		}
		if k, ok := ForArgv(argv); !ok || k != Hermes {
			t.Fatalf("got %q, %v", k, ok)
		}
	})

	t.Run("argv does not match unrelated python", func(t *testing.T) {
		if k, ok := ForArgv([]string{"python", "-m", "http.server"}); ok {
			t.Fatalf("got %q", k)
		}
		if k, ok := ForArgv(nil); ok {
			t.Fatalf("got %q", k)
		}
	})

	t.Run("argv requires all markers in A group", func(t *testing.T) {
		// hermes_cli.main alone (no gateway/dashboard subcommand) shouldn't match — e.g. --help.
		if k, ok := ForArgv([]string{"python", "-m", "hermes_cli.main", "--help"}); ok {
			t.Fatalf("got %q", k)
		}
	})

	t.Run("argv matched agents is exactly the argv marker agents", func(t *testing.T) {
		var want []Kind
		for _, k := range All() {
			if k.ArgvMarkers() != nil {
				want = append(want, k)
			}
		}
		got := ArgvMatchedAgents()
		if !slices.Equal(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		if !slices.Contains(got, Hermes) {
			t.Fatal("Hermes missing")
		}
	})
}

func TestAgentKindGo(t *testing.T) {
	t.Run("unknown kind degrades gracefully", func(t *testing.T) {
		k := Kind("mystery")
		if k.DisplayName() != "mystery" || k.Tier() != 0 || k.BinaryNames() != nil ||
			k.ArgvMarkers() != nil || k.IsGatewayScoped() {
			t.Fatal("unknown kind does not degrade to empty values")
		}
	})

	t.Run("generic names never match a path component", func(t *testing.T) {
		for _, p := range []string{"/Users/u/src/pi/build/tool", "/Users/u/src/cline/bin/x", "/opt/codex/bin/run"} {
			if PathMatchesAgent(p, AllBinaryNames()) {
				t.Errorf("PathMatchesAgent(%q) = true", p)
			}
			if k, ok := ForRunningProcess(lastPathComponent(p), p); ok {
				t.Errorf("ForRunningProcess(%q) = %q", p, k)
			}
		}
	})

	t.Run("returned collections are copies", func(t *testing.T) {
		Codex.BinaryNames()[0] = "mutated"
		Hermes.ArgvMarkers()[0][0] = "mutated"
		AllBinaryNames()["mutated"] = true
		ComponentMatchedBinaryNames()["pi"] = true
		if Codex.BinaryNames()[0] != "codex" || Hermes.ArgvMarkers()[0][0] != "hermes_cli.main" {
			t.Fatal("static tables were mutated through a returned slice")
		}
		if AllBinaryNames()["mutated"] || PathMatchesAgent("/src/pi/x", AllBinaryNames()) {
			t.Fatal("static sets were mutated through a returned map")
		}
	})

	t.Run("by binary name reverse lookup", func(t *testing.T) {
		if k, ok := ByBinaryName("Cursor"); !ok || k != Cursor {
			t.Fatalf("got %q, %v", k, ok)
		}
		if _, ok := ByBinaryName("python3"); ok {
			t.Fatal("python3 resolved")
		}
	})

	t.Run("last path component", func(t *testing.T) {
		cases := map[string]string{
			"": "", "/": "/", "pi": "pi", "/a/b": "b", "/a/b/": "b", "a//b": "b",
		}
		for in, want := range cases {
			if got := lastPathComponent(in); got != want {
				t.Errorf("lastPathComponent(%q) = %q, want %q", in, got, want)
			}
		}
	})
}
