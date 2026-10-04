package policy

import (
	"crypto/rand"
	"encoding/hex"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Agent holds are explicit, reasoned, time-boxed sleep blocks placed by an agent via
// `lidwake hold`, `lidwake run` or the MCP server. Distinct from hook assertions: they live in the
// "hold:" key namespace, carry model.OriginManual, and are governed by their TTL rather than the
// idle policy. The daemon is the authority that applies these rules.
const (
	// DefaultHoldTTL is the TTL of a hold requested without an explicit duration.
	DefaultHoldTTL = time.Hour
	// HoldKeyPrefix marks an assertion key as an agent hold.
	HoldKeyPrefix = "hold:"
	// DefaultHoldTool is the tool label stored on a hold that didn't name its originating agent.
	DefaultHoldTool = "manual"
	// UnknownTool is the tool label the CLI assumes when --tool is omitted. Every generated hook
	// names its tool, so this label marks a human or script invocation — which is what licenses
	// SessionKey's verbatim passthrough of colon-bearing keys, and release's strict exit on a
	// no-match.
	UnknownTool = "unknown"
)

// IsHoldKey reports whether key is in the agent-hold namespace.
func IsHoldKey(key string) bool { return strings.HasPrefix(key, HoldKeyPrefix) }

// SessionKey is the registry key for a hook-driven session hold. Hook sessions are keyed
// "<tool>:<sessionID>"; an id that is already a full agent-hold key ("hold:…") or already
// carries this tool's prefix ("<tool>:…", the form `status --json` prints) is used verbatim, so
// releasing an assertion by its displayed key targets it directly. Acquire and release derive the
// key identically, which is what guarantees a session's start-of-turn acquire and its matching
// end-of-turn release land on the same key. (The passthrough preserves that: hooks pass bare
// session ids, and a bare id never starts with "<tool>:".)
//
// With no named tool (UnknownTool), any colon-bearing id passes through verbatim — not just
// "unknown:"-prefixed ones. Every generated hook names its tool, so the no-tool caller is a human
// or script pasting a key from `status --json`, where prefixing would mint an unmatchable
// "unknown:<tool>:<id>" and the release would target nothing. The rule stays gated on
// UnknownTool so a hook whose session ids happen to contain colons still gets the tool-prefix
// derivation its acquire/release bracketing relies on.
func SessionKey(tool, sessionID string) string {
	if IsHoldKey(sessionID) || strings.HasPrefix(sessionID, tool+":") {
		return sessionID
	}
	if tool == UnknownTool && strings.Contains(sessionID, ":") {
		return sessionID
	}
	return tool + ":" + sessionID
}

// NewHoldKey mints a fresh hold key: "hold:" + 8 lowercase hex characters. Short enough to echo
// back to an agent, unique enough to never collide in practice.
func NewHoldKey() string {
	var b [4]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails on supported platforms
	return HoldKeyPrefix + hex.EncodeToString(b[:])
}

// ClampHoldTTL clamps a requested TTL in seconds (nil: none given) into [1 s, cap], defaulting to
// DefaultHoldTTL. The cap (the user's manualHoldMaxHours) is the hard ceiling: a forgetful agent
// can never pin the Mac awake longer than this. NaN counts as no request.
func ClampHoldTTL(requested *float64, capHours float64) float64 {
	limit := holdCapSeconds(capHours)
	wanted := DefaultHoldTTL.Seconds()
	if requested != nil && !math.IsNaN(*requested) {
		wanted = *requested
	}
	return min(max(wanted, 1), limit)
}

// ClampExpiry clamps an assertion's expiry so no acquired hold outlives the max-hold cap. The
// daemon applies this to every acquire: a TTL-carrying hook (the background-shell hold, which
// requests up to the CLI's 24 h ceiling) is brought down to the user's live manualHoldMaxHours,
// while a TTL-less hook or sniffed assertion (expiresAt nil) is left to the idle policy. This is
// the daemon-authoritative ceiling for hook TTLs, the sibling of ClampHoldTTL for explicit holds.
func ClampExpiry(expiresAt *time.Time, acquiredAt time.Time, capHours float64) *time.Time {
	if expiresAt == nil {
		return nil
	}
	maxExpiry := acquiredAt.Add(secondsToDuration(holdCapSeconds(capHours)))
	clamped := *expiresAt
	if maxExpiry.Before(clamped) {
		clamped = maxExpiry
	}
	return &clamped
}

// holdCapSeconds is the cap in seconds, never below one second (a degenerate cap must still
// yield a future expiry).
func holdCapSeconds(capHours float64) float64 {
	if math.IsNaN(capHours) {
		return 1
	}
	return max(1, capHours*3600)
}

// ParseDurationSeconds parses a human duration into seconds. It accepts a bare number (seconds),
// a single unit (30s, 45m, 2h, 1d) or a compound (1h30m, 90m, 2h15m30s), case-insensitively with
// surrounding blanks ignored. ok is false on garbage so callers can surface a usage error rather
// than silently mis-holding.
func ParseDurationSeconds(input string) (seconds float64, ok bool) {
	s := strings.ToLower(strings.TrimFunc(input, func(r rune) bool {
		return r == '\t' || unicode.Is(unicode.Zs, r)
	}))
	if s == "" {
		return 0, false
	}
	// Bare number → seconds. "inf"/"nan" parse as floats but are not durations.
	if bare, err := strconv.ParseFloat(s, 64); err == nil {
		if math.IsNaN(bare) || math.IsInf(bare, 0) || bare < 0 {
			return 0, false
		}
		return bare, true
	}
	units := map[rune]float64{'s': 1, 'm': 60, 'h': 3600, 'd': 86400}
	var total float64
	var number strings.Builder
	sawUnit := false
	for _, ch := range s {
		switch mult, isUnit := units[ch]; {
		case ch >= '0' && ch <= '9' || ch == '.':
			number.WriteRune(ch)
		case isUnit:
			if number.Len() == 0 {
				return 0, false
			}
			value, err := strconv.ParseFloat(number.String(), 64)
			if err != nil {
				return 0, false
			}
			total += value * mult
			number.Reset()
			sawUnit = true
		default:
			return 0, false
		}
	}
	// Trailing digits with no unit (e.g. "1h30") are ambiguous — reject.
	if !sawUnit || number.Len() > 0 || math.IsInf(total, 0) {
		return 0, false
	}
	return total, true
}
