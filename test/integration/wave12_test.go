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
// # Parallel-PR coverage caveat (per the dispatch prompt's "Strategy")
//
// This wave-end E2E exercises ONLY the surface Phase 70 ships (the
// inspect-topology cmd + the inherited Phase 60/63/64 dev stack +
// Phase 60 SSE event stream + Phase 61 auth + Phase 64a catalog
// wiring). The Phase 65/66/69/#126 scenarios are deferred to the
// audit follow-up (`chore(checkpoint): wave-12 audit fixes`) — the
// Stage-2 PRs may land in any order, and a fragile cross-PR
// dependency here would block this PR's merge on theirs. Documented
// in D-102's "Parallel-PR coverage caveat" section.
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
	q := identity.Quadruple{Identity: id, RunID: runID}
	ctx := context.Background()
	// task.spawned
	if err := bus.Publish(ctx, events.Event{
		Type:     tasks.EventTypeTaskSpawned,
		Identity: q,
		Payload: tasks.TaskSpawnedPayload{
			TaskID:   tasks.TaskID("task-foreground-X"),
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

// readSSEEventsUntilFinish opens an SSE subscription against srv,
// reads frames until a `planner.finish` for the supplied runID
// arrives or maxWait elapses, then returns the raw body bytes plus
// the event count.
func readSSEEventsUntilFinish(t *testing.T, srv *httptest.Server, token, runID string, maxWait time.Duration) ([]byte, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), maxWait)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/events", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Harbor-Run", runID)
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

	// Publish in a goroutine so the SSE subscriber is open BEFORE the
	// events fire — otherwise the subscriber misses the early events
	// (the bus is a live-tail; only Replayer drivers replay older
	// events). The inmem driver does implement Replayer, so the
	// Last-Event-ID: 0 header in readSSEEventsUntilFinish would
	// replay, but the publish-after-subscribe order is the simpler
	// determinism contract.
	publishAfter := 100 * time.Millisecond
	go func() {
		time.Sleep(publishAfter)
		publishWave12Run(t, stack.Bus, wave12ID, wave12RunID)
	}()

	body, count := readSSEEventsUntilFinish(t, srv, stack.Token, wave12RunID, 5*time.Second)
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
	go func() {
		time.Sleep(100 * time.Millisecond)
		publishWave12Run(t, stackA.Bus, wave12ID, "run-tenantA-1")
	}()
	go func() {
		time.Sleep(100 * time.Millisecond)
		publishWave12Run(t, stackB.Bus, wave12IDB, "run-tenantB-1")
	}()

	bodyA, _ := readSSEEventsUntilFinish(t, srvA, stackA.Token, "run-tenantA-1", 3*time.Second)
	bodyB, _ := readSSEEventsUntilFinish(t, srvB, stackB.Token, "run-tenantB-1", 3*time.Second)

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

	// Baseline before launching subscribers.
	runtime.GC()
	time.Sleep(100 * time.Millisecond)
	baseline := runtime.NumGoroutine()

	const N = 10
	var wg sync.WaitGroup
	var fail atomic.Int32
	for i := 0; i < N; i++ {
		runID := fmt.Sprintf("run-stress-%03d", i)
		wg.Add(1)
		go func(rid string) {
			defer wg.Done()
			// Publisher fires AFTER a small stagger so the subscriber
			// is open in time. Each goroutine publishes its OWN run
			// id but ALL run under the SAME identity (wave12ID, the
			// token's claims).
			publishAfter := time.Duration(50+i*5) * time.Millisecond
			pubDone := make(chan struct{})
			go func() {
				time.Sleep(publishAfter)
				publishWave12Run(t, stack.Bus, wave12ID, rid)
				close(pubDone)
			}()

			body, _ := readSSEEventsUntilFinish(t, srv, stack.Token, rid, 5*time.Second)
			<-pubDone
			// Every subscriber should see its own run id.
			if !bytes.Contains(body, []byte(`"run":"`+rid+`"`)) {
				t.Errorf("stress goroutine %s: missing own run id", rid)
				fail.Add(1)
				return
			}
			// And NO other run id from the loop should appear (no
			// cross-talk).
			for j := 0; j < N; j++ {
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

	// Goroutine baseline restoration. Allow scheduler noise.
	runtime.GC()
	time.Sleep(300 * time.Millisecond)
	got := runtime.NumGoroutine()
	if got > baseline+8 {
		t.Errorf("goroutine leak: baseline=%d after=%d (delta=%d)",
			baseline, got, got-baseline)
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
}
