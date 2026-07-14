// Cross-subsystem integration test (CLAUDE.md §17.1) for the
// projection-completeness band (Phase 177). It proves — with REAL drivers at
// every seam and the projector AS ASSEMBLED BY serve.BuildMux (no
// hand-mirrored wiring), no mocks at the boundary — that:
//
//   - tasks.list `has_pending_approval` is populated from the REAL pause
//     coordinator through the mux-assembled Service, so
//     `filter.has_pending_approval=true` narrows to real gated tasks;
//   - population is identity-scoped: session A's open approval gate never
//     bleeds onto session B's row (cross-session isolation);
//   - a run-less task in the SAME session as a gated run-ful sibling reads
//     has_pending_approval=false (intra-session cross-task isolation — the
//     run-less-task guard);
//   - a failure mode: memory.list `filter.agent_ids` over the unpopulated
//     producer identity LOUD-REJECTS rather than returning a false-empty
//     page.
//
// Runs under `-race`; includes an N≥10 concurrency stress over the shared mux.
package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
	memoryinmem "github.com/hurtener/Harbor/internal/memory/drivers/inmem"
	memprotocol "github.com/hurtener/Harbor/internal/memory/protocol"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/transports/stream"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/runtime/serve"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/sessions"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/telemetry"
)

// projCompRig is the real-assembly rig: the BuildMux-mounted mux plus the
// registries/coordinator the test seeds real state into.
type projCompRig struct {
	mux   *http.ServeMux
	tasks tasks.TaskRegistry
	coord pauseresume.Coordinator
}

// buildProjCompRig assembles the SAME MuxInput cmd/harbor assembles, with the
// test-kit WithoutValidator opt-out (identity flows via headers). No hand
// wiring of the enricher/approval seams — the mux installs them.
func buildProjCompRig(t *testing.T) projCompRig {
	t.Helper()
	red := auditpatterns.New()
	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver: "inmem", MaxSubscribersPerSession: 64, SubscriberBufferSize: 512,
		IdleTimeout: 60 * time.Second, DropWindow: time.Second, ReplayBufferSize: 512,
	}, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	st, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close(context.Background()) })
	taskReg, err := tasks.Open(context.Background(), tasks.Dependencies{
		Store: st, Bus: bus, Redactor: red, Cfg: config.TasksConfig{Driver: "inprocess"},
	})
	if err != nil {
		t.Fatalf("tasks.Open: %v", err)
	}
	t.Cleanup(func() { _ = taskReg.Close(context.Background()) })
	sessReg, err := sessions.New(st, config.SessionsConfig{
		IdleTTL: 24 * time.Hour, HardCap: 720 * time.Hour, SweepInterval: 15 * time.Minute,
	}, bus)
	if err != nil {
		t.Fatalf("sessions.New: %v", err)
	}
	t.Cleanup(func() { _ = sessReg.CloseRegistry(context.Background()) })
	surface, err := protocol.NewControlSurface(taskReg, steering.NewRegistry())
	if err != nil {
		t.Fatalf("NewControlSurface: %v", err)
	}
	metricsReg, metricsShutdown, err := telemetry.NewMetricsRegistry(
		config.TelemetryConfig{ServiceName: "projcomp-test"},
		telemetry.WithMetricReader(sdkmetric.NewManualReader()))
	if err != nil {
		t.Fatalf("NewMetricsRegistry: %v", err)
	}
	t.Cleanup(func() { _ = metricsShutdown(context.Background()) })
	coord := pauseresume.New()

	built, err := serve.BuildMux(serve.MuxInput{
		Cfg:          config.Defaults(),
		Surface:      surface,
		Bus:          bus,
		Redactor:     red,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Metrics:      metricsReg,
		Tasks:        taskReg,
		Sessions:     sessReg,
		State:        st,
		Coordinator:  coord,
		Validator:    nil, // WithoutValidator
		DisplayName:  "projcomp",
		InstanceID:   "projcomp",
		BuildVersion: "test",
		BuildCommit:  "test",
	})
	if err != nil {
		t.Fatalf("BuildMux: %v", err)
	}
	return projCompRig{mux: built.Mux, tasks: taskReg, coord: coord}
}

func projCompPost(t *testing.T, m *http.ServeMux, id identity.Identity, body string) prototypes.TaskListResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/tasks/list", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(stream.HeaderTenant, id.TenantID)
	req.Header.Set(stream.HeaderUser, id.UserID)
	req.Header.Set(stream.HeaderSession, id.SessionID)
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("tasks.list: status %d, body %s", rec.Code, rec.Body.String())
	}
	var resp prototypes.TaskListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode tasks.list: %v", err)
	}
	return resp
}

func projCompSpawn(t *testing.T, reg tasks.TaskRegistry, id identity.Identity, runID string) {
	t.Helper()
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	h, err := reg.Spawn(ctx, tasks.SpawnRequest{
		Identity:    identity.Quadruple{Identity: id, RunID: runID},
		Kind:        tasks.KindBackground,
		Description: "projection-completeness task",
		Query:       "q",
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if err := reg.MarkRunning(ctx, h.ID); err != nil {
		t.Fatalf("MarkRunning: %v", err)
	}
}

func projCompRequestApproval(t *testing.T, coord pauseresume.Coordinator, id identity.Identity, runID string) {
	t.Helper()
	ctx, err := identity.WithRun(context.Background(), id, runID)
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}
	if _, err := coord.Request(ctx, pauseresume.PauseRequest{
		Identity: id, Reason: pauseresume.ReasonApprovalRequired,
	}); err != nil {
		t.Fatalf("coord.Request: %v", err)
	}
}

func TestE2E_ProjectionCompleteness_TasksApprovalTruthfulAndIsolated(t *testing.T) {
	rig := buildProjCompRig(t)

	idA := identity.Identity{TenantID: "tenant-1", UserID: "user-1", SessionID: "session-A"}
	idB := identity.Identity{TenantID: "tenant-1", UserID: "user-1", SessionID: "session-B"}

	// Session A: a gated run-ful task PLUS a run-less sibling in the same
	// session. Session B: an ungated run-ful task.
	projCompSpawn(t, rig.tasks, idA, "run-a")
	projCompSpawn(t, rig.tasks, idA, "") // run-less sibling (the FAIL-F1 guard)
	projCompSpawn(t, rig.tasks, idB, "run-b")
	projCompRequestApproval(t, rig.coord, idA, "run-a")

	// Session A: the run-ful gated task is true; the run-less sibling is
	// false (intra-session cross-task isolation — a run-less task does NOT
	// inherit a sibling's gate).
	respA := projCompPost(t, rig.mux, idA, `{"filter":{}}`)
	if len(respA.Rows) != 2 {
		t.Fatalf("session A: want 2 rows, got %d", len(respA.Rows))
	}
	var gated, ungated int
	for _, r := range respA.Rows {
		if r.HasPendingApproval {
			gated++
		} else {
			ungated++
		}
	}
	if gated != 1 || ungated != 1 {
		t.Fatalf("session A: want exactly 1 gated + 1 run-less-false row, got gated=%d ungated=%d (the run-less task must not inherit the sibling gate)", gated, ungated)
	}

	// The facet narrows to exactly the gated run-ful task.
	facet := projCompPost(t, rig.mux, idA, `{"filter":{"has_pending_approval":true}}`)
	if len(facet.Rows) != 1 || !facet.Rows[0].HasPendingApproval {
		t.Fatalf("has_pending_approval=true facet on A: want exactly 1 gated row, got %+v", facet.Rows)
	}

	// Session B: no gate → false; the facet excludes it (cross-session
	// isolation — A's gate never bleeds into B).
	respB := projCompPost(t, rig.mux, idB, `{"filter":{}}`)
	if len(respB.Rows) != 1 || respB.Rows[0].HasPendingApproval {
		t.Fatalf("session B: want 1 row with has_pending_approval=false, got %+v", respB.Rows)
	}
	facetB := projCompPost(t, rig.mux, idB, `{"filter":{"has_pending_approval":true}}`)
	if len(facetB.Rows) != 0 {
		t.Fatalf("has_pending_approval=true facet on B: want 0 rows (isolation), got %d", len(facetB.Rows))
	}

	// Concurrency stress: N≥10 concurrent lists through the shared mux.
	const n = 24
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/v1/tasks/list", strings.NewReader(`{"filter":{"has_pending_approval":true}}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set(stream.HeaderTenant, idA.TenantID)
			req.Header.Set(stream.HeaderUser, idA.UserID)
			req.Header.Set(stream.HeaderSession, idA.SessionID)
			rec := httptest.NewRecorder()
			rig.mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				errs <- errors.New("concurrent list non-200")
				return
			}
			var r prototypes.TaskListResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
				errs <- err
				return
			}
			if len(r.Rows) != 1 || !r.Rows[0].HasPendingApproval {
				errs <- errors.New("concurrent list saw inconsistent has_pending_approval")
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatalf("concurrent list: %v", e)
	}
}

func TestE2E_ProjectionCompleteness_MemoryAgentFacetLoudRejects(t *testing.T) {
	red := auditpatterns.New()
	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver: "inmem", MaxSubscribersPerSession: 64, SubscriberBufferSize: 512,
		IdleTimeout: 60 * time.Second, DropWindow: time.Second, ReplayBufferSize: 512,
	}, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	defer func() { _ = bus.Close(context.Background()) }()
	stateStore, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	defer func() { _ = stateStore.Close(context.Background()) }()
	store, err := memoryinmem.New(memory.ConfigSnapshot{Strategy: memory.StrategyTruncation},
		memory.Deps{State: stateStore, Bus: bus}, memoryinmem.Options{})
	if err != nil {
		t.Fatalf("memoryinmem.New: %v", err)
	}
	id := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	if err := store.AddTurn(context.Background(), id, memory.ConversationTurn{
		UserMessage: "hello", AssistantResponse: "hi",
	}); err != nil {
		t.Fatalf("AddTurn: %v", err)
	}

	// Failure mode: agent_ids over the unpopulated producer identity loud-rejects.
	_, err = memprotocol.List(context.Background(),
		memprotocol.ListDeps{Store: store, DriverName: "inmem"},
		prototypes.MemoryListRequest{Filter: prototypes.MemoryFilter{AgentIDs: []string{"agent-x"}}}, id)
	if !errors.Is(err, memprotocol.ErrInvalidFilter) {
		t.Fatalf("memory agent_ids facet: err = %v, want ErrInvalidFilter (loud-reject, never a false-empty page)", err)
	}
}
