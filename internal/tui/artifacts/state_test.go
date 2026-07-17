package artifacts

import (
	"testing"

	"github.com/hurtener/Harbor/internal/protocol/types"
)

func TestDerive_MetadataOnlyAndPreviewPosture(t *testing.T) {
	state := Derive(types.ArtifactsListResponse{Rows: []types.ArtifactRow{{Ref: types.ArtifactRef{ID: "a", MimeType: "text/plain"}}}, TotalMatched: 3})
	if len(state.Rows) != 1 || state.TotalMatched != 3 || !Previewable(state.Rows[0].Ref) || Previewable(types.ArtifactRef{MimeType: "video/mp4"}) {
		t.Fatalf("state=%#v", state)
	}
}
