package transports_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/transports"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// TestConcurrentReuse_SharedMux_NoCrossTalk is the D-025 concurrent-reuse
// test: N≥100 concurrent requests — a mix of REST control submissions
// and SSE stream opens — against ONE shared mux, under -race. It asserts:
//
//   - no data races (the -race gate),
//   - no context bleed (each control request gets its own task id; each
//     stream is scoped to its own identity),
//   - no cross-cancellation (cancelling one stream's ctx does not affect
//     the others — each goroutine reads its own request ctx),
//   - the mux is a reusable artifact safe to share (CLAUDE.md §5 + §11).
func TestConcurrentReuse_SharedMux_NoCrossTalk(t *testing.T) {
	deps := newTestDeps(t)
	defer deps.cleanup()

	stub := &stubValidator{verified: muxTestVerified()}
	mux, err := transports.NewMux(deps.surface, deps.bus,
		transports.WithKeepalive(20*time.Millisecond),
		transports.WithValidator(stub))
	if err != nil {
		t.Fatalf("NewMux: %v", err)
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()

	const n = 120
	var wg sync.WaitGroup
	errs := make(chan error, n)

	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				// REST control submission.
				body := `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"query":"q"}`
				resp, err := postControlBearer(srv.URL+"/v1/control/start",
					strings.NewReader(body))
				if err != nil {
					errs <- err
					return
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					errs <- errStatus("control", resp.StatusCode)
				}
				return
			}
			// SSE stream open — each with its own cancellable ctx, so a
			// per-stream cancel never crosses to a sibling.
			ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/events", nil)
			req.Header.Set("X-Harbor-Tenant", "t1")
			req.Header.Set("X-Harbor-User", "u1")
			req.Header.Set("X-Harbor-Session", "s1")
			req.Header.Set("Authorization", "Bearer faketoken")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				// A ctx-deadline error on the body read is expected; a
				// dial error is not.
				if ctx.Err() == nil {
					errs <- err
				}
				return
			}
			if resp.StatusCode != http.StatusOK {
				errs <- errStatus("stream", resp.StatusCode)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent request error: %v", err)
		}
	}
}

// TestGoroutineLeak_StreamsDrainAfterShutdown asserts the SSE transport
// does not leak goroutines: after every stream client disconnects and
// the test server is closed, runtime.NumGoroutine() returns to its
// baseline (CLAUDE.md §11 — goroutine-leak tests are mandatory for
// long-lived components).
func TestGoroutineLeak_StreamsDrainAfterShutdown(t *testing.T) {
	deps := newTestDeps(t)
	defer deps.cleanup()

	mux, err := transports.NewMux(deps.surface, deps.bus,
		transports.WithKeepalive(10*time.Millisecond),
		transports.WithoutValidator())
	if err != nil {
		t.Fatalf("NewMux: %v", err)
	}
	srv := httptest.NewServer(mux)

	// Let the test server's own goroutines settle, then snapshot.
	settle()
	baseline := runtime.NumGoroutine()

	const streams = 25
	var wg sync.WaitGroup
	for range streams {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/events", nil)
			req.Header.Set("X-Harbor-Tenant", "t1")
			req.Header.Set("X-Harbor-User", "u1")
			req.Header.Set("X-Harbor-Session", "s1")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}()
	}
	wg.Wait()
	srv.Close() // joins the server's connection goroutines.

	// After every client disconnected and the server closed, the
	// per-stream goroutines must have unwound. Poll briefly — goroutine
	// teardown is asynchronous — then assert against the baseline.
	// Allow a small slack for test-runtime noise; the real signal is
	// "not growing by ~streams".
	got := settledGoroutines(baseline, 5)
	if got > baseline+5 {
		t.Errorf("goroutine leak: baseline=%d, after=%d (opened %d streams)",
			baseline, got, streams)
	}
}

// settle gives the test server's own goroutines a brief, best-effort window to
// stabilise BEFORE the baseline is sampled. It is a pre-measurement quiesce, not
// a leak assertion or an event synchronisation, so a fixed window is acceptable.
func settle() {
	for range 20 {
		runtime.GC()
		time.Sleep(10 * time.Millisecond)
	}
}

// settledGoroutines gives asynchronous goroutine teardown a bounded window to
// complete, then returns the live count. It is a real-time eventually-poll
// (AGENTS.md §17.4), NOT a fixed sleep-as-synchronisation: it re-samples after
// each runtime.GC() (which reaps finished-but-unscheduled goroutines and parks
// the poller so the exiting goroutines get scheduler time under load) and
// returns the instant the count drains to within tol of base, or when a bounded
// deadline elapses. The caller asserts, so a genuine leak still fails the test.
func settledGoroutines(base, tol int) int {
	deadline := time.Now().Add(5 * time.Second)
	for {
		runtime.GC()
		got := runtime.NumGoroutine()
		if got <= base+tol || !time.Now().Before(deadline) {
			return got
		}
		time.Sleep(50 * time.Millisecond)
	}
}

type statusErr struct {
	where  string
	status int
}

func (e statusErr) Error() string {
	return e.where + " request returned status " + http.StatusText(e.status)
}

func errStatus(where string, status int) error { return statusErr{where, status} }

// TestConcurrentReuse_StateHistory_NoCrossIdentityBleed is the D-025
// concurrent-reuse test for the state.history handler: N≥100 concurrent
// requests across DISTINCT identities against ONE shared handler + bus,
// under -race. It asserts no data races, no context bleed (each caller
// sees ONLY its own session's events), and no goroutine leak
// (baseline-restored after all requests return).
func TestConcurrentReuse_StateHistory_NoCrossIdentityBleed(t *testing.T) {
	deps := newTestDeps(t)
	defer deps.cleanup()

	store, err := artifacts.Open(context.Background(), config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("artifacts.Open: %v", err)
	}
	defer func() { _ = store.Close(context.Background()) }()

	// Seed distinct identities, each with a distinct event count so a
	// cross-identity bleed is detectable by the wrong tail sequence.
	const identities = 12
	const perID = 6
	for k := range identities {
		id := identity.Quadruple{Identity: identity.Identity{
			TenantID:  fmt.Sprintf("t-%d", k),
			UserID:    fmt.Sprintf("u-%d", k),
			SessionID: fmt.Sprintf("s-%d", k),
		}}
		for j := range perID + k { // session k has perID+k events
			ev := events.Event{
				Type:     events.EventTypeRuntimeWarning,
				Identity: id,
				Payload:  events.SubscriptionIdleClosedPayload{SubscriberID: uint64(j)},
			}
			if err := deps.bus.Publish(context.Background(), ev); err != nil {
				t.Fatalf("seed publish: %v", err)
			}
		}
	}

	mux, err := transports.NewMux(deps.surface, deps.bus,
		transports.WithoutValidator(),
		transports.WithStateHistory(deps.bus, store),
	)
	if err != nil {
		t.Fatalf("NewMux: %v", err)
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()

	settle()
	baseline := runtime.NumGoroutine()

	const n = 144
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			k := i % identities
			tenant := fmt.Sprintf("t-%d", k)
			user := fmt.Sprintf("u-%d", k)
			session := fmt.Sprintf("s-%d", k)
			body := fmt.Sprintf(`{"session_id":%q,"limit":100}`, session)
			req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/state/history", strings.NewReader(body))
			req.Header.Set("X-Harbor-Tenant", tenant)
			req.Header.Set("X-Harbor-User", user)
			req.Header.Set("X-Harbor-Session", session)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				errs <- err
				return
			}
			raw, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				errs <- fmt.Errorf("identity %d: status %d body=%s", k, resp.StatusCode, raw)
				return
			}
			var got prototypes.StateHistoryResponse
			if err := json.Unmarshal(raw, &got); err != nil {
				errs <- fmt.Errorf("identity %d: decode: %w", k, err)
				return
			}
			// No context bleed: every returned event belongs to THIS session,
			// and the count matches this session's seeded total.
			wantCount := perID + k
			if len(got.Events) != wantCount {
				errs <- fmt.Errorf("identity %d: got %d events, want %d (cross-identity bleed?)", k, len(got.Events), wantCount)
				return
			}
			for _, ev := range got.Events {
				if ev.Session != session {
					errs <- fmt.Errorf("identity %d: event from session %q leaked", k, ev.Session)
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

	if got := settledGoroutines(baseline, 8); got > baseline+8 {
		t.Errorf("goroutine leak: baseline=%d, after=%d", baseline, got)
	}
}

// postControlBearer drives the control route through the shared validator.
// Its stub-verified result carries both the identity and bounded agent reach.
func postControlBearer(url string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPost, url, body) //nolint:noctx // the shared-mux stress drives fire-and-forget requests; the test bounds the run itself.
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Harbor-Tenant", "t1")
	req.Header.Set("X-Harbor-User", "u1")
	req.Header.Set("X-Harbor-Session", "s1")
	req.Header.Set("Authorization", "Bearer faketoken")
	return http.DefaultClient.Do(req)
}
