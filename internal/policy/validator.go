package policy

import (
	"fmt"
	"math"
	"strings"
	"unicode/utf8"
)

// Validation for requests arriving over the CLI socket. The socket deliberately accepts any
// same-user caller (that's how agent hooks reach the daemon), so its inputs are untrusted:
// fields get hard length caps before they are stored, persisted and broadcast, and the key
// namespaces the daemon mints itself are off-limits to external acquires. Lengths count Unicode
// code points.
const (
	MaxKeyLength    = 256
	MaxToolLength   = 64
	MaxReasonLength = 1024
	// SniffedKeyPrefix is the key prefix of auto-acquired (process-sniffed) assertions, minted
	// only by the daemon.
	SniffedKeyPrefix = "sniffed:"
	// MaxTTLSeconds is the daemon's 24-hour max-age backstop; a longer TTL could never fire.
	MaxTTLSeconds = 24 * 3600.0
)

// ReservedKeyPrefixes are the namespaces the daemon mints itself. An external acquire planting a
// key here would confuse hold/sniff bookkeeping (IsHoldKey checks, per-key release).
func ReservedKeyPrefixes() []string { return []string{HoldKeyPrefix, SniffedKeyPrefix} }

// AcquireRejection is why an acquire is rejected, with the message sent back over the wire; ""
// when it is acceptable.
func AcquireRejection(key, tool string) string {
	if strings.TrimSpace(key) == "" {
		return "acquire requires a non-empty key"
	}
	if utf8.RuneCountInString(key) > MaxKeyLength {
		return fmt.Sprintf("key exceeds %d characters", MaxKeyLength)
	}
	if utf8.RuneCountInString(tool) > MaxToolLength {
		return fmt.Sprintf("tool exceeds %d characters", MaxToolLength)
	}
	for _, reserved := range ReservedKeyPrefixes() {
		if strings.HasPrefix(key, reserved) {
			return fmt.Sprintf("the '%s' key namespace is reserved", reserved)
		}
	}
	return ""
}

// ClampedReason truncates a reason to MaxReasonLength code points. Reasons are advisory display
// strings — truncate rather than reject.
func ClampedReason(reason string) string {
	if utf8.RuneCountInString(reason) <= MaxReasonLength {
		return reason
	}
	n := 0
	for i := range reason {
		if n == MaxReasonLength {
			return reason[:i]
		}
		n++
	}
	return reason
}

// ClampedTTL sanitizes a caller-chosen TTL in seconds. Non-finite or non-positive values are
// dropped (the idle sweep still governs the assertion — and a non-finite value can't be encoded
// as JSON when the assertion is persisted); anything beyond the 24-hour max-age backstop is
// capped to it.
func ClampedTTL(ttl *float64) *float64 {
	if ttl == nil || math.IsNaN(*ttl) || math.IsInf(*ttl, 0) || *ttl <= 0 {
		return nil
	}
	v := min(*ttl, MaxTTLSeconds)
	return &v
}
