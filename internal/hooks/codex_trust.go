package hooks

import (
	"os"
	"strings"
	"unicode"
	"unicode/utf8"
)

// CodexTrust is a best-effort reading of whether the user has trusted lidwake's Codex hooks.
//
// Codex won't run a command hook until it is trusted: the user approves it in the TUI via /hooks,
// and Codex records the approval in ~/.codex/config.toml as a table
// [hooks.state."<hooks.json path>:<event>:<group>:<handler>"] carrying a trusted_hash (event labels
// are snake_case there — user_prompt_submit, stop — though the hooks.json keys are CamelCase).
// We detect that a trusted_hash is recorded for our handlers rather than recomputing Codex's hash:
// it is taken over Codex's internal normalized handler identity, so reproducing it would couple us
// to Codex internals and break on an upgrade. The installer keeps our commands byte-stable — which
// is exactly what keeps an existing trusted_hash valid — so a recorded hash on our key is a
// reliable "trusted" signal.
//
// This is guidance, never a gate: a false "untrusted" just shows the instructions again; a false
// "trusted" at worst lets a still-untrusted hook fail quietly until Codex prompts the user.
type CodexTrust string

const (
	// CodexTrusted: every managed event has a recorded trusted_hash.
	CodexTrusted CodexTrust = "trusted"
	// CodexPartiallyTrusted: some, not all, managed events are trusted (e.g. the acquire hook was
	// approved but not a newly added Stop release).
	CodexPartiallyTrusted CodexTrust = "partiallyTrusted"
	// CodexUntrusted: no managed event has a recorded trusted_hash.
	CodexUntrusted CodexTrust = "untrusted"
	// CodexTrustUnknown: hooks.json isn't installed, or config.toml couldn't be read.
	CodexTrustUnknown CodexTrust = "unknown"
)

// codexManagedEventLabels are the events the Codex integration's acquire/release wire, labelled
// exactly as Codex keys them in config.toml.
var codexManagedEventLabels = []string{"user_prompt_submit", "stop"}

// CodexTrustStatus reads the trust state of the Codex hooks under home.
func CodexTrustStatus(home string) CodexTrust {
	ctx := hookContext{home: home}
	hooksPath := ctx.homePath(".codex", "hooks.json")
	if !exists(hooksPath) {
		return CodexTrustUnknown
	}
	data, err := os.ReadFile(ctx.homePath(".codex", "config.toml"))
	if err != nil || !utf8.Valid(data) {
		return CodexTrustUnknown
	}
	toml := string(data)
	trusted := 0
	for _, label := range codexManagedEventLabels {
		if hasTrustedHash(toml, hooksPath, label) {
			trusted++
		}
	}
	switch trusted {
	case 0:
		return CodexUntrusted
	case len(codexManagedEventLabels):
		return CodexTrusted
	default:
		return CodexPartiallyTrusted
	}
}

// hasTrustedHash reports whether toml records a non-empty trusted_hash in a [hooks.state."…"]
// table whose key names hooksPath and the snake_case eventLabel. Tolerant of key order and of
// toml_edit's standard quoted-dotted-key table headers.
func hasTrustedHash(toml, hooksPath, eventLabel string) bool {
	lines := strings.FieldsFunc(toml, isNewline)
	for i, line := range lines {
		if !isHooksStateHeader(line, hooksPath, eventLabel) {
			continue
		}
		// Scan the table body up to the next header.
		for _, body := range lines[i+1:] {
			body = trimHorizontalSpace(body)
			if strings.HasPrefix(body, "[") {
				break
			}
			if v, ok := trustedHashValue(body); ok && v != "" {
				return true
			}
		}
	}
	return false
}

func isHooksStateHeader(raw, hooksPath, eventLabel string) bool {
	line := trimHorizontalSpace(raw)
	if !strings.HasPrefix(line, "[hooks.state.") || !strings.HasSuffix(line, "]") {
		return false
	}
	// The whole `<path>:<event>:<group>:<handler>` key is one quoted segment, so both the path and
	// the `:<event>:` delimiter appear verbatim inside the header.
	return strings.Contains(line, hooksPath) && strings.Contains(line, ":"+eventLabel+":")
}

func trustedHashValue(line string) (string, bool) {
	if !strings.HasPrefix(line, "trusted_hash") {
		return "", false
	}
	_, value, ok := strings.Cut(line, "=")
	if !ok {
		return "", false
	}
	return strings.ReplaceAll(trimHorizontalSpace(value), `"`, ""), true
}

func isNewline(r rune) bool {
	switch r {
	case '\n', '\r', '\v', '\f', 0x85, 0x2028, 0x2029:
		return true
	}
	return false
}

// trimHorizontalSpace trims spaces and tabs (Unicode space separators included), not newlines.
func trimHorizontalSpace(s string) string {
	return strings.TrimFunc(s, func(r rune) bool { return r == '\t' || unicode.Is(unicode.Zs, r) })
}
