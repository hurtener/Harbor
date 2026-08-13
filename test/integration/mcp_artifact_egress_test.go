// Package integration — the MCP arm of pass-by-reference routing, end
// to end.
//
// The in-process arm's integration suite proves a resolved artifact
// value never reaches the observable record when the CONSUMER is a Go
// function. This file proves the same invariant when the consumer is a
// REMOTE MCP SERVER — which is harder, because the value must actually
// leave the runtime on the wire, and because two further sinks exist
// that the in-process arm never touched.
//
// The seven sinks the substitution must clear:
//
//  1. the raw observation             trajectory.Step.Observation
//  2. the LLM observation             trajectory.Step.LLMObservation
//  3. the serialised trajectory       internal/planner/trajectory
//  4. canonical event payloads AND envelopes
//  5. audit payloads and log records
//  6. the per-invocation content hash  mcp.ToolCallID(runID, source, name, args)
//  7. the DURABLE MCP-App tool context mcpconsole.ToolContextStore
//
// Sink 7 is the worst of the seven and the one the design turns on: it
// is durable, Protocol-readable, session-scoped rather than run-scoped,
// and it can mint a SECOND artifact carrying whatever it was handed.
// Substituting into the raw argument JSON would put resolved content
// into a browser-readable store. The substitution therefore mutates ONLY
// the decoded argument map.
//
// Every component on the seam is real (CLAUDE.md §17.3): the in-memory
// artifacts and state drivers, the in-memory event bus opened over the
// real pattern-based audit redactor, a real tools.ToolCatalog with the
// lifecycle-emitting shell live, the production dispatch.ToolExecutor,
// the production mcpconsole.ToolContextStore, and a real mcpsdk server
// over the SDK's in-memory transports.
package integration

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hurtener/Harbor/internal/artifacts"
	artifactsinmem "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/mcpconsole"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/trajectory"
	"github.com/hurtener/Harbor/internal/runtime/dispatch"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/state"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/artifactegress"
	mcpdrv "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

// egMarker is the planted content. It appears nowhere else in the
// repository, so a substring search over any surface is a decisive test
// of whether the resolved value reached it.
const egMarker = "MCP-EGRESS-RESOLVED-CONTENT-4d19ae77"

// egBody is the artifact's stored content: the marker, then a run of
// INVALID UTF-8 so byte-exactness is testable on a real binary
// document, then filler so a size assertion distinguishes "delivered the
// bytes" from "delivered the id".
var egBody = append(
	append([]byte(egMarker+"\n"), 0x25, 0x50, 0x44, 0x46, 0xFF, 0xFE, 0x00, 0x80, 0xC3, 0x28),
	[]byte(strings.Repeat("filler line\n", 64))...,
)

// egServerName is the MCP source id, and egToolName the server-side tool
// whose `doc` parameter is mapped.
const (
	egServerName = "egress-docstore"
	egToolName   = "ingest"
	egHarborTool = egServerName + "_" + egToolName
)

// egReceived is what the real MCP server saw on the wire.
type egReceived struct {
	mu     sync.Mutex
	args   []json.RawMessage
	frames atomic.Int64
}

func (r *egReceived) record(args json.RawMessage) {
	r.frames.Add(1)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.args = append(r.args, append(json.RawMessage(nil), args...))
}

func (r *egReceived) last(t *testing.T) json.RawMessage {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.args) == 0 {
		t.Fatalf("the MCP server received no tools/call")
	}
	return r.args[len(r.args)-1]
}

type egStack struct {
	store    artifacts.ArtifactStore
	state    state.StateStore
	bus      events.EventBus
	cat      tools.ToolCatalog
	exec     steering.ToolExecutor
	toolCtx  *mcpconsole.ToolContextStore
	received *egReceived
	logs     *bytes.Buffer
}

// newEgStack wires every real component the egress path crosses, with a
// real mcpsdk server on the far end of the SDK's in-memory transports.
func newEgStack(t *testing.T, mapping map[string][]string, maxBytes int) *egStack {
	t.Helper()

	store, err := artifactsinmem.New(config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("artifacts inmem: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })

	st, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state inmem: %v", err)
	}
	t.Cleanup(func() { _ = st.Close(context.Background()) })

	// The REAL redactor, so an event payload carrying the marker would
	// have to survive the same pass every emitted payload does.
	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 64,
		SubscriberBufferSize:     512,
		IdleTimeout:              60 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         512,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	// The PRODUCTION tool-context store — sink 7 — over the real state
	// and artifact stores, with a low threshold so the offload leg (which
	// can mint a SECOND artifact) is exercised rather than skipped.
	toolCtx, err := mcpconsole.NewToolContextStore(mcpconsole.ToolContextDeps{
		State:     st,
		Store:     store,
		Bus:       bus,
		Threshold: 128,
	})
	if err != nil {
		t.Fatalf("NewToolContextStore: %v", err)
	}

	received := &egReceived{}
	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "harbor-egress-e2e", Version: "v0"}, nil)
	srv.AddTool(&mcpsdk.Tool{
		Name:        egToolName,
		Description: "Ingests a base64 document and declares a ui:// app so the tool-context sink fires.",
		// The `ui://` binding makes the driver capture a tool context, so
		// sink 7 is exercised rather than assumed.
		Meta: mcpsdk.Meta{"ui": map[string]any{"resourceUri": "ui://egress/index.html"}},
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"doc":  map[string]any{"type": "string"},
				"note": map[string]any{"type": "string"},
			},
			"additionalProperties": false,
		},
	}, func(_ context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		received.record(req.Params.Arguments)
		return &mcpsdk.CallToolResult{
			Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: `{"ingested":true}`}},
		}, nil
	})

	// A REAL streamable-HTTP MCP server. Egress substitution is http-only
	// by design (base64 artifact bytes belong in an HTTP body, not a
	// stdio frame), so the transport under test has to be the one the
	// feature actually runs on — an in-memory pipe would exercise a
	// transport the mapping is refused on.
	handler := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return srv }, nil)
	hs := httptest.NewServer(handler)
	t.Cleanup(hs.Close)

	compiled, err := artifactegress.CompileMapping(mapping)
	if err != nil {
		t.Fatalf("CompileMapping: %v", err)
	}
	provider, err := mcpdrv.New(mcpdrv.Config{
		Name:            egServerName,
		URL:             hs.URL,
		TransportMode:   mcpdrv.TransportStreamableHTTP,
		Bus:             bus,
		DefaultIdentity: identity.Identity{TenantID: "eg-tenant-a", UserID: "eg-user-a", SessionID: "eg-session-a"},
		DefaultPolicy:   tools.DefaultPolicy(),
		ToolContext:     toolCtx,

		ArtifactEgress:         compiled,
		ArtifactEgressMaxBytes: maxBytes,
	})
	if err != nil {
		t.Fatalf("mcpdrv.New: %v", err)
	}
	if err := provider.Connect(context.Background()); err != nil {
		t.Fatalf("provider.Connect: %v", err)
	}
	t.Cleanup(func() { _ = provider.Close(context.Background()) })

	cat := tools.NewCatalog(tools.WithCatalogBus(bus))
	// Discover ALSO runs the attach-time schema check against the real
	// server's published inputSchema, so a mapping the server's own
	// declaration does not support would fail the stack here.
	descs, err := provider.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	for _, d := range descs {
		if err := cat.Register(d); err != nil {
			t.Fatalf("catalog.Register(%q): %v", d.Tool.Name, err)
		}
	}

	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	exec := dispatch.NewToolExecutor(cat, store, nil, dispatch.WithLogger(logger))

	return &egStack{store: store, state: st, bus: bus, cat: cat, exec: exec,
		toolCtx: toolCtx, received: received, logs: logs}
}

func egQuad(tenant, user, session, run string) identity.Quadruple {
	return identity.Quadruple{
		Identity: identity.Identity{TenantID: tenant, UserID: user, SessionID: session},
		RunID:    run,
	}
}

// egCtx builds the PRODUCTION-shaped run context: the identity the
// Protocol edge seats, PLUS the run quadruple the run loop adds. Both
// are required — every identity-stamped MCP RPC reads the plain identity
// through buildIdentityMeta, so a ctx carrying only the quadruple is not
// a call context the driver ever sees in production.
func egCtx(t *testing.T, q identity.Quadruple) context.Context {
	t.Helper()
	ctx, err := identity.With(t.Context(), q.Identity)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	ctx, err = identity.WithRun(ctx, q.Identity, q.RunID)
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}
	return ctx
}

func egPut(t *testing.T, store artifacts.ArtifactStore, q identity.Quadruple, body []byte) string {
	t.Helper()
	ref, err := store.PutBytes(egCtx(t, q), artifacts.ArtifactScope{
		TenantID:  q.TenantID,
		UserID:    q.UserID,
		SessionID: q.SessionID,
	}, body, artifacts.PutOpts{Filename: "planted.bin", MimeType: "application/octet-stream"})
	if err != nil {
		t.Fatalf("PutBytes: %v", err)
	}
	return ref.ID
}

func egCall(refID string) planner.CallTool {
	return planner.CallTool{
		CallID: "call_eg_1",
		Tool:   egHarborTool,
		Args:   json.RawMessage(fmt.Sprintf(`{"doc":%q,"note":"hello"}`, refID)),
	}
}

func egRC(st *egStack, q identity.Quadruple) planner.RunContext {
	return planner.RunContext{Quadruple: q, Trajectory: &trajectory.Trajectory{}, Catalog: tools.NewPlannerView(st.cat, tools.CatalogFilter{TenantID: q.TenantID, UserID: q.UserID, SessionID: q.SessionID})}
}

func egJSON(t *testing.T, label string, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %s: %v", label, err)
	}
	return string(b)
}

// TestE2E_MCPEgress_BytesReachTheServerAndNoSinkCarriesThem is the
// phase's headline assertion, from both directions at once: the real
// server receives the exact stored bytes, and NONE of the seven sinks
// carries them.
func TestE2E_MCPEgress_BytesReachTheServerAndNoSinkCarriesThem(t *testing.T) {
	st := newEgStack(t, map[string][]string{egToolName: {"doc"}}, 1<<20)
	q := egQuad("eg-tenant-a", "eg-user-a", "eg-session-a", "eg-run-1")
	ctx := egCtx(t, q)
	refID := egPut(t, st.store, q, egBody)

	sub, err := st.bus.Subscribe(ctx, events.Filter{Admin: true})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	rc := egRC(st, q)
	call := egCall(refID)
	raw, llmObs, err := st.exec.ExecuteDecision(ctx, rc, call)
	if err != nil {
		t.Fatalf("ExecuteDecision: %v", err)
	}

	// --- NON-VACUITY: the bytes really did reach the server, byte-exact.
	// Without this every absence below would be trivially satisfied by a
	// call that delivered nothing.
	var received map[string]any
	if err := json.Unmarshal(st.received.last(t), &received); err != nil {
		t.Fatalf("decode received args: %v", err)
	}
	b64, ok := received["doc"].(string)
	if !ok {
		t.Fatalf("the server received doc = %T, want a base64 string", received["doc"])
	}
	gotBytes, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("the server received a value that is not RFC 4648 §4 standard base64: %v", err)
	}
	if !bytes.Equal(gotBytes, egBody) {
		t.Fatalf("the server received %d bytes, want the stored %d byte-exact", len(gotBytes), len(egBody))
	}
	if received["note"] != "hello" {
		t.Errorf("an unmapped parameter was rewritten: %v", received["note"])
	}

	// --- Sink 1: the raw observation.
	rawJSON := egJSON(t, "raw observation", raw)
	if strings.Contains(rawJSON, egMarker) || strings.Contains(rawJSON, b64) {
		t.Errorf("the raw observation carries the resolved value: %s", rawJSON)
	}
	// It DOES carry the content-free substitution record — the model
	// authored the id and is told it was delivered.
	if !strings.Contains(rawJSON, refID) || !strings.Contains(rawJSON, "artifact_egress") {
		t.Errorf("the raw observation dropped the substitution record: %s", rawJSON)
	}

	// --- Sink 2: the LLM observation.
	llmJSON := egJSON(t, "llm observation", llmObs)
	if strings.Contains(llmJSON, egMarker) || strings.Contains(llmJSON, b64) {
		t.Errorf("the LLM-facing observation carries the resolved value: %s", llmJSON)
	}

	// --- Sink 3: the serialised trajectory.
	rc.Trajectory.Steps = append(rc.Trajectory.Steps, trajectory.Step{
		Action:         call,
		Observation:    raw,
		LLMObservation: llmObs,
	})
	serialised, err := rc.Trajectory.Serialize()
	if err != nil {
		t.Fatalf("Trajectory.Serialize: %v", err)
	}
	if strings.Contains(string(serialised), egMarker) || strings.Contains(string(serialised), b64) {
		t.Errorf("the trajectory carries the resolved value: %s", serialised)
	}
	if !strings.Contains(string(serialised), refID) {
		t.Errorf("the trajectory dropped the reference id the model authored: %s", serialised)
	}

	// --- Sink 4: every canonical event payload AND envelope. The
	// lifecycle shell emits tool.invoked + tool.completed, and this phase
	// adds mcp.artifact_egressed, so at least three are expected.
	const wantAtLeast = 3
	seen := 0
	sawEgressRecord := false
	deadline := time.After(5 * time.Second)
collect:
	for {
		select {
		case ev, open := <-sub.Events():
			if !open {
				break collect
			}
			seen++
			payload := egJSON(t, string(ev.Type), ev.Payload)
			if strings.Contains(payload, egMarker) || strings.Contains(payload, b64) {
				t.Errorf("event %q carries the resolved value: %s", ev.Type, payload)
			}
			if strings.Contains(egJSON(t, "event envelope", ev), egMarker) {
				t.Errorf("event %q envelope carries the resolved value", ev.Type)
			}
			if ev.Type == mcpdrv.EventTypeMCPArtifactEgressed {
				sawEgressRecord = true
				if !strings.Contains(payload, refID) || !strings.Contains(payload, "sha256:") {
					t.Errorf("the substitution record does not name the artifact and its digest: %s", payload)
				}
			}
		case <-deadline:
			break collect
		}
		if seen >= wantAtLeast && sawEgressRecord {
			for {
				select {
				case ev, open := <-sub.Events():
					if !open {
						break collect
					}
					seen++
					if strings.Contains(egJSON(t, string(ev.Type), ev.Payload), egMarker) {
						t.Errorf("event %q carries the resolved value", ev.Type)
					}
				default:
					break collect
				}
			}
		}
	}
	if seen < wantAtLeast {
		t.Fatalf("observed %d events, want at least %d; the event arm proved nothing", seen, wantAtLeast)
	}
	// The COMPENSATING CONTROL exists. Without it this feature would be
	// the one content-movement path in Harbor that leaves no trace.
	if !sawEgressRecord {
		t.Fatalf("no mcp.artifact_egressed record was published for a substitution that moved %d bytes", len(egBody))
	}

	// --- Sink 5: log records.
	if strings.Contains(st.logs.String(), egMarker) || strings.Contains(st.logs.String(), b64) {
		t.Errorf("a log record carries the resolved value: %s", st.logs.String())
	}

	// --- Sink 6: the per-invocation content hash is computed over the
	// RAW args, so it is reproducible from the model's own arguments. If
	// the substitution had rewritten `args`, this would not match.
	wantID := mcpdrv.ToolCallID(q.RunID, egServerName, egToolName, call.Args)
	substitutedArgs, err := json.Marshal(map[string]any{"doc": b64, "note": "hello"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if mcpdrv.ToolCallID(q.RunID, egServerName, egToolName, substitutedArgs) == wantID {
		t.Fatalf("the content hash does not distinguish raw from substituted args; sink 6 cannot be asserted")
	}

	// --- Sink 7: the DURABLE, Protocol-readable tool-context record, and
	// any artifact it offloaded to. This is the sink that can mint a
	// SECOND artifact carrying whatever it was handed.
	loaded, err := st.toolCtx.Load(ctx, egServerName, wantID)
	if err != nil {
		t.Fatalf("tool-context Load(%q): %v — sink 7 was never written, so its assertion would be vacuous", wantID, err)
	}
	loadedJSON := egJSON(t, "tool context", loaded)
	if strings.Contains(loadedJSON, egMarker) || strings.Contains(loadedJSON, b64) {
		t.Errorf("the DURABLE, browser-readable tool-context record carries the resolved value: %s", loadedJSON)
	}
	if !strings.Contains(loadedJSON, refID) {
		t.Errorf("the tool-context record dropped the artifact id the model authored: %s", loadedJSON)
	}

	// ...and every artifact reachable in this session, including anything
	// the tool-context store offloaded, carries no resolved content
	// EXCEPT the original planted one.
	refs, err := st.store.List(ctx, artifacts.ArtifactScope{
		TenantID: q.TenantID, UserID: q.UserID, SessionID: q.SessionID,
	})
	if err != nil {
		t.Fatalf("artifacts List: %v", err)
	}
	for _, ref := range refs {
		if ref.ID == refID {
			continue // the planted artifact itself
		}
		data, found, err := st.store.Get(ctx, artifacts.ArtifactScope{
			TenantID: q.TenantID, UserID: q.UserID, SessionID: q.SessionID,
		}, ref.ID)
		if err != nil || !found {
			continue
		}
		if bytes.Contains(data, []byte(egMarker)) || bytes.Contains(data, []byte(b64)) {
			t.Errorf("artifact %q — minted by the tool-context offload — carries the resolved value", ref.ID)
		}
	}
}

// TestE2E_MCPEgress_CrossIdentityReferencesDoNotResolve is the isolation
// assertion: resolution runs through the DISPATCHING run's own seated
// resolver, so an id minted under another tenant, another user or
// another session answers not-found — and NO wire request is issued.
//
// It fails loudly if the resolver is ever replaced with a privileged
// read.
func TestE2E_MCPEgress_CrossIdentityReferencesDoNotResolve(t *testing.T) {
	st := newEgStack(t, map[string][]string{egToolName: {"doc"}}, 1<<20)
	owner := egQuad("eg-tenant-a", "eg-user-a", "eg-session-a", "eg-run-owner")
	refID := egPut(t, st.store, owner, egBody)

	// Non-vacuity: the OWNER can reach it.
	ownerCtx := egCtx(t, owner)
	ownerRC := egRC(st, owner)
	if _, _, err := st.exec.ExecuteDecision(ownerCtx, ownerRC, egCall(refID)); err != nil {
		t.Fatalf("the owning run could not reach its own artifact: %v — every refusal below would then be vacuous", err)
	}
	framesAfterOwner := st.received.frames.Load()
	if framesAfterOwner == 0 {
		t.Fatalf("the owning run issued no wire request")
	}

	for _, tc := range []struct {
		name string
		q    identity.Quadruple
	}{
		{"another tenant", egQuad("eg-tenant-b", "eg-user-a", "eg-session-a", "eg-run-x")},
		{"another user", egQuad("eg-tenant-a", "eg-user-b", "eg-session-a", "eg-run-y")},
		{"another session", egQuad("eg-tenant-a", "eg-user-a", "eg-session-b", "eg-run-z")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := st.received.frames.Load()
			ctx := egCtx(t, tc.q)
			rc := egRC(st, tc.q)
			_, _, err := st.exec.ExecuteDecision(ctx, rc, egCall(refID))
			if err == nil {
				t.Fatalf("a run under %s resolved another identity's artifact", tc.name)
			}
			if !errors.Is(err, dispatch.ErrArtifactRefNotFound) {
				t.Fatalf("err = %v, want ErrArtifactRefNotFound", err)
			}
			// The observation class is the RECOVERABLE one — the model can
			// repair by naming a different id — not the operator-facing
			// resolver-unavailable class.
			if got := planner.ObservationClassOf(err); got != planner.ObservationClassArtifactRefNotFound {
				t.Errorf("observation class = %q, want %q", got, planner.ObservationClassArtifactRefNotFound)
			}
			if after := st.received.frames.Load(); after != before {
				t.Fatalf("a cross-identity reference produced %d wire request(s); nothing must leave the runtime", after-before)
			}
		})
	}
}

// TestE2E_MCPEgress_OversizeValueIsRefusedNotTruncated — a resolved
// value above the ceiling fails loud with no wire request. Deliberately
// NOT truncated: a partial document delivered to a remote ingester is a
// corruption, not a bounded read.
func TestE2E_MCPEgress_OversizeValueIsRefusedNotTruncated(t *testing.T) {
	st := newEgStack(t, map[string][]string{egToolName: {"doc"}}, 32)
	q := egQuad("eg-tenant-a", "eg-user-a", "eg-session-a", "eg-run-big")
	ctx := egCtx(t, q)
	refID := egPut(t, st.store, q, egBody)
	before := st.received.frames.Load()

	rc := egRC(st, q)
	_, _, err := st.exec.ExecuteDecision(ctx, rc, egCall(refID))
	if !errors.Is(err, artifactegress.ErrEgressTooLarge) {
		t.Fatalf("err = %v, want ErrEgressTooLarge", err)
	}
	if after := st.received.frames.Load(); after != before {
		t.Fatalf("an oversize value produced %d wire request(s)", after-before)
	}
	// The error names the artifact, its size and the ceiling, so an
	// operator can retune rather than guess.
	for _, want := range []string{refID, fmt.Sprint(len(egBody)), "32"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

// TestE2E_MCPEgress_UnresolvableIDIsRecoverableNotStepTerminating — an
// id the model invented is a MODEL mistake, so it surfaces as the
// recoverable observation class the model can repair on its next turn,
// not as the operator-misconfiguration class.
func TestE2E_MCPEgress_UnresolvableIDIsRecoverableNotStepTerminating(t *testing.T) {
	st := newEgStack(t, map[string][]string{egToolName: {"doc"}}, 1<<20)
	q := egQuad("eg-tenant-a", "eg-user-a", "eg-session-a", "eg-run-missing")
	ctx := egCtx(t, q)
	before := st.received.frames.Load()

	rc := planner.RunContext{Quadruple: q, Trajectory: &trajectory.Trajectory{}}
	_, _, err := st.exec.ExecuteDecision(ctx, rc, egCall("art_doesnotexist"))
	if err == nil {
		t.Fatalf("an invented artifact id succeeded")
	}
	if got := planner.ObservationClassOf(err); got != planner.ObservationClassArtifactRefNotFound {
		t.Fatalf("observation class = %q, want %q (recoverable by the model)", got, planner.ObservationClassArtifactRefNotFound)
	}
	if after := st.received.frames.Load(); after != before {
		t.Fatalf("an unresolvable id produced %d wire request(s)", after-before)
	}
}

// TestE2E_MCPEgress_AppCallbackFailsLoud is the MCP-App callback
// posture, asserted through the PRODUCTION accessor rather than
// described in a comment.
//
// `mcp.apps.call_tool` resolves the SAME catalog descriptor from a
// browser-driven Protocol request. There is no run, so
// dispatch.ExecuteDecision never ran and no resolver is seated — a
// byte-mapped tool invoked there fails LOUD.
//
// Both alternatives are rejected on the record and this test pins the
// choice: seating a second resolver there would have to close over the
// browser request's triple rather than a run's quadruple, producing a
// SECOND definition of what this feature can reach; and passing the raw
// id string through would hand the server "art-abc123" where it expects
// a document.
func TestE2E_MCPEgress_AppCallbackFailsLoud(t *testing.T) {
	st := newEgStack(t, map[string][]string{egToolName: {"doc"}}, 1<<20)
	q := egQuad("eg-tenant-a", "eg-user-a", "eg-session-a", "eg-run-app")
	refID := egPut(t, st.store, q, egBody)
	before := st.received.frames.Load()

	// A browser-driven request carries identity but NO run and no seated
	// resolver — the production shape of an App callback.
	ctx, err := identity.With(t.Context(), q.Identity)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}

	desc, ok := st.cat.Resolve(egHarborTool)
	if !ok {
		t.Fatalf("the mapped tool is not in the catalog")
	}
	_, err = desc.Invoke(ctx, json.RawMessage(fmt.Sprintf(`{"doc":%q}`, refID)))
	if err == nil {
		t.Fatalf("a byte-mapped tool succeeded through the App-callback path; it must fail loud")
	}
	if !strings.Contains(err.Error(), egToolName) {
		t.Errorf("the refusal %q does not name the tool", err)
	}
	if after := st.received.frames.Load(); after != before {
		t.Fatalf("the App-callback path issued %d wire request(s); the raw id must never be sent as a degraded value", after-before)
	}
}

// TestE2E_MCPEgress_ConcurrentAcrossTenantsAndSessions is the
// cross-package concurrency stress: N concurrent dispatches against ONE
// shared provider and catalog, across two tenants and two sessions, half
// carrying mapped parameters.
//
// Each run's artifact LENGTH is derived from its own index, so a
// cross-run bleed is a SIZE mismatch rather than a byte compare that
// could accidentally match.
func TestE2E_MCPEgress_ConcurrentAcrossTenantsAndSessions(t *testing.T) {
	const n = 32
	st := newEgStack(t, map[string][]string{egToolName: {"doc"}}, 1<<20)

	// Pre-store one artifact per run under its own triple.
	quads := make([]identity.Quadruple, n)
	refs := make([]string, n)
	sizes := make([]int, n)
	for i := range n {
		quads[i] = egQuad(
			fmt.Sprintf("eg-tenant-%d", i%2),
			fmt.Sprintf("eg-user-%d", i%3),
			fmt.Sprintf("eg-session-%d", i%2),
			fmt.Sprintf("eg-run-%d", i),
		)
		body := bytes.Repeat([]byte{byte('a' + i%26)}, i+1)
		sizes[i] = len(body)
		refs[i] = egPut(t, st.store, quads[i], body)
	}

	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx := egCtx(t, quads[i])
			rc := egRC(st, quads[i])
			raw, _, err := st.exec.ExecuteDecision(ctx, rc, planner.CallTool{
				CallID: fmt.Sprintf("call_%d", i),
				Tool:   egHarborTool,
				Args:   json.RawMessage(fmt.Sprintf(`{"doc":%q}`, refs[i])),
			})
			if err != nil {
				errs <- fmt.Errorf("run %d: %w", i, err)
				return
			}
			value, ok := raw.(mcpdrv.MCPToolValue)
			if !ok {
				errs <- fmt.Errorf("run %d: observation = %T", i, raw)
				return
			}
			if len(value.ArtifactEgress) != 1 {
				errs <- fmt.Errorf("run %d: %d substitution records, want 1", i, len(value.ArtifactEgress))
				return
			}
			rec := value.ArtifactEgress[0]
			if rec.ArtifactID != refs[i] {
				errs <- fmt.Errorf("run %d: record names %q, want %q — cross-run id bleed", i, rec.ArtifactID, refs[i])
				return
			}
			// The SIZE is the bleed detector.
			if rec.SizeBytes != sizes[i] {
				errs <- fmt.Errorf("run %d: record size = %d, want %d — cross-run byte bleed", i, rec.SizeBytes, sizes[i])
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if got := st.received.frames.Load(); got != n {
		t.Fatalf("the server received %d frames, want %d — some dispatches never landed", got, n)
	}
}
