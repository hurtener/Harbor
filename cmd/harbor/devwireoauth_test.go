package main

import (
	"bytes"
	"strings"
	"testing"

	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
)

// TestRegisterAllowWireOAuthDescriptorIfDev_Banner proves the dev-only
// wire-carried OAuth-descriptor hatch prints its stderr banner exactly when the
// escape hatch fired, is silent otherwise, is nil-stderr safe, AND that the
// capture is reciprocal — the boot-capture atomic reads the honest boot value
// (true when the hatch fired, false otherwise). The gate that consumes the
// captured flag is pinned in the agent-config wire-descriptor tests.
func TestRegisterAllowWireOAuthDescriptorIfDev_Banner(t *testing.T) {
	// The helper flips a package-level boot-capture atomic; restore it.
	t.Cleanup(func() { toolauth.RegisterAllowWireOAuthDescriptorCaptured(false) })

	// allow=true → the banner prints AND the capture flips true.
	var on bytes.Buffer
	registerAllowWireOAuthDescriptorIfDev(true, &on)
	if !strings.Contains(on.String(), WireOAuthDescriptorBanner) {
		t.Fatalf("allow=true: banner %q not printed, got %q", WireOAuthDescriptorBanner, on.String())
	}
	if !toolauth.AllowWireOAuthDescriptorCaptured() {
		t.Fatal("allow=true: the boot-capture atomic must read true (reciprocal with the banner)")
	}

	// allow=false → silent, and the capture flips back to the honest non-dev state.
	var off bytes.Buffer
	registerAllowWireOAuthDescriptorIfDev(false, &off)
	if off.Len() != 0 {
		t.Fatalf("allow=false: expected no banner, got %q", off.String())
	}
	if toolauth.AllowWireOAuthDescriptorCaptured() {
		t.Fatal("allow=false: the boot-capture atomic must read false")
	}

	// allow=true with a nil stderr must not panic (defensive, like the sibling helpers).
	registerAllowWireOAuthDescriptorIfDev(true, nil)
}
