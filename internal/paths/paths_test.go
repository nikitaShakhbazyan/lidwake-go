package paths

import (
	"runtime/debug"
	"testing"
)

func TestResolveVersion(t *testing.T) {
	info := func(v string) func() (*debug.BuildInfo, bool) {
		return func() (*debug.BuildInfo, bool) {
			return &debug.BuildInfo{Main: debug.Module{Path: "github.com/nikitaShakhbazyan/lidwake-go", Version: v}}, true
		}
	}
	noInfo := func() (*debug.BuildInfo, bool) { return nil, false }

	for _, tc := range []struct {
		name string
		v    string
		read func() (*debug.BuildInfo, bool)
		want string
	}{
		{"go install takes the module version", devVersion, info("v0.2.0"), "v0.2.0"},
		{"ldflags value wins over the module version", "v0.3.0", info("v0.2.0"), "v0.3.0"},
		{"devel build keeps the dev default", devVersion, info("(devel)"), devVersion},
		{"empty module version keeps the dev default", devVersion, info(""), devVersion},
		{"no build info keeps the dev default", devVersion, noInfo, devVersion},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveVersion(tc.v, tc.read); got != tc.want {
				t.Fatalf("resolveVersion(%q) = %q, want %q", tc.v, got, tc.want)
			}
		})
	}
}
