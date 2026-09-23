package protocol

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
)

// TestBuildDetail_FailsLoudlyOnHeavyBytesReachingInlinePath is the
// D-026 negative test the phase plan mandates: a row that was NOT
// classified heavy yet carries a value whose byte length exceeds the
// threshold MUST fail loudly with ErrContextLeak rather than inline
// the heavy bytes. This models a driver / projection bug; the
// defence-in-depth branch in buildDetail closes it (mirrors the
// LLM-edge ErrContextLeak posture in internal/llm/safety.go).
func TestBuildDetail_FailsLoudlyOnHeavyBytesReachingInlinePath(t *testing.T) {
	const threshold = 1024
	heavy := []byte(strings.Repeat("Z", threshold*2)) // 2x over threshold
	id := identity.Quadruple{Identity: identity.Identity{
		TenantID: "t", UserID: "u", SessionID: "s",
	}}

	err := BuildDetailLeakProbe(threshold, heavy, id)
	if !errors.Is(err, ErrContextLeak) {
		t.Fatalf("BuildDetailLeakProbe with heavy bytes on the inline path: err = %v, want ErrContextLeak (D-026)", err)
	}
}

// TestBuildDetail_LightBytesInlineCleanly pins the positive side: a row
// genuinely below the threshold inlines without an ErrContextLeak.
func TestBuildDetail_LightBytesInlineCleanly(t *testing.T) {
	const threshold = 4096
	light := bytes.Repeat([]byte("a"), 128)
	id := identity.Quadruple{Identity: identity.Identity{
		TenantID: "t", UserID: "u", SessionID: "s",
	}}
	err := BuildDetailLeakProbe(threshold, light, id)
	if err != nil {
		t.Fatalf("BuildDetailLeakProbe with light bytes: err = %v, want nil", err)
	}
}
