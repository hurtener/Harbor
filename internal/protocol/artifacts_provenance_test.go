package protocol_test

import (
	"context"
	"testing"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

// TestArtifactsListHandler_ProjectRow_ProvenanceSourceResolution asserts
// the projectRow source-discriminator else-chain (Phase 107f — D-176):
//
//   - canonical "source":"tool"        → ArtifactSourceTool
//   - canonical "source":"flow"        → ArtifactSourceSystem (flow runs
//     are runtime-produced; "flow" is not an enum member)
//   - pre-107f tool artifact (only a "tool" key, no "source") → tool
//     (the back-fill-free regression: the Console no longer shows blank)
//   - pre-107f flow artifact (only a "producer"/"flow" key) → system
//   - a user_upload still projects user_upload
//
// The refs are put DIRECTLY into the store with custom Source maps so we
// bypass the artifacts.put handler's user_upload default and exercise the
// projection over arbitrary storage-side provenance.
func TestArtifactsListHandler_ProjectRow_ProvenanceSourceResolution(t *testing.T) {
	t.Parallel()
	store := newInMemStore(t)
	s := newArtifactsSurface(t, store, "inmem")
	scope := artifacts.ArtifactScope{TenantID: "t1", UserID: "u1", SessionID: "s1"}
	ctx := context.Background()

	put := func(t *testing.T, body string, src map[string]any) string {
		t.Helper()
		ref, err := store.PutText(ctx, scope, body, artifacts.PutOpts{
			MimeType: "application/json",
			Source:   src,
		})
		if err != nil {
			t.Fatalf("store.PutText: %v", err)
		}
		return ref.ID
	}

	canonTool := put(t, "canonical tool", map[string]any{"source": "tool", "tool": "web_search"})
	canonFlow := put(t, "canonical flow", map[string]any{"source": "flow", "flow": "billing"})
	legacyTool := put(t, "legacy tool no source", map[string]any{"tool": "calc", "producer": "dev-tool-executor"})
	legacyFlow := put(t, "legacy flow no source", map[string]any{"flow": "billing", "producer": string(methods.MethodFlowsRunsDescribe)})
	legacyProducer := put(t, "legacy producer only", map[string]any{"producer": string(methods.MethodFlowsRunsDescribe)})
	upload := put(t, "an upload", map[string]any{"source": "user_upload"})

	resp, err := s.Dispatch(ctx, methods.MethodArtifactsList, &types.ArtifactsListRequest{
		Scope: types.ArtifactScope{Tenant: "t1", User: "u1", Session: "s1"},
	})
	if err != nil {
		t.Fatalf("artifacts.list: %v", err)
	}
	lr, ok := resp.(*types.ArtifactsListResponse)
	if !ok {
		t.Fatalf("response %T, want *types.ArtifactsListResponse", resp)
	}

	got := make(map[string]types.ArtifactSource, len(lr.Rows))
	for _, row := range lr.Rows {
		got[row.Ref.ID] = row.Source
	}

	want := map[string]types.ArtifactSource{
		canonTool:      types.ArtifactSourceTool,
		canonFlow:      types.ArtifactSourceSystem,
		legacyTool:     types.ArtifactSourceTool,
		legacyFlow:     types.ArtifactSourceSystem,
		legacyProducer: types.ArtifactSourceSystem,
		upload:         types.ArtifactSourceUserUpload,
	}
	for id, wantSrc := range want {
		if gotSrc, ok := got[id]; !ok {
			t.Errorf("ref %q missing from list rows", id)
		} else if gotSrc != wantSrc {
			t.Errorf("ref %q: Source = %q, want %q", id, gotSrc, wantSrc)
		}
	}

	// Regression guard: NONE of the tool/flow rows project a blank source.
	for _, row := range lr.Rows {
		if row.Source == "" {
			t.Errorf("ref %q projected a BLANK source (the pre-107f Console bug)", row.Ref.ID)
		}
	}
}
