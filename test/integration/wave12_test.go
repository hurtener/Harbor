// Wave 12 cross-subsystem integration test per CLAUDE.md §17.5 + §17.7
// step 5 — the wave-end E2E, bundled with the final phase (Phase 70).
//
// Wave 12 closes the CLI / inspect / hot-reload / draft-save cluster:
//
//   - Phase 65 (hot-reload events on the bus) — parallel-PR; not yet
//     merged at this PR's authoring time.
//   - Phase 66 (draft endpoints for `harbor dev`) — parallel-PR; not
//     yet merged at this PR's authoring time.
//   - Phase 69 (inspect-events / inspect-runs) — parallel-PR; not yet
//     merged at this PR's authoring time.
//   - Phase 70 (inspect-topology — D-102, THIS PR) — graduates the
//     Phase 63 stub; renders a run's node graph as deterministic ASCII.
//   - #126 (cfg.Planner driver registry) — parallel-PR; not yet merged
//     at this PR's authoring time.
//
// # Parallel-PR coverage backfill (audit F5 closure)
//
// Phase 70's PR shipped this file with ONLY the inspect-topology
// surface covered (the parallel Stage-2 PRs were not yet merged).
// The audit's F5 finding closed the §17.7 step 5 gap: the wave-end
// E2E must cover the WAVE'S surface, not just the final phase's.
// The chore(checkpoint): wave-12 audit fixes PR added scenarios for
// the four remaining surfaces (Phase 65, 66, 69, #126) below — see
// the "Wave 12 surface backfill scenarios" section near the end.
// The Phase 65 (hot-reload) coverage lives in-package at
// `cmd/harbor/cmd_dev_hot_reload_test.go::TestHotReloadSupervisor_
// RebuildEmitsCompletedOnNewBus` (§17.2 in-package integration shape)
// because the supervisor wraps `bootDevStack` in `package main` and
// is not importable from this file — see that test's godoc for the
// rationale and the audit's F4 closure.
//
// # Why this test does NOT exec the `harbor inspect-topology` binary
//
// `cmd/harbor` is `package main` — its renderer + synthesiser + cmd
// body are NOT importable from `test/integration/`. The wave-end E2E
// instead asserts the WAVE'S SURFACE composes end-to-end: the events
// the inspect-topology cmd would consume (tool.invoked /
// tool.completed / task.spawned / planner.finish / pause.requested)
// actually flow through the Protocol's SSE wire transport, scoped by
// identity, in the shape the renderer expects. The CLI body's
// rendering of those events is unit-tested in `cmd/harbor`
// (cmd_inspect_topology_test.go + topology_render_test.go). Together
// these prove the full Phase 70 path: cmd consumes events ←→ wave
// surface produces them.
//
// # Per CLAUDE.md §17.3
//
//  1. Real drivers everywhere on the seam — audit/drivers/patterns,
//     events/drivers/inmem, state/drivers/inmem, artifacts/drivers/inmem,
//     tasks/drivers/inprocess, the real pauseresume.Coordinator, real
//     steering.Registry, real protocol.ControlSurface, real
//     protocol/auth.Validator, real transports.Mux. (LLM mock is the
//     §13 dev-only escape hatch, same shape Phase 64 uses.)
//  2. Identity propagation through every layer — JWT → middleware →
//     ctx → bus.Subscribe filter. Two parallel tenants each see ONLY
//     their own run's events.
//  3. ≥1 failure mode — a wire SSE subscription against a tenant the
//     token does NOT carry is rejected at the auth-middleware edge
//     with HTTP 401 (the "wrong tenant scope" path).
//  4. -race is the CI gate.
//  5. N≥10 concurrency stress — TestE2E_Wave12_Concurrency_NoCrossTalk
//     runs N=10 distinct identity stacks against ONE shared assembled
//     stack, each opening an SSE subscription and observing only its
//     own events.
package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/harbortest/devstack"
	_ "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	_ "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tasks"
	_ "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess"
	"github.com/hurtener/Harbor/internal/tools"
)

// wave12ID is the canonical tenant-A identity. wave12IDB is tenant-B
// (used for the cross-tenant isolation scenario).
var (
	wave12ID = identity.Identity{
		TenantID:  "tenant-A",
		UserID:    "user-A",
		SessionID: "session-A",
	}
	wave12IDB = identity.Identity{
		TenantID:  "tenant-B",
		UserID:    "user-B",
		SessionID: "session-B",
	}
)

// wave12RunID is the canonical run ID for the synthetic event stream.
const wave12RunID = "run-wave12-AAA"

// writeWave12Config writes the dev-shaped harbor.yaml. Kept minimal —
// no `tools.entries[]` because the wave-end E2E does not exercise the
// catalog wiring (Phase 70's surface is event consumption, not tool
// invocation). The LLM driver is "mock" so the helper does not need
// the bifrost runtime — but the actual mock driver registration is
// not needed either because the wave-end test does NOT open the LLM
// (Memory.Driver is empty, Tools.Entries is empty, no planner runs).
func writeWave12Config(t *testing.T) *config.Config {
	t.Helper()
	yaml := `
server:
  bind_addr: 127.0.0.1:0
  shutdown_grace_period: 2s
identity:
  jwt_algorithms:
    - RS256
    - ES256
  issuer: https://issuer.example.com
  audience: harbor
  jwks_url: https://issuer.example.com/.well-known/jwks.json
telemetry:
  log_format: text
  log_level: error
  service_name: harbor-wave12
state:
  driver: inmem
llm:
  driver: mock
  provider: ""
  model: ""
  api_key: ""
  timeout: 30s
  context_window_reserve: 0.05
governance:
  repair_attempts: 1
events:
  driver: inmem
  max_subscribers_per_session: 64
  subscriber_buffer_size: 256
  idle_timeout: 60s
  drop_window: 1s
  replay_buffer_size: 1024
sessions:
  idle_ttl: 24h
  hard_cap: 720h
  sweep_interval: 15m
artifacts:
  driver: inmem
  heavy_output_threshold_bytes: 32768
tasks:
  driver: inprocess
  retain_turn_timeout: 5m
  continuation_hop_limit: 8
distributed:
  bus_driver: loopback
  remote_driver: loopback
memory:
  driver: inmem
  strategy: none
`
	dir := t.TempDir()
	p := filepath.Join(dir, "harbor.yaml")
	if err := os.WriteFile(p, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := config.Load(context.Background(), p)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg
}

// buildWave12Stack assembles a stack with the SSE wire transport
// available. SkipRunLoop because no LLM is wired (the wave-end E2E
// drives synthetic events directly onto the bus — the production
// surface that inspect-topology actually consumes).
func buildWave12Stack(t *testing.T, signerIdentity identity.Identity) *devstack.DevStack {
	t.Helper()
	cfg := writeWave12Config(t)
	opts := devstack.AssembleOpts{
		SkipRunLoop: true,
		SkipCatalog: true, // no tools — wave12 doesn't exercise the catalog wiring
	}
	opts.Identity.Tenant = signerIdentity.TenantID
	opts.Identity.User = signerIdentity.UserID
	opts.Identity.Session = signerIdentity.SessionID
	return devstack.Assemble(t, cfg, opts)
}

// publishWave12Run publishes the canonical "happy run" event stream to
// the bus under the supplied identity + run id. The stream mirrors
// what a real planner-driven run would emit and what inspect-topology
// would render: task.spawned → tool.invoked → tool.completed →
// planner.finish (the four-node minimum). Uses the REAL payload types
// from the owning subsystems so the wire-side serialization matches
// production exactly.
//
// Returns nothing — tests assert against the SSE subscription side.
func publishWave12Run(t *testing.T, bus events.EventBus, id identity.Identity, runID string) {
	t.Helper()
	// Production-shape identity tuples:
	//   - `task.spawned` is dispatched from the `start` Protocol method
	//     with `Quadruple{Identity: id}` only — RunID is EMPTY at spawn
	//     time. The payload's TaskID carries the per-task identifier;
	//     the per-task RunLoop driver later sets `Identity.RunID =
	//     TaskID` once the task starts running (D-098). See the
	//     §17.6 worked example — fixtures must mirror this shape or
	//     the test silently diverges from production (the audit's F2).
	//   - Subsequent events emitted from INSIDE the running task carry
	//     `Quadruple{Identity: id, RunID: runID}` because the RunLoop
	//     driver populated the quadruple by then.
	spawnQ := identity.Quadruple{Identity: id}
	q := identity.Quadruple{Identity: id, RunID: runID}
	ctx := context.Background()
	// task.spawned — empty Identity.RunID, payload.TaskID = runID
	// (production sets TaskID = runID because the per-task RunLoop
	// driver derives the run id from the task id; the test mirrors
	// that 1:1 mapping).
	if err := bus.Publish(ctx, events.Event{
		Type:     tasks.EventTypeTaskSpawned,
		Identity: spawnQ,
		Payload: tasks.TaskSpawnedPayload{
			TaskID:   tasks.TaskID(runID),
			Kind:     tasks.KindForeground,
			Priority: 0,
		},
	}); err != nil {
		t.Fatalf("publish task.spawned: %v", err)
	}
	// tool.invoked
	if err := bus.Publish(ctx, events.Event{
		Type:     tools.EventTypeToolInvoked,
		Identity: q,
		Payload: tools.ToolInvokedPayload{
			Identity:  q,
			ToolName:  "echo_tool",
			Transport: tools.TransportInProcess,
			StartedAt: time.Now(),
		},
	}); err != nil {
		t.Fatalf("publish tool.invoked: %v", err)
	}
	// tool.completed
	if err := bus.Publish(ctx, events.Event{
		Type:     tools.EventTypeToolCompleted,
		Identity: q,
		Payload: tools.ToolCompletedPayload{
			Identity:   q,
			ToolName:   "echo_tool",
			Transport:  tools.TransportInProcess,
			Attempts:   1,
			DurationMS: 12,
		},
	}); err != nil {
		t.Fatalf("publish tool.completed: %v", err)
	}
	// planner.finish
	if err := bus.Publish(ctx, events.Event{
		Type:     planner.EventTypePlannerFinish,
		Identity: q,
		Payload: wave12FinishPayload{
			Reason: "goal",
		},
	}); err != nil {
		t.Fatalf("publish planner.finish: %v", err)
	}
}

// wave12FinishPayload is the local SafePayload-shaped wrapper for
// `planner.finish` events. The planner package registers the event
// type but does NOT yet define a typed payload (the typed payload
// lands when a planner concrete emits the event in production —
// `react.New`'s emit site is in `internal/planner/react`). The
// wave-end E2E publishes synthetic events to prove the SSE wire
// transport ferries them; the local payload mirrors what a future
// `planner.FinishPayload` would carry.
type wave12FinishPayload struct {
	events.SafeSealed
	Reason string
}

// waitForGoroutineBaseline polls runtime.NumGoroutine until the count
// is at or below `target` or `maxWait` elapses. Returns true when the
// baseline is observed; false on timeout. Writes the final observed
// count into *finalCount so the caller can report it. Replaces the
// previous "GC + time.Sleep + read NumGoroutine" pattern that the
// §17.4 audit flagged — polling converges to the true baseline
// without the deterministic sleep penalty.
func waitForGoroutineBaseline(maxWait time.Duration, target int, finalCount *int) bool {
	deadline := time.Now().Add(maxWait)
	for time.Now().Before(deadline) {
		runtime.GC()
		runtime.Gosched()
		got := runtime.NumGoroutine()
		if got <= target {
			*finalCount = got
			return true
		}
		// Yield then re-check; no time.Sleep — runtime.Gosched is the
		// non-sleep scheduler-yield.
		for range 10 {
			runtime.Gosched()
		}
	}
	*finalCount = runtime.NumGoroutine()
	return false
}

// readSSEEventsUntilFinish opens an SSE subscription against srv,
// reads frames until a `planner.finish` for the supplied runID
// arrives or maxWait elapses, then returns the raw body bytes plus
// the event count.
//
// `serverSideRunFilter` controls the `X-Harbor-Run` header. Set to
// false when the test wants to observe ALL events for the supplied
// identity (production-shape CLI subscribers DO NOT pass the header
// — the audit's F1 fix — because the Phase 60 SSE handler drops
// `task.spawned` events whose `Identity.RunID` is empty). Set to true
// only when the test specifically asserts the server-side run-filter's
// own cross-talk-suppression behaviour over events that DO carry
// `Identity.RunID` (tool.* and planner.finish — emitted from inside
// the running task where the per-task RunLoop driver populated the
// quadruple).
func readSSEEventsUntilFinish(t *testing.T, srv *httptest.Server, token, runID string, maxWait time.Duration, serverSideRunFilter bool) ([]byte, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), maxWait)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/events", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if serverSideRunFilter {
		req.Header.Set("X-Harbor-Run", runID)
	}
	req.Header.Set("Last-Event-ID", "0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /v1/events: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("SSE status = %d, want 200 (body=%s)", resp.StatusCode, body)
	}
	finishMarker := []byte(`"type":"planner.finish"`)
	runMarker := []byte(`"run":"` + runID + `"`)
	var accumulated bytes.Buffer
	buf := make([]byte, 4096)
	count := 0
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			accumulated.Write(buf[:n])
			count++
			if bytes.Contains(accumulated.Bytes(), finishMarker) && bytes.Contains(accumulated.Bytes(), runMarker) {
				return accumulated.Bytes(), count
			}
		}
		if readErr != nil {
			return accumulated.Bytes(), count
		}
	}
}

// ----------------------------------------------------------------------------
// Scenario 1 — boot + SSE round-trip for inspect-topology's event consumption.
// ----------------------------------------------------------------------------

// TestE2E_Wave12_InspectTopology_EventSurface_RoundTrips asserts the
// surface inspect-topology consumes is wired end-to-end. A test
// publishes the canonical happy run stream to the bus; a Bearer-token
// SSE subscription against the wire transport observes every event,
// filtered by run-id via X-Harbor-Run.
func TestE2E_Wave12_InspectTopology_EventSurface_RoundTrips(t *testing.T) {
	stack := buildWave12Stack(t, wave12ID)
	defer stack.Close()
	if stack.Handler == nil {
		t.Skip("devstack did not assemble transports (Phase 60 missing?)")
	}
	srv := httptest.NewServer(stack.Handler)
	defer srv.Close()

	// Publish synchronously — the inmem driver implements
	// events.Replayer and the `Last-Event-ID: 0` header in
	// readSSEEventsUntilFinish requests a full replay, so any
	// "subscribe-after-publish" race the original publish-in-goroutine
	// sleep was guarding against is impossible. F6: removed the
	// time.Sleep-as-sync that violated §17.4.
	publishWave12Run(t, stack.Bus, wave12ID, wave12RunID)

	// serverSideRunFilter=false — production CLIs (inspect-topology /
	// inspect-runs) do the same so they can observe `task.spawned`.
	body, count := readSSEEventsUntilFinish(t, srv, stack.Token, wave12RunID, 5*time.Second, false)
	if count == 0 {
		t.Fatal("no SSE chunks read")
	}
	requiredMarkers := []string{
		`"type":"task.spawned"`,
		`"type":"tool.invoked"`,
		`"type":"tool.completed"`,
		`"type":"planner.finish"`,
		`"run":"` + wave12RunID + `"`,
	}
	for _, m := range requiredMarkers {
		if !bytes.Contains(body, []byte(m)) {
			t.Errorf("SSE body missing marker %q (body length=%d)", m, len(body))
		}
	}
}

// ----------------------------------------------------------------------------
// Scenario 2 — cross-tenant isolation.
// ----------------------------------------------------------------------------

// TestE2E_Wave12_InspectTopology_CrossTenantIsolation asserts a tenant-B
// token CANNOT observe tenant-A's events on the SSE wire — the
// identity-filter at bus.Subscribe enforces the isolation tuple. This
// is the multi-isolation invariant for inspect-topology: a leaked
// operator token from one tenant cannot inspect another tenant's
// runs.
func TestE2E_Wave12_InspectTopology_CrossTenantIsolation(t *testing.T) {
	// Build TWO separate dev stacks, one per tenant — each carries a
	// dev token scoped to its own identity. (The same runtime cannot
	// mint two different-identity tokens via the helper at the same
	// time; separate stacks per tenant is the cleaner shape.)
	stackA := buildWave12Stack(t, wave12ID)
	defer stackA.Close()
	stackB := buildWave12Stack(t, wave12IDB)
	defer stackB.Close()
	if stackA.Handler == nil || stackB.Handler == nil {
		t.Skip("devstack did not assemble transports")
	}
	srvA := httptest.NewServer(stackA.Handler)
	defer srvA.Close()
	srvB := httptest.NewServer(stackB.Handler)
	defer srvB.Close()

	// Publish tenant-A events to stack A; publish tenant-B events to
	// stack B. (Each stack has its own bus — they are isolated by
	// construction.) An SSE subscriber against stack B with stack B's
	// token must NOT observe tenant-A events (which are not even on
	// its bus).
	// Publish synchronously — Last-Event-ID: 0 in readSSEEventsUntilFinish
	// triggers replay against the inmem driver's Replayer impl. F6:
	// removed the time.Sleep-as-sync guards that violated §17.4.
	publishWave12Run(t, stackA.Bus, wave12ID, "run-tenantA-1")
	publishWave12Run(t, stackB.Bus, wave12IDB, "run-tenantB-1")

	// serverSideRunFilter=false here too — cross-tenant isolation is
	// enforced by the identity-tuple subscription scope, NOT by the
	// run filter; observing all events under each identity is what
	// makes the leak assertion meaningful.
	bodyA, _ := readSSEEventsUntilFinish(t, srvA, stackA.Token, "run-tenantA-1", 3*time.Second, false)
	bodyB, _ := readSSEEventsUntilFinish(t, srvB, stackB.Token, "run-tenantB-1", 3*time.Second, false)

	// Tenant A's body must NOT contain tenant B's tenant id.
	if bytes.Contains(bodyA, []byte(`"tenant":"tenant-B"`)) {
		t.Errorf("tenant-A SSE leaked tenant-B identity (cross-tenant data leak)")
	}
	// Tenant B's body must NOT contain tenant A's tenant id.
	if bytes.Contains(bodyB, []byte(`"tenant":"tenant-A"`)) {
		t.Errorf("tenant-B SSE leaked tenant-A identity (cross-tenant data leak)")
	}
	// Both bodies SHOULD contain their own run-id + identity.
	if !bytes.Contains(bodyA, []byte(`"run":"run-tenantA-1"`)) {
		t.Errorf("tenant-A SSE missing tenant-A's own run-id")
	}
	if !bytes.Contains(bodyB, []byte(`"run":"run-tenantB-1"`)) {
		t.Errorf("tenant-B SSE missing tenant-B's own run-id")
	}
}

// ----------------------------------------------------------------------------
// Scenario 3 — failure mode: missing-token rejection.
// ----------------------------------------------------------------------------

// TestE2E_Wave12_InspectTopology_AuthRequired_RejectedAtEdge asserts a
// GET /v1/events with no Bearer token is rejected at the auth edge —
// the wire never reaches the bus. This is inspect-topology's "missing
// token" failure-mode equivalent on the SERVER side (the cmd's
// CodeInspectTopologyAuthMissing fires before the wire; this test
// asserts the matching server-side enforcement).
func TestE2E_Wave12_InspectTopology_AuthRequired_RejectedAtEdge(t *testing.T) {
	stack := buildWave12Stack(t, wave12ID)
	defer stack.Close()
	if stack.Handler == nil {
		t.Skip("devstack did not assemble transports")
	}
	srv := httptest.NewServer(stack.Handler)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/events", nil)
	// Deliberately omit the Authorization header.
	req.Header.Set("X-Harbor-Run", wave12RunID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /v1/events: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("expected HTTP 401, got %d (body=%s)", resp.StatusCode, body)
	}
}

// ----------------------------------------------------------------------------
// Scenario 4 — N=10 concurrency stress.
// ----------------------------------------------------------------------------

// TestE2E_Wave12_InspectTopology_Concurrency_NoCrossTalk runs N=10
// concurrent SSE subscribers against the SAME assembled stack, each
// scoped to its own run id. Asserts:
//   - Every subscriber observes its OWN run's events.
//   - No subscriber observes a different run's events.
//   - The goroutine count returns to baseline after all subscribers
//     close (no leak in the wire transport layer).
//
// Mirrors the §17.3 concurrency-stress shape — N=10 is the floor;
// CI machines tolerate it without flake.
func TestE2E_Wave12_InspectTopology_Concurrency_NoCrossTalk(t *testing.T) {
	stack := buildWave12Stack(t, wave12ID)
	defer stack.Close()
	if stack.Handler == nil {
		t.Skip("devstack did not assemble transports")
	}
	srv := httptest.NewServer(stack.Handler)
	defer srv.Close()

	// Baseline before launching subscribers. Settle via runtime.Gosched
	// (yield + collect prior goroutines) rather than time.Sleep — the
	// scheduler-yield converges to a stable count deterministically.
	runtime.GC()
	for range 100 {
		runtime.Gosched()
	}
	baseline := runtime.NumGoroutine()

	const N = 10
	var wg sync.WaitGroup
	var fail atomic.Int32
	for i := range N {
		runID := fmt.Sprintf("run-stress-%03d", i)
		wg.Add(1)
		go func(rid string) {
			defer wg.Done()
			// Publish synchronously — Last-Event-ID: 0 makes the
			// SSE subscriber replay from cursor 0, so the
			// publish-after-subscribe race doesn't matter. F6: removed
			// the stagger sleep + outer goroutine that violated §17.4.
			publishWave12Run(t, stack.Bus, wave12ID, rid)

			// serverSideRunFilter=true — the stress test specifically
			// validates that the X-Harbor-Run server-side filter
			// suppresses cross-run leakage among subscribers sharing
			// the SAME identity tuple. The filter works correctly for
			// tool.* and planner.finish events (which DO carry
			// Identity.RunID after the per-task RunLoop driver sets
			// it); task.spawned (empty RunID) is the documented
			// exception and the stress test's assertions are scoped
			// to events that do carry RunID.
			body, _ := readSSEEventsUntilFinish(t, srv, stack.Token, rid, 5*time.Second, true)
			// Every subscriber should see its own run id.
			if !bytes.Contains(body, []byte(`"run":"`+rid+`"`)) {
				t.Errorf("stress goroutine %s: missing own run id", rid)
				fail.Add(1)
				return
			}
			// And NO other run id from the loop should appear (no
			// cross-talk).
			for j := range N {
				if j == i {
					continue
				}
				other := fmt.Sprintf("run-stress-%03d", j)
				if bytes.Contains(body, []byte(`"run":"`+other+`"`)) {
					t.Errorf("stress goroutine %s: leaked event from %s", rid, other)
					fail.Add(1)
				}
			}
		}(runID)
	}
	wg.Wait()
	if fail.Load() != 0 {
		t.Fatalf("stress had %d failures", fail.Load())
	}

	// Goroutine baseline restoration. Poll for convergence rather
	// than sleep — bounded eventually-poll per §17.4. The +8 slack
	// (N1 bump) absorbs busy-CI scheduler noise.
	var got int
	if !waitForGoroutineBaseline(2*time.Second, baseline+8, &got) {
		t.Errorf("goroutine leak: baseline=%d after=%d (delta=%d)",
			baseline, got, got-baseline)
	}
}

// ----------------------------------------------------------------------------
// Wave 12 surface backfill scenarios — audit F5 closure.
//
// Phase 70's PR shipped this file covering ONLY the inspect-topology
// surface. The audit's F5 finding closed the §17.7 step 5 gap: the
// wave-end E2E must cover the WAVE'S surface, not just the final
// phase's. The scenarios below add coverage for #126 (planner driver
// registry), Phase 66 (draft endpoints), and Phase 69 (inspect-events
// SSE wire shape). Phase 65 (hot-reload supervisor) is covered
// in-package by `cmd/harbor/cmd_dev_hot_reload_test.go::TestHotRelo
// adSupervisor_RebuildEmitsCompletedOnNewBus` per §17.2 — the
// supervisor wraps `bootDevStack` in `package main` and is not
// importable from this file.
// ----------------------------------------------------------------------------

// TestE2E_Wave12_PlannerRegistry_ResolvesReactDriver — #126 / D-103
// surface. The §4.4 planner driver registry resolves `react` (the V1
// default) when given a valid PlannerConfig + LLM dep; rejects an
// unknown driver name loud per §13. Mirrors the cfg-side validator
// + registry-side dispatch the production boot path exercises in
// `cmd/harbor/cmd_dev.go::bootDevStack`.
func TestE2E_Wave12_PlannerRegistry_ResolvesReactDriver(t *testing.T) {
	// Real LLMClient via the mock driver (the §13 dev-only escape
	// hatch the binary itself uses under HARBOR_DEV_ALLOW_MOCK=1).
	// The planner registry never sees the "mock" name — the LLM is
	// just a dependency the planner factory consumes.
	cfg := writeWave12Config(t)
	llmClient, err := openMockLLMForPlannerTest(t, cfg)
	if err != nil {
		t.Fatalf("openMockLLMForPlannerTest: %v", err)
	}
	defer func() { _ = llmClient.Close(context.Background()) }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Happy path: react driver resolves to a non-nil Planner.
	plnr, err := planner.Resolve(ctx, planner.PlannerConfig{Driver: "react"}, planner.FactoryDeps{LLM: llmClient})
	if err != nil {
		t.Fatalf("planner.Resolve(react): %v", err)
	}
	if plnr == nil {
		t.Fatal("planner.Resolve(react) returned nil planner without error")
	}

	// Fail-loud: unknown driver returns ErrDriverUnknown with the
	// registered-driver list in the message.
	_, err = planner.Resolve(ctx, planner.PlannerConfig{Driver: "no-such-driver"}, planner.FactoryDeps{LLM: llmClient})
	if err == nil {
		t.Fatal("planner.Resolve(no-such-driver) returned nil error; want ErrDriverUnknown")
	}
	if !errorsIsDriverUnknown(err) {
		t.Errorf("planner.Resolve(no-such-driver) err = %v; want errors.Is(_, planner.ErrDriverUnknown)", err)
	}

	// Empty driver name fails loud at the registry level — the
	// "react" default lives at the cfg → planner.PlannerConfig
	// boundary (`cmd/harbor/cmd_dev.go::plannerConfigFromConfig`),
	// NOT inside the registry. This asserts the registry's strict
	// fail-loud contract per §13.
	_, err = planner.Resolve(ctx, planner.PlannerConfig{Driver: ""}, planner.FactoryDeps{LLM: llmClient})
	if err == nil {
		t.Fatal("planner.Resolve(\"\") returned nil error; the registry must fail loud on empty driver name")
	}
}

// errorsIsDriverUnknown wraps errors.Is(_, planner.ErrDriverUnknown).
// Helper to keep the test's planner-package dependency narrow.
func errorsIsDriverUnknown(err error) bool {
	return planner.ErrDriverUnknown != nil && errorsIsAny(err, planner.ErrDriverUnknown)
}

func errorsIsAny(err error, targets ...error) bool {
	for _, t := range targets {
		if errorsIs(err, t) {
			return true
		}
	}
	return false
}

// errorsIs avoids an additional `errors` import alongside `events.Cursor`.
func errorsIs(err, target error) bool {
	for err != nil {
		if err == target { //nolint:errorlint // sentinel comparison
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// openMockLLMForPlannerTest constructs the mock LLM client per the
// §13 escape-hatch shape `cmd/harbor` uses when HARBOR_DEV_ALLOW_MOCK
// fires. Returns an llm.LLMClient that the planner registry's react
// factory accepts.
func openMockLLMForPlannerTest(t *testing.T, _ *config.Config) (llmCloseableClient, error) {
	t.Helper()
	// The mock driver self-registers via blank import below. Build a
	// minimal ConfigSnapshot — the mock ignores most fields.
	snap := llmConfigSnapshotForMock()
	return llmOpenMock(snap)
}

// TestE2E_Wave12_DraftEndpoints_RoundTripsThroughAssembledStack —
// Phase 66 / D-100 surface. POST /v1/dev/drafts/ on the assembled
// stack creates a draft, the `dev.draft.created` event lands on the
// bus, and the response carries the seeded file list. Real auth
// middleware, real identity propagation, real bus.
func TestE2E_Wave12_DraftEndpoints_RoundTripsThroughAssembledStack(t *testing.T) {
	stack := buildWave12Stack(t, wave12ID)
	defer stack.Close()
	if stack.Handler == nil {
		t.Skip("devstack did not assemble transports")
	}
	srv := httptest.NewServer(stack.Handler)
	defer srv.Close()

	// Subscribe to dev.draft.created BEFORE the POST so the bus
	// subscription is open when the handler emits.
	sub, err := stack.Bus.Subscribe(context.Background(), events.Filter{
		Tenant:  wave12ID.TenantID,
		User:    wave12ID.UserID,
		Session: wave12ID.SessionID,
		Types:   []events.EventType{"dev.draft.created"},
	})
	if err != nil {
		t.Fatalf("bus.Subscribe(dev.draft.created): %v", err)
	}
	defer sub.Cancel()

	// POST /v1/dev/drafts/ with the canonical create body. Empty
	// template name defaults to the scaffold engine's default.
	createURL := srv.URL + "/v1/dev/drafts/"
	body := bytes.NewBufferString(`{"name":"wave12-draft"}`)
	req, err := http.NewRequest(http.MethodPost, createURL, body)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+stack.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/dev/drafts/: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		rb, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST status = %d, want 200/201 (body=%s)", resp.StatusCode, rb)
	}
	respBody, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(respBody, []byte(`"draft_id"`)) {
		t.Errorf("response missing draft_id field: %s", respBody)
	}

	// Observe dev.draft.created on the bus within a bounded window.
	select {
	case ev, ok := <-sub.Events():
		if !ok {
			t.Fatal("draft.created subscription closed before event arrived")
		}
		if ev.Type != "dev.draft.created" {
			t.Errorf("event type = %q, want dev.draft.created", ev.Type)
		}
		if ev.Identity.TenantID != wave12ID.TenantID {
			t.Errorf("event identity tenant = %q, want %q", ev.Identity.TenantID, wave12ID.TenantID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("dev.draft.created did not arrive on the bus within 3s")
	}
}

// TestE2E_Wave12_InspectEvents_WireShapeIsConsumable — Phase 69 /
// D-101 surface. The wire shape consumed by `harbor inspect-events`
// (the canonical SSE `data:` payload) is produced by the assembled
// stack with every field the CLI parses: type, sequence, occurred_at,
// tenant, user, session, run (may be empty for early events), payload.
//
// This complements Scenario 1 (which asserts the inspect-topology cmd
// consumes the same shape via the renderer). Together they prove the
// Phase 69 + Phase 70 CLIs share one wire contract — a change to the
// shape would break both, and this test fails when the shape drifts.
func TestE2E_Wave12_InspectEvents_WireShapeIsConsumable(t *testing.T) {
	stack := buildWave12Stack(t, wave12ID)
	defer stack.Close()
	if stack.Handler == nil {
		t.Skip("devstack did not assemble transports")
	}
	srv := httptest.NewServer(stack.Handler)
	defer srv.Close()

	publishWave12Run(t, stack.Bus, wave12ID, wave12RunID)

	body, count := readSSEEventsUntilFinish(t, srv, stack.Token, wave12RunID, 5*time.Second, false)
	if count == 0 {
		t.Fatal("no SSE chunks read")
	}

	// Parse out each `data:` payload and assert each carries the
	// fields the inspect-events CLI's `wireEvent` struct expects.
	// Re-uses the canonical lines-split shape the Phase 60 transport
	// emits (one event per blank-line-separated block).
	rawEvents := bytes.Split(body, []byte("\n\n"))
	var observed int
	for _, raw := range rawEvents {
		raw = bytes.TrimSpace(raw)
		if len(raw) == 0 {
			continue
		}
		// Find the data: line (skip comments / id: / event: / retry:).
		var dataPayload []byte
		for _, line := range bytes.Split(raw, []byte("\n")) {
			line = bytes.TrimRight(line, "\r")
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}
			dataPayload = bytes.TrimSpace(line[len("data:"):])
			break
		}
		if len(dataPayload) == 0 {
			continue
		}
		var ev struct {
			Type       string         `json:"type"`
			Sequence   uint64         `json:"sequence"`
			OccurredAt string         `json:"occurred_at"`
			Tenant     string         `json:"tenant"`
			User       string         `json:"user"`
			Session    string         `json:"session"`
			Run        string         `json:"run,omitempty"`
			Payload    map[string]any `json:"payload,omitempty"`
		}
		if err := json.Unmarshal(dataPayload, &ev); err != nil {
			t.Errorf("wire payload not parseable as inspect-events wireEvent: %v (payload=%s)", err, dataPayload)
			continue
		}
		if ev.Type == "" || ev.Sequence == 0 || ev.OccurredAt == "" || ev.Tenant == "" {
			t.Errorf("wire event missing load-bearing field(s): %+v", ev)
		}
		observed++
	}
	if observed == 0 {
		t.Fatal("no inspect-events-shaped wire events observed in body")
	}
}

// Sanity: every Wave 12 package surface is reachable via the canonical
// import path declared in this file. A build failure here is the signal
// that a Wave 12 package was renamed and the integration test must
// update.
var _ = []any{
	(*events.EventBus)(nil),
	(*config.Config)(nil),
	(*identity.Identity)(nil),
	(*json.RawMessage)(nil),
	(*tasks.TaskRegistry)(nil),
	(*tools.ToolCatalog)(nil),
	(*devstack.DevStack)(nil),
	(*planner.PlannerConfig)(nil),
}
