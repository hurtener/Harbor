package stream_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	_ "github.com/hurtener/Harbor/internal/agentcfg/drivers/statestore"
	"github.com/hurtener/Harbor/internal/agentcfg/sessionoverlay"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/transports/stream"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/agentcfg/projection"
	"github.com/hurtener/Harbor/internal/state"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tools"
	mcpdrv "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
	toolsprotocol "github.com/hurtener/Harbor/internal/tools/protocol"
)

type readCountingStore struct {
	state.StateStore
	reads atomic.Int64
}

func (s *readCountingStore) Load(ctx context.Context, id identity.Quadruple, kind string) (state.StateRecord, error) {
	s.reads.Add(1)
	return s.StateStore.Load(ctx, id, kind)
}

func (s *readCountingStore) ListKindForIdentity(ctx context.Context, id identity.Quadruple, prefix string) ([]state.StateRecord, error) {
	s.reads.Add(1)
	return s.StateStore.ListKindForIdentity(ctx, id, prefix)
}

// newRevisionToolsHandler wires the production configuration projection
// (agent config, session overlay, catalog, MCP registry) behind the real
// Tools HTTP handler.
func newRevisionToolsHandler(t *testing.T) (*stream.ToolsHandler, *readCountingStore, tools.ToolCatalog) {
	t.Helper()
	st, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatal(err)
	}
	store := &readCountingStore{StateStore: st}
	bus, err := eventsinmem.New(config.EventsConfig{
		Driver: "inmem", MaxSubscribersPerSession: 16, SubscriberBufferSize: 64,
		IdleTimeout: time.Minute, DropWindow: time.Second,
	}, auditpatterns.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()); _ = st.Close(context.Background()) })
	agents, err := agentcfg.Open(context.Background(), agentcfg.Config{}, agentcfg.Deps{State: store, Bus: bus})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agents.SetRevision(context.Background(), identity.Quadruple{Identity: toolsHandlerID}, "agent-a", agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{}, agentcfg.SetOptions{}); err != nil {
		t.Fatal(err)
	}
	overlay, err := sessionoverlay.NewStore(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	cat := tools.NewCatalog()
	for i := range 3 {
		if err := cat.Register(tools.ToolDescriptor{Tool: tools.Tool{Name: fmt.Sprintf("tool_%d", i), SideEffects: tools.SideEffectRead}, Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
			return tools.ToolResult{}, nil
		}}); err != nil {
			t.Fatal(err)
		}
	}
	registry := mcpdrv.NewRegistry()
	proj, err := toolsprotocol.NewCatalogProjector(cat, toolsprotocol.WithCatalogViewResolver(projection.CatalogViewResolver{
		Registry: agents, SessionOverlay: overlay, Catalog: cat, OwnerResolver: registry,
	}))
	if err != nil {
		t.Fatal(err)
	}
	svc, err := toolsprotocol.NewService(proj)
	if err != nil {
		t.Fatal(err)
	}
	h, err := stream.NewToolsHandler(svc,
		stream.WithToolsReachAuthorizer(auth.NewAgentReachAuthorizer()),
		stream.WithToolsAgentResolver(toolsAgentResolverFunc(func(context.Context, identity.Identity, string) (bool, error) { return true, nil })))
	if err != nil {
		t.Fatal(err)
	}
	return h, store, cat
}

// TestToolsHandler_ConditionalConfigurationList is the wire-level round trip
// of if_revision through the strict request decoder, plus the transport
// admission ordering: a reach-refused or non-admin caller is refused before
// any revision read.
func TestToolsHandler_ConditionalConfigurationList(t *testing.T) {
	h, store, cat := newRevisionToolsHandler(t)
	admin := []auth.Scope{auth.ScopeAdmin}
	status, body := doToolsRequest(t, h, "list", `{"view":"configuration","agent_id":"agent-a"}`, &toolsHandlerID, admin)
	if status != http.StatusOK {
		t.Fatalf("configuration list status=%d body=%s", status, body)
	}
	var full prototypes.ToolListResponse
	if err := json.Unmarshal(body, &full); err != nil {
		t.Fatal(err)
	}
	if full.Revision == "" || len(full.Tools) != 3 || full.Tools[0].SideEffect != "read" || full.Tools[0].LoadingMode != "always" {
		t.Fatalf("full list = %+v", full)
	}

	cond := fmt.Sprintf(`{"view":"configuration","agent_id":"agent-a","if_revision":%q}`, full.Revision)
	status, body = doToolsRequest(t, h, "list", cond, &toolsHandlerID, admin)
	if status != http.StatusOK {
		t.Fatalf("conditional status=%d body=%s", status, body)
	}
	var nm prototypes.ToolListResponse
	if err := json.Unmarshal(body, &nm); err != nil {
		t.Fatal(err)
	}
	if !nm.NotModified || nm.Revision != full.Revision || len(nm.Tools) != 0 {
		t.Fatalf("conditional list = %s", body)
	}

	if err := cat.Register(tools.ToolDescriptor{Tool: tools.Tool{Name: "tool_new"}, Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
		return tools.ToolResult{}, nil
	}}); err != nil {
		t.Fatal(err)
	}
	status, body = doToolsRequest(t, h, "list", cond, &toolsHandlerID, admin)
	var changed prototypes.ToolListResponse
	if status != http.StatusOK || json.Unmarshal(body, &changed) != nil || changed.NotModified || len(changed.Tools) != 4 || changed.Revision == full.Revision {
		t.Fatalf("changed conditional status=%d body=%s", status, body)
	}

	before := store.reads.Load()
	status, _ = doToolsRequest(t, h, "list", `{"view":"configuration","agent_id":"agent-b","if_revision":"x"}`, &toolsHandlerID, admin)
	if status != http.StatusForbidden {
		t.Fatalf("reach-refused status=%d, want 403", status)
	}
	status, _ = doToolsRequest(t, h, "list", cond, &toolsHandlerID, []auth.Scope{})
	if status == http.StatusOK {
		t.Fatal("non-admin configuration list admitted")
	}
	if reads := store.reads.Load() - before; reads != 0 {
		t.Fatalf("refused requests made %d revision reads", reads)
	}
}
