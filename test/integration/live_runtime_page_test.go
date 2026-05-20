// Phase 73b cross-subsystem integration test per CLAUDE.md §17 — the
// Console Live Runtime page's Protocol surface exercised end-to-end
// against real drivers, with no mocks at any seam.
//
// The Live Runtime page is a pure composition over already-shipped
// Protocol surfaces plus the two `[wave-13-extends]` additions Phase
// 73b lands:
//
//   - the `tasks.list` status-counter-strip aggregate (Phase 73b /
//     D-126) — the header five-chip strip;
//   - the run-scoped `events.subscribe` filter (the structured
//     counterpart to D-082's `X-Harbor-Run` carrier) — the bottom-dock
//     Trace tab;
//   - the `topology.snapshot` projection (Phase 74 / D-114) — the
//     topology canvas.
//
// Surfaces composed (real drivers everywhere — CLAUDE.md §17.3 #1):
//
//   - Phase 20 internal/tasks — the real in-process TaskRegistry.
//   - Phase 73d internal/tasks/protocol — the Tasks Protocol Service.
//   - Phase 74 internal/runtime/engine — a real engine wired as the
//     ControlSurface's TopologyAccessor.
//   - Phase 05 internal/events — the real in-memory event bus.
//   - Phase 60 internal/protocol/transports — the REST + SSE wire mux.
//   - Phase 61 internal/protocol/auth — the JWT validator + middleware.
//
// This is the §13 primitive-with-consumer discharge for Phase 73b's
// Go-side surface: the first end-to-end consumer of the status-counter-
// strip aggregate + the run-scoped SSE filter + the topology snapshot,
// with identity propagation across every surface, a missing-identity
// failure mode, and an N≥10 concurrent-SSE-subscriber stress run.
//
// Per §17.4 the test uses channel/scanner-bound `eventually`-style
// waits with bounded real-time deadlines — never time.Sleep as a sync
// primitive.
package integration_test

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/hurtener/Harbor/internal/audit"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/transports"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/engine"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tasks"
	_ "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess"
	tasksprotocol "github.com/hurtener/Harbor/internal/tasks/protocol"
)

const phase73bKid = "phase73b-kid"

var fixedNowPhase73b = time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)

type phase73bDeps struct {
	mux     *http.ServeMux
	priv    *ecdsa.PrivateKey
	reg     tasks.TaskRegistry
	bus     events.EventBus
	cleanup func()
}

// phase73bEngineAccessor adapts a real engine.Engine into a
// protocol.TopologyAccessor — the structural seam the cmd/harbor wiring
// builds for an engine-bearing Runtime.
type phase73bEngineAccessor struct {
	eng    engine.Engine
	tenant string
}

func (a *phase73bEngineAccessor) Topology(ctx context.Context) (prototypes.TopologyProjection, error) {
	return a.eng.Topology(ctx)
}

func (a *phase73bEngineAccessor) TenantID() string { return a.tenant }

var _ protocol.TopologyAccessor = (*phase73bEngineAccessor)(nil)

// newPhase73bDeps assembles the dev stack with a real engine wired as
// the ControlSurface's TopologyAccessor and the Tasks Protocol Service
// wired into the wire mux — the full surface the Live Runtime page
// consumes.
func newPhase73bDeps(t *testing.T) *phase73bDeps {
	t.Helper()

	priv, pub := loadES256Phase61(t)
	red := auditpatterns.New()

	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 64,
		SubscriberBufferSize:     512,
		IdleTimeout:              60 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         512,
	}, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	store, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		_ = bus.Close(context.Background())
		t.Fatalf("state.Open: %v", err)
	}
	taskReg, err := tasks.Open(context.Background(), tasks.Dependencies{
		Store:    store,
		Bus:      bus,
		Redactor: audit.Redactor(red),
		Cfg:      config.TasksConfig{Driver: "inprocess"},
	})
	if err != nil {
		_ = store.Close(context.Background())
		_ = bus.Close(context.Background())
		t.Fatalf("tasks.Open: %v", err)
	}

	// A real engine wired to the same bus — the topology canvas's
	// `topology.snapshot` source.
	in := engine.Node{Name: "ingress", Func: engineNodeFunc}
	mid := engine.Node{Name: "router", Func: engineNodeFunc}
	out := engine.Node{Name: "egress", Func: engineNodeFunc}
	eng, err := engine.New([]engine.Adjacency{
		{From: in, To: []engine.Node{mid}},
		{From: mid, To: []engine.Node{out}},
	}, engine.WithEventBus(bus))
	if err != nil {
		_ = taskReg.Close(context.Background())
		_ = store.Close(context.Background())
		_ = bus.Close(context.Background())
		t.Fatalf("engine.New: %v", err)
	}

	projector, err := tasksprotocol.NewRegistryProjector(taskReg)
	if err != nil {
		t.Fatalf("NewRegistryProjector: %v", err)
	}
	tasksSvc, err := tasksprotocol.NewService(projector,
		tasksprotocol.WithBus(bus),
		tasksprotocol.WithRedactor(red),
	)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	keys := newES256KeySet(phase73bKid, pub)
	now := func() time.Time { return fixedNowPhase73b }
	v, err := auth.NewValidator(keys, auth.WithClock(now), auth.WithRedactor(red))
	if err != nil {
		t.Fatalf("auth.NewValidator: %v", err)
	}

	surface, err := protocol.NewControlSurface(taskReg, steering.NewRegistry(),
		protocol.WithTopologyAccessor(&phase73bEngineAccessor{eng: eng, tenant: "tenant-A"}),
		protocol.WithEventBus(bus),
	)
	if err != nil {
		_ = taskReg.Close(context.Background())
		_ = store.Close(context.Background())
		_ = bus.Close(context.Background())
		t.Fatalf("protocol.NewControlSurface: %v", err)
	}

	mux, err := transports.NewMux(surface, bus,
		transports.WithKeepalive(50*time.Millisecond),
		transports.WithValidator(v),
		transports.WithRedactor(red),
		transports.WithTasksService(tasksSvc),
	)
	if err != nil {
		t.Fatalf("transports.NewMux: %v", err)
	}

	return &phase73bDeps{
		mux:  mux,
		priv: priv,
		reg:  taskReg,
		bus:  bus,
		cleanup: func() {
			_ = taskReg.Close(context.Background())
			_ = store.Close(context.Background())
			_ = bus.Close(context.Background())
		},
	}
}

func phase73bClaims(id identity.Identity, scopes []string) jwt.MapClaims {
	return jwt.MapClaims{
		"iss":     "https://idp.test",
		"sub":     id.UserID,
		"exp":     fixedNowPhase73b.Add(15 * time.Minute).Unix(),
		"nbf":     fixedNowPhase73b.Add(-1 * time.Minute).Unix(),
		"tenant":  id.TenantID,
		"user":    id.UserID,
		"session": id.SessionID,
		"scopes":  scopes,
	}
}

// postTasksPhase73b issues a POST /v1/tasks/{verb} with the supplied JWT.
func postTasksPhase73b(t *testing.T, srvURL, verb, body, token string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srvURL+"/v1/tasks/"+verb, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// postTopologyPhase73b issues a POST /v1/control/topology.snapshot.
func postTopologyPhase73b(t *testing.T, srvURL, body, token string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srvURL+"/v1/control/topology.snapshot",
		strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// seedTaskPhase73b spawns one foreground task in id and advances it to
// the requested status.
func seedTaskPhase73b(t *testing.T, reg tasks.TaskRegistry, id identity.Identity, status tasks.TaskStatus) tasks.TaskID {
	t.Helper()
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	h, err := reg.Spawn(ctx, tasks.SpawnRequest{
		Identity:    identity.Quadruple{Identity: id},
		Kind:        tasks.KindForeground,
		Description: "live-runtime integration task",
		Query:       "integration query",
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	switch status {
	case tasks.StatusPending:
		// already Pending
	case tasks.StatusRunning:
		if err := reg.MarkRunning(ctx, h.ID); err != nil {
			t.Fatalf("MarkRunning: %v", err)
		}
	case tasks.StatusFailed:
		if err := reg.MarkRunning(ctx, h.ID); err != nil {
			t.Fatalf("MarkRunning: %v", err)
		}
		if err := reg.MarkFailed(ctx, h.ID, tasks.TaskError{Code: "tool_timeout", Message: "timed out"}); err != nil {
			t.Fatalf("MarkFailed: %v", err)
		}
	default:
		t.Fatalf("seedTaskPhase73b: unsupported status %q", status)
	}
	return h.ID
}

// TestE2E_Phase73b_LiveRuntimePage is the §13 primitive-with-consumer
// binding test for the Live Runtime page's Protocol surface.
func TestE2E_Phase73b_LiveRuntimePage(t *testing.T) {
	deps := newPhase73bDeps(t)
	defer deps.cleanup()

	srv := httptest.NewServer(deps.mux)
	defer srv.Close()

	idA := identity.Identity{TenantID: "tenant-A", UserID: "u-A", SessionID: "s-A"}
	idB := identity.Identity{TenantID: "tenant-B", UserID: "u-B", SessionID: "s-B"}
	tokA := signES256Wave10(t, deps.priv, phase73bClaims(idA, nil), phase73bKid)
	tokB := signES256Wave10(t, deps.priv, phase73bClaims(idB, nil), phase73bKid)

	// Session A: 2 running, 1 pending, 1 failed. Session B: 1 failed.
	seedTaskPhase73b(t, deps.reg, idA, tasks.StatusRunning)
	seedTaskPhase73b(t, deps.reg, idA, tasks.StatusRunning)
	seedTaskPhase73b(t, deps.reg, idA, tasks.StatusPending)
	seedTaskPhase73b(t, deps.reg, idA, tasks.StatusFailed)
	seedTaskPhase73b(t, deps.reg, idB, tasks.StatusFailed)

	// (i) the status-counter-strip aggregate — the header five-chip
	// strip — round-trips and is identity-scoped.
	t.Run("status_counter_strip_aggregate", func(t *testing.T) {
		status, body := postTasksPhase73b(t, srv.URL, "list",
			`{"include_status_counter_strip":true}`, tokA)
		if status != http.StatusOK {
			t.Fatalf("tasks.list: status = %d, want 200; body=%s", status, body)
		}
		var resp prototypes.TaskListResponse
		if err := json.Unmarshal(body, &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.StatusCounterStrip == nil {
			t.Fatal("status_counter_strip is nil — Phase 73b opt-in aggregate missing")
		}
		s := resp.StatusCounterStrip
		if s.Running != 2 || s.Pending != 1 || s.Failed != 1 || s.Completed != 0 || s.Paused != 0 {
			t.Fatalf("session A strip = %+v, want {Pending:1 Running:2 Completed:0 Paused:0 Failed:1}", *s)
		}
	})

	// (ii) the strip is identity-scoped — session B never sees A's counts.
	t.Run("status_counter_strip_identity_scoped", func(t *testing.T) {
		status, body := postTasksPhase73b(t, srv.URL, "list",
			`{"include_status_counter_strip":true}`, tokB)
		if status != http.StatusOK {
			t.Fatalf("tasks.list(B): status = %d, want 200; body=%s", status, body)
		}
		var resp prototypes.TaskListResponse
		if err := json.Unmarshal(body, &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.StatusCounterStrip == nil {
			t.Fatal("session B strip is nil")
		}
		if resp.StatusCounterStrip.Running != 0 {
			t.Errorf("session B strip Running = %d, want 0 — session A's counts leaked",
				resp.StatusCounterStrip.Running)
		}
		if resp.StatusCounterStrip.Failed != 1 {
			t.Errorf("session B strip Failed = %d, want 1", resp.StatusCounterStrip.Failed)
		}
	})

	// (iii) the topology snapshot — the topology canvas's source —
	// returns the engine's node + edge set.
	t.Run("topology_snapshot", func(t *testing.T) {
		status, body := postTopologyPhase73b(t, srv.URL, `{}`, tokA)
		if status != http.StatusOK {
			t.Fatalf("topology.snapshot: status = %d, want 200; body=%s", status, body)
		}
		var proj prototypes.TopologyProjection
		if err := json.Unmarshal(body, &proj); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(proj.Nodes) != 3 {
			t.Fatalf("topology nodes = %d, want 3 (ingress/router/egress)", len(proj.Nodes))
		}
		if len(proj.Edges) != 2 {
			t.Fatalf("topology edges = %d, want 2", len(proj.Edges))
		}
	})

	// (iv) failure mode — a tasks.list without the identity triple is
	// rejected at the Phase 61 edge with identity_required (401).
	t.Run("missing_identity_rejected", func(t *testing.T) {
		status, body := postTasksPhase73b(t, srv.URL, "list",
			`{"include_status_counter_strip":true}`, "")
		if status != http.StatusUnauthorized {
			t.Fatalf("tasks.list without identity: status = %d, want 401; body=%s", status, body)
		}
	})

	// (v) the run-scoped SSE filter — the Trace tab — narrows the event
	// stream to one run id (D-082's X-Harbor-Run carrier). A subscriber
	// scoped to run r1 sees r1's events and never r2's.
	t.Run("trace_tab_run_scoped_filter", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/events", nil)
		req.Header.Set("Authorization", "Bearer "+tokA)
		req.Header.Set("X-Harbor-Run", "run-trace-1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("open SSE: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("SSE status = %d, want 200", resp.StatusCode)
		}

		// Publish one event for run-trace-1 and one for run-trace-2.
		publishRunEventPhase73b(t, deps.bus, idA, "run-trace-1")
		publishRunEventPhase73b(t, deps.bus, idA, "run-trace-2")

		sc := bufio.NewScanner(resp.Body)
		deadline := time.Now().Add(1500 * time.Millisecond)
		sawRun1 := false
		for sc.Scan() && time.Now().Before(deadline) {
			line := sc.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := strings.TrimPrefix(line, "data: ")
			if strings.Contains(data, "run-trace-2") {
				t.Fatal("run-scoped Trace-tab subscriber leaked a foreign run's event")
			}
			if strings.Contains(data, "run-trace-1") {
				sawRun1 = true
			}
		}
		if !sawRun1 {
			t.Error("run-scoped Trace-tab subscriber did not receive its own run's event")
		}
	})

	// (vi) concurrency stress — N≥10 concurrent SSE subscribers + a
	// tasks.list status-strip read, asserting no cross-talk.
	t.Run("concurrency_stress", func(t *testing.T) {
		const n = 14
		var wg sync.WaitGroup
		errCh := make(chan string, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(useA bool) {
				defer wg.Done()
				tok := tokA
				wantRunning := 2
				if !useA {
					tok = tokB
					wantRunning = 0
				}
				status, body := postTasksPhase73b(t, srv.URL, "list",
					`{"include_status_counter_strip":true}`, tok)
				if status != http.StatusOK {
					errCh <- "list status=" + http.StatusText(status)
					return
				}
				var resp prototypes.TaskListResponse
				if err := json.Unmarshal(body, &resp); err != nil {
					errCh <- "list decode: " + err.Error()
					return
				}
				if resp.StatusCounterStrip == nil || resp.StatusCounterStrip.Running != wantRunning {
					errCh <- "strip cross-talk under concurrency"
					return
				}
				// Open + immediately close an SSE subscription so the
				// stress also exercises the subscriber lifecycle.
				ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
				defer cancel()
				req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/events", nil)
				req.Header.Set("Authorization", "Bearer "+tok)
				r, err := http.DefaultClient.Do(req)
				if err == nil {
					_ = r.Body.Close()
				}
			}(i%2 == 0)
		}
		wg.Wait()
		close(errCh)
		for msg := range errCh {
			t.Error(msg)
		}
	})
}

// publishRunEventPhase73b publishes one run-scoped event onto the bus
// so the run-scoped SSE filter test has traffic to narrow. The runID is
// the X-Harbor-Run carrier the Trace tab subscribes against (D-082);
// it also appears in the payload so the SSE-line scanner can correlate.
func publishRunEventPhase73b(t *testing.T, bus events.EventBus, id identity.Identity, runID string) {
	t.Helper()
	ev := events.Event{
		Type:     events.EventTypeRuntimeRunCancelled,
		Identity: identity.Quadruple{Identity: id, RunID: runID},
		Payload:  events.RunCancelledPayload{RunID: runID, CancelledAt: fixedNowPhase73b.UnixNano()},
	}
	if err := bus.Publish(context.Background(), ev); err != nil {
		t.Fatalf("bus.Publish(%s): %v", runID, err)
	}
}
