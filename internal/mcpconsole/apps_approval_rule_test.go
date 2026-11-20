package mcpconsole_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/mcpconsole"
	"github.com/hurtener/Harbor/internal/tools"
	mcp "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

// A tool an approval rule gates is refused on the rendered-App path with
// a typed error (it cannot park for a decision there) instead of running
// ungated; an ungated sibling still runs.
func TestAppCall_ApprovalRuleGatedToolRefused(t *testing.T) {
	ctx := idCtx(t)
	cat := gateCatalog(t)
	registry := newAppsRegistry(t, nil)
	if err := registry.Register(context.Background(), mcp.ServerRegistration{
		Provider: &stubProvider{id: "srv-b"}, Transport: "inmemory", InitialState: mcp.ServerStateOnline,
	}); err != nil {
		t.Fatalf("Register srv-b: %v", err)
	}
	acc, err := mcpconsole.NewAppsAccessor(mcpconsole.AppsDeps{
		Registry: registry, Catalog: cat, Store: newAppsStore(t), Bus: newAppsBus(t),
		ToolContext: newAppsToolCtx(t), Threshold: 1024,
		ApprovalGated: func(tl tools.Tool) bool { return tl.Name == "srv-a_echo" },
	})
	if err != nil {
		t.Fatalf("NewAppsAccessor: %v", err)
	}
	_, err = acc.CallTool(ctx, "", "srv-a_echo", json.RawMessage(`{}`))
	if !errors.Is(err, mcpconsole.ErrAppToolApprovalRequired) {
		t.Fatalf("gated App call err = %v, want ErrAppToolApprovalRequired", err)
	}
	if _, err := acc.CallTool(ctx, "", "srv-b_ping", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("ungated call rejected: %v", err)
	}
}
