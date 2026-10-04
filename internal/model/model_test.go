package model

import (
	"encoding/json"
	"testing"
)

func TestAssertionWithoutOriginDecodesAsHook(t *testing.T) {
	var a Assertion
	if err := json.Unmarshal([]byte(`{"key":"k","tool":"t","pid":1,"processName":"p","acquiredAt":"2026-10-04T10:00:00Z","lastActivityAt":"2026-10-04T10:00:00Z"}`), &a); err != nil {
		t.Fatal(err)
	}
	if a.Origin != OriginHook {
		t.Fatalf("origin = %q, want hook", a.Origin)
	}
	var m Assertion
	if err := json.Unmarshal([]byte(`{"key":"k","origin":"manual"}`), &m); err != nil || m.Origin != OriginManual {
		t.Fatalf("explicit origin lost: %q %v", m.Origin, err)
	}
}
