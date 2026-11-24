package version

import (
	"strings"
	"testing"
)

func TestString(t *testing.T) {
	got := String()
	if !strings.HasPrefix(got, "gomedic ") {
		t.Fatalf("unexpected version string: %q", got)
	}
	if !strings.Contains(got, Version) {
		t.Fatalf("expected version %q in %q", Version, got)
	}
}
