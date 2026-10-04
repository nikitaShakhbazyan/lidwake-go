package hooks

import "strings"

// FallbackSlug is the slug used when a typed name has no slug-able characters (blank, or all
// punctuation / non-Latin), and the default agent name.
const FallbackSlug = "my-agent"

// ManualHookSnippet renders the copy-paste snippets for wiring an arbitrary agent into lidwake by
// hand. No daemon or CLI change is needed: the daemon socket accepts any --tool from a same-user
// caller, so `lidwake acquire … --tool <slug>` works end to end for a tool lidwake has never heard
// of. This just turns a typed agent name into the exact commands, keyed on a slug of that name.
type ManualHookSnippet struct {
	// Name is the trimmed agent name as typed, used verbatim in the hold reason; the slug when the
	// input is blank, so the reason is never an empty `"" session`.
	Name string
	// Slug is the --tool value: the name lowercased and reduced to [a-z0-9-]. It labels the hold,
	// so it must be stable and shell-safe.
	Slug string
}

// NewManualHookSnippet builds the snippets for agentName.
func NewManualHookSnippet(agentName string) ManualHookSnippet {
	trimmed := strings.TrimSpace(agentName)
	slug := Slug(trimmed)
	name := trimmed
	if name == "" {
		name = slug
	}
	return ManualHookSnippet{Name: name, Slug: slug}
}

// Acquire is for an agent that fires start/end hooks. $SESSION_ID is the placeholder the user maps
// to their agent's session-id variable; quoting guards a value with spaces.
func (s ManualHookSnippet) Acquire() string {
	return `lidwake acquire "$SESSION_ID" --tool ` + s.Slug
}

// Release pairs with Acquire on the agent's end/stop hook. Same --tool so it targets the key the
// acquire placed — dropping --tool would release unknown:$SESSION_ID and leak the real hold.
func (s ManualHookSnippet) Release() string {
	return `lidwake release "$SESSION_ID" --tool ` + s.Slug
}

// WrapperScript is for an agent with no hooks: acquire, run the real command, release — keyed on
// the wrapper's own PID ($$), which the release reuses so the hold brackets the command's
// lifetime. `exit $status` preserves the wrapped command's exit code.
func (s ManualHookSnippet) WrapperScript() string {
	return "#!/bin/sh\n" +
		"# Keep your Mac awake while " + s.Name + " runs, then let it sleep again.\n" +
		"lidwake acquire $$ --tool " + s.Slug + "\n" +
		"<your-agent-command> \"$@\"\n" +
		"status=$?\n" +
		"lidwake release $$ --tool " + s.Slug + "\n" +
		"exit $status"
}

// OneShotHold keeps the Mac awake for a background job for up to 2 h, ending when this shell ($$)
// exits, the time runs out, or the hold is released — no start/end hooks needed.
func (s ManualHookSnippet) OneShotHold() string {
	return `lidwake hold --for 2h --pid $$ --reason "` + s.Name + ` session"`
}

// Slug reduces a display name to a shell- and menu-safe slug: lowercased, [a-z0-9] kept, runs of
// anything else collapsed to a single "-", leading and trailing hyphens trimmed. Punctuation and
// non-Latin characters are dropped ("Café Bot!" → "caf-bot"); nothing left yields FallbackSlug.
func Slug(name string) string {
	var out strings.Builder
	pendingSeparator := false
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			if pendingSeparator && out.Len() > 0 {
				out.WriteByte('-')
			}
			pendingSeparator = false
			out.WriteRune(r)
			continue
		}
		// Any other character is a word boundary: the next kept character emits one hyphen.
		pendingSeparator = true
	}
	if out.Len() == 0 {
		return FallbackSlug
	}
	return out.String()
}
