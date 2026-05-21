// Wave 13 cross-subsystem integration test per CLAUDE.md §17.5 + §17.7
// step 5 — the wave-end E2E, bundled with the final phase (Phase 75a).
//
// Wave 13 is the Console subsystem: the foundation phases (72/72a-h, 74)
// plus the 14 per-page Protocol + UI bundles (73a-73n). Each per-page
// Protocol surface ships its OWN §17.1 integration test
// (`sessions_page_test.go`, `tasks_page_test.go`, `agents_page_test.go`,
// `events_*_test.go`, …). This wave-end aggregator does NOT re-test each
// page method — it exercises the CONSOLIDATED Wave 13 observability seam
// the whole Console depends on: the canonical event bus + the Phase 60
// SSE wire transport, end-to-end, with identity propagation.
//
// # Failure mode covered (CLAUDE.md §17.3 #3)
//
// The named failure mode is a MISSING-IDENTITY rejection (D-033): an SSE
// subscription with no Bearer token is rejected at the auth-middleware
// edge with HTTP 401 — the wire never reaches the bus. This is the
// cross-page identity gate: the Console is a Protocol client and every
// `/v1/*` call it makes carries a verified `(tenant, user, session)`
// triple; a tokenless call is refused before any data is read.
//
// # Per CLAUDE.md §17.3
//
//  1. Real drivers everywhere on the seam — `devstack.Assemble` (D-094)
//     opens the patterns audit redactor + the inmem events / state /
//     artifacts / tasks drivers + the real auth.Validator + the real
//     transports.Mux. No mocks at the boundary.
//  2. Identity propagation — every event published carries its
//     `(tenant, user, session)` triple; the SSE subscription's
//     identity-filter is asserted to admit only the caller's own
//     events. A wire-type round-trip decodes the SSE `data:` payload
//     and asserts the decoded identity matches the caller's triple
//     verbatim.
//  3. ≥1 failure mode — the missing-identity (no-token) rejection.
//  4. -race is the CI gate.
//  5. N≥10 concurrency stress — TestE2E_Wave13_ConcurrentSSESubscribers
//     runs N=12 concurrent SSE subscribers against one shared stack,
//     each scoped to its own run id via the server-side `X-Harbor-Run`
//     run-filter; each observes ONLY its own run's events (no
//     cross-talk) and the goroutine count returns to baseline after
//     teardown.
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
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	_ "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess"
	"github.com/hurtener/Harbor/internal/tools"
)

// writeWave13Config writes a minimal dev-shaped harbor.yaml — the same
// shape `writeWave12Config` uses, kept local so the wave-end E2E does
// not couple to a sibling wave's test helper.
func writeWave13Config(t *testing.T) *config.Config {
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
  service_name: harbor-wave13
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

// buildWave13Stack assembles a real stack with the SSE wire transport.
// SkipRunLoop because the wave-end E2E drives synthetic events onto the
// bus directly — the production observability surface the whole Console
// consumes.
func buildWave13Stack(t *testing.T, signer identity.Identity) *devstack.DevStack {
	t.Helper()
	cfg := writeWave13Config(t)
	opts := devstack.AssembleOpts{
		SkipRunLoop: true,
		SkipCatalog: true,
	}
	opts.Identity.Tenant = signer.TenantID
	opts.Identity.User = signer.UserID
	opts.Identity.Session = signer.SessionID
	return devstack.Assemble(t, cfg, opts)
}

// publishWave13Events publishes a small Console-shaped event stream to
// the bus under `id`. The Console's per-page views (Live Runtime,
// Events, Tasks) all consume `events.subscribe`; this is the canonical
// stream a Console SSE subscription observes.
func publishWave13Events(t *testing.T, bus events.EventBus, id identity.Identity, runID string) {
	t.Helper()
	q := identity.Quadruple{Identity: id, RunID: runID}
	ctx := context.Background()
	if err := bus.Publish(ctx, events.Event{
		Type:     tools.EventTypeToolInvoked,
		Identity: q,
		Payload: tools.ToolInvokedPayload{
			Identity:  q,
			ToolName:  "wave13_probe_tool",
			Transport: tools.TransportInProcess,
			StartedAt: time.Now(),
		},
	}); err != nil {
		t.Fatalf("publish tool.invoked: %v", err)
	}
	if err := bus.Publish(ctx, events.Event{
		Type:     tools.EventTypeToolCompleted,
		Identity: q,
		Payload: tools.ToolCompletedPayload{
			Identity:   q,
			ToolName:   "wave13_probe_tool",
			Transport:  tools.TransportInProcess,
			Attempts:   1,
			DurationMS: 7,
		},
	}); err != nil {
		t.Fatalf("publish tool.completed: %v", err)
	}
}

// readWave13SSE opens an SSE subscription against srv, reads frames
// until the `tool.completed` marker for runID arrives or maxWait
// elapses, then returns the raw body bytes.
//
// `serverRunFilter` controls the `X-Harbor-Run` header. The SSE
// subscription's identity-filter scopes by the `(tenant, user,
// session)` triple — NOT by run; for subscribers that SHARE an
// identity but want run-scoped isolation (the concurrency stress
// below), the server-side run-filter (`X-Harbor-Run`) is the
// suppression mechanism. The cross-TENANT isolation test leaves it
// false — there the identity-tuple scope is the isolation boundary.
func readWave13SSE(t *testing.T, srv *httptest.Server, token, runID string, maxWait time.Duration, serverRunFilter bool) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), maxWait)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/events", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if serverRunFilter {
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
	completedMarker := []byte(`"type":"tool.completed"`)
	runMarker := []byte(`"run":"` + runID + `"`)
	var acc bytes.Buffer
	buf := make([]byte, 4096)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			acc.Write(buf[:n])
			if bytes.Contains(acc.Bytes(), completedMarker) && bytes.Contains(acc.Bytes(), runMarker) {
				return acc.Bytes()
			}
		}
		if readErr != nil {
			return acc.Bytes()
		}
	}
}

// TestE2E_Wave13_PerPageProtocolRoundTrip — the Console's observability
// seam round-trips end-to-end. A synthetic Console-shaped event stream
// is published to the bus; a Bearer-token SSE subscription against the
// Phase 60 wire transport observes every event, and a wire-type
// round-trip decodes the SSE `data:` payload and asserts the decoded
// identity matches the caller's triple verbatim.
func TestE2E_Wave13_PerPageProtocolRoundTrip(t *testing.T) {
	id := identity.Identity{TenantID: "tenant-w13", UserID: "user-w13", SessionID: "session-w13"}
	stack := buildWave13Stack(t, id)
	defer stack.Close()
	if stack.Handler == nil {
		t.Skip("devstack did not assemble transports (Phase 60 missing?)")
	}
	srv := httptest.NewServer(stack.Handler)
	defer srv.Close()

	const runID = "run-wave13-roundtrip"
	publishWave13Events(t, stack.Bus, id, runID)

	body := readWave13SSE(t, srv, stack.Token, runID, 5*time.Second, false)
	if len(body) == 0 {
		t.Fatal("no SSE chunks read")
	}

	// Decode each `data:` payload; assert the wire-type carries the
	// caller's identity triple verbatim (the round-trip).
	var decoded int
	for _, raw := range bytes.Split(body, []byte("\n\n")) {
		raw = bytes.TrimSpace(raw)
		if len(raw) == 0 {
			continue
		}
		var dataPayload []byte
		for _, line := range bytes.Split(raw, []byte("\n")) {
			line = bytes.TrimRight(line, "\r")
			if bytes.HasPrefix(line, []byte("data:")) {
				dataPayload = bytes.TrimSpace(line[len("data:"):])
				break
			}
		}
		if len(dataPayload) == 0 {
			continue
		}
		var ev struct {
			Type    string `json:"type"`
			Tenant  string `json:"tenant"`
			User    string `json:"user"`
			Session string `json:"session"`
		}
		if err := json.Unmarshal(dataPayload, &ev); err != nil {
			t.Errorf("SSE wire payload not decodable: %v", err)
			continue
		}
		if ev.Tenant != id.TenantID || ev.User != id.UserID || ev.Session != id.SessionID {
			t.Errorf("wire-type identity = (%s,%s,%s), want (%s,%s,%s) — identity round-trip breach",
				ev.Tenant, ev.User, ev.Session, id.TenantID, id.UserID, id.SessionID)
		}
		decoded++
	}
	if decoded == 0 {
		t.Fatal("no decodable SSE wire events observed")
	}
}

// TestE2E_Wave13_CrossPageIdentityIsolation — two tenants, two stacks;
// each tenant's SSE subscription observes ONLY its own events. A
// Console attached as tenant-A can never observe tenant-B's runs — the
// multi-isolation invariant the whole Console subsystem rests on.
func TestE2E_Wave13_CrossPageIdentityIsolation(t *testing.T) {
	idA := identity.Identity{TenantID: "tenant-w13-A", UserID: "u-A", SessionID: "s-A"}
	idB := identity.Identity{TenantID: "tenant-w13-B", UserID: "u-B", SessionID: "s-B"}
	stackA := buildWave13Stack(t, idA)
	defer stackA.Close()
	stackB := buildWave13Stack(t, idB)
	defer stackB.Close()
	if stackA.Handler == nil || stackB.Handler == nil {
		t.Skip("devstack did not assemble transports")
	}
	srvA := httptest.NewServer(stackA.Handler)
	defer srvA.Close()
	srvB := httptest.NewServer(stackB.Handler)
	defer srvB.Close()

	publishWave13Events(t, stackA.Bus, idA, "run-A")
	publishWave13Events(t, stackB.Bus, idB, "run-B")

	bodyA := readWave13SSE(t, srvA, stackA.Token, "run-A", 3*time.Second, false)
	bodyB := readWave13SSE(t, srvB, stackB.Token, "run-B", 3*time.Second, false)

	if bytes.Contains(bodyA, []byte(`"tenant":"tenant-w13-B"`)) {
		t.Error("tenant-A SSE leaked tenant-B identity — cross-tenant data leak")
	}
	if bytes.Contains(bodyB, []byte(`"tenant":"tenant-w13-A"`)) {
		t.Error("tenant-B SSE leaked tenant-A identity — cross-tenant data leak")
	}
	if !bytes.Contains(bodyA, []byte(`"run":"run-A"`)) {
		t.Error("tenant-A SSE missing its own run")
	}
	if !bytes.Contains(bodyB, []byte(`"run":"run-B"`)) {
		t.Error("tenant-B SSE missing its own run")
	}
}

// TestE2E_Wave13_FailureMode_MissingIdentity — the named §17.3 failure
// mode (D-033): a GET /v1/events with no Bearer token is rejected at
// the auth-middleware edge with HTTP 401. The wire never reaches the
// bus — the Console's identity gate is the security boundary.
func TestE2E_Wave13_FailureMode_MissingIdentity(t *testing.T) {
	id := identity.Identity{TenantID: "tenant-w13", UserID: "user-w13", SessionID: "session-w13"}
	stack := buildWave13Stack(t, id)
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /v1/events: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("tokenless /v1/events: status = %d, want 401 (body=%s)", resp.StatusCode, body)
	}
}

// TestE2E_Wave13_ConcurrentSSESubscribers — N=12 concurrent SSE
// subscribers against ONE shared assembled stack, each scoped to its
// own run id. Asserts: every subscriber observes its own run; no
// subscriber observes a different run (no cross-talk); the goroutine
// count returns to baseline after every subscriber closes (no leak in
// the wire transport layer).
func TestE2E_Wave13_ConcurrentSSESubscribers(t *testing.T) {
	id := identity.Identity{TenantID: "tenant-w13", UserID: "user-w13", SessionID: "session-w13"}
	stack := buildWave13Stack(t, id)
	defer stack.Close()
	if stack.Handler == nil {
		t.Skip("devstack did not assemble transports")
	}
	srv := httptest.NewServer(stack.Handler)
	defer srv.Close()

	// Baseline before launching subscribers — settle via scheduler
	// yield, never time.Sleep (§17.4).
	runtime.GC()
	for i := 0; i < 100; i++ {
		runtime.Gosched()
	}
	baseline := runtime.NumGoroutine()

	const N = 12
	var wg sync.WaitGroup
	var fail atomic.Int32
	for i := 0; i < N; i++ {
		runID := fmt.Sprintf("run-w13-stress-%03d", i)
		wg.Add(1)
		go func(idx int, rid string) {
			defer wg.Done()
			// Publish synchronously — Last-Event-ID: 0 makes the SSE
			// subscriber replay from cursor 0, so the publish-after-
			// subscribe race cannot matter.
			publishWave13Events(t, stack.Bus, id, rid)
			body := readWave13SSE(t, srv, stack.Token, rid, 5*time.Second, true)
			if !bytes.Contains(body, []byte(`"run":"`+rid+`"`)) {
				t.Errorf("stress %s: missing own run id", rid)
				fail.Add(1)
				return
			}
			for j := 0; j < N; j++ {
				if j == idx {
					continue
				}
				other := fmt.Sprintf("run-w13-stress-%03d", j)
				if bytes.Contains(body, []byte(`"run":"`+other+`"`)) {
					t.Errorf("stress %s: leaked event from %s — cross-talk", rid, other)
					fail.Add(1)
				}
			}
		}(i, runID)
	}
	wg.Wait()
	if fail.Load() != 0 {
		t.Fatalf("stress had %d failures", fail.Load())
	}

	// Goroutine baseline restoration — bounded eventually-poll (§17.4).
	var got int
	if !waitForWave13Baseline(2*time.Second, baseline+8, &got) {
		t.Errorf("goroutine leak: baseline=%d after=%d (delta=%d)", baseline, got, got-baseline)
	}
}

// waitForWave13Baseline polls runtime.NumGoroutine until the count is
// at or below `target` or `maxWait` elapses. Mirrors wave12's
// `waitForGoroutineBaseline` shape; kept local so the wave-end E2E does
// not couple to a sibling wave's helper.
func waitForWave13Baseline(maxWait time.Duration, target int, finalCount *int) bool {
	deadline := time.Now().Add(maxWait)
	for time.Now().Before(deadline) {
		runtime.GC()
		runtime.Gosched()
		if got := runtime.NumGoroutine(); got <= target {
			*finalCount = got
			return true
		}
		for i := 0; i < 10; i++ {
			runtime.Gosched()
		}
	}
	*finalCount = runtime.NumGoroutine()
	return false
}
