// wave_v124_test.go — the wave-end end-to-end smoke for the v1.24 wave
// (CLAUDE.md §17.7 step 5), rebuilt after the §17.5 checkpoint audit found the
// first cut was a partial shell: it would have compiled and passed verbatim
// after reverting one of the phases it claimed to exercise, it never performed
// the restart the re-attach phase exists for, it carried one identity triple
// across six legs, and two of its guards compared compile-time constants.
//
// What this file composes, and what each leg proves about the SEAM rather than
// the feature:
//
//   - Caller-named agent selection → run-start re-attach. A `start` naming
//     agent A is dispatched through the REAL ControlSurface with the
//     production AgentResolver, runs through the REAL RunLoopDriver, and the
//     connection that comes back carries the NAMED agent's owner tag. Nothing
//     hands the agent id to the reconcile by hand — that is the whole seam.
//   - A GENUINE restart. The connection is attached and proven live BEFORE the
//     runtime side is rebuilt, so the "did it come back" assertions are about
//     something that actually went away.
//   - `meta_annotations` `_meta` path nesting survives the rebuild, asserted by
//     walking EVERY declared segment rather than the first dot.
//   - Egress substitution survives the rebuild. A byte-eligible connection,
//     re-attached, still puts artifact BYTES in the outbound body — the
//     declaration a sibling phase added to the descriptor must not be silently
//     dropped by the path that rebuilds the connection. The deployment's
//     lowered ceiling survives it too.
//   - The split heavy-content thresholds, asserted BEHAVIOURALLY: one payload
//     in the band between them inlines at the LLM edge and offloads on the
//     Protocol reply.
//   - Multi-isolation: two tenants x two users, with negative assertions — one
//     owner's reconcile cannot reach another's registration, an identity-less
//     read fails closed, and a caller cannot name a foreign tenant's agent.
//
// Every leg ships at least one FAILURE mode, and the composed surface closes
// under an N>=12 concurrency stress with a settle-looped goroutine baseline
// (§17.3).
package integration_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	goruntime "runtime"
	"strings"
	"sync"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hurtener/Harbor/internal/agentcfg"
	_ "github.com/hurtener/Harbor/internal/agentcfg/drivers/statestore"
	"github.com/hurtener/Harbor/internal/artifacts"
	_ "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/dispatch"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/runtime/serve"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tasks"
	_ "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/artifactegress"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
	mcpdrv "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

// The wave's identity surface: TWO tenants and, inside the first, TWO users.
// `agent_id` is deliberately NOT part of this set — CLAUDE.md §6 states that an
// agent is not an isolation principal, so a test that varied only the agent
// would assert nothing about multi-isolation.
const (
	wvTenantA = "wv124-tenant-a"
	wvTenantB = "wv124-tenant-b"
	wvUserA1  = "wv124-user-a1"
	wvUserA2  = "wv124-user-a2"
	wvUserB1  = "wv124-user-b1"

	// wvBootAgent is the runtime's boot-configured agent — the id a run
	// falls back to when the caller names none. Every owner-tag assertion
	// below is only meaningful because it differs from the named agents.
	wvBootAgent = "wv124-boot-agent"
	wvAgentA    = "wv124-agent-alpha"
	wvAgentB    = "wv124-agent-bravo"

	// wvEgressCeiling is a deliberately LOW deployment ceiling, so a
	// re-attach that drops it reverts to the 8 MiB default and the oversize
	// refusal below stops firing.
	wvEgressCeiling = 4096
)

// wvMarker is planted inside the artifact body. It appears nowhere else in the
// repository, so finding it in the outbound argument object is decisive.
const wvMarker = "WAVE-V124-EGRESS-BYTES-6b1f2c9e"

// wvBody is the artifact content: the marker, then INVALID UTF-8 so a Go-string
// round-trip would corrupt it, then filler so a size check separates "delivered
// the bytes" from "delivered the id".
var wvBody = append(
	append([]byte(wvMarker+"\n"), 0x25, 0x50, 0x44, 0x46, 0xFF, 0xFE, 0x00, 0x80, 0xC3, 0x28),
	[]byte(strings.Repeat("wave filler\n", 24))...,
)

// ---------------------------------------------------------------------------
// The spec-derived MCP fixture (§17.8: the official go-sdk, never a
// hand-authored transcript).
// ---------------------------------------------------------------------------

// wvFixture is one fixture server plus readers for what its tool handler
// actually observed, so every arrival assertion is made SERVER-side.
type wvFixture struct {
	*httptest.Server
	mu       sync.Mutex
	lastMeta map[string]any
	lastArgs json.RawMessage
	calls    int
}

func (f *wvFixture) meta() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]any{}
	for k, v := range f.lastMeta {
		out[k] = v
	}
	return out
}

func (f *wvFixture) args(t *testing.T) map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.lastArgs) == 0 {
		t.Fatal("the fixture server received no tools/call")
	}
	var out map[string]any
	if err := json.Unmarshal(f.lastArgs, &out); err != nil {
		t.Fatalf("decode the received arguments: %v", err)
	}
	return out
}

func (f *wvFixture) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// newWVFixture spins a go-sdk MCP server behind streamable HTTP. Its one tool
// declares a string `doc` parameter (the egress mapping target — the attach's
// schema check requires the server to have PUBLISHED it, string-typed) and
// records both the `_meta` and the raw argument object it received.
func newWVFixture(t *testing.T) *wvFixture {
	t.Helper()
	f := &wvFixture{}
	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "harbor-wave-v124-fixture", Version: "v0"}, nil)
	srv.AddTool(&mcpsdk.Tool{
		Name:        "ingest",
		Description: "records the arguments and the _meta it received",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"doc":  map[string]any{"type": "string"},
				"note": map[string]any{"type": "string"},
			},
			"additionalProperties": false,
		},
	}, func(_ context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		f.mu.Lock()
		f.calls++
		f.lastMeta = map[string]any{}
		for k, v := range req.Params.GetMeta() {
			f.lastMeta[k] = v
		}
		if raw, err := json.Marshal(req.Params.Arguments); err == nil {
			f.lastArgs = raw
		}
		f.mu.Unlock()
		return &mcpsdk.CallToolResult{
			Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: `{"ingested":true}`}},
		}, nil
	})
	f.Server = httptest.NewServer(
		mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return srv }, nil))
	return f
}

// ---------------------------------------------------------------------------
// The rig: the whole composed stack, with the runtime side rebuildable so a
// RESTART can be modelled rather than described.
// ---------------------------------------------------------------------------

type wvRig struct {
	state    state.StateStore
	bus      events.EventBus
	cfg      agentcfg.Registry
	tasks    tasks.TaskRegistry
	surface  *protocol.ControlSurface
	art      artifacts.ArtifactStore
	observer *wvObserver

	// The process-local runtime side — everything a restart destroys.
	catalog  tools.ToolCatalog
	mcpReg   *mcpdrv.Registry
	attacher *serve.MCPConnectionAttacher
	detacher *serve.MCPConnectionDetacher
	driver   *serve.RunLoopDriver
	exec     steering.ToolExecutor
}

// wvObserver is the planner: it records, per run id, the identity the run was
// seated under, and signals arrival. A run that reached the planner has already
// completed run-start reconciliation (the run loop reconciles BEFORE it
// projects the catalog), which is what makes awaiting it a valid barrier.
// It hands each waiter its OWN closed-on-arrival channel rather than a shared
// signal channel: a shared best-effort signal drops wakeups the moment N
// concurrent runs land at once, which turns a passing stress into a timeout
// that looks like a hang in the code under test.
type wvObserver struct {
	mu      sync.Mutex
	seen    map[string]identity.Quadruple
	waiters map[string]chan struct{}
}

func (o *wvObserver) Next(_ context.Context, rc planner.RunContext) (planner.Decision, error) {
	o.mu.Lock()
	o.seen[rc.Quadruple.RunID] = rc.Quadruple
	if ch, ok := o.waiters[rc.Quadruple.RunID]; ok {
		close(ch)
		delete(o.waiters, rc.Quadruple.RunID)
	}
	o.mu.Unlock()
	return planner.Finish{Reason: planner.FinishGoal}, nil
}

func (o *wvObserver) get(runID string) (identity.Quadruple, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	q, ok := o.seen[runID]
	return q, ok
}

// arrived returns a channel closed once runID has reached the planner. Already
// arrived returns an already-closed channel, so there is no lost-wakeup window
// between the dispatch and the wait.
func (o *wvObserver) arrived(runID string) <-chan struct{} {
	o.mu.Lock()
	defer o.mu.Unlock()
	ch := make(chan struct{})
	if _, ok := o.seen[runID]; ok {
		close(ch)
		return ch
	}
	if existing, ok := o.waiters[runID]; ok {
		return existing
	}
	o.waiters[runID] = ch
	return ch
}

func newWVRig(t *testing.T) *wvRig {
	t.Helper()
	red := auditpatterns.New()

	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver: "inmem", MaxSubscribersPerSession: 64, SubscriberBufferSize: 1024,
		IdleTimeout: time.Minute, DropWindow: time.Second, ReplayBufferSize: 1024,
	}, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}

	st, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}

	cfgReg, err := agentcfg.Open(context.Background(), agentcfg.Config{},
		agentcfg.Deps{State: st, Bus: bus})
	if err != nil {
		t.Fatalf("agentcfg.Open: %v", err)
	}

	taskReg, err := tasks.Open(context.Background(), tasks.Dependencies{
		Store: st, Bus: bus, Redactor: red, Cfg: config.TasksConfig{Driver: "inprocess"},
	})
	if err != nil {
		t.Fatalf("tasks.Open: %v", err)
	}

	art, err := artifacts.Open(context.Background(), config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("artifacts.Open: %v", err)
	}

	// The PRODUCTION agent resolver: a caller-named agent is admitted only
	// when the CALLER's own tenant carries an admin revision for it.
	surface, err := protocol.NewControlSurface(taskReg, steering.NewRegistry(),
		protocol.WithAgentResolver(serve.NewAgentResolverAdapter(cfgReg, wvBootAgent)),
		protocol.WithAgentReachAuthorizer(auth.NewAgentReachAuthorizer()))
	if err != nil {
		t.Fatalf("NewControlSurface: %v", err)
	}

	r := &wvRig{
		state: st, bus: bus, cfg: cfgReg, tasks: taskReg, surface: surface, art: art,
		observer: &wvObserver{
			seen:    map[string]identity.Quadruple{},
			waiters: map[string]chan struct{}{},
		},
	}
	// Registered AFTER every fixture's own Close (callers build fixtures
	// FIRST): cleanups run LIFO, so the attacher drains its live transports
	// here BEFORE httptest waits on those same connections.
	t.Cleanup(func() {
		r.closeRuntimeSide()
		_ = art.Close(context.Background())
		_ = taskReg.Close(context.Background())
		_ = cfgReg.Close(context.Background())
		_ = st.Close(context.Background())
		_ = bus.Close(context.Background())
	})
	r.boot(t)
	return r
}

func (r *wvRig) closeRuntimeSide() {
	if r.driver != nil {
		_ = r.driver.Close(context.Background())
		r.driver = nil
	}
	if r.attacher != nil {
		_ = r.attacher.Close(context.Background())
		r.attacher = nil
	}
}

// boot builds (or REBUILDS) the process-local runtime side over the SAME state
// store and agent-config registry. Calling it a second time IS the restart:
// every live registration, every live transport and every catalog entry is
// gone; only the revision spine survives.
func (r *wvRig) boot(t *testing.T) {
	t.Helper()
	r.closeRuntimeSide()

	r.catalog = tools.NewCatalog()
	r.mcpReg = mcpdrv.NewRegistry()
	r.attacher = serve.NewMCPConnectionAttacher(r.catalog, r.mcpReg, r.bus, nil,
		identity.Identity{TenantID: wvTenantA, UserID: wvUserA1, SessionID: "wv-default"},
		nil, nil, nil,
		serve.WithReattachTimeout(20*time.Second),
		// The deployment's lowered egress ceiling. It must survive the restart:
		// a re-attach that drops it silently restores the 8 MiB default.
		serve.WithArtifactEgressMaxBytes(wvEgressCeiling))
	r.detacher = serve.NewMCPConnectionDetacher(r.catalog, r.mcpReg, nil)
	// The production dispatch executor over the SAME catalog the re-attach
	// registers into: it is what seats the artifact resolver egress
	// substitution reads, so a tool invoked through it is invoked exactly as a
	// planner's CallTool decision invokes it.
	r.exec = dispatch.NewToolExecutor(r.catalog, r.art, nil)

	rl, err := steering.NewRunLoop(steering.NewRegistry(),
		pauseresume.New(pauseresume.WithBus(r.bus)), steering.WithRunLoopBus(r.bus))
	if err != nil {
		t.Fatalf("steering.NewRunLoop: %v", err)
	}
	driver, err := serve.NewRunLoopDriver(serve.RunLoopDriverOptions{
		Bus:           r.bus,
		RunLoop:       rl,
		Planner:       r.observer,
		Tasks:         r.tasks,
		AgentConfig:   r.cfg,
		AgentConfigID: wvBootAgent,
		// BOTH reconcile legs wired, exactly as the production boot wires them.
		ConnectionDetacher:   r.detacher,
		ConnectionReattacher: r.attacher,
	})
	if err != nil {
		t.Fatalf("NewRunLoopDriver: %v", err)
	}
	if err := driver.Start(context.Background()); err != nil {
		t.Fatalf("driver.Start: %v", err)
	}
	r.driver = driver
}

func wvID(tenant, user string) identity.Identity {
	return identity.Identity{TenantID: tenant, UserID: user, SessionID: "sess-" + user}
}

func wvCtx(t *testing.T, id identity.Identity) context.Context {
	t.Helper()
	ctx, err := identity.WithVerified(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.WithVerified: %v", err)
	}
	return auth.WithAgentReach(ctx, []string{wvBootAgent, wvAgentA, wvAgentB})
}

// wvDeclare writes an agent-scoped revision under `id`'s tenant declaring one
// connection. This is the versioned desired state a run start reconciles
// against — and writing it is also what makes `agentID` resolvable by the
// production AgentResolver for that tenant.
func (r *wvRig) wvDeclare(t *testing.T, id identity.Identity, agentID string, conns ...agentcfg.MCPConnectionDescriptor) {
	t.Helper()
	if _, err := r.cfg.SetRevision(context.Background(), identity.Quadruple{Identity: id},
		agentID, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{
			Connections: &agentcfg.ConnectionsSection{Servers: conns},
		}, agentcfg.SetOptions{}); err != nil {
		t.Fatalf("declare %q for %s/%s: %v", agentID, id.TenantID, agentID, err)
	}
}

// wvRunNamingAgent dispatches a REAL `start` naming `agentID` and blocks until
// the spawned run reaches the planner. Nothing here hands the agent id to the
// reconcile: it travels StartRequest.AgentID -> task.AgentID -> the run loop's
// effective agent -> the reattacher's owner tag. That chain IS the seam this
// wave's two phases meet on, and a leg that called the reconcile helper with a
// literal agent id would bypass all of it.
func (r *wvRig) wvRunNamingAgent(t *testing.T, id identity.Identity, agentID, key string) identity.Quadruple {
	t.Helper()
	resp, err := r.surface.Dispatch(wvCtx(t, id), methods.MethodStart, &prototypes.StartRequest{
		Identity:       prototypes.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID},
		Query:          "wave v1.24 composed surface",
		IdempotencyKey: key,
		AgentID:        agentID,
	})
	if err != nil {
		t.Fatalf("start naming %q: %v", agentID, err)
	}
	taskID := resp.(*prototypes.StartResponse).TaskID
	select {
	case <-r.observer.arrived(taskID):
	case <-time.After(60 * time.Second):
		t.Fatalf("the run for task %s never reached the planner", taskID)
	}
	q, ok := r.observer.get(taskID)
	if !ok {
		t.Fatalf("the run for task %s signalled arrival but recorded nothing", taskID)
	}
	return q
}

// wvStartRefused dispatches a `start` that must be REFUSED, and returns the
// error so the caller can classify it.
func (r *wvRig) wvStartRefused(t *testing.T, id identity.Identity, agentID, key string) error {
	t.Helper()
	_, err := r.surface.Dispatch(wvCtx(t, id), methods.MethodStart, &prototypes.StartRequest{
		Identity:       prototypes.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID},
		Query:          "wave v1.24 refusal",
		IdempotencyKey: key,
		AgentID:        agentID,
	})
	return err
}

// wvHTTPConn is a plain declared http connection.
func wvHTTPConn(name, url string) agentcfg.MCPConnectionDescriptor {
	return agentcfg.MCPConnectionDescriptor{
		Name: name, Transport: agentcfg.MCPTransportHTTP, URL: url,
	}
}

// wvInvoke calls a re-attached tool through the catalog under `id`.
func (r *wvRig) wvInvoke(t *testing.T, id identity.Identity, tool, args string) {
	t.Helper()
	d, ok := r.catalog.Resolve(tool)
	if !ok {
		t.Fatalf("%q is not in the catalog — the connection did not come back", tool)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ctx, err := identity.With(ctx, id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	if _, err := d.Invoke(ctx, json.RawMessage(args)); err != nil {
		t.Fatalf("invoking %q: %v", tool, err)
	}
}

// wvMetaNode walks EVERY segment of a declared `_meta` path and returns the
// leaf. A first-dot-only check proves one boundary out of N-1 and would pass on
// a `{"harbor": {"deployment.tier": "…"}}` half-nesting.
func wvMetaNode(t *testing.T, meta map[string]any, path string) any {
	t.Helper()
	segments := strings.Split(path, ".")
	var cur any = meta
	for i, seg := range segments {
		node, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("_meta.%s is %#v, want a nested object so segment %q can nest under it",
				strings.Join(segments[:i], "."), cur, seg)
		}
		next, present := node[seg]
		if !present {
			t.Fatalf("_meta path %q broke at segment %q (node keys: %v) — the annotation "+
				"reached the wire flattened or not at all", path, seg, wvKeys(node))
		}
		cur = next
	}
	return cur
}

func wvKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// ---------------------------------------------------------------------------
// Leg 1 — the caller-named agent seam meeting the run-start re-attach, across
// a GENUINE restart.
// ---------------------------------------------------------------------------

// TestE2E_WaveV124_NamedAgentRunReattachesItsOwnDeclaredConnection is the
// wave's headline composition.
//
// It proves, in order: the connection is LIVE before the restart (so the
// freshness guard is not vacuous); the restart really removed it; a `start`
// naming agent A brings back agent A's connection under agent A's owner tag
// (never the boot agent's); and agent B's declared set is untouched by agent
// A's sweep.
func TestE2E_WaveV124_NamedAgentRunReattachesItsOwnDeclaredConnection(t *testing.T) {
	fixA := newWVFixture(t)
	t.Cleanup(fixA.Close)
	fixB := newWVFixture(t)
	t.Cleanup(fixB.Close)

	r := newWVRig(t)
	idA := wvID(wvTenantA, wvUserA1)

	const annotationPath = "harbor.deployment.tier"
	const annotationValue = "wave-v124"
	connA := wvHTTPConn("wv-a", fixA.URL)
	connA.MetaAnnotations = map[string]string{annotationPath: annotationValue}
	r.wvDeclare(t, idA, wvAgentA, connA)
	r.wvDeclare(t, idA, wvAgentB, wvHTTPConn("wv-b", fixB.URL))

	// --- PRE-RESTART: both connections are genuinely LIVE. Without this the
	// "it came back" assertions below would be indistinguishable from "it was
	// never gone", and the restart the phase exists for would never happen.
	qA0 := r.wvRunNamingAgent(t, idA, wvAgentA, "wv-pre-a")
	if qA0.TenantID != wvTenantA || qA0.UserID != wvUserA1 {
		t.Fatalf("the run was seated under %+v, want the dispatching triple", qA0.Identity)
	}
	r.wvRunNamingAgent(t, idA, wvAgentB, "wv-pre-b")
	for name := range map[string]struct{}{"wv-a": {}, "wv-b": {}} {
		if _, ok := r.mcpReg.OwnerOf(name); !ok {
			t.Fatalf("%q is not live before the restart — the restart leg would prove nothing", name)
		}
	}
	r.wvInvoke(t, idA, "wv-a_ingest", `{"note":"pre-restart"}`)
	preRestartCalls := fixA.callCount()
	if preRestartCalls == 0 {
		t.Fatal("the pre-restart call never reached the fixture")
	}

	// --- THE RESTART. Everything process-local is destroyed; only the
	// revision spine survives.
	r.boot(t)
	for _, name := range []string{"wv-a", "wv-b"} {
		if _, ok := r.mcpReg.OwnerOf(name); ok {
			t.Fatalf("%q survived the rebuild — the runtime side is not actually fresh", name)
		}
	}
	if _, ok := r.catalog.Resolve("wv-a_ingest"); ok {
		t.Fatal("the rebuilt catalog still carries the pre-restart tool")
	}

	// --- THE SEAM. A `start` NAMING agent A. The agent id is never handed to
	// the reconcile by this test.
	qA := r.wvRunNamingAgent(t, idA, wvAgentA, "wv-post-a")
	if qA.RunID == "" {
		t.Fatal("the run carries no run id")
	}
	owner, ok := r.mcpReg.OwnerOf("wv-a")
	if !ok {
		t.Fatal("agent A's declared connection did not come back at its run start")
	}
	if want := (toolauth.Owner{Tenant: wvTenantA, Agent: wvAgentA}); owner != want {
		t.Fatalf("owner = %+v, want %+v — the re-attach did not inherit the run's "+
			"CALLER-NAMED agent (the boot agent is %q)", owner, want, wvBootAgent)
	}
	if owner.Agent == wvBootAgent {
		t.Fatal("the connection came back under the BOOT agent: StartRequest.AgentID did not " +
			"reach the reconcile, so the two phases are not actually wired together")
	}
	// Agent B's set is NOT touched by agent A's sweep: it is still absent.
	if _, ok := r.mcpReg.OwnerOf("wv-b"); ok {
		t.Fatal("agent A's run start attached agent B's declared connection — the reconcile " +
			"view is not owner-scoped")
	}

	// --- The re-attached connection is genuinely live, and the declared
	// annotation reaches the wire NESTED through every segment.
	r.wvInvoke(t, idA, "wv-a_ingest", `{"note":"post-restart"}`)
	if fixA.callCount() <= preRestartCalls {
		t.Fatal("the post-restart call never reached the fixture")
	}
	meta := fixA.meta()
	if len(meta) == 0 {
		t.Fatal("the fixture observed no `_meta` at all — the identity stamp did not reach the wire")
	}
	if _, flat := meta[annotationPath]; flat {
		t.Fatalf("the dotted annotation key reached the wire FLAT (%q) instead of nested",
			annotationPath)
	}
	if leaf := wvMetaNode(t, meta, annotationPath); fmt.Sprint(leaf) != annotationValue {
		t.Fatalf("the annotation leaf at %q is %#v, want %q", annotationPath, leaf, annotationValue)
	}

	// --- FAILURE MODE: agent B's third party is gone. Its run start fails
	// LOUD on the canonical event, and does not disturb agent A.
	fixB.Close()
	sub, err := r.bus.Subscribe(context.Background(), events.Filter{Admin: true})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()
	failed := make(chan agentcfg.MCPConnectionLifecyclePayload, 4)
	go func() {
		for ev := range sub.Events() {
			if ev.Type != agentcfg.EventTypeMCPConnectionReattachFailed {
				continue
			}
			if p, ok := ev.Payload.(agentcfg.MCPConnectionLifecyclePayload); ok {
				select {
				case failed <- p:
				default:
				}
			}
		}
	}()
	qB := r.wvRunNamingAgent(t, idA, wvAgentB, "wv-post-b")
	select {
	case p := <-failed:
		if p.State != agentcfg.MCPReattachClassTransportFailed {
			t.Fatalf("failure class = %q, want %q", p.State, agentcfg.MCPReattachClassTransportFailed)
		}
		if p.Author.RunID != qB.RunID {
			t.Fatalf("the failure event carries RunID %q, want the reconciling run's %q",
				p.Author.RunID, qB.RunID)
		}
		if p.AgentID != wvAgentB {
			t.Fatalf("the failure event names agent %q, want %q", p.AgentID, wvAgentB)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("an unreachable declared connection produced no reattach_failed event")
	}
	if _, ok := r.mcpReg.OwnerOf("wv-b"); ok {
		t.Fatal("a failed re-attach still registered the connection")
	}
	if _, ok := r.catalog.Resolve("wv-a_ingest"); !ok {
		t.Fatal("agent B's failed re-attach disturbed agent A's live connection")
	}

	// --- FAILURE MODE: a caller naming an agent their own tenant does not
	// declare is refused at the edge, so no run and no sweep happen at all.
	if err := r.wvStartRefused(t, idA, "wv124-agent-nonexistent", "wv-refused"); err == nil {
		t.Fatal("a start naming an undeclared agent must be refused")
	}
}

// ---------------------------------------------------------------------------
// Leg 2 — egress substitution survives the rebuild (the audit's Finding 1).
// ---------------------------------------------------------------------------

// TestE2E_WaveV124_ByteEligibleConnectionStillSubstitutesAfterReattach is the
// leg the first cut of this file wrote a comment for and never wrote:
//
//	"A phase that rebuilds a connection must not silently drop the surface a
//	 sibling phase added to it."
//
// The re-attach rebuilds the driver config field by field. Before this leg
// existed, it dropped `artifact_byte_eligible` and `artifact_params`, so after
// ANY restart a byte-eligible connection came back `online`, emitted
// `mcp.connection.reattached`, and passed the model's `art-…` id to the remote
// server as a LITERAL STRING for the rest of the process's life.
//
// The assertion is made SERVER-side against the real wire, not against the
// registry's view of its own config.
func TestE2E_WaveV124_ByteEligibleConnectionStillSubstitutesAfterReattach(t *testing.T) {
	fix := newWVFixture(t)
	t.Cleanup(fix.Close)

	r := newWVRig(t)
	id := wvID(wvTenantA, wvUserA1)

	conn := wvHTTPConn("wv-egress", fix.URL)
	conn.ArtifactByteEligible = true
	conn.ArtifactParams = map[string][]string{"ingest": {"doc"}}
	r.wvDeclare(t, id, wvAgentA, conn)

	// Live once, then restart, then bring it back through a caller-named run.
	r.wvRunNamingAgent(t, id, wvAgentA, "wv-eg-pre")
	if _, ok := r.mcpReg.OwnerOf("wv-egress"); !ok {
		t.Fatal("the byte-eligible connection was not attached before the restart")
	}
	r.boot(t)
	if _, ok := r.mcpReg.OwnerOf("wv-egress"); ok {
		t.Fatal("the runtime side is not actually fresh")
	}
	r.wvRunNamingAgent(t, id, wvAgentA, "wv-eg-post")
	if _, ok := r.mcpReg.OwnerOf("wv-egress"); !ok {
		t.Fatal("the byte-eligible connection did not come back")
	}

	// The artifact the model will name, stored under the dispatching triple.
	q := identity.Quadruple{Identity: id, RunID: "wv-eg-run"}
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	ctx, err = identity.WithRun(ctx, id, q.RunID)
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}
	ref, err := r.art.PutBytes(ctx, artifacts.ArtifactScope{
		TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID,
	}, wvBody, artifacts.PutOpts{Filename: "planted.bin", MimeType: "application/octet-stream"})
	if err != nil {
		t.Fatalf("PutBytes: %v", err)
	}

	// Through the PRODUCTION dispatch executor — the seat that makes the
	// artifact resolver available, exactly as a planner CallTool decision does.
	if _, _, err := r.exec.ExecuteDecision(ctx, planner.RunContext{Quadruple: q,
		Catalog: tools.NewPlannerView(r.catalog, tools.CatalogFilter{TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID})},
		planner.CallTool{
			CallID: "wv-eg-call",
			Tool:   "wv-egress_ingest",
			Args:   json.RawMessage(fmt.Sprintf(`{"doc":%q,"note":"post-restart"}`, ref.ID)),
		}); err != nil {
		t.Fatalf("ExecuteDecision on the re-attached byte-eligible connection: %v", err)
	}

	// The decisive assertion, from the SERVER's side of the wire.
	got := fix.args(t)
	docValue, _ := got["doc"].(string)
	if docValue == ref.ID {
		t.Fatalf("the re-attached connection is byte-INELIGIBLE: the server received the "+
			"artifact id %q as a literal string instead of the artifact's bytes. The "+
			"declaration rode the revision spine but the run-start re-attach did not "+
			"carry it into the rebuilt connection", ref.ID)
	}
	decoded, derr := base64.StdEncoding.DecodeString(docValue)
	if derr != nil {
		t.Fatalf("the mapped parameter is not standard base64 (%v); got %.64q", derr, docValue)
	}
	if string(decoded) != string(wvBody) {
		t.Fatalf("the delivered bytes are not the stored bytes: got %d bytes, want %d",
			len(decoded), len(wvBody))
	}
	if !strings.Contains(string(decoded), wvMarker) {
		t.Fatal("the delivered bytes do not carry the planted marker")
	}
	// The UNMAPPED parameter is untouched — the substitution is scoped to the
	// declared mapping, not to every string in the body.
	if note, _ := got["note"].(string); note != "post-restart" {
		t.Fatalf("the unmapped `note` parameter is %q, want it passed through verbatim", note)
	}

	// --- MULTI-ISOLATION on the wave's newest feature (§17.3 item 2, the
	// NEGATIVE half). Widening the RECIPIENT does not widen the reachable
	// artifact SET: that stays the dispatching run's own (tenant, user,
	// session). A run under a DIFFERENT user of the SAME tenant naming the
	// same id must be refused and must issue no wire request — the id is not
	// a bearer capability, and a re-attached connection must not become a
	// cross-user read door.
	other := wvID(wvTenantA, wvUserA2)
	otherQ := identity.Quadruple{Identity: other, RunID: "wv-eg-foreign-run"}
	otherCtx, err := identity.With(context.Background(), other)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	otherCtx, err = identity.WithRun(otherCtx, other, otherQ.RunID)
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}
	foreignCallsBefore := fix.callCount()
	if _, _, ferr := r.exec.ExecuteDecision(otherCtx, planner.RunContext{Quadruple: otherQ,
		Catalog: tools.NewPlannerView(r.catalog, tools.CatalogFilter{TenantID: other.TenantID, UserID: other.UserID, SessionID: other.SessionID})},
		planner.CallTool{
			CallID: "wv-eg-foreign",
			Tool:   "wv-egress_ingest",
			Args:   json.RawMessage(fmt.Sprintf(`{"doc":%q}`, ref.ID)),
		}); ferr == nil {
		t.Fatalf("a run under %s/%s resolved %s/%s's artifact %q — the re-attached "+
			"byte-eligible connection widened the reachable artifact SET, not just the "+
			"recipient", other.TenantID, other.UserID, id.TenantID, id.UserID, ref.ID)
	}
	if fix.callCount() != foreignCallsBefore {
		t.Fatal("the refused cross-user substitution still issued a wire request")
	}

	// --- FAILURE MODE + the CEILING's survival. The deployment configured a
	// low ceiling; a re-attach that dropped it would silently restore the
	// 8 MiB default and this oversize artifact would be delivered instead of
	// refused.
	oversize := make([]byte, wvEgressCeiling*2)
	for i := range oversize {
		oversize[i] = 'z'
	}
	bigRef, err := r.art.PutBytes(ctx, artifacts.ArtifactScope{
		TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID,
	}, oversize, artifacts.PutOpts{Filename: "oversize.bin", MimeType: "application/octet-stream"})
	if err != nil {
		t.Fatalf("PutBytes(oversize): %v", err)
	}
	callsBefore := fix.callCount()
	_, _, eerr := r.exec.ExecuteDecision(ctx, planner.RunContext{Quadruple: q,
		Catalog: tools.NewPlannerView(r.catalog, tools.CatalogFilter{TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID})},
		planner.CallTool{
			CallID: "wv-eg-oversize",
			Tool:   "wv-egress_ingest",
			Args:   json.RawMessage(fmt.Sprintf(`{"doc":%q}`, bigRef.ID)),
		})
	if eerr == nil {
		t.Fatalf("an artifact of %d bytes was delivered under a %d-byte deployment ceiling — "+
			"the re-attach dropped the ceiling and reverted to the default",
			len(oversize), wvEgressCeiling)
	}
	if !errors.Is(eerr, artifactegress.ErrEgressTooLarge) {
		t.Fatalf("oversize refusal = %v, want it marked with ErrEgressTooLarge so an "+
			"operator can tell a ceiling refusal from a transport failure", eerr)
	}
	if fix.callCount() != callsBefore {
		t.Fatal("the refused oversize call still issued a wire request — the ceiling is " +
			"checked after the send, not before it")
	}
}

// ---------------------------------------------------------------------------
// Leg 3 — multi-isolation across TENANTS and USERS (§17.3 item 2).
// ---------------------------------------------------------------------------

// TestE2E_WaveV124_ReconcileIsBoundedByTheIdentityTriple varies the two axes
// that ARE isolation principals. `agent_id` is deliberately not one of them
// (CLAUDE.md §6): a test that varied only the agent would assert nothing here.
//
// Both directions are asserted: the positive (each triple's own declared set
// comes back) and the NEGATIVE (a read under the wrong triple returns nothing,
// and a caller cannot name a foreign tenant's agent).
func TestE2E_WaveV124_ReconcileIsBoundedByTheIdentityTriple(t *testing.T) {
	fixA1 := newWVFixture(t)
	t.Cleanup(fixA1.Close)
	fixA2 := newWVFixture(t)
	t.Cleanup(fixA2.Close)
	fixB1 := newWVFixture(t)
	t.Cleanup(fixB1.Close)

	r := newWVRig(t)

	// Tenant A, user 1 and user 2 — SAME tenant, so they share the tenant's
	// agent revisions, and tenant B, a different tenant entirely.
	idA1 := wvID(wvTenantA, wvUserA1)
	idA2 := wvID(wvTenantA, wvUserA2)
	idB1 := wvID(wvTenantB, wvUserB1)

	r.wvDeclare(t, idA1, wvAgentA, wvHTTPConn("wv-iso-a", fixA1.URL))
	// Tenant B declares a connection under the SAME agent id. The owner tag is
	// (tenant, agent), so the two must not collide.
	r.wvDeclare(t, idB1, wvAgentA, wvHTTPConn("wv-iso-b", fixB1.URL))
	// A second agent inside tenant A, so the tenant axis and the agent axis
	// are separable in the assertions below.
	r.wvDeclare(t, idA2, wvAgentB, wvHTTPConn("wv-iso-a2", fixA2.URL))

	qA1 := r.wvRunNamingAgent(t, idA1, wvAgentA, "wv-iso-a1")
	qB1 := r.wvRunNamingAgent(t, idB1, wvAgentA, "wv-iso-b1")
	qA2 := r.wvRunNamingAgent(t, idA2, wvAgentB, "wv-iso-a2")

	// IDENTITY PROPAGATION: each run was seated under its OWN dispatching
	// triple, all the way to the planner.
	for _, c := range []struct {
		label string
		got   identity.Quadruple
		want  identity.Identity
	}{
		{"tenant A / user 1", qA1, idA1},
		{"tenant B / user 1", qB1, idB1},
		{"tenant A / user 2", qA2, idA2},
	} {
		if c.got.Identity != c.want {
			t.Fatalf("%s: the run reached the planner under %+v, want %+v",
				c.label, c.got.Identity, c.want)
		}
	}

	// Each declared connection came back under its OWN (tenant, agent) owner.
	for _, c := range []struct {
		name  string
		owner toolauth.Owner
	}{
		{"wv-iso-a", toolauth.Owner{Tenant: wvTenantA, Agent: wvAgentA}},
		{"wv-iso-b", toolauth.Owner{Tenant: wvTenantB, Agent: wvAgentA}},
		{"wv-iso-a2", toolauth.Owner{Tenant: wvTenantA, Agent: wvAgentB}},
	} {
		got, ok := r.mcpReg.OwnerOf(c.name)
		if !ok {
			t.Fatalf("%q did not come back", c.name)
		}
		if got != c.owner {
			t.Fatalf("%q owner = %+v, want %+v — two tenants sharing one agent id "+
				"collided", c.name, got, c.owner)
		}
	}

	// NEGATIVE 1 — the RECONCILE VIEW is the boundary, and it is owner-scoped.
	//
	// The MCP registry itself is deliberately PROCESS-GLOBAL: the owner tag is
	// a reconcile-view filter, never a dispatch or isolation key. So the thing
	// to assert is not "tenant A cannot list tenant B's registration" (it
	// can — by design), it is that tenant A's run start cannot REACH tenant
	// B's registration. Tenant A's agent A un-declares its connection; its
	// next run start must tear down exactly ITS OWN and nothing else.
	r.wvDeclare(t, idA1, wvAgentA)
	r.wvRunNamingAgent(t, idA1, wvAgentA, "wv-iso-a1-undeclare")
	if _, ok := r.mcpReg.OwnerOf("wv-iso-a"); ok {
		t.Fatal("the un-declared connection survived its owner's run-start reconcile")
	}
	for _, survivor := range []string{"wv-iso-b", "wv-iso-a2"} {
		if _, ok := r.mcpReg.OwnerOf(survivor); !ok {
			t.Fatalf("CROSS-OWNER TEARDOWN: tenant A / agent A's reconcile detached %q, which "+
				"belongs to a different (tenant, agent) owner", survivor)
		}
	}

	// NEGATIVE 2 / FAILURE MODE — identity is mandatory on the read path. A
	// registry read with no identity in ctx fails CLOSED rather than returning
	// the process-global set.
	if _, _, err := r.mcpReg.ListServers(context.Background(), mcpdrv.ListFilter{}); err == nil {
		t.Fatal("ListServers with no identity in ctx returned rows — identity is mandatory " +
			"and the read must fail closed")
	} else if !errors.Is(err, mcpdrv.ErrRegistryIdentityMissing) {
		t.Fatalf("identity-less ListServers = %v, want ErrRegistryIdentityMissing", err)
	}
	// ...and with identity it answers, so the guard above is not passing on a
	// broken read path.
	if _, _, err := r.mcpReg.ListServers(
		mustIdentityCtx(t, idB1.TenantID, idB1.UserID, idB1.SessionID),
		mcpdrv.ListFilter{}); err != nil {
		t.Fatalf("ListServers with identity: %v", err)
	}

	// NEGATIVE 3 / FAILURE MODE — tenant B names an agent only tenant A
	// declares. The resolver keys on the CALLER's tenant, so the id is simply
	// not present: refused, and indistinguishable from a never-existing id.
	if err := r.wvStartRefused(t, idB1, wvAgentB, "wv-iso-foreign"); err == nil {
		t.Fatalf("tenant B naming tenant A's agent %q was ACCEPTED — the agent axis leaked "+
			"across the tenant boundary", wvAgentB)
	}
	// The same id under its OWN tenant still works, so the refusal above is a
	// boundary and not a broken resolver.
	r.wvRunNamingAgent(t, idA2, wvAgentB, "wv-iso-a2-again")
}

// ---------------------------------------------------------------------------
// Leg 4 — the split heavy-content thresholds, BEHAVIOURALLY.
// ---------------------------------------------------------------------------

// TestE2E_WaveV124_OnePayloadInTheBandInlinesForTheModelAndOffloadsForTheConsole
// replaces the first cut's constant comparison, which compared two untyped
// constants (so the compiler folded both branches) and was a verbatim
// duplicate of the unit test in internal/config.
//
// One payload, two consumers, opposite answers. That is the phase's whole
// claim, and it is only real through the actual wiring: a `mux.go` that keeps
// threading the LLM-context threshold into the Console arms turns this red
// while every constant still differs.
func TestE2E_WaveV124_OnePayloadInTheBandInlinesForTheModelAndOffloadsForTheConsole(t *testing.T) {
	rig := newHeavyThresholdRig(t, 0)
	srv := httptest.NewServer(rig.mux)
	defer srv.Close()

	id := identity.Identity{TenantID: wvTenantA, UserID: wvUserA1, SessionID: "s-wv-band"}

	store, err := artifacts.Open(context.Background(), config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("artifacts.Open: %v", err)
	}
	defer func() { _ = store.Close(context.Background()) }()

	// The SAME size on both sides — 64 KiB, above the Console inline bound and
	// below the LLM-context threshold.
	if htConsoleBand <= config.DefaultConsoleInlinePayloadBytes ||
		htConsoleBand >= config.DefaultHeavyOutputThresholdBytes {
		t.Fatalf("the probe size %d is no longer between the two bounds (%d, %d) — this leg "+
			"would assert nothing", htConsoleBand,
			config.DefaultConsoleInlinePayloadBytes, config.DefaultHeavyOutputThresholdBytes)
	}

	// The LLM edge INLINES it: the model gets the content.
	obs := htRunTool(t, htExecutor(t, rig, store, htConsoleBand), id, "wv-band-llm")
	if obs["truncated"] == true {
		t.Fatalf("a %d-byte result was promoted to a stub at the LLM edge — the prompt budget "+
			"is still following the Console's bound", htConsoleBand)
	}
	if blob, _ := obs["blob"].(string); len(blob) != htConsoleBand {
		t.Fatalf("the inlined observation carries %d bytes, want %d", len(blob), htConsoleBand)
	}

	// The Protocol replies a browser reads OFFLOAD it: memory.get, memory.list
	// and pause.list all select the by-reference arm at the same size.
	htAssertConsoleArmsOffload(t, srv.URL, rig, id, "wv-band-console")

	// FAILURE MODE: a second tenant's identical payload is not readable under
	// the first tenant's triple — the Console arms are identity-scoped too.
	other := identity.Identity{TenantID: wvTenantB, UserID: wvUserB1, SessionID: "s-wv-band-other"}
	htSeedHeavyTurn(t, rig.memory, other, htConsoleBand)
	status, body := htPost(t, srv.URL, "/v1/memory/list", id, `{}`)
	if status != http.StatusOK {
		t.Fatalf("memory.list: status = %d, body = %s", status, body)
	}
	var listResp prototypes.MemoryListResponse
	if err := json.Unmarshal(body, &listResp); err != nil {
		t.Fatalf("decode memory.list: %v", err)
	}
	for _, item := range listResp.Items {
		if strings.Contains(item.Key, other.SessionID) {
			t.Fatalf("CROSS-TENANT LEAK: %s's memory.list returned %s's row %q",
				id.TenantID, other.TenantID, item.Key)
		}
	}
}

// ---------------------------------------------------------------------------
// Leg 5 — the concurrency stress over the composed surface, with a real
// goroutine baseline.
// ---------------------------------------------------------------------------

// TestE2E_WaveV124_ConcurrentComposedSweepHoldsEverySeam replaces the first
// cut's compile-time type assertion, which backed a race-safety claim with a
// statement the compiler checks and the runtime never exercises.
//
// N interleaved run starts across TWO tenants and TWO users against ONE shared
// attacher + registry + catalog, with per-worker cross-talk detection and a
// settle-looped `runtime.NumGoroutine()` baseline.
func TestE2E_WaveV124_ConcurrentComposedSweepHoldsEverySeam(t *testing.T) {
	fixA := newWVFixture(t)
	t.Cleanup(fixA.Close)
	fixB := newWVFixture(t)
	t.Cleanup(fixB.Close)
	// A permanently-dead third party, so the stress covers the failing arm as
	// well as the healthy one: its refusals must never disturb the others.
	dead := newWVFixture(t)
	deadURL := dead.URL
	dead.Close()

	r := newWVRig(t)
	idA := wvID(wvTenantA, wvUserA1)
	idB := wvID(wvTenantB, wvUserB1)

	r.wvDeclare(t, idA, wvAgentA, wvHTTPConn("wv-conc-a", fixA.URL))
	r.wvDeclare(t, idB, wvAgentA, wvHTTPConn("wv-conc-b", fixB.URL))
	r.wvDeclare(t, idA, wvAgentB, wvHTTPConn("wv-conc-dead", deadURL))

	const n = 12
	type worker struct {
		id      identity.Identity
		agent   string
		wantOwn string
	}
	workers := []worker{
		{idA, wvAgentA, "wv-conc-a"},
		{idB, wvAgentA, "wv-conc-b"},
		{idA, wvAgentB, "wv-conc-dead"},
	}

	// ONE warm-up run per worker, so the baseline below is the STEADY state of
	// an already-booted stack with every transport dialled. Taking it before
	// the rig exists would count the rig's own goroutines as the stress's
	// leak, which makes the guard fail for a reason that has nothing to do
	// with what it claims to measure.
	for wi := range workers {
		r.wvRunNamingAgent(t, workers[wi].id, workers[wi].agent, fmt.Sprintf("wv-conc-warm-%d", wi))
	}
	http.DefaultClient.CloseIdleConnections()
	baseline := goruntime.NumGoroutine()

	var wg sync.WaitGroup
	errCh := make(chan error, n*len(workers))
	start := make(chan struct{})
	for i := range n {
		for wi := range workers {
			wg.Add(1)
			go func(i, wi int) {
				defer wg.Done()
				<-start
				w := workers[wi]
				q := r.wvRunNamingAgent(t, w.id, w.agent, fmt.Sprintf("wv-conc-%d-%d", wi, i))
				// CROSS-TALK: the run must be seated under ITS OWN triple.
				if q.Identity != w.id {
					errCh <- fmt.Errorf("worker %d/%d: run seated under %+v, want %+v",
						wi, i, q.Identity, w.id)
					return
				}
				// The reachable declared set is this owner's own.
				if w.wantOwn != "wv-conc-dead" {
					owner, ok := r.mcpReg.OwnerOf(w.wantOwn)
					if !ok {
						errCh <- fmt.Errorf("worker %d/%d: %q is not live", wi, i, w.wantOwn)
						return
					}
					if owner != (toolauth.Owner{Tenant: w.id.TenantID, Agent: w.agent}) {
						errCh <- fmt.Errorf("worker %d/%d: %q owner = %+v, want (%s, %s)",
							wi, i, w.wantOwn, owner, w.id.TenantID, w.agent)
						return
					}
				}
			}(i, wi)
		}
	}
	close(start)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent composed sweep: %v", err)
	}

	// Exactly one registration per healthy connection after the storm — the
	// under-lock idempotency re-check held.
	ctx := mustIdentityCtx(t, wvTenantA, wvUserA1, "sess-"+wvUserA1)
	servers, _, lerr := r.mcpReg.ListServers(ctx, mcpdrv.ListFilter{})
	if lerr != nil {
		t.Fatalf("ListServers: %v", lerr)
	}
	counts := map[string]int{}
	for _, s := range servers {
		counts[s.Name]++
	}
	if counts["wv-conc-a"] != 1 {
		t.Fatalf("wv-conc-a registrations = %d after %d concurrent run starts, want exactly 1",
			counts["wv-conc-a"], n)
	}
	if counts["wv-conc-dead"] != 0 {
		t.Fatalf("wv-conc-dead registrations = %d, want 0 (its third party is down)",
			counts["wv-conc-dead"])
	}
	if _, ok := r.catalog.Resolve("wv-conc-a_ingest"); !ok {
		t.Fatal("the healthy connection's tool did not survive the concurrent storm")
	}

	// A real goroutine baseline. The stress ran against the SAME booted stack
	// the baseline was sampled from, so anything above it after the settle
	// loop was left behind by the N run starts themselves. This is the
	// assertion a compile-time type assertion cannot make: it is the only
	// thing here that can go red on a per-run leak.
	http.DefaultClient.CloseIdleConnections()
	deadline := time.Now().Add(20 * time.Second)
	for goruntime.NumGoroutine() > baseline && time.Now().Before(deadline) {
		goruntime.Gosched()
	}
	if delta := goruntime.NumGoroutine() - baseline; delta > 0 {
		t.Errorf("goroutine leak after %d concurrent run starts: baseline=%d, after=%d (+%d)",
			n*len(workers), baseline, goruntime.NumGoroutine(), delta)
	}
}
