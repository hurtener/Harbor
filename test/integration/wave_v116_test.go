//go:build darwin || linux

// Wave v1.16 parallel-intent-coordination boundary regression gate
// (§17.5 / §17.7 step 5).
//
// The v1.16 wave (phases 185–191) taught the runtime to fan work out and
// coordinate the intent behind it: the Batch decision + executor (185/186),
// the task-management meta-tools and the operator > agent > cascade cancel
// hierarchy (187), background-wake notification honesty (188), cache-token
// telemetry capture (189), the synthetic default-agent row (190), and the
// southbound OAuth insufficient-scope leg (191 — THIS phase).
//
// This gate proves all six surfaces compose over REAL production drivers
// (assembled runtime stack over inmem state/tasks/events/artifacts, the real
// notifications Subscriber, the real cost-emitting LLM safety client, the
// real wire transport + Agent Registry, and the real MCP driver against an
// httptest fixture). Identity — the (tenant, user, session) triple, plus run
// where a run exists — is asserted on EVERY leg. At least one failure mode
// per surface. The OAuth leg runs N=12 tenants concurrently with NO
// cross-talk, and a second batch-stack stress runs N=12 concurrent sessions;
// both under -race (§17.3).
//
//  1. (185/186) A mixed tool+spawn Batch turn on the assembled stack:
//     identity propagates onto every spawned task record + tool provenance.
//  2. (187) The `_cancel_task` meta-tool + the operator > agent > cascade
//     cancel hierarchy, including the `isolate` detach + the
//     ErrTaskNotOwnDescendant scope-violation failure mode.
//  3. (188) A background-wake `task.completed` (NotifyOnComplete=true) round-
//     trips to a `notification.task_completed` reaching the conversation
//     surface, identity carried unchanged.
//  4. (189) Cache read/write tokens ride a real completion through the LLM
//     safety client's `llm.cost.recorded` emit — a fake driver returns usage
//     with cache tokens (CI-safe; no live provider).
//  5. (190) `agents.list` returns the synthetic default-agent row
//     (is_default: true) attributed to the caller's own verified triple.
//  6. (191) An insufficient-scope MCP call surfaces the structured
//     *tools.ErrInsufficientScope, tenant-isolated across N concurrent
//     tenants with no field bleed.
//
// Helpers from batch_executor_test.go / notifications_topic_test.go /
// agents_page_test.go (same package) are reused so this gate composes the
// exact production wiring those phase tests already prove in isolation.
package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	artifactsInmem "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/dispatch"
	"github.com/hurtener/Harbor/internal/runtime/notifications"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tools"
	mcpdrv "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

// wv116CacheDriverName is the fake LLM driver this gate registers to prove
// the cache-token telemetry leg (189) against the real cost-emitting safety
// client, with NO live provider. Registered once from init() (llm.Register
// panics on a duplicate name, so registration must be binary-global, not
// per-test).
const wv116CacheDriverName = "wavev116cache"

// wv116CacheDriver is a deterministic fake Driver returning a completion whose
// Usage carries a cache read/write split — the exact shape a prompt-cached
// provider response reports. It exercises the real `llm.cost.recorded` emit
// path (safety.go) without a network call.
type wv116CacheDriver struct{}

func (wv116CacheDriver) Complete(_ context.Context, _ llm.CompleteRequest) (llm.CompleteResponse, error) {
	return llm.CompleteResponse{
		Content: "ok",
		Cost:    llm.Cost{TotalCost: 0.01, Currency: "USD"},
		Usage: llm.Usage{
			PromptTokens:     1000,
			CompletionTokens: 200,
			TotalTokens:      1200,
			CacheReadTokens:  800,
			CacheWriteTokens: 150,
		},
	}, nil
}

func (wv116CacheDriver) Close(context.Context) error { return nil }

func init() {
	llm.Register(wv116CacheDriverName, func(llm.ConfigSnapshot, llm.Deps) (llm.Driver, error) {
		return wv116CacheDriver{}, nil
	})
}

func TestE2E_WaveV116(t *testing.T) {
	t.Run("batch_spawn_tools_turn", testWaveV116BatchSpawnTools)              // 185/186
	t.Run("metatool_cancel_hierarchy", testWaveV116MetaToolCancelHierarchy)   // 187
	t.Run("background_wake_notification", testWaveV116BackgroundWake)         // 188
	t.Run("cache_token_capture", testWaveV116CacheTokenCapture)               // 189
	t.Run("default_agent_row", testWaveV116DefaultAgentRow)                   // 190
	t.Run("oauth_insufficient_scope_tenant_isolated", testWaveV116OAuthScope) // 191
	t.Run("batch_concurrency_stress", testWaveV116BatchConcurrencyStress)     // N>=10 stress
}

// testWaveV116BatchSpawnTools proves the 185/186 Batch surface end-to-end on
// the assembled production stack: a mixed 2-tool + 3-spawn Batch dispatches
// all five branches, identity propagates onto every spawned task record
// (triple + ParentTaskID) and onto the tool invocation's provenance.
func testWaveV116BatchSpawnTools(t *testing.T) {
	stack, seen := buildBatchStack(t, 0)
	id := identity.Identity{TenantID: "acme", UserID: "u1", SessionID: "sess-v116-batch"}

	tripleCtx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	parentID := spawnParent(t, stack.Tasks, tripleCtx, id)

	ctx, rc := batchIDCtx(t, id, string(parentID))
	rawAny, _, err := stack.Executor.ExecuteDecision(ctx, rc, mixedBatch("acme"))
	if err != nil {
		t.Fatalf("ExecuteDecision(Batch): %v", err)
	}
	obs := rawAny.(planner.BatchObservation)
	if len(obs.Tools) != 2 || len(obs.Spawns) != 3 {
		t.Fatalf("observation tools=%d spawns=%d, want 2/3", len(obs.Tools), len(obs.Spawns))
	}
	// Tool provenance: the in-proc tool observed THIS session's identity.
	if _, ok := seen.Load(id.SessionID); !ok {
		t.Errorf("tool did not observe session %q via ctx identity", id.SessionID)
	}
	// Identity propagation onto every spawned task record.
	for i, sp := range obs.Spawns {
		if sp.Error != "" || sp.TaskID == "" {
			t.Fatalf("spawn %d did not register: %+v", i, sp)
		}
		task, gErr := stack.Tasks.Get(tripleCtx, tasks.TaskID(sp.TaskID))
		if gErr != nil {
			t.Fatalf("Get(%s): %v", sp.TaskID, gErr)
		}
		if task.Identity.Identity != id {
			t.Errorf("spawned task %s identity = %+v, want %+v", sp.TaskID, task.Identity.Identity, id)
		}
		if task.ParentTaskID == nil || *task.ParentTaskID != parentID {
			t.Errorf("spawned task %s ParentTaskID = %v, want %s", sp.TaskID, task.ParentTaskID, parentID)
		}
	}
}

// testWaveV116MetaToolCancelHierarchy proves the 187 surface: the
// `_cancel_task` meta-tool (planner.CancelTask) dispatched through the real
// toolExecutor, plus the operator > agent > cascade cancel hierarchy and the
// isolate detach. It exercises, on one shared task tree:
//
//   - agent-initiated `_cancel_task` on a descendant it spawned (cascade
//     child C2) → cancelled;
//   - the failure mode: a SIBLING run in the SAME session cancelling C1 is
//     rejected loud with dispatch.ErrTaskNotOwnDescendant (a scope violation,
//     never a silent narrowing);
//   - ancestor cascade: cancelling parent P sweeps the cascade child but
//     leaves the isolate child C1 RUNNING (isolate detaches from an
//     ancestor's cascade);
//   - operator direct cancel on the isolate child C1 → cancelled (no
//     uncancellable task — isolate never blocks a direct operator cancel).
//
// Identity (triple) is asserted on the spawned records; every dispatch
// attaches the run's identity via the executor's own identity.With path.
func testWaveV116MetaToolCancelHierarchy(t *testing.T) {
	stack, _ := buildBatchStack(t, 0)
	id := identity.Identity{TenantID: "acme", UserID: "u1", SessionID: "sess-v116-cancel"}
	tripleCtx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}

	parentID := spawnParent(t, stack.Tasks, tripleCtx, id)
	ctx, rc := batchIDCtx(t, id, string(parentID))

	// Dispatch a Batch spawning an isolate child (C1) and a cascade child
	// (C2), both under P. propagate_on_cancel is model-expressible for the
	// first time in this wave (187).
	batch := planner.Batch{
		Spawns: []planner.SpawnTask{
			{Spec: planner.SpawnSpec{Query: "isolate-child", PropagateOnCancel: tasks.PropagateIsolate}, CallID: "s0"},
			{Spec: planner.SpawnSpec{Query: "cascade-child", PropagateOnCancel: tasks.PropagateCascade}, CallID: "s1"},
		},
	}
	rawAny, _, err := stack.Executor.ExecuteDecision(ctx, rc, batch)
	if err != nil {
		t.Fatalf("ExecuteDecision(Batch): %v", err)
	}
	spawns := rawAny.(planner.BatchObservation).Spawns
	if len(spawns) != 2 {
		t.Fatalf("spawns=%d, want 2", len(spawns))
	}
	c1 := tasks.TaskID(spawns[0].TaskID) // isolate
	c2 := tasks.TaskID(spawns[1].TaskID) // cascade

	// Identity propagation onto both descendant records.
	for _, cid := range []tasks.TaskID{c1, c2} {
		task, gErr := stack.Tasks.Get(tripleCtx, cid)
		if gErr != nil {
			t.Fatalf("Get(%s): %v", cid, gErr)
		}
		if task.Identity.Identity != id {
			t.Errorf("descendant %s identity = %+v, want %+v", cid, task.Identity.Identity, id)
		}
	}

	// (agent) The run's own `_cancel_task` on the cascade child it spawned.
	rawC, _, err := stack.Executor.ExecuteDecision(ctx, rc, planner.CancelTask{TaskID: c2, Reason: "agent: losing branch"})
	if err != nil {
		t.Fatalf("ExecuteDecision(CancelTask c2): %v", err)
	}
	if cancelled, _ := rawC.(map[string]any)["cancelled"].(bool); !cancelled {
		t.Errorf("agent _cancel_task on own descendant returned cancelled=false, want true")
	}

	// (failure mode) A SIBLING run in the SAME session cannot cancel C1 — it
	// is not that run's descendant. Fail loud with ErrTaskNotOwnDescendant.
	siblingParent := spawnParent(t, stack.Tasks, tripleCtx, id)
	sibCtx, sibRc := batchIDCtx(t, id, string(siblingParent))
	_, _, sibErr := stack.Executor.ExecuteDecision(sibCtx, sibRc, planner.CancelTask{TaskID: c1, Reason: "sibling: not allowed"})
	if !errors.Is(sibErr, dispatch.ErrTaskNotOwnDescendant) {
		t.Fatalf("sibling _cancel_task err = %v, want ErrTaskNotOwnDescendant (scope violation)", sibErr)
	}

	// (cascade) Cancelling parent P sweeps cascade descendants but must leave
	// the isolate child C1 RUNNING — isolate detaches from an ancestor cascade.
	if _, cErr := stack.Tasks.Cancel(tripleCtx, parentID, "operator: cancel run"); cErr != nil {
		t.Fatalf("Cancel(parent): %v", cErr)
	}
	c1After, err := stack.Tasks.Get(tripleCtx, c1)
	if err != nil {
		t.Fatalf("Get(c1 after cascade): %v", err)
	}
	if c1After.Status == tasks.StatusCancelled {
		t.Errorf("isolate child C1 was swept by the ancestor cascade — isolate must detach it")
	}

	// (operator) A direct operator cancel on the isolate child C1 always
	// succeeds — there is no uncancellable task.
	ok, cErr := stack.Tasks.Cancel(tripleCtx, c1, "operator: direct cancel")
	if cErr != nil {
		t.Fatalf("direct Cancel(c1): %v", cErr)
	}
	if !ok {
		t.Errorf("direct operator cancel on the isolate child returned (false, nil); want a real transition")
	}
}

// testWaveV116BackgroundWake proves the 188 surface end-to-end over the
// REAL producer chain, not a hand-published payload: a real background
// task Spawned with NotifyOnComplete=true (the exact field `spawnOne`
// stamps in internal/runtime/dispatch) is driven to completion through
// the real task engine's MarkComplete, which emits `task.completed`
// carrying the flag echoed from the Task record; the real notifications
// Subscriber converts it to `notification.task_completed` reaching the
// conversation surface, identity carried unchanged. Because the whole
// chain is real, a future break in `spawnOne → Spawn → MarkComplete →
// TaskCompletedPayload.NotifyOnComplete` (dispatch.go / engine.go) fails
// THIS integration gate, not only the tasks-engine unit test.
//
// The failure mode is the negative half — a real task Spawned with
// NotifyOnComplete=false and completed the same way produces no wake
// notification.
func testWaveV116BackgroundWake(t *testing.T) {
	bus := openNotificationsBus(t)

	// REAL task engine over the SAME bus: production tasks.Open (inprocess
	// driver) over a real inmem StateStore + the canonical audit redactor.
	// This is the exact registry `spawnOne` calls Spawn on, and whose
	// MarkComplete emits task.completed (engine.go). No mock at the seam
	// (§17.3).
	store, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	reg, err := tasks.Open(context.Background(), tasks.Dependencies{
		Store:    store,
		Bus:      bus,
		Redactor: auditpatterns.New(),
		Cfg:      config.TasksConfig{Driver: "inprocess"},
	})
	if err != nil {
		t.Fatalf("tasks.Open: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close(context.Background()) })

	listener, err := bus.Subscribe(context.Background(), events.Filter{
		Admin: true,
		Types: []events.EventType{notifications.EventTypeNotificationTaskCompleted},
	})
	if err != nil {
		t.Fatalf("Subscribe listener: %v", err)
	}
	defer listener.Cancel()

	startSubscriber(t, bus)

	idQuiet := triple("v116-quiet")
	idWake := triple("v116-wake")

	// completeReal spawns a REAL background task carrying the notify opt-in
	// exactly as spawnOne does, then drives it Pending→Running→Complete via
	// the engine's real transitions so MarkComplete emits the real
	// task.completed with NotifyOnComplete echoed from the persisted record.
	completeReal := func(id identity.Quadruple, notify bool) {
		t.Helper()
		ctx, wErr := identity.With(context.Background(), id.Identity)
		if wErr != nil {
			t.Fatalf("identity.With(%v): %v", id.Identity, wErr)
		}
		h, sErr := reg.Spawn(ctx, tasks.SpawnRequest{
			Identity:         id,
			Kind:             tasks.KindBackground,
			Description:      "wave-v116 background wake",
			NotifyOnComplete: notify,
		})
		if sErr != nil {
			t.Fatalf("Spawn(notify=%v): %v", notify, sErr)
		}
		if rErr := reg.MarkRunning(ctx, h.ID); rErr != nil {
			t.Fatalf("MarkRunning(%s): %v", h.ID, rErr)
		}
		if cErr := reg.MarkComplete(ctx, h.ID, tasks.TaskResult{Value: []byte(`"done"`)}); cErr != nil {
			t.Fatalf("MarkComplete(%s): %v", h.ID, cErr)
		}
	}

	// Drive the QUIET (no-notify) task to completion FIRST: FIFO ordering
	// means a wrongly-synthesised notification for it would arrive before
	// the wake.
	completeReal(idQuiet, false)
	completeReal(idWake, true)

	// Bounded wait (no time.Sleep as a sync primitive, §17.4).
	deadline := time.After(30 * time.Second)
	var wake events.Event
	select {
	case ev, ok := <-listener.Events():
		if !ok {
			t.Fatal("listener channel closed before the wake notification arrived")
		}
		wake = ev
	case <-deadline:
		t.Fatal("deadline before notification.task_completed arrived")
	}

	// The notification rode the REAL producer chain — assert it is the
	// task-completed topic carrying the wake task's notify opt-in.
	if wake.Type != notifications.EventTypeNotificationTaskCompleted {
		t.Errorf("wake event type = %q, want %q", wake.Type, notifications.EventTypeNotificationTaskCompleted)
	}
	// Identity propagation across the whole Spawn→MarkComplete→Subscriber
	// seam: the triple + run reach the conversation surface unchanged.
	if wake.Identity != idWake {
		t.Errorf("wake notification identity bled: got %v, want %v", wake.Identity, idWake)
	}

	// Failure-mode negative: the NotifyOnComplete=false completion produced
	// no further notification (proves the flag actually gates the wake — a
	// producer that lost the flag would notify on BOTH and trip this).
	select {
	case ev, ok := <-listener.Events():
		if ok {
			t.Fatalf("unexpected extra notification %q — a NotifyOnComplete=false completion must produce none", ev.Type)
		}
	case <-time.After(500 * time.Millisecond):
		// No extra notification — correct.
	}
}

// testWaveV116CacheTokenCapture proves the 189 surface: cache read/write
// tokens ride a real completion through the LLM safety client's mandatory
// `llm.cost.recorded` emit. A fake driver returns usage with a cache split;
// the emitted CostRecordedPayload must carry both counts unchanged and the
// caller's identity quadruple. The failure mode is the mandatory identity
// guard — a completion with no identity in ctx fails loud.
func testWaveV116CacheTokenCapture(t *testing.T) {
	bus := openNotificationsBus(t)
	artStore, err := artifactsInmem.New(config.ArtifactsConfig{})
	if err != nil {
		t.Fatalf("artifacts inmem: %v", err)
	}
	t.Cleanup(func() { _ = artStore.Close(context.Background()) })

	const model = "wave-v116/cache"
	client, err := llm.Open(context.Background(), llm.ConfigSnapshot{
		Driver: wv116CacheDriverName,
		ModelProfiles: map[string]llm.ModelProfile{
			model: {ContextWindowTokens: 100000, TokenEstimator: "chars_div_4"},
		},
		// Isolate the mandatory safety band (which owns the cost emit) from
		// the optional wrapper layers — none are needed to prove the emit.
		DisableCorrections: true,
		DisableDowngrade:   true,
		DisableRetry:       true,
		DisableGovernance:  true,
	}, llm.Deps{Artifacts: artStore, Bus: bus})
	if err != nil {
		t.Fatalf("llm.Open: %v", err)
	}
	t.Cleanup(func() { _ = client.Close(context.Background()) })

	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Admin: true,
		Types: []events.EventType{llm.EventTypeCostRecorded},
	})
	if err != nil {
		t.Fatalf("Subscribe cost: %v", err)
	}
	defer sub.Cancel()

	id := identity.Identity{TenantID: "acme", UserID: "u-cache", SessionID: "sess-v116-cache"}
	ctx, err := identity.WithRun(context.Background(), id, "run-v116-cache")
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}
	text := "hello"
	if _, err := client.Complete(ctx, llm.CompleteRequest{
		Model:    model,
		Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &text}}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	deadline := time.After(5 * time.Second)
	select {
	case ev := <-sub.Events():
		p, ok := ev.Payload.(llm.CostRecordedPayload)
		if !ok {
			t.Fatalf("cost payload type = %T, want CostRecordedPayload", ev.Payload)
		}
		if p.Usage.CacheReadTokens != 800 {
			t.Errorf("Usage.CacheReadTokens = %d, want 800", p.Usage.CacheReadTokens)
		}
		if p.Usage.CacheWriteTokens != 150 {
			t.Errorf("Usage.CacheWriteTokens = %d, want 150", p.Usage.CacheWriteTokens)
		}
		// The base counts are untouched by the cache split.
		if p.Usage.PromptTokens != 1000 || p.Usage.TotalTokens != 1200 {
			t.Errorf("base Usage altered: %+v", p.Usage)
		}
		// Identity propagation onto the cost record.
		if p.Identity.Identity != id {
			t.Errorf("cost record identity = %+v, want %+v", p.Identity.Identity, id)
		}
	case <-deadline:
		t.Fatal("no llm.cost.recorded event observed within 5s")
	}

	// Failure mode: identity is mandatory — a completion with no identity in
	// ctx fails loud (never a silent unattributed cost record).
	if _, err := client.Complete(context.Background(), llm.CompleteRequest{
		Model:    model,
		Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &text}}},
	}); !errors.Is(err, llm.ErrIdentityMissing) {
		t.Errorf("Complete without identity err = %v, want ErrIdentityMissing", err)
	}
}

// testWaveV116DefaultAgentRow proves the 190 surface end-to-end over the real
// wire transport + real StateStore-backed Agent Registry: a runtime with zero
// registrations returns exactly one honest is_default row attributed to the
// caller's own verified triple (identity propagation). The failure mode is the
// wire-edge auth guard — an unauthenticated agents.list is rejected 401.
func testWaveV116DefaultAgentRow(t *testing.T) {
	deps := newPhase73eDepsWithDefault(t)
	defer deps.cleanup()

	srv := httptest.NewServer(deps.mux)
	defer srv.Close()

	id := identity.Identity{TenantID: "tenant-v116", UserID: "u-v116", SessionID: "s-v116"}
	tok := signES256Wave10(t, deps.priv, phase73eClaims(id, nil), phase73eKid)

	status, body := postAgents(t, srv.URL, "list", `{}`, tok)
	if status != http.StatusOK {
		t.Fatalf("agents.list: status=%d, want 200; body=%s", status, body)
	}
	var listResp prototypes.AgentListResponse
	if err := json.Unmarshal(body, &listResp); err != nil {
		t.Fatalf("decode agents.list: %v", err)
	}
	if len(listResp.Agents) != 1 || listResp.Aggregates.Total != 1 {
		t.Fatalf("narrow list len=%d total=%d, want 1/1 (the synthetic default row)", len(listResp.Agents), listResp.Aggregates.Total)
	}
	row := listResp.Agents[0]
	if !row.IsDefault || row.ID != wellKnownDefaultAgentID {
		t.Fatalf("narrow row = %+v, want IsDefault with the well-known id", row)
	}
	// Identity propagation: the synthetic row is attributed to the caller's
	// OWN verified triple, not a global.
	if row.Identity.Tenant != id.TenantID || row.Identity.User != id.UserID || row.Identity.Session != id.SessionID {
		t.Errorf("default row identity = %+v, want caller's own triple %+v", row.Identity, id)
	}

	// Failure mode: an unauthenticated agents.list is rejected at the wire edge.
	noAuthReq, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/agents/list", strings.NewReader(`{}`))
	noAuthResp, err := http.DefaultClient.Do(noAuthReq)
	if err != nil {
		t.Fatalf("no-auth request: %v", err)
	}
	_ = noAuthResp.Body.Close()
	if noAuthResp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no-auth agents.list: status=%d, want 401", noAuthResp.StatusCode)
	}
}

// --- 191: southbound OAuth insufficient-scope leg (THIS phase's surface) ---

// wv116Scope403Server fronts a real go-sdk streamable-HTTP MCP server: the
// handshake + tools/list pass through, but a tools/call for `needs_scope`
// answers 403 + `WWW-Authenticate: ... error="insufficient_scope"` (RFC 6750
// §3.1). It records the `_meta` triple of each shortfall call so the test can
// assert per-tenant identity propagation at the wire with no cross-talk.
type wv116Scope403Server struct {
	inner     http.Handler
	challenge string

	mu    sync.Mutex
	metas []map[string]string
}

func (s *wv116Scope403Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		body, err := io.ReadAll(r.Body)
		_ = r.Body.Close()
		if err == nil {
			if name, meta := wv116CallToolMeta(body); name == "needs_scope" {
				s.mu.Lock()
				s.metas = append(s.metas, meta)
				s.mu.Unlock()
				w.Header().Set("WWW-Authenticate", s.challenge)
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":"insufficient_scope"}`))
				return
			}
			r.Body = io.NopCloser(strings.NewReader(string(body)))
			r.ContentLength = int64(len(body))
		}
	}
	s.inner.ServeHTTP(w, r)
}

func (s *wv116Scope403Server) snapshot() []map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]map[string]string, len(s.metas))
	copy(out, s.metas)
	return out
}

// wv116CallToolMeta returns the tools/call target name + `_meta` map from a
// JSON-RPC body, or ("", nil) when the body is not a tools/call.
func wv116CallToolMeta(body []byte) (string, map[string]string) {
	var msg struct {
		Method string `json:"method"`
		Params struct {
			Name string            `json:"name"`
			Meta map[string]string `json:"_meta"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &msg); err != nil || msg.Method != "tools/call" {
		return "", nil
	}
	return msg.Params.Name, msg.Params.Meta
}

// wv116NewScope403Server builds the httptest MCP fixture: a real go-sdk server
// with a `needs_scope` tool, fronted by the 403 injector.
func wv116NewScope403Server(t *testing.T, challenge string) (*httptest.Server, *wv116Scope403Server) {
	t.Helper()
	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "harbor-v116-scope-fixture", Version: "v0"}, nil)
	okHandler := func(_ context.Context, _ *mcpsdk.CallToolRequest, _ struct{}) (*mcpsdk.CallToolResult, any, error) {
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "ok"}}}, nil, nil
	}
	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "needs_scope",
		Description: "needs_scope",
		InputSchema: map[string]any{"type": "object", "additionalProperties": false},
	}, okHandler)
	front := &wv116Scope403Server{
		inner:     mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return srv }, nil),
		challenge: challenge,
	}
	hs := httptest.NewServer(front)
	t.Cleanup(hs.Close)
	return hs, front
}

// wv116WWWAuthChallenge loads the spec-derived RFC 6750 §3.1 challenge header
// value from the committed fixture (§17.8) — a wrong-field mutation must fail
// the capture, not silently pass.
func wv116WWWAuthChallenge(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("../../internal/tools/auth/testdata/oauthdiscovery/www_authenticate_insufficient_scope.txt")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	line := strings.TrimSpace(string(raw))
	return strings.TrimSpace(strings.TrimPrefix(line, "WWW-Authenticate:"))
}

// testWaveV116OAuthScope proves the 191 surface: an insufficient-scope MCP
// call surfaces the structured *tools.ErrInsufficientScope through the REAL
// MCP driver against the httptest fixture. It runs N=12 distinct
// (tenant, user, session) triples CONCURRENTLY, each with a fresh provider (a
// 403 closes the streamable session), and asserts each returns its OWN typed
// error with no field bleed AND that the wire saw each tenant's own identity
// in `_meta` — tenant isolation with no cross-talk. Runs under -race.
func testWaveV116OAuthScope(t *testing.T) {
	challenge := wv116WWWAuthChallenge(t)
	hs, front := wv116NewScope403Server(t, challenge)
	bus := openNotificationsBus(t)

	const N = 12
	scopeErrs := make([]*tools.ErrInsufficientScope, N)
	fails := make([]error, N)
	tenants := make([]string, N)

	var wg sync.WaitGroup
	wg.Add(N)
	for i := range N {
		go func(i int) {
			defer wg.Done()
			id := identity.Identity{
				TenantID:  fmt.Sprintf("tenant-%d", i),
				UserID:    fmt.Sprintf("user-%d", i),
				SessionID: fmt.Sprintf("sess-%d", i),
			}
			tenants[i] = id.TenantID

			// A fresh provider per tenant — the 403 closes the session, so a
			// shared provider cannot be reused across the shortfall calls.
			p, err := mcpdrv.New(mcpdrv.Config{
				Name:             "scopemcp",
				TransportMode:    mcpdrv.TransportStreamableHTTP,
				URL:              hs.URL,
				Bus:              bus,
				DefaultIdentity:  identity.Identity{TenantID: "sys", UserID: "sys", SessionID: "sys"},
				OnScopeShortfall: func(mcpdrv.ScopeShortfall) {},
			})
			if err != nil {
				fails[i] = fmt.Errorf("mcp.New: %w", err)
				return
			}
			defer func() { _ = p.Close(context.Background()) }()
			if err := p.Connect(context.Background()); err != nil {
				fails[i] = fmt.Errorf("Connect: %w", err)
				return
			}
			// Discover BEFORE the shortfall call (a 403 closes the session).
			descs, err := p.Discover(context.Background())
			if err != nil {
				fails[i] = fmt.Errorf("Discover: %w", err)
				return
			}
			var needsScope tools.ToolDescriptor
			for _, d := range descs {
				if d.Tool.Name == "scopemcp_needs_scope" {
					needsScope = d
				}
			}
			if needsScope.Invoke == nil {
				fails[i] = errors.New("needs_scope not discovered")
				return
			}

			ctx, err := identity.With(context.Background(), id)
			if err != nil {
				fails[i] = fmt.Errorf("identity.With: %w", err)
				return
			}
			_, callErr := needsScope.Invoke(ctx, json.RawMessage(`{}`))
			var se *tools.ErrInsufficientScope
			if !errors.As(callErr, &se) {
				fails[i] = fmt.Errorf("Invoke err = %w, want *tools.ErrInsufficientScope", callErr)
				return
			}
			scopeErrs[i] = se
		}(i)
	}
	wg.Wait()

	for i := range N {
		if fails[i] != nil {
			t.Errorf("tenant %d: %v", i, fails[i])
			continue
		}
		se := scopeErrs[i]
		if se == nil {
			t.Errorf("tenant %d: no structured error captured", i)
			continue
		}
		// No field bleed: each tenant's typed error carries the fixture's own
		// verbatim challenge + parsed required scopes + tool name + origin.
		if se.ToolName != "needs_scope" {
			t.Errorf("tenant %d: ToolName = %q, want needs_scope", i, se.ToolName)
		}
		if se.WWWAuthenticate != challenge {
			t.Errorf("tenant %d: WWWAuthenticate = %q, want verbatim %q", i, se.WWWAuthenticate, challenge)
		}
		if len(se.RequiredScopes) != 2 || se.RequiredScopes[0] != "read:calendar" || se.RequiredScopes[1] != "write:calendar" {
			t.Errorf("tenant %d: RequiredScopes = %v, want [read:calendar write:calendar]", i, se.RequiredScopes)
		}
		if !strings.HasPrefix(se.Origin, "http") || se.DownstreamResource == "" {
			t.Errorf("tenant %d: origin/resource not populated: %+v", i, se)
		}
	}

	// Wire-level tenant isolation: the fixture saw exactly one shortfall call
	// per tenant, each carrying that tenant's OWN identity triple in `_meta`
	// — no cross-talk, no bleed.
	metas := front.snapshot()
	if len(metas) != N {
		t.Fatalf("fixture recorded %d shortfall calls, want %d", len(metas), N)
	}
	seenTenants := make(map[string]int, N)
	for _, m := range metas {
		tenant := m["tenant"]
		// The triple must be internally consistent per call (no bleed across
		// the three components within one request).
		wantUser := strings.Replace(tenant, "tenant-", "user-", 1)
		wantSess := strings.Replace(tenant, "tenant-", "sess-", 1)
		if m["user"] != wantUser || m["session"] != wantSess {
			t.Errorf("shortfall _meta triple bled: %v", m)
		}
		seenTenants[tenant]++
	}
	for _, tenant := range tenants {
		if seenTenants[tenant] != 1 {
			t.Errorf("tenant %q seen %d times at the wire, want exactly 1 (cross-talk)", tenant, seenTenants[tenant])
		}
	}
}

// testWaveV116BatchConcurrencyStress is the §17.3 cross-package concurrency
// stress: N=12 concurrent sessions each dispatch a mixed Batch against ONE
// shared assembled stack; each session's spawned tasks stay scoped to that
// session (no cross-session leakage). Runs under -race.
func testWaveV116BatchConcurrencyStress(t *testing.T) {
	const n = 12
	stack, _ := buildBatchStack(t, 0)

	var wg sync.WaitGroup
	errCh := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			id := identity.Identity{
				TenantID:  fmt.Sprintf("stress-tenant-%d", idx),
				UserID:    fmt.Sprintf("stress-user-%d", idx),
				SessionID: fmt.Sprintf("stress-session-%d", idx),
			}
			ctx, rc := batchIDCtx(t, id, fmt.Sprintf("run-%d", idx))
			rawAny, _, err := stack.Executor.ExecuteDecision(ctx, rc, mixedBatch(fmt.Sprintf("%d", idx)))
			if err != nil {
				errCh <- fmt.Errorf("session %d: %w", idx, err)
				return
			}
			if len(rawAny.(planner.BatchObservation).Spawns) != 3 {
				errCh <- fmt.Errorf("session %d: spawns != 3", idx)
				return
			}
			tripleCtx, wErr := identity.With(context.Background(), id)
			if wErr != nil {
				errCh <- wErr
				return
			}
			list, lErr := stack.Tasks.List(tripleCtx, id, tasks.TaskFilter{})
			if lErr != nil {
				errCh <- fmt.Errorf("session %d List: %w", idx, lErr)
				return
			}
			if len(list) != 3 {
				errCh <- fmt.Errorf("session %d: List returned %d tasks, want exactly its own 3 (cross-session leakage)", idx, len(list))
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
}
