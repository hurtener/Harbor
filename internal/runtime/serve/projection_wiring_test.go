package serve

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/transports/stream"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/sessions"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/telemetry"
	"github.com/hurtener/Harbor/internal/tools"
)

// projWiringDeps bundles the real subsystem handles a projection prod-wiring
// test drives through BuildMux.
type projWiringDeps struct {
	tasks   tasks.TaskRegistry
	sess    *sessions.Registry
	coord   pauseresume.Coordinator
	catalog tools.ToolCatalog
	in      MuxInput
}

// projWiringToolName is the single tool the prod-wiring catalog registers so
// the tools surface has a real row to project.
const projWiringToolName = "probe_tool"

// buildProjWiringMux assembles the SAME MuxInput cmd/harbor assembles (real
// task registry + session registry + pause coordinator + event bus), with
// Validator=nil (the test-kit WithoutValidator opt-out so requests reach the
// handlers). The returned deps expose the registries + coordinator so the
// test can seed real state that the mux-mounted Service must project.
func buildProjWiringMux(t *testing.T) projWiringDeps {
	t.Helper()
	red := auditpatterns.New()
	bus := mkDriverTestBus(t, red)
	taskReg := mkDriverTestTaskRegistry(t, bus, red)
	steerReg := steering.NewRegistry()
	surface, err := protocol.NewControlSurface(taskReg, steerReg)
	if err != nil {
		t.Fatalf("NewControlSurface: %v", err)
	}
	metricsReg, metricsShutdown, err := telemetry.NewMetricsRegistry(
		config.TelemetryConfig{ServiceName: "projwiring-test"},
		telemetry.WithMetricReader(sdkmetric.NewManualReader()))
	if err != nil {
		t.Fatalf("NewMetricsRegistry: %v", err)
	}
	t.Cleanup(func() { _ = metricsShutdown(context.Background()) })
	st, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close(context.Background()) })
	sessReg, err := sessions.New(st, config.SessionsConfig{
		IdleTTL: 24 * time.Hour, HardCap: 720 * time.Hour, SweepInterval: 15 * time.Minute,
	}, bus)
	if err != nil {
		t.Fatalf("sessions.New: %v", err)
	}
	t.Cleanup(func() { _ = sessReg.CloseRegistry(context.Background()) })
	coord := pauseresume.New()

	// A real catalog with one tool so the tools surface has a row to project
	// and the annotator wiring (WithAnnotator, gated on Catalog + State) fires.
	cat := tools.NewCatalog()
	if err := cat.Register(tools.ToolDescriptor{
		Tool: tools.Tool{
			Name:        projWiringToolName,
			Description: "prod-wiring probe tool",
			Transport:   tools.TransportInProcess,
			SideEffects: tools.SideEffectRead,
			Loading:     tools.LoadingAlways,
		},
		Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
			return tools.ToolResult{}, nil
		},
	}); err != nil {
		t.Fatalf("catalog.Register: %v", err)
	}

	in := MuxInput{
		Cfg:          config.Defaults(),
		Surface:      surface,
		Bus:          bus,
		Redactor:     red,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Metrics:      metricsReg,
		Tasks:        taskReg,
		Sessions:     sessReg,
		Catalog:      cat,
		State:        st,
		Coordinator:  coord,
		Validator:    nil, // WithoutValidator: requests reach the handlers (identity via headers)
		DisplayName:  "projwiring-test",
		InstanceID:   "projwiring-test",
		BuildVersion: "test",
		BuildCommit:  "test",
	}
	return projWiringDeps{tasks: taskReg, sess: sessReg, coord: coord, catalog: cat, in: in}
}

// postMux issues an identity-headered POST through the mounted mux.
func postMux(t *testing.T, m http.Handler, path string, id identity.Identity, body string) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(stream.HeaderTenant, id.TenantID)
	req.Header.Set(stream.HeaderUser, id.UserID)
	req.Header.Set(stream.HeaderSession, id.SessionID)
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

// TestProdWiring_TasksListThroughBuildMux is the tasks prod-wiring test named
// by the projection-completeness contract (Half B — never-wired). It drives
// `tasks.list` through the projector AS ASSEMBLED BY BuildMux (NOT a
// hand-mirrored replica of the wiring) so a refactor that drops the
// `WithApprovalChecker(NewApprovalChecker(in.Coordinator))` block from
// BuildMux ships has_pending_approval=false on a gated fleet and FAILS this
// test. Removing that mux.go block and re-running this test MUST turn it red.
func TestProdWiring_TasksListThroughBuildMux(t *testing.T) {
	deps := buildProjWiringMux(t)
	built, err := BuildMux(deps.in)
	if err != nil {
		t.Fatalf("BuildMux: %v", err)
	}

	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s-gated"}
	// Spawn a run-ful task and open a REAL run-scoped approval gate on it.
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	h, err := deps.tasks.Spawn(ctx, tasks.SpawnRequest{
		Identity:    identity.Quadruple{Identity: id, RunID: "run-1"},
		Kind:        tasks.KindBackground,
		Description: "gated task",
		Query:       "q",
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if err := deps.tasks.MarkRunning(ctx, h.ID); err != nil {
		t.Fatalf("MarkRunning: %v", err)
	}
	runCtx, err := identity.WithRun(context.Background(), id, "run-1")
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}
	if _, err := deps.coord.Request(runCtx, pauseresume.PauseRequest{
		Identity: id,
		Reason:   pauseresume.ReasonApprovalRequired,
	}); err != nil {
		t.Fatalf("coord.Request: %v", err)
	}

	code, body := postMux(t, built.Mux, "/v1/tasks/list", id, `{"filter":{}}`)
	if code != http.StatusOK {
		t.Fatalf("tasks.list through BuildMux: status %d, body %s", code, body)
	}
	var resp prototypes.TaskListResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode tasks.list response: %v (body %s)", err, body)
	}
	if len(resp.Rows) != 1 {
		t.Fatalf("tasks.list returned %d rows, want 1", len(resp.Rows))
	}
	if !resp.Rows[0].HasPendingApproval {
		t.Fatal("has_pending_approval=false through the REAL BuildMux assembly — " +
			"the WithApprovalChecker block is not wired (the never-wired variant this test exists to catch)")
	}
}

// TestProdWiring_ToolsAnnotatorThroughBuildMux is the tools prod-wiring test
// named by the projection-completeness contract (Half B — never-wired). It
// drives `tools.list` with an annotator-backed OAuth-status facet through the
// projector AS ASSEMBLED BY BuildMux (NOT a hand-mirrored replica of the
// wiring). With the production Annotator wired (BuildMux's
// `WithAnnotator(...)` block, gated on Catalog + State) the facet is ACCEPTED
// (200) and the response-riding catalog aggregates are NOT marked partial; a
// refactor that drops the WithAnnotator block leaves AnnotationsAvailable()==
// false, so the Service LOUD-REJECTS the facet (invalid_request/400) and marks
// the aggregates partial — this test then turns red, catching the never-wired
// variant that MOTIVATED the whole HA-24 band (D-313 / D-314). Removing that
// mux.go block and re-running this test MUST turn it red.
func TestProdWiring_ToolsAnnotatorThroughBuildMux(t *testing.T) {
	deps := buildProjWiringMux(t)
	built, err := BuildMux(deps.in)
	if err != nil {
		t.Fatalf("BuildMux: %v", err)
	}

	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s-tools"}

	// (a) The annotator-backed OAuth facet is ACCEPTED through the REAL
	// assembly — the flip from D-313's loud-reject. A dropped WithAnnotator
	// regresses this to a 400.
	code, body := postMux(t, built.Mux, "/v1/tools/list", id,
		`{"filter":{"oauth_statuses":["Required"]}}`)
	if code != http.StatusOK {
		t.Fatalf("tools.list oauth_statuses facet through BuildMux: status %d "+
			"(want 200 — the annotator must be wired; a 400 means WithAnnotator is not installed, the never-wired variant this test exists to catch), body %s",
			code, body)
	}

	// (b) The response-riding aggregates are REAL (not marked partial) with the
	// annotator wired — the fabricated-zero the class kills is gone.
	code, body = postMux(t, built.Mux, "/v1/tools/list", id, `{"filter":{}}`)
	if code != http.StatusOK {
		t.Fatalf("tools.list through BuildMux: status %d, body %s", code, body)
	}
	var resp prototypes.ToolListResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode tools.list response: %v (body %s)", err, body)
	}
	if resp.AggregatesPartial {
		t.Fatal("aggregates_partial=true through the REAL BuildMux assembly — " +
			"the WithAnnotator block is not wired (the never-wired variant this test exists to catch)")
	}
	if resp.Aggregates.Total != 1 {
		t.Fatalf("Aggregates.Total = %d, want 1 (the one registered tool)", resp.Aggregates.Total)
	}
}

// TestApprovalChecker_RunlessTaskDoesNotInheritSiblingGate pins the FAIL-F1
// guard directly: an open run-scoped approval gate in a session must NOT be
// attributed to a run-less (RunID=="") sibling task in the same session. A
// run-less task holds no run-scoped gate, so the honest answer is false —
// without the guard the pause-list scan would fan across every gate in the
// session and mis-attribute the sibling's gate.
func TestApprovalChecker_RunlessTaskDoesNotInheritSiblingGate(t *testing.T) {
	coord := pauseresume.New()
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}
	runCtx, err := identity.WithRun(context.Background(), id, "run-ful")
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}
	if _, err := coord.Request(runCtx, pauseresume.PauseRequest{
		Identity: id, Reason: pauseresume.ReasonApprovalRequired,
	}); err != nil {
		t.Fatalf("coord.Request: %v", err)
	}
	checker := NewApprovalChecker(coord)
	// The run-ful task with the matching run reads true.
	if !checker.HasPendingApproval(context.Background(), id, "run-ful") {
		t.Fatal("run-ful gated task: HasPendingApproval = false, want true")
	}
	// A run-less sibling in the SAME session must read false (no inheritance).
	if checker.HasPendingApproval(context.Background(), id, "") {
		t.Fatal("run-less sibling inherited the run-ful task's gate — cross-task mis-attribution (FAIL-F1)")
	}
	// A different run in the same session also reads false (run isolation).
	if checker.HasPendingApproval(context.Background(), id, "other-run") {
		t.Fatal("a different run inherited the gate — run scoping is broken")
	}
}

// TestProdWiring_SessionsListCounterFacetThroughBuildMux is the sessions
// prod-wiring test named by the projection-completeness contract (Half B). It
// drives `sessions.list` with a numeric-counter facet through the projector
// AS ASSEMBLED BY BuildMux. With the CounterEnricher wired (BuildMux's
// `bus != nil && Tasks != nil && Coordinator != nil` block) the facet is
// ACCEPTED (200); a refactor that drops that WithEnricher block leaves
// CountersAvailable()==false, so the Service loud-rejects the facet
// (invalid_request/400) — this test then turns red, catching the never-wired
// variant. (An unwired build returning a false-empty page is exactly the
// defect the enricher fixed; the loud-reject is the honest degradation.)
func TestProdWiring_SessionsListCounterFacetThroughBuildMux(t *testing.T) {
	deps := buildProjWiringMux(t)
	built, err := BuildMux(deps.in)
	if err != nil {
		t.Fatalf("BuildMux: %v", err)
	}
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s-1"}
	// Seed a real session so the list has a row to project.
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	if _, err := deps.sess.Open(ctx, id.SessionID, id); err != nil {
		t.Fatalf("sessions.Open: %v", err)
	}

	code, body := postMux(t, built.Mux, "/v1/sessions/list", id, `{"filter":{"cost_above_cents":0}}`)
	if code != http.StatusOK {
		t.Fatalf("sessions.list cost_above_cents facet through BuildMux: status %d (want 200 — the counter enricher must be wired), body %s", code, body)
	}
}

// TestProdWiring_V128ProjectionSurfacesThroughBuildMux is the serve-band
// HA-64/65 wiring proof: opening the turns + rollups projections over
// the REAL bus and threading them into MuxInput makes the
// `sessions.turns.list` and `observability.query` routes LIVE through
// the real BuildMux assembly (never 404), the projection-backed
// sessions counter facet is accepted, and the projection stores feed
// the erasure-cascade fencers. A refactor that drops the
// WithSessionTurnsService / WithObservabilityService / projection-
// enricher / eraser-fencer blocks from BuildMux turns this test red.
func TestProdWiring_V128ProjectionSurfacesThroughBuildMux(t *testing.T) {
	deps := buildProjWiringMux(t)
	ctx := context.Background()
	cfg := config.Defaults()
	cfg.Sessions.Turns = config.TurnsConfig{Driver: "inmem"}
	cfg.Observability = config.ObservabilityConfig{Rollups: config.RollupsConfig{Driver: "inmem"}}

	turnsProj, turnsSvc, turnsCloser, tErr := OpenTurnsProjection(ctx, cfg, TurnsProjectionDeps{
		Bus: deps.in.Bus, Sessions: deps.sess, Tasks: deps.tasks,
		Artifacts: deps.in.Artifacts, Logger: deps.in.Logger,
	})
	if tErr != nil {
		t.Fatalf("OpenTurnsProjection: %v", tErr)
	}
	t.Cleanup(func() {
		if turnsCloser != nil {
			_ = turnsCloser(ctx)
		}
	})
	if turnsProj == nil || turnsSvc == nil {
		t.Fatal("inmem turns projection must wire a projector + service")
	}

	rollupsStore, rollupsWorker, rollupsCloser, rErr := OpenRollupsProjection(ctx, cfg, RollupsProjectionDeps{
		Bus: deps.in.Bus, Logger: deps.in.Logger,
	})
	if rErr != nil {
		t.Fatalf("OpenRollupsProjection: %v", rErr)
	}
	t.Cleanup(func() {
		if rollupsCloser != nil {
			_ = rollupsCloser(ctx)
		}
	})
	if rollupsStore == nil || rollupsWorker == nil {
		t.Fatal("inmem rollups projection must wire a store + quality source")
	}

	in := deps.in
	in.TurnsProjector = turnsProj
	in.TurnsStore = turnsSvc.Store()
	in.RollupsStore = rollupsStore
	in.RollupsQuality = rollupsWorker
	built, err := BuildMux(in)
	if err != nil {
		t.Fatalf("BuildMux (projections wired): %v", err)
	}

	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s-proj"}

	// (a) sessions.turns.list is LIVE (the WithSessionTurnsService block
	//     is wired): a real request dispatches into the projection
	//     service — an empty inmem projection answers an empty page, never
	//     a 404.
	code, body := postMux(t, built.Mux, "/v1/sessions/turns/list", id,
		`{"identity":{"tenant":"t","user":"u","session":"s-proj"},"session_id":"s-proj","limit":10}`)
	if code == http.StatusNotFound {
		t.Fatalf("sessions.turns.list = 404 — the HA-64 turns service is not mounted (the WithSessionTurnsService block is missing)")
	}
	if code != http.StatusOK {
		t.Fatalf("sessions.turns.list status = %d (want 200 — the turns projection must serve an empty page), body %s", code, body)
	}

	// (b) observability.query is LIVE (the WithObservabilityService block
	//     is wired): a real bounded query dispatches into the rollup
	//     service — an empty inmem projection answers empty rows + the
	//     mandatory freshness block, never a 404.
	code, body = postMux(t, built.Mux, "/v1/observability/query", id,
		`{"from":"2026-05-19T09:00:00Z","to":"2026-05-19T10:00:00Z","bucket":"hour","measures":["llm_completions"],"limit":100}`)
	if code == http.StatusNotFound {
		t.Fatalf("observability.query = 404 — the HA-65 rollup service is not mounted (the WithObservabilityService block is missing)")
	}
	if code != http.StatusOK {
		t.Fatalf("observability.query status = %d (want 200 — the rollup projection must answer empty rows), body %s", code, body)
	}

	// (c) The projection-backed sessions counter enricher is wired: a
	//     numeric-counter facet is ACCEPTED (200), never the loud-reject
	//     of an unwired enricher.
	if _, err := deps.sess.Open(ctx, id.SessionID, id); err != nil {
		t.Fatalf("sessions.Open: %v", err)
	}
	code, body = postMux(t, built.Mux, "/v1/sessions/list", id, `{"filter":{"cost_above_cents":0}}`)
	if code != http.StatusOK {
		t.Fatalf("sessions.list cost_above_cents facet with the projection enricher wired: status %d (want 200), body %s", code, body)
	}
}
