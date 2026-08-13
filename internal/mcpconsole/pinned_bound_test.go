// pinned_bound_test.go — phase 213 (D-358). Every other test in this
// package supplies an explicit small threshold, so none of them observes
// which CONSTANT the unconfigured fallback resolves. These arms do.
//
// The pin: `mcp.servers.read_resource`, `mcp.apps.call_tool` and
// `mcp.apps.tool_context` are browser-rendered Protocol replies, so they
// select inline-versus-reference at the Console inline-payload bound
// (32 KiB) and did NOT follow the LLM-context heavy-output threshold up
// to 128 KiB. 64 KiB is the size that distinguishes the two: inside the
// raised offload band, still by-reference here.
//
// Mutation witness: re-sourcing `defaultHeavyThreshold` on
// `config.DefaultHeavyOutputThresholdBytes` makes every 64 KiB case
// below ride inline and this file FAILS.

package mcpconsole_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/mcpconsole"
	"github.com/hurtener/Harbor/internal/tools"
	mcp "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

// capturedToolContext builds a capture whose RESULT half carries the
// payload under test and whose input half stays trivially small.
func capturedToolContext(callID string, result json.RawMessage) mcp.CapturedToolContext {
	return mcp.CapturedToolContext{
		ServerID: "srv-a", ToolCallID: callID, Tool: "render",
		Input: json.RawMessage(`{}`), Result: result,
	}
}

// pinnedBandBytes sits inside the raised LLM-context offload band and
// above the pinned Console inline-payload bound.
const pinnedBandBytes = 64 * 1024

// TestMCPConsole_PinnedBound_ReadResource_OffloadsInTheRaisedBand — an
// ORDINARY (non-`ui://`) resource of 64 KiB still ships by reference on
// the unconfigured fallback.
func TestMCPConsole_PinnedBound_ReadResource_OffloadsInTheRaisedBand(t *testing.T) {
	body := []byte(strings.Repeat("A", pinnedBandBytes))
	acc, err := mcpconsole.NewAppsAccessor(mcpconsole.AppsDeps{
		Registry:    newAppsRegistry(t, body),
		Catalog:     tools.NewCatalog(),
		Store:       newAppsStore(t),
		Bus:         newAppsBus(t),
		ToolContext: newAppsToolCtx(t),
		// Threshold deliberately UNSET: exercise the fallback constant.
	})
	if err != nil {
		t.Fatalf("NewAppsAccessor: %v", err)
	}
	got, err := acc.ReadResource(idCtx(t), "srv-a", "mcp://srv-a/report.txt")
	if err != nil {
		t.Fatalf("ReadResource: %v", err)
	}
	if got.Artifact == nil {
		t.Fatalf("a %d-byte resource rode INLINE on the unconfigured fallback; the Console "+
			"inline-payload bound (%d) must not have followed the LLM-context threshold (%d)",
			len(body), config.DefaultConsoleInlinePayloadBytes, config.DefaultHeavyOutputThresholdBytes)
	}
	if len(got.Inline) != 0 {
		t.Errorf("offloaded resource also inlined %d bytes", len(got.Inline))
	}
}

// TestMCPConsole_PinnedBound_CallTool_OffloadsInTheRaisedBand — the App
// tool-result half of the same pin.
func TestMCPConsole_PinnedBound_CallTool_OffloadsInTheRaisedBand(t *testing.T) {
	blob := strings.Repeat("B", pinnedBandBytes)
	cat := tools.NewCatalog()
	if err := cat.Register(tools.ToolDescriptor{
		Tool: tools.Tool{Name: "srv-a_bulky"},
		Invoke: func(_ context.Context, _ json.RawMessage) (tools.ToolResult, error) {
			return tools.ToolResult{Value: map[string]any{"blob": blob}}, nil
		},
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	acc, err := mcpconsole.NewAppsAccessor(mcpconsole.AppsDeps{
		Registry:    newAppsRegistry(t, nil),
		Catalog:     cat,
		Store:       newAppsStore(t),
		Bus:         newAppsBus(t),
		ToolContext: newAppsToolCtx(t),
	})
	if err != nil {
		t.Fatalf("NewAppsAccessor: %v", err)
	}
	res, err := acc.CallTool(idCtx(t), "", "srv-a_bulky", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.Artifact == nil {
		t.Fatalf("a %d-byte App tool result rode INLINE on the unconfigured fallback", len(blob))
	}
	if len(res.Inline) != 0 {
		t.Errorf("offloaded tool result also inlined %d bytes", len(res.Inline))
	}
}

// TestMCPConsole_PinnedBound_ToolContext_OffloadsInTheRaisedBand — the
// captured-context half, which resolves its fallback in a different
// constructor and so needs its own witness.
func TestMCPConsole_PinnedBound_ToolContext_OffloadsInTheRaisedBand(t *testing.T) {
	// Threshold unset → the fallback constant.
	tc := newToolCtxStore(t, 0)
	ctx := idCtx(t)
	heavy := json.RawMessage(`{"blob":"` + strings.Repeat("C", pinnedBandBytes) + `"}`)
	if err := tc.Capture(ctx, capturedToolContext("call-pinned", heavy)); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	row, err := tc.Load(ctx, "srv-a", "call-pinned")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if row.Result.Artifact == nil {
		t.Fatalf("a %d-byte captured result rode INLINE on the unconfigured fallback", len(heavy))
	}
	if len(row.Result.Inline) != 0 {
		t.Errorf("offloaded captured result also inlined %d bytes", len(row.Result.Inline))
	}
}

// TestMCPConsole_PinnedBound_UIDocumentCarveOutSurvives — the `ui://`
// App-document cap is a THIRD bound and is unaffected: a 64 KiB App
// document still renders inline, which is the property the App cap
// exists to guarantee on every artifact driver.
func TestMCPConsole_PinnedBound_UIDocumentCarveOutSurvives(t *testing.T) {
	body := []byte(strings.Repeat("D", pinnedBandBytes))
	acc, err := mcpconsole.NewAppsAccessor(mcpconsole.AppsDeps{
		Registry:    newAppsRegistry(t, body),
		Catalog:     tools.NewCatalog(),
		Store:       newAppsStore(t),
		Bus:         newAppsBus(t),
		ToolContext: newAppsToolCtx(t),
	})
	if err != nil {
		t.Fatalf("NewAppsAccessor: %v", err)
	}
	got, err := acc.ReadResource(idCtx(t), "srv-a", "ui://app/main.html")
	if err != nil {
		t.Fatalf("ReadResource: %v", err)
	}
	if got.Artifact != nil {
		t.Fatalf("a %d-byte `ui://` App document was offloaded; the App-document cap must still cover it", len(body))
	}
	if len(got.Inline) != len(body) {
		t.Errorf("Inline = %d bytes, want %d", len(got.Inline), len(body))
	}
}
