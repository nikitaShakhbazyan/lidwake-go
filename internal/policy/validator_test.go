package policy

import (
	"math"
	"strings"
	"testing"
	"unicode/utf8"
)

// The CLI socket accepts any same-user caller, so its fields are untrusted input: identity fields
// get hard caps, display fields get truncated, and daemon-minted key namespaces are rejected so
// an external acquire can't impersonate a hold or a sniffed assertion.
func TestCLIRequestValidator(t *testing.T) {
	t.Run("a normal hook acquire passes", func(t *testing.T) {
		if r := AcquireRejection("claude-code:abc123", "claude-code"); r != "" {
			t.Fatalf("rejected: %s", r)
		}
	})

	t.Run("empty and whitespace-only keys are rejected", func(t *testing.T) {
		for _, key := range []string{"", "  \n"} {
			if AcquireRejection(key, "claude-code") == "" {
				t.Errorf("%q accepted", key)
			}
		}
	})

	t.Run("oversized key and tool are rejected at the boundary", func(t *testing.T) {
		maxKey := strings.Repeat("k", MaxKeyLength)
		if r := AcquireRejection(maxKey, "t"); r != "" {
			t.Errorf("max key rejected: %s", r)
		}
		if r := AcquireRejection(maxKey+"k", "t"); r != "key exceeds 256 characters" {
			t.Errorf("oversized key: %q", r)
		}
		maxTool := strings.Repeat("t", MaxToolLength)
		if r := AcquireRejection("k", maxTool); r != "" {
			t.Errorf("max tool rejected: %s", r)
		}
		if r := AcquireRejection("k", maxTool+"t"); r != "tool exceeds 64 characters" {
			t.Errorf("oversized tool: %q", r)
		}
		// Lengths count characters, not bytes.
		if r := AcquireRejection(strings.Repeat("é", MaxKeyLength), "t"); r != "" {
			t.Errorf("multi-byte max key rejected: %s", r)
		}
	})

	t.Run("reserved key namespaces are rejected", func(t *testing.T) {
		if r := AcquireRejection("hold:deadbeef", "evil"); r != "the 'hold:' key namespace is reserved" {
			t.Errorf("hold: %q", r)
		}
		if r := AcquireRejection("sniffed:claude:123", "evil"); r != "the 'sniffed:' key namespace is reserved" {
			t.Errorf("sniffed: %q", r)
		}
		// A reserved word elsewhere in the key is fine — only the prefix is the namespace.
		if r := AcquireRejection("claude-code:hold:x", "claude-code"); r != "" {
			t.Errorf("embedded reserved word rejected: %s", r)
		}
	})

	t.Run("ttl clamping drops garbage and caps at the backstop", func(t *testing.T) {
		for _, in := range []*float64{nil, f64(0), f64(-5), f64(math.Inf(1)), f64(math.NaN())} {
			if got := ClampedTTL(in); got != nil {
				t.Errorf("%v: got %v, want nil (a non-finite TTL can't be persisted)", fmtF(in), *got)
			}
		}
		if got := ClampedTTL(f64(600)); got == nil || *got != 600 {
			t.Errorf("600: got %v", got)
		}
		if got := ClampedTTL(f64(9e99)); got == nil || *got != 86400 {
			t.Errorf("finite-but-absurd must cap at the 24 h backstop, got %v", got)
		}
	})

	t.Run("duration parser rejects non-finite spellings", func(t *testing.T) {
		for _, in := range []string{"inf", "nan", "infinity", "+Inf"} {
			if got, ok := ParseDurationSeconds(in); ok {
				t.Errorf("%q: got %v", in, got)
			}
		}
		if got, ok := ParseDurationSeconds("30m"); !ok || got != 1800 {
			t.Errorf("30m: got %v (ok=%v)", got, ok)
		}
	})

	t.Run("reasons are truncated not rejected", func(t *testing.T) {
		if got := ClampedReason(""); got != "" {
			t.Errorf("empty: %q", got)
		}
		if got := ClampedReason("short"); got != "short" {
			t.Errorf("short: %q", got)
		}
		long := strings.Repeat("r", MaxReasonLength+100)
		if got := ClampedReason(long); utf8.RuneCountInString(got) != MaxReasonLength {
			t.Errorf("long: %d characters", utf8.RuneCountInString(got))
		}
		// Truncation never splits a multi-byte character.
		wide := strings.Repeat("ж", MaxReasonLength+1)
		got := ClampedReason(wide)
		if !utf8.ValidString(got) || utf8.RuneCountInString(got) != MaxReasonLength {
			t.Errorf("wide: valid=%v characters=%d", utf8.ValidString(got), utf8.RuneCountInString(got))
		}
		exact := strings.Repeat("ж", MaxReasonLength)
		if ClampedReason(exact) != exact {
			t.Error("a reason at the limit was changed")
		}
	})
}
