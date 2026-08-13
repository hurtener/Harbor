// internal/runtime/dispatch/dispatch_batch_test.go — Batch executor
// dispatch tests: dispatch-table routing, auto-grouping, the structural
// whole-batch rejections (breadth cap, FailFast disagreement, defensive
// RetainTurn re-check), per-branch/per-spawn error-as-value, the
// declaration-order-preservation invariant under reversed completion
// latency, and the D-025 concurrent-reuse guarantees.
//
// Real drivers everywhere on the seam (CLAUDE.md §17.3): a real
// inprocess TaskRegistry over an inmem StateStore + a real inmem
// ArtifactStore + a real inmem EventBus, wrapped in a thin call-counting
// decorator so the "exactly one ResolveOrCreateGroup" and "zero Spawn on
// structural reject" assertions can count calls WITHOUT replacing the
// registry's real behaviour with a fake.

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/virtualagent"
)

// countingRegistry embeds a REAL TaskRegistry and counts the two calls
// the Batch dispatch makes (ResolveOrCreateGroup + Spawn) so tests can
// assert dispatch-side accounting without a behaviour-replacing fake.
type countingRegistry struct {
	tasks.TaskRegistry
	mu           sync.Mutex
	resolveCalls int
	spawnCalls   int
}

type countingProgressRegistry struct {
	*countingRegistry
	progressLookups atomic.Int64
	progressReports atomic.Int64
}

func (c *countingProgressRegistry) ProgressReporter(context.Context, tasks.TaskID) (tasks.ProgressReporter, error) {
	c.progressLookups.Add(1)
	return progressReporterFunc(func(context.Context, tasks.ReportProgressRequest) (tasks.ProgressReportResult, error) {
		c.progressReports.Add(1)
		return tasks.ProgressReportResult{Recorded: true}, nil
	}), nil
}

type progressReporterFunc func(context.Context, tasks.ReportProgressRequest) (tasks.ProgressReportResult, error)

func (f progressReporterFunc) ReportProgress(ctx context.Context, req tasks.ReportProgressRequest) (tasks.ProgressReportResult, error) {
	return f(ctx, req)
}

func (c *countingRegistry) ResolveOrCreateGroup(ctx context.Context, req tasks.GroupRequest) (*tasks.TaskGroup, error) {
	c.mu.Lock()
	c.resolveCalls++
	c.mu.Unlock()
	return c.TaskRegistry.ResolveOrCreateGroup(ctx, req)
}

func (c *countingRegistry) Spawn(ctx context.Context, req tasks.SpawnRequest) (tasks.TaskHandle, error) {
	c.mu.Lock()
	c.spawnCalls++
	c.mu.Unlock()
	return c.TaskRegistry.Spawn(ctx, req)
}

func (c *countingRegistry) counts() (resolve, spawn int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.resolveCalls, c.spawnCalls
}

// newBatchTestExecutor builds a batch-capable executor over a real
// inprocess registry (wrapped for call counting), a real inmem artifact
// store, and the supplied catalog, with the given breadth cap.
func newBatchTestExecutor(t *testing.T, cat tools.ToolCatalog, maxBatchSpawns int) (steering.ToolExecutor, *countingRegistry) {
	t.Helper()
	bus := mkSpawnAwaitTestBus(t)
	reg := &countingRegistry{TaskRegistry: mkSpawnAwaitTestTaskRegistry(t, bus)}
	exec := NewToolExecutor(cat, newTestArtifactStore(t), reg,
		WithHeavyThreshold(32*1024),
		WithMaxSpawnDepth(4),
		WithMaxBatchSpawns(maxBatchSpawns))
	return exec, reg
}

// TestExecutor_Batch_DispatchTable — ExecuteDecision routes a Batch to
// the batch method: a mixed 2-tool + 2-spawn batch dispatches all four
// branches and returns a BatchObservation aligned to declaration order.
func TestExecutor_Batch_DispatchTable(t *testing.T) {
	cat := tools.NewCatalog()
	registerEcho(t, cat, "alpha")
	registerEcho(t, cat, "beta")
	exec, reg := newBatchTestExecutor(t, cat, 5)

	q := dispatchTestQuad("r-batch")
	d := planner.Batch{
		Tools: []planner.CallTool{
			{Tool: "alpha", Args: json.RawMessage(`{"x":1}`), CallID: "t0"},
			{Tool: "beta", Args: json.RawMessage(`{"y":2}`), CallID: "t1"},
		},
		Spawns: []planner.SpawnTask{
			{Spec: planner.SpawnSpec{Query: "sub-a"}, CallID: "s0"},
			{Spec: planner.SpawnSpec{Query: "sub-b"}, CallID: "s1"},
		},
	}
	rawAny, llmAny, err := exec.ExecuteDecision(dispatchTestCtx(t, q), dispatchRunContext(cat, q), d)
	if err != nil {
		t.Fatalf("ExecuteDecision(Batch): %v", err)
	}
	raw, ok := rawAny.(planner.BatchObservation)
	if !ok {
		t.Fatalf("raw type = %T, want planner.BatchObservation", rawAny)
	}
	if _, ok := llmAny.(planner.BatchObservation); !ok {
		t.Fatalf("llm type = %T, want planner.BatchObservation", llmAny)
	}
	if len(raw.Tools) != 2 || len(raw.Spawns) != 2 {
		t.Fatalf("observation tools=%d spawns=%d, want 2/2", len(raw.Tools), len(raw.Spawns))
	}
	// Tools index-aligned + succeeded.
	for i, want := range []string{"t0", "t1"} {
		if raw.Tools[i].CallID != want || raw.Tools[i].Index != i {
			t.Errorf("raw.Tools[%d] = {CallID:%q Index:%d}, want {%q %d}", i, raw.Tools[i].CallID, raw.Tools[i].Index, want, i)
		}
		if raw.Tools[i].Error != "" || raw.Tools[i].Value == nil {
			t.Errorf("tool branch %d should succeed: %+v", i, raw.Tools[i])
		}
	}
	// Spawns index-aligned + registered ({task_id} present, no error).
	for i, want := range []string{"s0", "s1"} {
		if raw.Spawns[i].CallID != want || raw.Spawns[i].Index != i {
			t.Errorf("raw.Spawns[%d] = {CallID:%q Index:%d}, want {%q %d}", i, raw.Spawns[i].CallID, raw.Spawns[i].Index, want, i)
		}
		if raw.Spawns[i].Error != "" || raw.Spawns[i].TaskID == "" {
			t.Errorf("spawn branch %d should register: %+v", i, raw.Spawns[i])
		}
	}
	// Two ungrouped spawns → exactly one auto-group + two Spawn calls.
	resolve, spawn := reg.counts()
	if resolve != 1 {
		t.Errorf("ResolveOrCreateGroup calls = %d, want exactly 1 (>=2 ungrouped spawns auto-group)", resolve)
	}
	if spawn != 2 {
		t.Errorf("Spawn calls = %d, want 2", spawn)
	}
	// Both auto-grouped spawns share the same non-empty group id.
	if raw.Spawns[0].GroupID == "" || raw.Spawns[0].GroupID != raw.Spawns[1].GroupID {
		t.Errorf("auto-grouped spawns must share one group id: %q vs %q", raw.Spawns[0].GroupID, raw.Spawns[1].GroupID)
	}
}

// TestExecutor_Batch_AutoGroup_ExplicitGroupNeverOverwritten — a spawn
// carrying an explicit GroupID is passed through unchanged and is NOT
// routed into the auto-created group; a single ungrouped spawn in an
// otherwise-explicit batch does NOT trigger ResolveOrCreateGroup.
func TestExecutor_Batch_AutoGroup_ExplicitGroupNeverOverwritten(t *testing.T) {
	cat := tools.NewCatalog()
	registerEcho(t, cat, "tool")
	exec, reg := newBatchTestExecutor(t, cat, 5)

	q := dispatchTestQuad("r-batch")
	explicit := tasks.TaskGroupID("operator-group-1")
	// Pre-create the explicit (Open) group so the explicit-group spawn can
	// join it. Use the underlying registry directly so this setup call is
	// NOT counted against the auto-group assertion below.
	if _, err := reg.TaskRegistry.ResolveOrCreateGroup(spawnAwaitIDCtx(t), tasks.GroupRequest{
		ID: explicit, SessionID: dispatchTestID, Description: "operator group",
	}); err != nil {
		t.Fatalf("pre-create explicit group: %v", err)
	}
	d := planner.Batch{
		Tools: []planner.CallTool{{Tool: "tool", Args: json.RawMessage(`{}`), CallID: "t0"}},
		Spawns: []planner.SpawnTask{
			{Spec: planner.SpawnSpec{Query: "explicit"}, GroupID: explicit, CallID: "s0"},
			{Spec: planner.SpawnSpec{Query: "ungrouped"}, CallID: "s1"},
		},
	}
	rawAny, _, err := exec.ExecuteDecision(dispatchTestCtx(t, q), dispatchRunContext(cat, q), d)
	if err != nil {
		t.Fatalf("ExecuteDecision(Batch): %v", err)
	}
	raw := rawAny.(planner.BatchObservation)
	if raw.Spawns[0].GroupID != string(explicit) {
		t.Errorf("explicit spawn GroupID = %q, want %q (never overwritten)", raw.Spawns[0].GroupID, explicit)
	}
	// Only ONE ungrouped spawn → no auto-group (needs >=2).
	if resolve, _ := reg.counts(); resolve != 0 {
		t.Errorf("ResolveOrCreateGroup calls = %d, want 0 (a single ungrouped spawn keeps the ad-hoc path)", resolve)
	}
	// The single ungrouped spawn keeps the empty-group ad-hoc path.
	if raw.Spawns[1].GroupID != "" {
		t.Errorf("single ungrouped spawn GroupID = %q, want empty", raw.Spawns[1].GroupID)
	}
}

// TestExecutor_Batch_BreadthCap_RejectsWholeBatch — len(Spawns) > cap →
// zero tool invokes, zero Spawn calls, one whole-batch error naming the
// cap and the actual count.
func TestExecutor_Batch_BreadthCap_RejectsWholeBatch(t *testing.T) {
	cat := tools.NewCatalog()
	var invokes atomic.Int64
	if err := cat.Register(tools.ToolDescriptor{
		Tool: tools.Tool{Name: "counted"},
		Invoke: func(_ context.Context, _ json.RawMessage) (tools.ToolResult, error) {
			invokes.Add(1)
			return tools.ToolResult{Value: "ok"}, nil
		},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	exec, reg := newBatchTestExecutor(t, cat, 2) // cap = 2

	q := dispatchTestQuad("r-batch")
	d := planner.Batch{
		Tools: []planner.CallTool{{Tool: "counted", Args: json.RawMessage(`{}`), CallID: "t0"}},
		Spawns: []planner.SpawnTask{
			{Spec: planner.SpawnSpec{Query: "a"}, CallID: "s0"},
			{Spec: planner.SpawnSpec{Query: "b"}, CallID: "s1"},
			{Spec: planner.SpawnSpec{Query: "c"}, CallID: "s2"},
		},
	}
	_, _, err := exec.ExecuteDecision(dispatchTestCtx(t, q), dispatchRunContext(cat, q), d)
	if err == nil {
		t.Fatal("Batch with 3 spawns under cap=2 returned nil error, want whole-batch rejection")
	}
	if !errors.Is(err, planner.ErrInvalidDecision) {
		t.Errorf("breadth-cap rejection err = %v, want wrapped planner.ErrInvalidDecision", err)
	}
	if !strings.Contains(err.Error(), "max_batch_spawns") || !strings.Contains(err.Error(), "3") {
		t.Errorf("rejection error = %q, want it to name max_batch_spawns and the actual count 3", err.Error())
	}
	if got := invokes.Load(); got != 0 {
		t.Errorf("tool invokes = %d, want 0 (no tool branch dispatches on a rejected batch)", got)
	}
	if resolve, spawn := reg.counts(); resolve != 0 || spawn != 0 {
		t.Errorf("registry calls resolve=%d spawn=%d, want 0/0 (no spawn registers on a rejected batch)", resolve, spawn)
	}
}

// TestExecutor_Batch_FailFastDisagreement_Rejects — two ungrouped spawns
// with disagreeing Spec.FailFast → zero dispatch, error names both values.
func TestExecutor_Batch_FailFastDisagreement_Rejects(t *testing.T) {
	cat := tools.NewCatalog()
	var invokes atomic.Int64
	if err := cat.Register(tools.ToolDescriptor{
		Tool: tools.Tool{Name: "counted"},
		Invoke: func(_ context.Context, _ json.RawMessage) (tools.ToolResult, error) {
			invokes.Add(1)
			return tools.ToolResult{Value: "ok"}, nil
		},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	exec, reg := newBatchTestExecutor(t, cat, 5)

	q := dispatchTestQuad("r-batch")
	d := planner.Batch{
		Tools: []planner.CallTool{{Tool: "counted", Args: json.RawMessage(`{}`), CallID: "t0"}},
		Spawns: []planner.SpawnTask{
			{Spec: planner.SpawnSpec{Query: "a", FailFast: true}, CallID: "s0"},
			{Spec: planner.SpawnSpec{Query: "b", FailFast: false}, CallID: "s1"},
		},
	}
	_, _, err := exec.ExecuteDecision(dispatchTestCtx(t, q), dispatchRunContext(cat, q), d)
	if err == nil {
		t.Fatal("Batch with FailFast disagreement returned nil error, want rejection")
	}
	if !errors.Is(err, planner.ErrInvalidDecision) {
		t.Errorf("FailFast-disagreement rejection err = %v, want wrapped planner.ErrInvalidDecision", err)
	}
	if !strings.Contains(err.Error(), "FailFast") {
		t.Errorf("rejection error = %q, want it to name FailFast", err.Error())
	}
	if got := invokes.Load(); got != 0 {
		t.Errorf("tool invokes = %d, want 0", got)
	}
	if resolve, spawn := reg.counts(); resolve != 0 || spawn != 0 {
		t.Errorf("registry calls resolve=%d spawn=%d, want 0/0", resolve, spawn)
	}
}

// TestExecutor_Batch_RetainTurn_DefensiveReject — a spawn with
// RetainTurn=true (which NewBatch would reject at construction, but a
// raw Batch value can carry) → zero dispatch, wrapped ErrInvalidDecision.
func TestExecutor_Batch_RetainTurn_DefensiveReject(t *testing.T) {
	cat := tools.NewCatalog()
	registerEcho(t, cat, "tool")
	exec, reg := newBatchTestExecutor(t, cat, 5)

	q := dispatchTestQuad("r-batch")
	d := planner.Batch{
		Tools: []planner.CallTool{{Tool: "tool", Args: json.RawMessage(`{}`), CallID: "t0"}},
		Spawns: []planner.SpawnTask{
			{Spec: planner.SpawnSpec{Query: "a"}, CallID: "s0"},
			{Spec: planner.SpawnSpec{Query: "b", RetainTurn: true}, CallID: "s1"},
		},
	}
	_, _, err := exec.ExecuteDecision(dispatchTestCtx(t, q), dispatchRunContext(cat, q), d)
	if err == nil {
		t.Fatal("Batch with a RetainTurn spawn returned nil error, want defensive rejection")
	}
	if !errors.Is(err, planner.ErrInvalidDecision) {
		t.Errorf("err = %v, want wrapped planner.ErrInvalidDecision", err)
	}
	if resolve, spawn := reg.counts(); resolve != 0 || spawn != 0 {
		t.Errorf("registry calls resolve=%d spawn=%d, want 0/0", resolve, spawn)
	}
}

// TestExecutor_Batch_ToolCapExceeded_StructuralReject — a Batch with more
// tool branches than the parallel cap is rejected as a STRUCTURAL
// pre-check (wrapped ErrInvalidDecision) BEFORE any side effect, not as a
// mid-dispatch parallel abort: zero tool invokes, zero registry calls.
func TestExecutor_Batch_ToolCapExceeded_StructuralReject(t *testing.T) {
	cat := tools.NewCatalog()
	var invokes atomic.Int64
	if err := cat.Register(tools.ToolDescriptor{
		Tool: tools.Tool{Name: "counted"},
		Invoke: func(_ context.Context, _ json.RawMessage) (tools.ToolResult, error) {
			invokes.Add(1)
			return tools.ToolResult{Value: "ok"}, nil
		},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	exec, reg := newBatchTestExecutor(t, cat, 5)

	q := dispatchTestQuad("r-batch")
	// One over the parallel cap.
	toolBranches := make([]planner.CallTool, planner.AbsoluteMaxParallel+1)
	for i := range toolBranches {
		toolBranches[i] = planner.CallTool{Tool: "counted", Args: json.RawMessage(`{}`), CallID: fmt.Sprintf("t%d", i)}
	}
	d := planner.Batch{
		Tools:  toolBranches,
		Spawns: []planner.SpawnTask{{Spec: planner.SpawnSpec{Query: "a"}, CallID: "s0"}},
	}
	_, _, err := exec.ExecuteDecision(dispatchTestCtx(t, q), dispatchRunContext(cat, q), d)
	if err == nil {
		t.Fatal("over-parallel-cap Batch returned nil error, want structural rejection")
	}
	if !errors.Is(err, planner.ErrInvalidDecision) {
		t.Errorf("tool-cap rejection err = %v, want wrapped planner.ErrInvalidDecision", err)
	}
	if got := invokes.Load(); got != 0 {
		t.Errorf("tool invokes = %d, want 0 (structural pre-check before any dispatch)", got)
	}
	if resolve, spawn := reg.counts(); resolve != 0 || spawn != 0 {
		t.Errorf("registry calls resolve=%d spawn=%d, want 0/0", resolve, spawn)
	}
}

// TestExecutor_Batch_Invalid_DoesNotPersistProgress — structural rejection
// must happen before the durable progress reporter is even bound. The
// progress observation still uses the native CallID/Index shape on valid
// batches; this regression only guards the invalid-batch side effect.
func TestExecutor_Batch_Invalid_DoesNotPersistProgress(t *testing.T) {
	cat := tools.NewCatalog()
	registerEcho(t, cat, "counted")
	base := &countingRegistry{TaskRegistry: mkSpawnAwaitTestTaskRegistry(t, mkSpawnAwaitTestBus(t))}
	reg := &countingProgressRegistry{countingRegistry: base}
	exec := NewToolExecutor(cat, newTestArtifactStore(t), reg, WithMaxBatchSpawns(1))

	q := dispatchTestQuad("r-batch")
	d := planner.Batch{
		Tools: []planner.CallTool{{Tool: "counted", Args: json.RawMessage(`{}`), CallID: "t0"}},
		Spawns: []planner.SpawnTask{
			{Spec: planner.SpawnSpec{Query: "a"}, CallID: "s0"},
			{Spec: planner.SpawnSpec{Query: "b"}, CallID: "s1"},
		},
		Progress: []planner.TaskProgress{{CallID: "p0", Phase: "halfway"}},
	}
	_, _, err := exec.ExecuteDecision(dispatchTestCtx(t, q), dispatchRunContext(cat, q), d)
	if err == nil || !errors.Is(err, planner.ErrInvalidDecision) {
		t.Fatalf("invalid batch error = %v, want wrapped planner.ErrInvalidDecision", err)
	}
	if got := reg.progressLookups.Load(); got != 0 {
		t.Fatalf("progress reporter lookups = %d, want 0 for invalid batch", got)
	}
	if got := reg.progressReports.Load(); got != 0 {
		t.Fatalf("durable progress reports = %d, want 0 for invalid batch", got)
	}
}

// TestExecutor_Batch_DegenerateCombinedCount_Rejects — a programmatic
// Batch with fewer than two combined branches reaching ExecuteDecision is
// rejected loudly (wrapped ErrInvalidDecision), never a silent no-op
// success (the same invariant NewBatch enforces at construction).
func TestExecutor_Batch_DegenerateCombinedCount_Rejects(t *testing.T) {
	cat := tools.NewCatalog()
	registerEcho(t, cat, "solo")
	exec, reg := newBatchTestExecutor(t, cat, 5)
	q := dispatchTestQuad("r-batch")

	cases := []planner.Batch{
		{}, // zero branches
		{Tools: []planner.CallTool{{Tool: "solo", Args: json.RawMessage(`{}`), CallID: "t0"}}}, // one tool
		{Spawns: []planner.SpawnTask{{Spec: planner.SpawnSpec{Query: "a"}, CallID: "s0"}}},     // one spawn
	}
	for i, d := range cases {
		_, _, err := exec.ExecuteDecision(dispatchTestCtx(t, q), dispatchRunContext(cat, q), d)
		if err == nil {
			t.Errorf("case %d: degenerate Batch returned nil error, want rejection", i)
			continue
		}
		if !errors.Is(err, planner.ErrInvalidDecision) {
			t.Errorf("case %d: err = %v, want wrapped planner.ErrInvalidDecision", i, err)
		}
	}
	if resolve, spawn := reg.counts(); resolve != 0 || spawn != 0 {
		t.Errorf("registry calls resolve=%d spawn=%d, want 0/0 (no side effects on a degenerate batch)", resolve, spawn)
	}
}

// TestExecutor_Batch_ToolHalfError_NoOrphanGroup — the W1 regression
// guard: when the tool half returns a whole-call error (a malformed Join)
// AFTER the structural pre-checks pass, the auto-group is NEVER created
// (no orphaned memberless group) and no spawn registers. Group creation
// only follows a SUCCESSFUL tool-half dispatch.
func TestExecutor_Batch_ToolHalfError_NoOrphanGroup(t *testing.T) {
	cat := tools.NewCatalog()
	registerEcho(t, cat, "good")
	exec, reg := newBatchTestExecutor(t, cat, 5)

	q := dispatchTestQuad("r-batch")
	d := planner.Batch{
		Tools: []planner.CallTool{{Tool: "good", Args: json.RawMessage(`{}`), CallID: "t0"}},
		// A malformed Join makes parallel.Execute return a whole-call
		// error (ErrParallelInvalidJoin) — the residual tool-half error
		// path that must not orphan the auto-group.
		Join: &planner.JoinSpec{Kind: planner.JoinKind("bogus")},
		Spawns: []planner.SpawnTask{
			{Spec: planner.SpawnSpec{Query: "a"}, CallID: "s0"},
			{Spec: planner.SpawnSpec{Query: "b"}, CallID: "s1"},
		},
	}
	_, _, err := exec.ExecuteDecision(dispatchTestCtx(t, q), dispatchRunContext(cat, q), d)
	if err == nil {
		t.Fatal("malformed-Join Batch returned nil error, want the tool-half error surfaced")
	}
	// The auto-group must NOT have been created, and no spawn registered.
	if resolve, spawn := reg.counts(); resolve != 0 || spawn != 0 {
		t.Errorf("registry calls resolve=%d spawn=%d, want 0/0 — a tool-half failure orphaned a group or registered a spawn", resolve, spawn)
	}
}

// TestExecutor_Batch_PerBranchErrorAsValue — a resolve-miss tool branch
// and a sealed-group (registry-reject) spawn alongside valid branches:
// everything else dispatches; only the two failing entries carry Error,
// never a whole-batch abort (D-169 item 3 parity, both halves).
func TestExecutor_Batch_PerBranchErrorAsValue(t *testing.T) {
	cat := tools.NewCatalog()
	registerEcho(t, cat, "good")
	exec, reg := newBatchTestExecutor(t, cat, 5)

	q := dispatchTestQuad("r-batch")
	idCtx := dispatchTestCtx(t, q)
	setupCtx := spawnAwaitIDCtx(t) // triple-scoped ctx for the registry setup calls

	// Create + seal a group so a spawn joining it is rejected by the
	// registry (a per-spawn reject, not a whole-batch abort). Use the
	// underlying registry directly so setup does not skew the counters.
	grp, err := reg.TaskRegistry.ResolveOrCreateGroup(setupCtx, tasks.GroupRequest{
		SessionID: dispatchTestID, OwnerTaskID: "", Description: "sealed",
	})
	if err != nil {
		t.Fatalf("ResolveOrCreateGroup: %v", err)
	}
	if err := reg.SealGroup(setupCtx, grp.ID); err != nil {
		t.Fatalf("SealGroup: %v", err)
	}

	d := planner.Batch{
		Tools: []planner.CallTool{
			{Tool: "good", Args: json.RawMessage(`{}`), CallID: "t0"},
			{Tool: "missing", Args: json.RawMessage(`{}`), CallID: "t1"}, // resolve miss
		},
		Spawns: []planner.SpawnTask{
			{Spec: planner.SpawnSpec{Query: "ok"}, CallID: "s0"},                      // valid
			{Spec: planner.SpawnSpec{Query: "sealed"}, GroupID: grp.ID, CallID: "s1"}, // registry reject
		},
	}
	rawAny, _, err := exec.ExecuteDecision(idCtx, dispatchRunContext(cat, q), d)
	if err != nil {
		t.Fatalf("ExecuteDecision(Batch): unexpected whole-batch err: %v", err)
	}
	raw := rawAny.(planner.BatchObservation)
	// Tool 0 ok, tool 1 resolve-miss error.
	if raw.Tools[0].Error != "" || raw.Tools[0].Value == nil {
		t.Errorf("tool 0 (good) should succeed: %+v", raw.Tools[0])
	}
	if raw.Tools[1].Error == "" {
		t.Errorf("tool 1 (missing) should carry a resolve-miss error, got %+v", raw.Tools[1])
	}
	// Spawn 0 registered, spawn 1 registry-rejected.
	if raw.Spawns[0].Error != "" || raw.Spawns[0].TaskID == "" {
		t.Errorf("spawn 0 (valid) should register: %+v", raw.Spawns[0])
	}
	if raw.Spawns[1].Error == "" || raw.Spawns[1].TaskID != "" {
		t.Errorf("spawn 1 (sealed group) should carry a registry-reject error and no task id: %+v", raw.Spawns[1])
	}
}

// TestExecutor_Batch_ToolCountAccounting — the count the runloop's
// OnToolDispatched hook reads (planner.DecisionInvocationCount) is
// len(Tools) for a mixed batch and 0 for a spawns-only batch: spawns
// never increment tool-invocation accounting.
func TestExecutor_Batch_ToolCountAccounting(t *testing.T) {
	mixed := planner.Batch{
		Tools:  []planner.CallTool{{Tool: "a", CallID: "t0"}, {Tool: "b", CallID: "t1"}},
		Spawns: []planner.SpawnTask{{Spec: planner.SpawnSpec{Query: "x"}, CallID: "s0"}},
	}
	if got := planner.DecisionInvocationCount(mixed); got != 2 {
		t.Errorf("DecisionInvocationCount(mixed Batch) = %d, want 2 (len(Tools))", got)
	}
	spawnsOnly := planner.Batch{
		Spawns: []planner.SpawnTask{
			{Spec: planner.SpawnSpec{Query: "x"}, CallID: "s0"},
			{Spec: planner.SpawnSpec{Query: "y"}, CallID: "s1"},
		},
	}
	if got := planner.DecisionInvocationCount(spawnsOnly); got != 0 {
		t.Errorf("DecisionInvocationCount(spawns-only Batch) = %d, want 0", got)
	}
}

// TestExecutor_Batch_SpawnsOnly_NoTools — a spawns-only batch (empty
// Tools) dispatches its spawns with an empty Tools observation (the
// parallel executor is never invoked with an empty branch set).
func TestExecutor_Batch_SpawnsOnly_NoTools(t *testing.T) {
	cat := tools.NewCatalog()
	exec, _ := newBatchTestExecutor(t, cat, 5)
	q := dispatchTestQuad("r-batch")
	d := planner.Batch{
		Spawns: []planner.SpawnTask{
			{Spec: planner.SpawnSpec{Query: "a"}, CallID: "s0"},
			{Spec: planner.SpawnSpec{Query: "b"}, CallID: "s1"},
		},
	}
	rawAny, _, err := exec.ExecuteDecision(dispatchTestCtx(t, q), dispatchRunContext(cat, q), d)
	if err != nil {
		t.Fatalf("ExecuteDecision(spawns-only Batch): %v", err)
	}
	raw := rawAny.(planner.BatchObservation)
	if len(raw.Tools) != 0 {
		t.Errorf("Tools observation = %d entries, want 0 for a spawns-only batch", len(raw.Tools))
	}
	if len(raw.Spawns) != 2 {
		t.Errorf("Spawns observation = %d entries, want 2", len(raw.Spawns))
	}
}

func TestExecutor_Batch_OrdinaryArtifactsPreserveOrderAndPairing(t *testing.T) {
	store := newTestArtifactStore(t)
	refs := make([]artifacts.ArtifactRef, 2)
	for i, mime := range []string{"image/png", "image/jpeg"} {
		ref, err := store.PutBytes(context.Background(), artifacts.ArtifactScope{TenantID: dispatchTestID.TenantID, UserID: dispatchTestID.UserID, SessionID: dispatchTestID.SessionID}, []byte{byte(i)}, artifacts.PutOpts{MimeType: mime})
		if err != nil {
			t.Fatal(err)
		}
		refs[i] = ref
	}
	bus := mkSpawnAwaitTestBus(t)
	reg := &countingRegistry{TaskRegistry: mkSpawnAwaitTestTaskRegistry(t, bus)}
	exec := NewToolExecutor(tools.NewCatalog(), store, reg, WithMaxBatchSpawns(5))
	q := dispatchTestQuad("r-artifact-batch")
	rc := planner.RunContext{Quadruple: q, DispositionPolicy: planner.DispositionPolicy{ByMIME: map[string]planner.AttachmentDisposition{"image/*": planner.DispositionRef}}}
	d := planner.Batch{Spawns: []planner.SpawnTask{
		{CallID: "first", Spec: planner.SpawnSpec{Query: "first", InputArtifactIDs: []string{refs[0].ID}, InputArtifactDispositions: map[string]string{refs[0].ID: "inline"}}},
		{CallID: "second", Spec: planner.SpawnSpec{Query: "second", InputArtifactIDs: []string{refs[1].ID}}},
	}}
	rawAny, _, err := exec.ExecuteDecision(dispatchTestCtx(t, q), rc, d)
	if err != nil {
		t.Fatal(err)
	}
	raw := rawAny.(planner.BatchObservation)
	if len(raw.Spawns) != 2 || raw.Spawns[0].CallID != "first" || raw.Spawns[1].CallID != "second" {
		t.Fatalf("spawn order = %+v", raw.Spawns)
	}
	for i, want := range []struct{ id, disposition string }{{refs[0].ID, "inline"}, {refs[1].ID, "ref"}} {
		task, err := reg.Get(dispatchTestCtx(t, q), tasks.TaskID(raw.Spawns[i].TaskID))
		if err != nil {
			t.Fatal(err)
		}
		if len(task.InputArtifactIDs) != 1 || task.InputArtifactIDs[0] != want.id || task.InputArtifactDispositions[want.id] != want.disposition {
			t.Fatalf("sibling %d leaked pairing: %+v", i, task)
		}
	}
}

func TestExecutor_SpawnTask_VirtualProfileOwnsArtifactDisposition(t *testing.T) {
	bus := mkSpawnAwaitTestBus(t)
	reg := mkSpawnAwaitTestTaskRegistry(t, bus)
	store := newTestArtifactStore(t)
	ref, err := store.PutBytes(context.Background(), artifacts.ArtifactScope{TenantID: dispatchTestID.TenantID, UserID: dispatchTestID.UserID, SessionID: dispatchTestID.SessionID}, []byte("png"), artifacts.PutOpts{MimeType: "image/png"})
	if err != nil {
		t.Fatal(err)
	}
	profile := virtualagent.Profile{Key: "reviewer", Label: "Reviewer", Parent: "top-agent", InputCount: 1, InputDisposition: "ref", OutputSchema: json.RawMessage(`{"type":"object"}`)}
	profile = virtualagent.NormalizeProfile(profile)
	frozen, err := virtualagent.NewFrozenMap(virtualagent.Map{Owner: "top-agent", Profiles: []virtualagent.Profile{profile}}, "rev-1", strings.Repeat("a", 64), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := virtualagent.WithFrozenMap(dispatchTestCtx(t, dispatchTestQuad("r-virtual")), frozen)
	q := dispatchTestQuad("r-virtual")
	exec := NewToolExecutor(tools.NewCatalog(), store, reg)
	raw, _, err := exec.ExecuteDecision(ctx, planner.RunContext{Quadruple: q, DispositionPolicy: planner.DispositionPolicy{Default: planner.DispositionInline}}, planner.SpawnTask{Spec: planner.SpawnSpec{Query: "virtual", VirtualAgent: "reviewer", InputArtifactIDs: []string{ref.ID}, InputArtifactDispositions: map[string]string{ref.ID: "inline"}}})
	if err != nil {
		t.Fatal(err)
	}
	task, err := reg.Get(dispatchTestCtx(t, q), tasks.TaskID(raw.(map[string]any)["task_id"].(string)))
	if err != nil {
		t.Fatal(err)
	}
	if task.InputArtifactDispositions[ref.ID] != "ref" || task.VirtualAgent == nil || task.VirtualAgent.Profile.InputCount != 1 || string(task.VirtualAgent.Profile.OutputSchema) != `{"type":"object"}` {
		t.Fatalf("virtual profile authority drifted: %+v", task)
	}
}

// TestExecutor_Batch_PreservesDeclarationOrder — with tool branches
// instrumented to complete in REVERSE declaration order (the last branch
// finishes first), the BatchObservation stays index-aligned to
// declaration order for BOTH halves. The concrete regression guard for
// "Go map iteration must never determine reply order."
func TestExecutor_Batch_PreservesDeclarationOrder(t *testing.T) {
	const nTools = 4
	cat := tools.NewCatalog()
	// Each tool sleeps longer the EARLIER it appears, so completion order
	// is the reverse of declaration order.
	for i := range nTools {
		name := fmt.Sprintf("tool%d", i)
		delay := time.Duration(nTools-i) * 15 * time.Millisecond
		if err := cat.Register(tools.ToolDescriptor{
			Tool: tools.Tool{Name: name},
			Invoke: func(ctx context.Context, _ json.RawMessage) (tools.ToolResult, error) {
				select {
				case <-time.After(delay):
				case <-ctx.Done():
					return tools.ToolResult{}, ctx.Err()
				}
				return tools.ToolResult{Value: map[string]any{"name": name}}, nil
			},
		}); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}
	exec, _ := newBatchTestExecutor(t, cat, 5)

	q := dispatchTestQuad("r-batch")
	toolBranches := make([]planner.CallTool, nTools)
	for i := range nTools {
		toolBranches[i] = planner.CallTool{Tool: fmt.Sprintf("tool%d", i), Args: json.RawMessage(`{}`), CallID: fmt.Sprintf("t%d", i)}
	}
	spawns := []planner.SpawnTask{
		{Spec: planner.SpawnSpec{Query: "a"}, CallID: "s0"},
		{Spec: planner.SpawnSpec{Query: "b"}, CallID: "s1"},
		{Spec: planner.SpawnSpec{Query: "c"}, CallID: "s2"},
	}
	d := planner.Batch{Tools: toolBranches, Spawns: spawns}

	rawAny, _, err := exec.ExecuteDecision(dispatchTestCtx(t, q), dispatchRunContext(cat, q), d)
	if err != nil {
		t.Fatalf("ExecuteDecision(Batch): %v", err)
	}
	raw := rawAny.(planner.BatchObservation)
	for i := range d.Tools {
		if raw.Tools[i].CallID != d.Tools[i].CallID || raw.Tools[i].Index != i {
			t.Errorf("raw.Tools[%d] = {CallID:%q Index:%d}, want CallID=%q Index=%d (declaration order must survive reverse completion)",
				i, raw.Tools[i].CallID, raw.Tools[i].Index, d.Tools[i].CallID, i)
		}
	}
	for i := range d.Spawns {
		if raw.Spawns[i].CallID != d.Spawns[i].CallID || raw.Spawns[i].Index != i {
			t.Errorf("raw.Spawns[%d] = {CallID:%q Index:%d}, want CallID=%q Index=%d",
				i, raw.Spawns[i].CallID, raw.Spawns[i].Index, d.Spawns[i].CallID, i)
		}
	}
}

// TestExecutor_Batch_ConcurrentReuse — N=128 concurrent Batch dispatches
// against ONE shared executor + ONE shared registry, each under its own
// identity, under -race. Asserts the four D-025 guarantees: no data
// races, no context bleed (each tool branch observes its OWN session; no
// spawn leaks across identities), no cancellation cross-talk (a subset of
// runs dispatch on a pre-cancelled ctx; the live runs are unaffected),
// and no goroutine leak after all runs settle.
func TestExecutor_Batch_ConcurrentReuse(t *testing.T) {
	const n = 128
	cat := tools.NewCatalog()
	// whoami echoes the session id it observed via ctx identity.
	if err := cat.Register(tools.ToolDescriptor{
		Tool: tools.Tool{Name: "whoami"},
		Invoke: func(ctx context.Context, _ json.RawMessage) (tools.ToolResult, error) {
			q, ok := identity.QuadrupleFrom(ctx)
			if !ok {
				return tools.ToolResult{}, errors.New("missing identity in branch ctx")
			}
			return tools.ToolResult{Value: map[string]any{"session": q.SessionID}}, nil
		},
	}); err != nil {
		t.Fatalf("register whoami: %v", err)
	}
	registerEcho(t, cat, "side")
	exec, _ := newBatchTestExecutor(t, cat, 5)

	baseline := runtime.NumGoroutine()

	var wg sync.WaitGroup
	errCh := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			id := identity.Identity{
				TenantID:  "tenant-" + strconv.Itoa(idx),
				UserID:    "user-" + strconv.Itoa(idx),
				SessionID: "session-" + strconv.Itoa(idx),
			}
			runID := "r-" + strconv.Itoa(idx)
			q := identity.Quadruple{Identity: id, RunID: runID}
			rc := dispatchRunContext(cat, q)
			ctx, wErr := identity.WithRun(context.Background(), id, runID)
			if wErr != nil {
				errCh <- wErr
				return
			}
			d := planner.Batch{
				Tools: []planner.CallTool{
					{Tool: "whoami", Args: json.RawMessage(`{}`), CallID: "t0"},
					{Tool: "side", Args: json.RawMessage(`{}`), CallID: "t1"},
				},
				Spawns: []planner.SpawnTask{
					{Spec: planner.SpawnSpec{Query: "a-" + strconv.Itoa(idx)}, CallID: "s0"},
					{Spec: planner.SpawnSpec{Query: "b-" + strconv.Itoa(idx)}, CallID: "s1"},
				},
			}

			// Cancellation cross-talk probe: odd runs dispatch on a
			// pre-cancelled ctx (outcome ignored); even (live) runs MUST
			// still fully succeed, proving one run's cancel never aborts
			// another's batch.
			if idx%2 == 1 {
				cctx, cancel := context.WithCancel(ctx)
				cancel()
				_, _, _ = exec.ExecuteDecision(cctx, rc, d)
				return
			}

			rawAny, _, err := exec.ExecuteDecision(ctx, rc, d)
			if err != nil {
				errCh <- fmt.Errorf("run %d: %w", idx, err)
				return
			}
			raw := rawAny.(planner.BatchObservation)
			if len(raw.Tools) != 2 || len(raw.Spawns) != 2 {
				errCh <- fmt.Errorf("run %d: tools=%d spawns=%d, want 2/2", idx, len(raw.Tools), len(raw.Spawns))
				return
			}
			m, ok := raw.Tools[0].Value.(map[string]any)
			if !ok || m["session"] != id.SessionID {
				errCh <- fmt.Errorf("run %d: context bleed — whoami saw %v, want session %q", idx, raw.Tools[0].Value, id.SessionID)
				return
			}
			for _, sp := range raw.Spawns {
				if sp.Error != "" || sp.TaskID == "" {
					errCh <- fmt.Errorf("run %d: spawn did not register: %+v", idx, sp)
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
	assertGoroutineBaseline(t, baseline)
}
