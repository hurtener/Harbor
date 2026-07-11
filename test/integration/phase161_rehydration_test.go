// Phase 161 integration test — session-reopen read-back carries content-free
// per-turn metadata (D-293). REAL drivers on every seam: the inmem-backed
// durable EventBus, the inmem StateStore + ArtifactStore, the patterns audit
// redactor, a REAL LLM client (via llm.Open over the mock driver — the
// dev-posture provider that reports synthetic usage/cost), and a REAL tool
// catalog wired with the runtime bus so its universal descriptor-wrap shell
// emits tool lifecycle events. The MCP leg is proven against the REAL stdio
// MCP fixture (`cmd/harbor-mcptest-stdio`) via the devstack, per §17.8.
//
// The producers this phase closes (llm.cost.recorded at the safety wrapper;
// tool.invoked/.completed at the catalog shell) publish onto the ONE bus that
// state.history reads back, so a reopen reconstructs what the live stream
// showed. Assertions (§17.3): (1) the read-back window carries the named
// content-free keys for the cost + tool + decision families with the run
// quadruple on the envelope; (2) a tool driven with sentinel args + a sentinel
// result never leaks either into any payload on the page; (3) the {type, key}
// set on a live subscription equals the read-back set for the cost/tool
// families (stream ≡ read-back); (4) a cross-tenant read still refuses (404),
// and a missing-identity read is rejected (401) — the read path is untouched.
package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/harbortest/devstack"
	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/audit"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/events/drivers/durable"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	_ "github.com/hurtener/Harbor/internal/llm/mock"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/protocol"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/transports"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	statinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tasks"
	_ "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess"
	"github.com/hurtener/Harbor/internal/tools"
)

const p161Sentinel = "SUPER-SECRET-ARG-VALUE-do-not-persist"

type p161Stack struct {
	srv    *httptest.Server
	bus    events.EventBus
	llm    llm.LLMClient
	cat    tools.ToolCatalog
	closfn func()
}

func newP161Stack(t *testing.T) *p161Stack {
	t.Helper()
	red := auditpatterns.New()
	st, err := statinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state inmem: %v", err)
	}
	bus, err := durable.New(context.Background(), config.EventsConfig{
		Driver:                   "durable",
		MaxSubscribersPerSession: 32,
		SubscriberBufferSize:     256,
		IdleTimeout:              time.Minute,
		DropWindow:               time.Second,
		ReplayBufferSize:         256,
	}, red, st)
	if err != nil {
		t.Fatalf("durable.New: %v", err)
	}
	store, err := artifacts.Open(context.Background(), config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("artifacts.Open: %v", err)
	}
	taskReg, err := tasks.Open(context.Background(), tasks.Dependencies{
		Store: st, Bus: bus, Redactor: audit.Redactor(red),
		Cfg: config.TasksConfig{Driver: "inprocess"},
	})
	if err != nil {
		t.Fatalf("tasks.Open: %v", err)
	}
	surface, err := protocol.NewControlSurface(taskReg, steering.NewRegistry())
	if err != nil {
		t.Fatalf("NewControlSurface: %v", err)
	}
	mux, err := transports.NewMux(surface, bus,
		transports.WithoutValidator(),
		transports.WithStateHistory(bus, store),
	)
	if err != nil {
		t.Fatalf("NewMux: %v", err)
	}
	srv := httptest.NewServer(mux)

	// REAL LLM client over the mock driver (dev-posture synthetic usage/cost),
	// on the SAME bus state.history reads back — the R1 cost emit rides the
	// mandatory safety band.
	client, err := llm.Open(context.Background(), llm.ConfigSnapshot{
		Driver:               "mock",
		ContextWindowReserve: 0.05,
		HeavyOutputThreshold: 32 * 1024,
		Model:                "m",
		ModelProfiles: map[string]llm.ModelProfile{
			"m": {ContextWindowTokens: 4000, TokenEstimator: "chars_div_4"},
		},
	}, llm.Deps{Artifacts: store, Bus: bus})
	if err != nil {
		t.Fatalf("llm.Open: %v", err)
	}

	// REAL tool catalog wired with the bus → the descriptor-wrap shell emits
	// tool lifecycle events for every registered tool (R2).
	cat := tools.NewCatalog(tools.WithCatalogBus(bus))

	closfn := func() {
		srv.Close()
		_ = client.Close(context.Background())
		_ = taskReg.Close(context.Background())
		_ = store.Close(context.Background())
		_ = bus.Close(context.Background())
		_ = st.Close(context.Background())
	}
	t.Cleanup(closfn)
	return &p161Stack{srv: srv, bus: bus, llm: client, cat: cat, closfn: closfn}
}

func (s *p161Stack) historyPost(t *testing.T, body string, id identity.Identity) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, s.srv.URL+"/v1/state/history", strings.NewReader(body))
	if id.TenantID != "" {
		req.Header.Set("X-Harbor-Tenant", id.TenantID)
		req.Header.Set("X-Harbor-User", id.UserID)
		req.Header.Set("X-Harbor-Session", id.SessionID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/state/history: %v", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, raw
}

// runQuadCtx stamps the full run quadruple, mirroring the run-loop dispatch
// edge — so every producer's event is turn-attributable.
func runQuadCtx(t *testing.T, id identity.Identity, runID string) context.Context {
	t.Helper()
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	ctx, err = identity.WithRun(ctx, id, runID)
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}
	return ctx
}

// driveTurn runs the three real producers under a run quadruple: an LLM
// completion (cost.recorded), a tool invocation with sentinel args + result
// (tool.invoked/.completed), and a planner.decision emit (the react shape).
func (s *p161Stack) driveTurn(t *testing.T, id identity.Identity, runID string) {
	t.Helper()
	ctx := runQuadCtx(t, id, runID)

	// (a) LLM completion → llm.cost.recorded via the safety wrapper.
	text := "reconstruct my stats please"
	if _, err := s.llm.Complete(ctx, llm.CompleteRequest{
		Model:    "m",
		Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &text}}},
	}); err != nil {
		t.Fatalf("llm.Complete: %v", err)
	}

	// (b) A tool whose ARGS + RESULT both carry the sentinel — the shell's
	//     content-free payloads must never persist either. Transport=MCP so the
	//     read-back proves MCP-transport tools now carry lifecycle events.
	if err := s.cat.Register(tools.ToolDescriptor{
		Tool: tools.Tool{Name: "mcp_echo", Transport: tools.TransportMCP, Source: "mcptest"},
		Invoke: func(_ context.Context, args json.RawMessage) (tools.ToolResult, error) {
			return tools.ToolResult{Value: map[string]any{"echoed": string(args)}}, nil
		},
	}); err != nil {
		t.Fatalf("catalog.Register: %v", err)
	}
	desc, ok := s.cat.Resolve("mcp_echo")
	if !ok {
		t.Fatal("Resolve mcp_echo")
	}
	sentinelArgs := json.RawMessage(fmt.Sprintf(`{"note":%q}`, p161Sentinel))
	if _, err := desc.Invoke(ctx, sentinelArgs); err != nil {
		t.Fatalf("tool Invoke: %v", err)
	}

	// (c) planner.decision (the react payload shape; producer unchanged by this
	//     phase). Carries DecisionKind + Tool — the reduce-relevant keys.
	q := identity.Quadruple{Identity: id, RunID: runID}
	if err := s.bus.Publish(ctx, events.Event{
		Type:       planner.EventTypePlannerDecision,
		Identity:   q,
		OccurredAt: time.Now(),
		Payload: planner.DecisionPayload{
			Identity:       q,
			DecisionKind:   "CallTool",
			Tool:           "mcp_echo",
			ReasoningChars: 4,
			ReasoningTrace: "plan",
			OccurredAt:     time.Now(),
		},
	}); err != nil {
		t.Fatalf("publish planner.decision: %v", err)
	}
}

func (s *p161Stack) readWindow(t *testing.T, id identity.Identity) prototypes.StateHistoryResponse {
	t.Helper()
	status, raw := s.historyPost(t, fmt.Sprintf(`{"session_id":%q,"limit":200}`, id.SessionID), id)
	if status != http.StatusOK {
		t.Fatalf("state.history status=%d body=%s", status, raw)
	}
	var page prototypes.StateHistoryResponse
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatalf("unmarshal page: %v (%s)", err, raw)
	}
	return page
}

// payloadKeys collects the top-level payload keys per event type observed in a
// window (the reduce-relevant "named metadata keys"), plus whether the
// envelope run was populated.
func namedKeySet(page prototypes.StateHistoryResponse) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	for _, ev := range page.Events {
		keys := out[ev.Type]
		if keys == nil {
			keys = map[string]bool{}
			out[ev.Type] = keys
		}
		if ev.Run != "" {
			keys["<run>"] = true
		}
		if m, ok := ev.Payload.(map[string]any); ok {
			for k := range m {
				keys[k] = true
			}
		}
	}
	return out
}

// TestE2E_Phase161_Readback_NamedKeys_And_Redaction is the phase's binding
// integration test: the named content-free keys survive read-back, the run
// quadruple is on the envelope, the sentinel appears NOWHERE, and the read
// path's identity scoping is unchanged.
func TestE2E_Phase161_Readback_NamedKeys_And_Redaction(t *testing.T) {
	s := newP161Stack(t)
	id := identity.Identity{TenantID: "t161", UserID: "u161", SessionID: "s161"}
	const runID = "run-161-a"

	// Subscribe live BEFORE driving so we can compare stream ≡ read-back.
	sub, err := s.bus.Subscribe(context.Background(), events.Filter{
		Admin: true,
		Types: []events.EventType{llm.EventTypeCostRecorded, tools.EventTypeToolInvoked, tools.EventTypeToolCompleted},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	s.driveTurn(t, id, runID)

	// Collect the live stream's {type -> keys} for the cost/tool families.
	liveTypes := map[string]bool{}
	deadline := time.After(2 * time.Second)
	for len(liveTypes) < 3 {
		select {
		case ev := <-sub.Events():
			liveTypes[string(ev.Type)] = true
			if ev.Identity.RunID != runID {
				t.Errorf("live event %s not turn-attributable: RunID=%q", ev.Type, ev.Identity.RunID)
			}
		case <-deadline:
			t.Fatalf("live stream missing families; saw %v", liveTypes)
		}
	}

	page := s.readWindow(t, id)
	keys := namedKeySet(page)

	// (1) Named content-free keys present per family, run on the envelope.
	assertFamily := func(typ string, want ...string) {
		got, ok := keys[typ]
		if !ok {
			t.Fatalf("read-back missing %s events (types present: %v)", typ, keyTypes(keys))
		}
		if !got["<run>"] {
			t.Errorf("%s: envelope run empty in read-back (attribution-dead)", typ)
		}
		for _, k := range want {
			if !got[k] {
				t.Errorf("%s: read-back payload missing key %q (present: %v)", typ, k, keyList(got))
			}
		}
	}
	assertFamily("llm.cost.recorded", "Usage", "Model", "Cost", "ContextWindowTokens")
	assertFamily("tool.invoked", "ToolName", "Transport")
	assertFamily("tool.completed", "ToolName", "Transport", "DurationMS")
	assertFamily("planner.decision", "DecisionKind", "Tool")

	// (2) Sentinel redaction — the tool's args + result never reached ANY
	//     payload on the page (§7 rule 7; content-free by construction).
	if strings.Contains(string(mustMarshal(t, page)), p161Sentinel) {
		t.Fatalf("SENTINEL LEAK: %q appears in the state.history read-back page", p161Sentinel)
	}

	// (3) Stream ≡ read-back for the cost/tool families (same bus events).
	for _, typ := range []string{"llm.cost.recorded", "tool.invoked", "tool.completed"} {
		if !liveTypes[typ] {
			t.Errorf("family %q seen in read-back but not on the live stream", typ)
		}
		if _, ok := keys[typ]; !ok {
			t.Errorf("family %q seen on the live stream but not in read-back", typ)
		}
	}

	// (4) Identity scoping unchanged: cross-tenant read refuses (404); a
	//     missing-identity read is rejected (401).
	other := identity.Identity{TenantID: "t-other", UserID: "u161", SessionID: "s161"}
	status, raw := s.historyPost(t, `{"session_id":"s161","limit":50}`, other)
	if status != http.StatusNotFound {
		t.Fatalf("cross-tenant read = %d, want 404 (no existence leak); body=%s", status, raw)
	}
	assertCodeIs(t, raw, protoerrors.CodeNotFound)

	status, raw = s.historyPost(t, `{"session_id":"s161","limit":50}`, identity.Identity{})
	if status != http.StatusUnauthorized {
		t.Fatalf("missing-identity read = %d, want 401; body=%s", status, raw)
	}
	assertCodeIs(t, raw, protoerrors.CodeIdentityRequired)
}

func keyTypes(m map[string]map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func keyList(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// TestE2E_Phase161_MCPLeg_RealStdioFixture proves R2's tool lifecycle events
// fire for a tool registered through the REAL MCP attach path against the REAL
// stdio MCP fixture (`cmd/harbor-mcptest-stdio`), per §17.8 — the descriptor
// the devstack registered via `catalog.Register` is shelled by the universal
// lifecycle emitter, so invoking it publishes tool.invoked/.completed with the
// MCP transport + the run quadruple, and the sentinel message never leaks into
// a payload. This is the leg a unit fixture cannot rubber-stamp: the tool came
// from a live MCP server's discovery, not a hand-authored descriptor.
func TestE2E_Phase161_MCPLeg_RealStdioFixture(t *testing.T) {
	binPath := buildMCPTestServer(t)

	cfgPath := writeDevConfig(t)
	cfg, err := config.Load(context.Background(), cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	cfg.Tools.MCPServers = []config.MCPServerConfig{
		{Name: "mcptest", TransportMode: "stdio", Command: []string{binPath}},
	}

	stack := devstack.Assemble(t, cfg, devstack.AssembleOpts{
		LLMConfigSnapshot: phase83gMockLLMSnapshot(cfg),
	})
	defer stack.Close()
	if stack.Catalog == nil || stack.Bus == nil {
		t.Fatal("devstack: Catalog/Bus nil")
	}

	desc, ok := stack.Catalog.Resolve("mcptest_echo")
	if !ok {
		t.Fatal("catalog: mcptest_echo not registered by the MCP attach path")
	}
	if desc.Tool.Transport != tools.TransportMCP {
		t.Fatalf("mcptest_echo transport = %q, want mcp", desc.Tool.Transport)
	}

	sub, err := stack.Bus.Subscribe(context.Background(), events.Filter{
		Admin: true,
		Types: []events.EventType{tools.EventTypeToolInvoked, tools.EventTypeToolCompleted},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	devID := identity.Identity{
		TenantID:  devstack.DefaultDevTenant,
		UserID:    devstack.DefaultDevUser,
		SessionID: devstack.DefaultDevSession,
	}
	ctx := runQuadCtx(t, devID, "run-161-mcp")

	args := json.RawMessage(fmt.Sprintf(`{"message":%q}`, p161Sentinel))
	if _, err := desc.Invoke(ctx, args); err != nil {
		t.Fatalf("MCP tool Invoke: %v", err)
	}

	var invoked, completed *events.Event
	deadline := time.After(3 * time.Second)
	for invoked == nil || completed == nil {
		select {
		case ev := <-sub.Events():
			if ev.Type == tools.EventTypeToolInvoked {
				invoked = &ev
			}
			if ev.Type == tools.EventTypeToolCompleted {
				completed = &ev
			}
		case <-deadline:
			t.Fatalf("did not observe tool.invoked+completed for the MCP tool (invoked=%v completed=%v)", invoked != nil, completed != nil)
		}
	}

	// Turn-attributable (the b3 fix): the envelope carries the full quadruple.
	for _, ev := range []*events.Event{invoked, completed} {
		if ev.Identity.RunID != "run-161-mcp" {
			t.Errorf("MCP %s not turn-attributable: RunID=%q", ev.Type, ev.Identity.RunID)
		}
		// Content-free: the sentinel message is never in the lifecycle payload.
		if strings.Contains(string(mustMarshal(t, ev.Payload)), p161Sentinel) {
			t.Fatalf("SENTINEL LEAK in MCP %s payload", ev.Type)
		}
	}
	if p, ok := invoked.Payload.(tools.ToolInvokedPayload); !ok || p.Transport != tools.TransportMCP {
		t.Errorf("tool.invoked payload = %+v, want MCP transport", invoked.Payload)
	}
}
