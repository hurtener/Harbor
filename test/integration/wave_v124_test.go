package integration_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/agentcfg/projection"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
	mcpdrv "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

// wave_v124_test.go — the wave-end end-to-end smoke for the v1.24 wave
// (CLAUDE.md §17.7 step 5). It composes the wave's shipped surfaces across ONE
// stack of REAL drivers rather than testing each in isolation, because the two
// bug classes a wave-end smoke exists to catch — cross-package wiring gaps and
// cross-subsystem concurrency interactions — are invisible to per-phase tests.
//
// Surfaces composed here, and what each leg proves about the SEAM rather than
// the feature:
//
//   - Run-start connection re-attach: a connection an agent's active revision
//     declares comes back at the next run start of a fresh runtime, and its tools
//     are callable — the wave's newest primitive, exercised end to end.
//   - Caller-named agent selection: the reconcile legs are owner-scoped by the
//     RUN's effective agent, so two agents under one tenant reconcile their OWN
//     declared sets. This is the seam between the two phases: the re-attach leg
//     inherits its owner from the agent the run resolved.
//   - `meta_annotations` `_meta` path nesting: an annotation set declared on a
//     re-attached connection survives the re-attach and reaches the wire, so the
//     nesting phase's surface is not silently dropped by the attach path that
//     rebuilds the connection.
//   - The split heavy-content thresholds: the two constants answer DIFFERENT
//     questions and must not be re-aliased by a later edit.
//
// Every leg asserts identity propagation, at least one FAILURE mode, and the
// whole composes under an N≥10 concurrency stress with the goroutine baseline
// checked (§17.3).

const waveV124Agent2 = "agent-ra-wave-b"

// httptestServerWrapper pairs a fixture server with a reader for the LAST
// `_meta` its tool handler observed, so an annotation's arrival on the wire is
// asserted from the SERVER's side rather than inferred from the client's config.
type httptestServerWrapper struct {
	*httptest.Server
	meta func() map[string]any
}

// waveV124Fixture spins a spec-derived MCP server (official go-sdk) that ECHOES
// the `_meta` it received back as text, so an annotation's arrival on the wire is
// observable end to end rather than inferred (§17.8).
func waveV124Fixture(t *testing.T) *httptestServerWrapper {
	t.Helper()
	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "harbor-wave-v124-fixture", Version: "v0"}, nil)
	var (
		mu       sync.Mutex
		lastMeta map[string]any
	)
	mcpsdk.AddTool(srv,
		&mcpsdk.Tool{
			Name:        "echo",
			Description: "echo",
			InputSchema: map[string]any{
				"type":                 "object",
				"properties":           map[string]any{"text": map[string]any{"type": "string"}},
				"additionalProperties": false,
			},
		},
		func(_ context.Context, req *mcpsdk.CallToolRequest, in struct {
			Text string `json:"text"`
		}) (*mcpsdk.CallToolResult, any, error) {
			mu.Lock()
			lastMeta = map[string]any{}
			for k, v := range req.Params.GetMeta() {
				lastMeta[k] = v
			}
			mu.Unlock()
			return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: in.Text}}}, nil, nil
		},
	)
	hs := httptest.NewServer(mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return srv }, nil))
	return &httptestServerWrapper{Server: hs, meta: func() map[string]any {
		mu.Lock()
		defer mu.Unlock()
		out := map[string]any{}
		for k, v := range lastMeta {
			out[k] = v
		}
		return out
	}}
}

// TestE2E_WaveV124_ComposedSurface is the wave-end smoke.
func TestE2E_WaveV124_ComposedSurface(t *testing.T) {
	// Fixtures FIRST — the harness's cleanup drains the attacher's live
	// transports and must run before httptest waits on those connections.
	fixA := waveV124Fixture(t)
	t.Cleanup(fixA.Close)
	fixB := waveV124Fixture(t)
	t.Cleanup(fixB.Close)

	h := newRaHarness(t)

	// ---------------------------------------------------------------------
	// Leg 1 — two agents under ONE tenant each declare their own connection.
	// Agent A's carries a nested `_meta` annotation path; agent B's does not.
	// ---------------------------------------------------------------------
	const annotationPath = "harbor.deployment.tier"
	const annotationValue = "wave-v124"
	if _, err := h.cfgReg.SetRevision(context.Background(), identity.Quadruple{Identity: raID()},
		raAgent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{
			Connections: &agentcfg.ConnectionsSection{Servers: []agentcfg.MCPConnectionDescriptor{{
				Name: "wave-a", Transport: agentcfg.MCPTransportHTTP, URL: fixA.URL,
				MetaAnnotations: map[string]string{annotationPath: annotationValue},
			}}},
		}); err != nil {
		t.Fatalf("seed agent A: %v", err)
	}
	seedAgentConnection(t, h, waveV124Agent2, "wave-b", fixB.URL)

	// A FRESH runtime over the same spine: nothing is live.
	h.bootRuntimeSide(t)
	if _, exists := h.mcpReg.OwnerOf("wave-a"); exists {
		t.Fatal("the rebuilt runtime is not actually fresh")
	}

	// ---------------------------------------------------------------------
	// Leg 2 — each agent's run start re-attaches its OWN connection, under its
	// OWN owner tag. This is the caller-named-agent seam meeting the re-attach
	// leg: the owner comes from the agent the RUN resolved.
	// ---------------------------------------------------------------------
	quadA := identity.Quadruple{Identity: raID(), RunID: "wave-run-a"}
	quadB := identity.Quadruple{Identity: raID(), RunID: "wave-run-b"}

	if _, attached, err := h.reconcile(t, quadA, raAgent); err != nil || attached != 1 {
		t.Fatalf("agent A run start: attached=%d err=%v, want 1,nil", attached, err)
	}
	if _, attached, err := h.reconcile(t, quadB, waveV124Agent2); err != nil || attached != 1 {
		t.Fatalf("agent B run start: attached=%d err=%v, want 1,nil", attached, err)
	}

	// IDENTITY PROPAGATION: each registration carries its own (tenant, agent).
	for name, agent := range map[string]string{"wave-a": raAgent, "wave-b": waveV124Agent2} {
		owner, ok := h.mcpReg.OwnerOf(name)
		if !ok {
			t.Fatalf("%q did not come back", name)
		}
		if want := (toolauth.Owner{Tenant: raTenant, Agent: agent}); owner != want {
			t.Fatalf("%q owner = %+v, want %+v — the re-attach leg did not inherit the run's effective agent",
				name, owner, want)
		}
	}
	// Agent A's sweep must never have attached agent B's connection or vice
	// versa: each declared set is its own.
	if _, _, err := h.reconcile(t, quadA, raAgent); err != nil {
		t.Fatalf("agent A second run start: %v", err)
	}
	if owner, _ := h.mcpReg.OwnerOf("wave-b"); owner.Agent != waveV124Agent2 {
		t.Fatalf("agent A's sweep took over agent B's connection: owner=%+v", owner)
	}

	// ---------------------------------------------------------------------
	// Leg 3 — the re-attached connection is genuinely LIVE, and the
	// `meta_annotations` declared on it survive the re-attach and reach the wire
	// as a NESTED `_meta` path. A phase that rebuilds a connection must not
	// silently drop the surface a sibling phase added to it.
	// ---------------------------------------------------------------------
	d, ok := h.catalog.Resolve("wave-a_echo")
	if !ok {
		t.Fatal("the re-attached connection's tool is not in the catalog")
	}
	callCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	callCtx, idErr := identity.With(callCtx, raID())
	if idErr != nil {
		t.Fatalf("identity.With: %v", idErr)
	}
	if _, err := d.Invoke(callCtx, []byte(`{"text":"wave"}`)); err != nil {
		t.Fatalf("calling the re-attached tool: %v", err)
	}
	meta := fixA.meta()
	if len(meta) == 0 {
		t.Fatal("the fixture observed no `_meta` at all — the identity stamp did not reach the wire")
	}
	head := strings.SplitN(annotationPath, ".", 2)[0]
	nested, present := meta[head]
	if !present {
		t.Fatalf("the declared annotation's root segment %q is absent from the wire `_meta` (saw keys %v) — "+
			"the re-attach dropped the annotation set", head, metaKeys(meta))
	}
	if _, flat := meta[annotationPath]; flat {
		t.Fatalf("the dotted annotation key reached the wire FLAT (%q) instead of nested — the nesting "+
			"surface regressed through the re-attach path", annotationPath)
	}
	if !strings.Contains(fmt.Sprint(nested), annotationValue) {
		t.Fatalf("the nested annotation node %v does not carry the declared value %q", nested, annotationValue)
	}

	// ---------------------------------------------------------------------
	// Leg 4 — FAILURE MODE. Agent B's third party goes away. Its run start
	// fails loud, reports the outcome on the canonical event, and does NOT
	// disturb agent A's live connection or the sweep's other work.
	// ---------------------------------------------------------------------
	coll := newRaCollector(t, h.bus, raID())
	// Detach B first so the next sweep treats it as declared-but-absent.
	if err := h.detacher.Detach(context.Background(), "wave-b",
		toolauth.Owner{Tenant: raTenant, Agent: waveV124Agent2}); err != nil {
		t.Fatalf("detach B: %v", err)
	}
	fixB.Close()

	_, attached, err := h.reconcile(t, quadB, waveV124Agent2)
	if err == nil {
		t.Fatal("an unreachable declared connection must fail LOUD")
	}
	if !errors.Is(err, projection.ErrReconcileReattach) {
		t.Fatalf("err = %v, want it marked as a re-attach failure so the caller can classify it", err)
	}
	if attached != 0 {
		t.Fatalf("attached = %d, want 0", attached)
	}
	seen := coll.await(t, "the reattach_failed event", func(e []events.Event) bool {
		return len(raPayloads(e, agentcfg.EventTypeMCPConnectionReattachFailed)) >= 1
	})
	p := raPayloads(seen, agentcfg.EventTypeMCPConnectionReattachFailed)[0]
	if p.State != agentcfg.MCPReattachClassTransportFailed {
		t.Fatalf("failure class = %q, want %q", p.State, agentcfg.MCPReattachClassTransportFailed)
	}
	if p.Author.RunID != quadB.RunID {
		t.Fatalf("the failure event carries RunID %q, want the reconciling run's %q", p.Author.RunID, quadB.RunID)
	}
	// Agent A is untouched by agent B's failure — the cross-owner blast radius.
	if _, ok := h.catalog.Resolve("wave-a_echo"); !ok {
		t.Fatal("agent B's failed re-attach disturbed agent A's live connection")
	}

	// ---------------------------------------------------------------------
	// Leg 5 — the split heavy-content thresholds answer DIFFERENT questions.
	// A later edit that re-aliases them fails here rather than silently
	// reshaping every Protocol reply a browser renders.
	// ---------------------------------------------------------------------
	if config.DefaultHeavyOutputThresholdBytes == config.DefaultConsoleInlinePayloadBytes {
		t.Fatal("the LLM-context threshold and the Console inline-payload bound were re-aliased; " +
			"they price different things and must be pinned independently")
	}
	if config.DefaultConsoleInlinePayloadBytes >= config.DefaultHeavyOutputThresholdBytes {
		t.Fatalf("the Console inline bound (%d) is not below the LLM-context threshold (%d)",
			config.DefaultConsoleInlinePayloadBytes, config.DefaultHeavyOutputThresholdBytes)
	}

	// ---------------------------------------------------------------------
	// Leg 6 — CONCURRENCY STRESS across the composed surface: N interleaved run
	// starts for BOTH agents against the ONE shared attacher + registry.
	// ---------------------------------------------------------------------
	const N = 12
	var wg sync.WaitGroup
	errCh := make(chan error, 2*N)
	for range N {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if _, _, err := h.reconcile(t, quadA, raAgent); err != nil {
				errCh <- fmt.Errorf("agent A: %w", err)
			}
		}()
		go func() {
			defer wg.Done()
			// Agent B's third party is DOWN, so these sweeps fail or are
			// suppressed by the retry window. Neither may disturb agent A, and
			// neither may fail with anything but a marked re-attach error.
			if _, _, err := h.reconcile(t, quadB, waveV124Agent2); err != nil &&
				!errors.Is(err, projection.ErrReconcileReattach) {
				errCh <- fmt.Errorf("agent B produced an unexpected error class: %w", err)
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for e := range errCh {
		t.Fatalf("concurrent composed sweep: %v", e)
	}

	// Agent A survived the storm: exactly one live registration, under its own
	// owner, with its tool still callable.
	servers, _, lerr := h.mcpReg.ListServers(mustIdentityCtx(t, raTenant, raUser, raSession), mcpdrv.ListFilter{})
	if lerr != nil {
		t.Fatalf("ListServers: %v", lerr)
	}
	counts := map[string]int{}
	for _, s := range servers {
		counts[s.Name]++
	}
	if counts["wave-a"] != 1 {
		t.Fatalf("wave-a registrations = %d after %d concurrent run starts, want exactly 1", counts["wave-a"], N)
	}
	if counts["wave-b"] != 0 {
		t.Fatalf("wave-b registrations = %d, want 0 (its third party is down)", counts["wave-b"])
	}
	if _, ok := h.catalog.Resolve("wave-a_echo"); !ok {
		t.Fatal("agent A's tool did not survive the concurrent storm")
	}
	if owner, ok := h.mcpReg.OwnerOf("wave-a"); !ok ||
		owner != (toolauth.Owner{Tenant: raTenant, Agent: raAgent}) {
		t.Fatalf("wave-a owner = %+v ok=%t after the storm, want agent A's own tag", owner, ok)
	}

	// The reattacher is a compiled artifact shared across every run above; its
	// only mutable state is the retry table, guarded by the attach lock. The
	// -race detector is the gate for that claim and runs over this whole test.
	var _ projection.ConnectionReattacher = h.attacher
}

// metaKeys returns the observed `_meta` keys for a failure message.
func metaKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
