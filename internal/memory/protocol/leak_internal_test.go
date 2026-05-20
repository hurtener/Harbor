package protocol

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/identity"
)

// stubArtifactStore is a minimal ArtifactStore that records nothing —
// the leak test never reaches the artifact-routing branch (the leak
// fires first). It exists only so BuildDetailLeakProbe has a non-nil
// store argument; it is NOT a production-path stub (CLAUDE.md §13 — it
// lives in an _test.go file and is never wired into a registry).
type stubArtifactStore struct{}

func (stubArtifactStore) PutBytes(context.Context, artifacts.ArtifactScope, []byte, artifacts.PutOpts) (artifacts.ArtifactRef, error) {
	return artifacts.ArtifactRef{}, nil
}
func (stubArtifactStore) PutText(context.Context, artifacts.ArtifactScope, string, artifacts.PutOpts) (artifacts.ArtifactRef, error) {
	return artifacts.ArtifactRef{}, nil
}
func (stubArtifactStore) Get(context.Context, artifacts.ArtifactScope, string) ([]byte, bool, error) {
	return nil, false, nil
}
func (stubArtifactStore) GetRef(context.Context, artifacts.ArtifactScope, string) (*artifacts.ArtifactRef, bool, error) {
	return nil, false, nil
}
func (stubArtifactStore) Exists(context.Context, artifacts.ArtifactScope, string) (bool, error) {
	return false, nil
}
func (stubArtifactStore) Delete(context.Context, artifacts.ArtifactScope, string) (bool, error) {
	return false, nil
}
func (stubArtifactStore) List(context.Context, artifacts.ArtifactScope) ([]artifacts.ArtifactRef, error) {
	return nil, nil
}
func (stubArtifactStore) Close(context.Context) error { return nil }

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

	err := BuildDetailLeakProbe(context.Background(), stubArtifactStore{}, threshold, heavy, id)
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
	err := BuildDetailLeakProbe(context.Background(), stubArtifactStore{}, threshold, light, id)
	if err != nil {
		t.Fatalf("BuildDetailLeakProbe with light bytes: err = %v, want nil", err)
	}
}
