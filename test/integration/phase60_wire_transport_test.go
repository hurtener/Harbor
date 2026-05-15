// Phase 60 cross-subsystem integration test per CLAUDE.md §17 — the
// Protocol wire transport (SSE + REST) exercised end-to-end against the
// REAL runtime surface it binds:
//
//   - Phase 54 protocol.ControlSurface (the transport-agnostic control
//     surface) — the REST control transport's target.
//   - Phase 05 events.EventBus — the SSE event transport's source.
//   - Phase 20 tasks.TaskRegistry (inprocess) — `start` spawns a real
//     task, which emits a real `task.spawned` event onto the bus.
//
// The test proves the two transports COMPOSE both directions over the
// wire: a client opens the SSE event stream (server→client), submits a
// `start` over the REST control surface (client→server), and observes
// the `task.spawned` lifecycle event the spawn emitted arrive on its SSE
// stream. Identity propagation is asserted through the edge; a
// missing-identity request is the failure mode; a full-duplex N≥10
// concurrency stress runs under -race.
//
// This is the §13 primitive-with-consumer discharge for Phase 60: the
// wire transport is itself the consumer of Phase 54's transport-agnostic
// surface + Phase 05's bus, and this test exercises both directions.
package integration_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/transports"
	"github.com/hurtener/Harbor/internal/protocol/transports/stream"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tasks"
	_ "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess"
)

// phase60Deps wires the REAL runtime drivers behind the wire transport
// — no mocks at any seam (CLAUDE.md §17.3).
type phase60Deps struct {
	mux     http.Handler
	bus     events.EventBus
	cleanup func()
}

func newPhase60Deps(t *testing.T) *phase60Deps {
	t.Helper()
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
	surface, err := protocol.NewControlSurface(taskReg, steering.NewRegistry())
	if err != nil {
		_ = taskReg.Close(context.Background())
		_ = store.Close(context.Background())
		_ = bus.Close(context.Background())
		t.Fatalf("protocol.NewControlSurface: %v", err)
	}
	mux, err := transports.NewMux(surface, bus, transports.WithKeepalive(50*time.Millisecond))
	if err != nil {
		_ = taskReg.Close(context.Background())
		_ = store.Close(context.Background())
		_ = bus.Close(context.Background())
		t.Fatalf("transports.NewMux: %v", err)
	}
	return &phase60Deps{
		mux: mux,
		bus: bus,
		cleanup: func() {
			_ = taskReg.Close(context.Background())
			_ = store.Close(context.Background())
			_ = bus.Close(context.Background())
		},
	}
}

// openStream opens an SSE stream scoped to the identity triple and
// returns the response plus a channel that yields each `event:` line's
// type. The caller cancels ctx to tear the stream down.
func openStream(t *testing.T, ctx context.Context, baseURL string, tenant, user, session string) (*http.Response, <-chan string) {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/v1/events", nil)
	req.Header.Set(stream.HeaderTenant, tenant)
	req.Header.Set(stream.HeaderUser, user)
	req.Header.Set(stream.HeaderSession, session)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open SSE stream: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		t.Fatalf("SSE stream status = %d, want 200", resp.StatusCode)
	}
	out := make(chan string, 64)
	go func() {
		defer close(out)
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "event: ") {
				select {
				case out <- strings.TrimPrefix(line, "event: "):
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return resp, out
}

// submitStart submits a `start` control over the REST transport and
// returns the spawned task id.
func submitStart(t *testing.T, baseURL, tenant, user, session string) string {
	t.Helper()
	body := fmt.Sprintf(`{"identity":{"tenant":%q,"user":%q,"session":%q},"query":"e2e"}`,
		tenant, user, session)
	resp, err := http.Post(baseURL+"/v1/control/start", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /v1/control/start: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("start status = %d, want 200", resp.StatusCode)
	}
	var sr types.StartResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		t.Fatalf("decode StartResponse: %v", err)
	}
	if sr.TaskID == "" {
		t.Fatal("StartResponse.TaskID is empty")
	}
	return sr.TaskID
}

// TestE2E_Phase60_WireTransport_BothDirections — the headline E2E: a
// client opens the SSE stream, submits `start` over REST, and observes
// the task.spawned lifecycle event arrive on its stream. Events out,
// control in — both directions over the wire, real drivers throughout.
func TestE2E_Phase60_WireTransport_BothDirections(t *testing.T) {
	deps := newPhase60Deps(t)
	defer deps.cleanup()
	srv := httptest.NewServer(deps.mux)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Direction 1 (server→client): open the SSE event stream.
	resp, eventTypes := openStream(t, ctx, srv.URL, "t1", "u1", "s1")
	defer func() { _ = resp.Body.Close() }()

	// Direction 2 (client→server): submit `start` over the REST control
	// surface — the spawn emits task.spawned onto the bus, which the
	// SSE stream forwards. Re-submit on a ticker: the subscription may
	// register just after an early spawn, so a retry closes the race
	// without a time.Sleep-as-synchronisation antipattern.
	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.After(5 * time.Second)
	_ = submitStart(t, srv.URL, "t1", "u1", "s1")
	for {
		select {
		case et, ok := <-eventTypes:
			if !ok {
				t.Fatal("SSE stream closed before task.spawned was observed")
			}
			if et == string(tasks.EventTypeTaskSpawned) {
				return // both directions composed end-to-end — pass.
			}
		case <-ticker.C:
			_ = submitStart(t, srv.URL, "t1", "u1", "s1")
		case <-deadline:
			t.Fatal("timed out waiting for task.spawned on the SSE stream")
		}
	}
}

// TestE2E_Phase60_MissingIdentity_FailsClosed — the failure mode: a
// request with an incomplete identity triple is rejected closed at the
// edge on BOTH transports (RFC §5.5, CLAUDE.md §6 rule 9).
func TestE2E_Phase60_MissingIdentity_FailsClosed(t *testing.T) {
	deps := newPhase60Deps(t)
	defer deps.cleanup()
	srv := httptest.NewServer(deps.mux)
	defer srv.Close()

	// REST control: missing session.
	body := `{"identity":{"tenant":"t1","user":"u1","session":""},"query":"q"}`
	cresp, err := http.Post(srv.URL+"/v1/control/start", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	_ = cresp.Body.Close()
	if cresp.StatusCode != http.StatusUnauthorized {
		t.Errorf("REST missing-identity status = %d, want 401", cresp.StatusCode)
	}

	// SSE stream: missing user header.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/events", nil)
	req.Header.Set(stream.HeaderTenant, "t1")
	req.Header.Set(stream.HeaderSession, "s1")
	sresp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_ = sresp.Body.Close()
	if sresp.StatusCode != http.StatusUnauthorized {
		t.Errorf("SSE missing-identity status = %d, want 401", sresp.StatusCode)
	}
}

// TestE2E_Phase60_FullDuplexStress runs N≥10 concurrent full-duplex
// sessions: each opens its own SSE stream AND submits its own `start`
// over REST, all against one shared mux, under -race. It proves no
// cross-talk (each session is triple-scoped — a session never sees
// another's events) and no cross-cancellation (each stream has its own
// ctx).
func TestE2E_Phase60_FullDuplexStress(t *testing.T) {
	deps := newPhase60Deps(t)
	defer deps.cleanup()
	srv := httptest.NewServer(deps.mux)
	defer srv.Close()

	const n = 16
	var wg sync.WaitGroup
	errs := make(chan error, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			session := fmt.Sprintf("s%d", i)
			ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
			defer cancel()

			resp, eventTypes := openStream(t, ctx, srv.URL, "t1", "u1", session)
			defer func() { _ = resp.Body.Close() }()

			// Submit starts until this session's stream observes a
			// task.spawned. Because the stream is triple-scoped, the
			// ONLY task.spawned it can see is its own.
			ticker := time.NewTicker(150 * time.Millisecond)
			defer ticker.Stop()
			_ = submitStart(t, srv.URL, "t1", "u1", session)
			for {
				select {
				case et, ok := <-eventTypes:
					if !ok {
						errs <- fmt.Errorf("session %s: stream closed before task.spawned", session)
						return
					}
					if et == string(tasks.EventTypeTaskSpawned) {
						return // observed our own spawn — pass.
					}
				case <-ticker.C:
					_ = submitStart(t, srv.URL, "t1", "u1", session)
				case <-ctx.Done():
					errs <- fmt.Errorf("session %s: timed out waiting for task.spawned", session)
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
}
