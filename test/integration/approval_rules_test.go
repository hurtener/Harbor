// Approval rules for dynamic MCP tools (D-519) — integration test.
//
// A `tools.approval_rules[]` row gates tools no `tools.entries[]` row can
// name in advance: tools an MCP server discovers at runtime. This test
// attaches a REAL MCP server (the official go-sdk streamable-HTTP
// handler, §17.8) to a catalog carrying the rule wrapper, and drives the
// gated tool through the real production path:
// deterministic planner → steering RunLoop → dispatch executor →
// catalog descriptor → approval rule gate → MCP call.
//
// Real drivers on every seam (§17.3): pauseresume Coordinator, in-mem
// bus + patterns redactor, in-mem StateStore (decision record), steering
// Registry + RunLoop, dispatch.NewToolExecutor.
//
// Covered:
//   - the destructive tool matched by name glob + annotation pauses,
//     emits tool.approval_requested with a redacted ArgsSummary, and
//     runs with the ORIGINAL args on approve; tool.approved names the
//     deciding principal carried on the control (who decided) and the
//     durable decision record keeps it after the pause is gone;
//   - reject fails the call cleanly (the MCP tool never runs) with a
//     typed tool.rejected Decision "reject";
//   - the read-only tool of the same server is NOT gated (unmatched
//     tools behave as before).
package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hurtener/Harbor/internal/artifacts"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	eventsInmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/deterministic"
	"github.com/hurtener/Harbor/internal/runtime/dispatch"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/approval"
	"github.com/hurtener/Harbor/internal/tools/catalog"
	mcpdrv "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

// arID is the documented dummy identity of the gated session.
var arID = identity.Identity{TenantID: "tenant-ar", UserID: "user-ar", SessionID: "session-ar"}

// arApprover is the verified principal that decides (a participant of
// the session, with a signed display label).
var arApprover = pauseresume.Decider{TenantID: "tenant-ar", UserID: "approver-ar", SessionID: "session-ar", Speaker: "Approver A"}

const (
	arGatedTool    = "crm_delete_record"
	arUngatedTool  = "crm_read_record"
	arWaitDeadline = 10 * time.Second
)

type arEnv struct {
	bus       events.EventBus
	cat       tools.ToolCatalog
	gates     map[string]*approval.ApprovalGate
	steerReg  *steering.Registry
	runLoop   *steering.RunLoop
	executor  steering.ToolExecutor
	decisions approval.DecisionLog
	deletes   *atomic.Int64
	reads     *atomic.Int64
}

func arMCPServer(t *testing.T, deletes, reads *atomic.Int64) *httptest.Server {
	t.Helper()
	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "approval-rules-fixture", Version: "v0"}, nil)
	destructive := true
	type args struct {
		ID       string `json:"id"`
		Password string `json:"password,omitempty"`
	}
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id":       map[string]any{"type": "string"},
			"password": map[string]any{"type": "string"},
		},
	}
	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "delete_record",
		Description: "delete one record",
		InputSchema: schema,
		Annotations: &mcpsdk.ToolAnnotations{DestructiveHint: &destructive},
	}, func(_ context.Context, _ *mcpsdk.CallToolRequest, in args) (*mcpsdk.CallToolResult, any, error) {
		deletes.Add(1)
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "deleted " + in.ID + " with " + in.Password}}}, nil, nil
	})
	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "read_record",
		Description: "read one record",
		InputSchema: schema,
		Annotations: &mcpsdk.ToolAnnotations{ReadOnlyHint: true},
	}, func(_ context.Context, _ *mcpsdk.CallToolRequest, in args) (*mcpsdk.CallToolResult, any, error) {
		reads.Add(1)
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "read " + in.ID}}}, nil, nil
	})
	hs := httptest.NewServer(mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return srv }, nil))
	t.Cleanup(hs.Close)
	return hs
}

func buildAREnv(t *testing.T) *arEnv {
	t.Helper()
	ctx := context.Background()
	red := auditpatterns.New()
	bus, err := eventsInmem.New(config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     256,
		IdleTimeout:              2 * time.Second,
		DropWindow:               50 * time.Millisecond,
	}, red)
	if err != nil {
		t.Fatalf("eventsInmem.New: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(ctx) })
	stateStore, err := state.Open(ctx, config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = stateStore.Close(ctx) })
	decisions, err := approval.NewStateDecisionLog(stateStore)
	if err != nil {
		t.Fatalf("NewStateDecisionLog: %v", err)
	}
	coord := pauseresume.New(pauseresume.WithBus(bus), pauseresume.WithCheckpointStore(stateStore),
		pauseresume.WithResolutionRecorder(approval.NewDecisionRecorder(decisions)))

	rules := []config.ToolApprovalRuleConfig{{
		Tools:       []string{"crm_*"},
		Annotations: []string{"destructive"},
		Approval:    config.ToolApprovalConfig{Policy: "deny-all", Reason: "destructive CRM tools need a human"},
	}}
	built, err := catalog.BuildApprovalRules(rules, nil, catalog.Deps{
		Coordinator: coord,
		Bus:         bus,
		Redactor:    red,
		Authorizer:  approval.NewIdentityAuthorizer(),
	})
	if err != nil {
		t.Fatalf("BuildApprovalRules: %v", err)
	}
	t.Cleanup(func() {
		for _, g := range built.Gates {
			_ = g.Close(ctx)
		}
	})
	cat := tools.NewCatalog(tools.WithCatalogBus(bus), tools.WithRegistrationGate(built.Wrap))

	deletes, reads := &atomic.Int64{}, &atomic.Int64{}
	hs := arMCPServer(t, deletes, reads)
	var closers []func(context.Context) error
	t.Cleanup(func() {
		for i := len(closers) - 1; i >= 0; i-- {
			_ = closers[i](ctx)
		}
	})
	if err := mcpdrv.Attach(ctx, config.MCPServerConfig{Name: "crm", TransportMode: "streamable_http", URL: hs.URL}, mcpdrv.AttachDeps{
		Catalog:         cat,
		Registry:        mcpdrv.NewRegistry(),
		Bus:             bus,
		DefaultIdentity: arID,
		Closers:         &closers,
	}); err != nil {
		t.Fatalf("mcp Attach: %v", err)
	}

	steerReg := steering.NewRegistry()
	rl, err := steering.NewRunLoop(steerReg, coord, steering.WithApprovalGates(built.Gates), steering.WithRunLoopBus(bus))
	if err != nil {
		t.Fatalf("NewRunLoop: %v", err)
	}
	artStore, err := artifacts.Open(ctx, config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("artifacts.Open: %v", err)
	}
	t.Cleanup(func() { _ = artStore.Close(ctx) })
	taskReg, err := tasks.Open(ctx, tasks.Dependencies{Store: stateStore, Bus: bus, Redactor: red, Cfg: config.TasksConfig{Driver: "inprocess"}})
	if err != nil {
		t.Fatalf("tasks.Open: %v", err)
	}
	t.Cleanup(func() { _ = taskReg.Close(ctx) })
	return &arEnv{
		bus: bus, cat: cat, gates: built.Gates, steerReg: steerReg, runLoop: rl,
		executor: dispatch.NewToolExecutor(cat, artStore, taskReg), decisions: decisions,
		deletes: deletes, reads: reads,
	}
}

func arSpec(t *testing.T, env *arEnv, q identity.Quadruple, tool string) steering.RunSpec {
	t.Helper()
	p, err := deterministic.NewDeterministicPlanner(deterministic.WithSteps(
		&deterministic.CallToolStep{
			Tool: tool,
			ArgsBuilder: func(planner.RunContext) (json.RawMessage, error) {
				return json.RawMessage(`{"id":"rec-7","password":"hunter2"}`), nil
			},
			When: func(rc planner.RunContext) bool { return rc.Trajectory == nil || len(rc.Trajectory.Steps) == 0 },
		},
		&deterministic.FinishStep{Reason: planner.FinishGoal},
	))
	if err != nil {
		t.Fatalf("NewDeterministicPlanner: %v", err)
	}
	return steering.RunSpec{
		Planner: p,
		Base: planner.RunContext{
			Quadruple:  q,
			Goal:       "exercise an approval rule",
			Trajectory: &planner.Trajectory{},
			Catalog:    tools.NewPlannerView(env.cat, tools.CatalogFilter{TenantID: q.TenantID, UserID: q.UserID, SessionID: q.SessionID}),
		},
		MaxSteps:     6,
		ToolExecutor: env.executor,
	}
}

func arSubscribe(t *testing.T, bus events.EventBus, types ...events.EventType) events.Subscription {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	sub, err := bus.Subscribe(ctx, events.Filter{Tenant: arID.TenantID, User: arID.UserID, Session: arID.SessionID, Types: types})
	if err != nil {
		cancel()
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(func() { sub.Cancel(); cancel() })
	return sub
}

func arNext(t *testing.T, sub events.Subscription, typ events.EventType) events.Event {
	t.Helper()
	deadline := time.After(arWaitDeadline)
	for {
		select {
		case ev, ok := <-sub.Events():
			if !ok {
				t.Fatalf("subscription closed before %s", typ)
			}
			if ev.Type == typ {
				return ev
			}
		case <-deadline:
			t.Fatalf("timeout waiting for %s", typ)
		}
	}
}

func arRun(env *arEnv, spec steering.RunSpec) <-chan error {
	done := make(chan error, 1)
	go func() {
		_, err := env.runLoop.Run(context.Background(), spec)
		done <- err
	}()
	return done
}

func arEnqueue(t *testing.T, env *arEnv, q identity.Quadruple, typ steering.ControlType, token, reason string) {
	t.Helper()
	in, err := env.steerReg.Lookup(q)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if err := in.Enqueue(steering.ControlEvent{
		Type:         typ,
		Identity:     q,
		CallerScope:  steering.ScopeOwnerUser,
		CallerTenant: q.TenantID,
		Decider:      arApprover,
		Payload:      map[string]any{"token": token, "reason": reason},
	}); err != nil {
		t.Fatalf("Enqueue(%s): %v", typ, err)
	}
}

func TestApprovalRules_DynamicMCPTool_ApproveRunsWithDecider(t *testing.T) {
	env := buildAREnv(t)
	sub := arSubscribe(t, env.bus, approval.EventTypeToolApprovalRequested, approval.EventTypeToolApproved, approval.EventTypeToolRejected)
	q := identity.Quadruple{Identity: arID, RunID: "run-ar-approve"}
	done := arRun(env, arSpec(t, env, q, arGatedTool))

	req := arNext(t, sub, approval.EventTypeToolApprovalRequested).Payload.(approval.ToolApprovalRequestedPayload)
	if req.Tool != arGatedTool {
		t.Fatalf("approval requested for %q, want %q", req.Tool, arGatedTool)
	}
	summary, _ := json.Marshal(req.ArgsSummary)
	if strings.Contains(string(summary), "hunter2") {
		t.Fatalf("ArgsSummary leaked the password: %s", summary)
	}
	if !strings.Contains(string(summary), "rec-7") {
		t.Fatalf("ArgsSummary lost the non-secret arg: %s", summary)
	}
	if got := env.deletes.Load(); got != 0 {
		t.Fatalf("gated MCP tool ran %d times before approval", got)
	}

	arEnqueue(t, env, q, steering.ControlApprove, req.PauseToken, "checked the record")
	approved := arNext(t, sub, approval.EventTypeToolApproved).Payload.(approval.ToolApprovedPayload)
	if approved.DecidedByUser != arApprover.UserID || approved.DecidedBySpeaker != arApprover.Speaker ||
		approved.DecidedByTenant != arApprover.TenantID || approved.DecidedBySession != arApprover.SessionID {
		t.Errorf("tool.approved decided-by = %+v, want %+v", approved, arApprover)
	}
	if approved.DecidedAt.IsZero() || approved.ApproverReason != "checked the record" {
		t.Errorf("tool.approved DecidedAt=%v reason=%q", approved.DecidedAt, approved.ApproverReason)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(arWaitDeadline):
		t.Fatal("run did not finish after approve")
	}
	if got := env.deletes.Load(); got != 1 {
		t.Fatalf("gated MCP tool ran %d times, want 1", got)
	}

	// The decision is durable after the pause checkpoint is gone.
	recs, err := approval.InterventionReader{Log: env.decisions}.RecentInterventions(context.Background(), arID)
	if err != nil || len(recs) != 1 {
		t.Fatalf("RecentInterventions = %+v, %v; want one record", recs, err)
	}
	r := recs[0]
	if r.Type != "tool_approval" || r.Outcome != "approved" || r.Tool != arGatedTool || r.PauseToken != req.PauseToken ||
		r.DecidedBy == nil || r.DecidedBy.UserID != arApprover.UserID || r.DecidedBy.Speaker != arApprover.Speaker ||
		r.DecisionReason != "checked the record" || r.Reason != "destructive CRM tools need a human" {
		t.Fatalf("intervention record = %+v", r)
	}
	if b, _ := json.Marshal(r.ArgsSummary); strings.Contains(string(b), "hunter2") {
		t.Fatalf("decision record leaked the password: %s", b)
	}
}

func TestApprovalRules_DynamicMCPTool_RejectFailsCleanly(t *testing.T) {
	env := buildAREnv(t)
	sub := arSubscribe(t, env.bus, approval.EventTypeToolApprovalRequested, approval.EventTypeToolRejected)
	q := identity.Quadruple{Identity: arID, RunID: "run-ar-reject"}
	spec := arSpec(t, env, q, arGatedTool)
	done := arRun(env, spec)

	req := arNext(t, sub, approval.EventTypeToolApprovalRequested).Payload.(approval.ToolApprovalRequestedPayload)
	arEnqueue(t, env, q, steering.ControlReject, req.PauseToken, "")
	rej := arNext(t, sub, approval.EventTypeToolRejected).Payload.(approval.ToolRejectedPayload)
	if rej.Decision != string(pauseresume.DecisionReject) || rej.DecidedByUser != arApprover.UserID || rej.Tool != arGatedTool {
		t.Fatalf("tool.rejected = %+v", rej)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(arWaitDeadline):
		t.Fatal("run did not finish after reject")
	}
	if got := env.deletes.Load(); got != 0 {
		t.Fatalf("rejected MCP tool ran %d times", got)
	}
	steps := spec.Base.Trajectory.Steps
	if len(steps) != 1 || steps[0].Error == "" {
		t.Fatalf("rejected step should carry the rejection as its error, got %+v", steps)
	}
	recs, err := env.decisions.Recent(context.Background(), arID)
	if err != nil || len(recs) != 1 || recs[0].Outcome != approval.OutcomeRejected {
		t.Fatalf("decision record = %+v, %v", recs, err)
	}
}

func TestApprovalRules_UnmatchedToolIsNotGated(t *testing.T) {
	env := buildAREnv(t)
	sub := arSubscribe(t, env.bus, approval.EventTypeToolApprovalRequested)
	q := identity.Quadruple{Identity: arID, RunID: "run-ar-read"}
	done := arRun(env, arSpec(t, env, q, arUngatedTool))
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(arWaitDeadline):
		t.Fatal("ungated run did not finish")
	}
	if got := env.reads.Load(); got != 1 {
		t.Fatalf("read-only tool ran %d times, want 1", got)
	}
	select {
	case ev := <-sub.Events():
		t.Fatalf("unmatched tool requested approval: %+v", ev)
	default:
	}
}
