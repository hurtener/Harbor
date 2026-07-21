package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/tools"
)

// nilTagsCache is a fake ToolSearchCache whose Search returns a single tool
// with a nil Tags slice — the shape an MCP tool discovered carrying
// `tags: null` takes. It drives toolSearch's emit boundary deterministically
// without needing a real search index.
type nilTagsCache struct{}

func (nilTagsCache) Search(_ context.Context, _ string, _ []string, _ int) ([]tools.Tool, error) {
	return []tools.Tool{{Name: "untagged", Description: "a discovered tool with no tags", Tags: nil}}, nil
}
func (nilTagsCache) Sync(context.Context, []tools.Tool) error { return nil }
func (nilTagsCache) Close() error                             { return nil }

// TestToolSearch_NilTags_SerializesAsEmptyArray is the regression for the
// tool_search output-schema break: a tool with no tags has a nil Tags slice,
// which JSON-marshals to `null` and fails tool_search's own required-array
// `tags` output schema (`/tools/N/tags: got null, want array`) — so a single
// MCP tool discovered without tags broke the entire tool_search result. The
// emit boundary must normalize nil to an empty array.
func TestToolSearch_NilTags_SerializesAsEmptyArray(t *testing.T) {
	cat := tools.NewCatalog(tools.WithSearchCache(nilTagsCache{}))

	// Precondition: the catalog yields a tool with a nil Tags slice.
	if res := cat.Search(context.Background(), "anything", nil, 10); len(res) != 1 || res[0].Tags != nil {
		t.Fatalf("precondition: want one tool with nil Tags, got %+v", res)
	}

	out, err := toolSearch(skillTestCtx(t), cat, ToolSearchArgs{Query: "anything"})
	if err != nil {
		t.Fatalf("toolSearch: %v", err)
	}
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal tool_search output: %v", err)
	}
	if strings.Contains(string(b), `"tags":null`) {
		t.Fatalf("tool_search emitted tags:null — fails the required-array output schema: %s", b)
	}
	if !strings.Contains(string(b), `"tags":[]`) {
		t.Fatalf("expected a tool with tags:[] in the tool_search output, got: %s", b)
	}
}
