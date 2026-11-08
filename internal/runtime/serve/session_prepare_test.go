package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/agentcfg/sessionoverlay"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/types"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/telemetry"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
)

// fakePrepareDeps is a controllable reconcile + stamp pair.
type fakePrepareDeps struct {
	mu        sync.Mutex
	stampVal  string
	stampOK   bool
	stampErr  error
	clean     bool
	calls     atomic.Int64
	gate      chan struct{} // when non-nil, reconcile blocks until closed or ctx done
	onRun     func(ctx context.Context, q identity.Quadruple)
	bumpFirst bool // the first reconcile moves the stamp once (its own attaches)
}

func newFakePrepareDeps() *fakePrepareDeps {
	return &fakePrepareDeps{stampVal: "s1", stampOK: true, clean: true}
}

func (f *fakePrepareDeps) reconcile(ctx context.Context, _ string, q identity.Quadruple) bool {
	n := f.calls.Add(1)
	if f.onRun != nil {
		f.onRun(ctx, q)
	}
	if f.gate != nil {
		select {
		case <-f.gate:
		case <-ctx.Done():
			return false
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.bumpFirst && n == 1 {
		f.stampVal += "+attached"
	}
	return f.clean
}

func (f *fakePrepareDeps) stamp(context.Context, identity.Identity, string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stampVal, f.stampOK, f.stampErr
}

func (f *fakePrepareDeps) setStamp(v string) {
	f.mu.Lock()
	f.stampVal = v
	f.mu.Unlock()
}

func testPrepareLimits() sessionPrepareLimits {
	return sessionPrepareLimits{
		Freshness: time.Minute, FailedRetry: 10 * time.Second, JoinWait: 5 * time.Second,
		WorkBudget: 10 * time.Second, MaxEntries: 64, Workers: 4,
	}
}

func newTestPreparer(t *testing.T, deps *fakePrepareDeps, limits sessionPrepareLimits, route llm.ProviderRouteConfig) *sessionPreparer {
	t.Helper()
	p := newSessionPreparer(slog.New(slog.NewTextHandler(io.Discard, nil)), limits, route, deps.reconcile, deps.stamp)
	t.Cleanup(p.Close)
	return p
}

func prepID(tenant, user string) identity.Identity {
	return identity.Identity{TenantID: tenant, UserID: user, SessionID: "home-" + user}
}

// testAdmissions is the documented dummy (all-zero KEK) admission authority
// the prepare tests mint reach admissions with — the same authority type the
// control surface and run start share in production.
var testAdmissions = func() *tasks.AgentReachAdmissionAuthority {
	sealer, err := toolauth.NewAESGCMSealer(make([]byte, toolauth.KEKSizeBytes))
	if err != nil {
		panic(err)
	}
	authority, err := tasks.NewAgentReachAdmissionAuthority(sealer)
	if err != nil {
		panic(err)
	}
	return authority
}()

// prepCtx is an admitted request/run context: verified identity, runtime,
// reach narrowed to agent, and a sealed reach admission for exactly that
// subject — what the control surface hands the preparer and what run start
// restores from a task's receipt.
func prepCtx(t *testing.T, id identity.Identity, agent string) context.Context {
	t.Helper()
	return prepCtxRuntime(t, id, agent, "runtime-1")
}

func prepCtxRuntime(t *testing.T, id identity.Identity, agent, runtimeID string) context.Context {
	t.Helper()
	ctx, err := identity.WithVerified(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	ctx = auth.WithAgentReach(auth.WithRuntimeID(ctx, runtimeID), []string{agent})
	ctx, err = testAdmissions.Admit(ctx, id, agent)
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

func mustPrepare(t *testing.T, p *sessionPreparer, id identity.Identity, agent string, route *llm.ProviderRoute) types.PrepareState {
	t.Helper()
	state, err := p.PrepareSession(prepCtx(t, id, agent), protocol.SessionPrepareRequest{Identity: id, EffectiveAgentID: agent, ProviderRoute: route})
	if err != nil {
		t.Fatalf("PrepareSession: %v", err)
	}
	return state
}

// waitPrepared blocks until the record for (id, agent) completes.
func waitPrepared(t *testing.T, p *sessionPreparer, id identity.Identity, agent string) {
	t.Helper()
	p.mu.Lock()
	entry := p.entries[sessionPrepareKey{tenant: id.TenantID, user: id.UserID, agent: agent}]
	p.mu.Unlock()
	if entry == nil {
		t.Fatal("no preparation record")
	}
	select {
	case <-entry.done:
	case <-time.After(10 * time.Second):
		t.Fatal("preparation did not complete")
	}
}

func TestSessionPrepare_PrepareThenRunSkipsReconcile(t *testing.T) {
	deps := newFakePrepareDeps()
	p := newTestPreparer(t, deps, testPrepareLimits(), llm.ProviderRouteConfig{})
	id := prepID("t1", "u1")
	if got := mustPrepare(t, p, id, "agent", nil); got != types.PrepareStateScheduled {
		t.Fatalf("state = %q", got)
	}
	waitPrepared(t, p, id, "agent")
	if deps.calls.Load() != 1 {
		t.Fatalf("reconcile passes = %d, want 1 (stable stamp)", deps.calls.Load())
	}
	// The run uses a DIFFERENT session than the home-screen prepare.
	run := identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "chat-42"}
	if got := p.consume(prepCtx(t, run, "agent"), run, "agent"); got != telemetry.TurnPreparedHit {
		t.Fatalf("consume = %v, want hit", got)
	}
	// A repeated prepare inside the freshness window is a no-op.
	if got := mustPrepare(t, p, id, "agent", nil); got != types.PrepareStateFresh {
		t.Fatalf("repeat state = %q, want fresh", got)
	}
	if deps.calls.Load() != 1 {
		t.Fatalf("fresh repeat reconciled again: %d", deps.calls.Load())
	}
}

func TestSessionPrepare_FirstPassAttachesThenVerifies(t *testing.T) {
	deps := newFakePrepareDeps()
	deps.bumpFirst = true
	p := newTestPreparer(t, deps, testPrepareLimits(), llm.ProviderRouteConfig{})
	id := prepID("t", "u")
	mustPrepare(t, p, id, "a", nil)
	waitPrepared(t, p, id, "a")
	if deps.calls.Load() != 2 {
		t.Fatalf("passes = %d, want 2 (attach then verify)", deps.calls.Load())
	}
	if got := p.consume(prepCtx(t, id, "a"), id, "a"); got != telemetry.TurnPreparedHit {
		t.Fatalf("consume = %v, want hit on the post-attach stamp", got)
	}
}

func TestSessionPrepare_InFlightJoin(t *testing.T) {
	deps := newFakePrepareDeps()
	deps.gate = make(chan struct{})
	p := newTestPreparer(t, deps, testPrepareLimits(), llm.ProviderRouteConfig{})
	id := prepID("t", "u")
	mustPrepare(t, p, id, "a", nil)
	if got := mustPrepare(t, p, id, "a", nil); got != types.PrepareStateInFlight {
		t.Fatalf("second state = %q, want in_flight", got)
	}
	result := make(chan telemetry.TurnPrepared, 1)
	go func() { result <- p.consume(prepCtx(t, id, "a"), id, "a") }()
	select {
	case got := <-result:
		t.Fatalf("consume returned %v before the in-flight preparation finished", got)
	case <-time.After(50 * time.Millisecond):
	}
	close(deps.gate)
	if got := <-result; got != telemetry.TurnPreparedJoined {
		t.Fatalf("consume = %v, want joined", got)
	}
}

func TestSessionPrepare_JoinWaitIsBounded(t *testing.T) {
	deps := newFakePrepareDeps()
	deps.gate = make(chan struct{})
	limits := testPrepareLimits()
	limits.JoinWait = 20 * time.Millisecond
	p := newTestPreparer(t, deps, limits, llm.ProviderRouteConfig{})
	id := prepID("t", "u")
	mustPrepare(t, p, id, "a", nil)
	started := time.Now()
	if got := p.consume(prepCtx(t, id, "a"), id, "a"); got != telemetry.TurnPreparedStale {
		t.Fatalf("consume = %v, want stale after the bounded wait", got)
	}
	if waited := time.Since(started); waited > 2*time.Second {
		t.Fatalf("join waited %v", waited)
	}
	// A cancelled run context also stops waiting.
	ctx, cancel := context.WithCancel(prepCtx(t, id, "a"))
	cancel()
	if got := p.consume(ctx, id, "a"); got != telemetry.TurnPreparedStale {
		t.Fatalf("consume(cancelled) = %v", got)
	}
	close(deps.gate)
}

func TestSessionPrepare_StaleFallsBackToNormalPath(t *testing.T) {
	t.Run("inputs changed", func(t *testing.T) {
		deps := newFakePrepareDeps()
		p := newTestPreparer(t, deps, testPrepareLimits(), llm.ProviderRouteConfig{})
		id := prepID("t", "u")
		mustPrepare(t, p, id, "a", nil)
		waitPrepared(t, p, id, "a")
		deps.setStamp("s2") // e.g. a config revision or a detach after prepare
		if got := p.consume(prepCtx(t, id, "a"), id, "a"); got != telemetry.TurnPreparedStale {
			t.Fatalf("consume = %v, want stale", got)
		}
	})
	t.Run("expired", func(t *testing.T) {
		deps := newFakePrepareDeps()
		p := newTestPreparer(t, deps, testPrepareLimits(), llm.ProviderRouteConfig{})
		now := time.Now()
		var offset atomic.Int64
		p.now = func() time.Time { return now.Add(time.Duration(offset.Load())) }
		id := prepID("t", "u")
		mustPrepare(t, p, id, "a", nil)
		waitPrepared(t, p, id, "a")
		offset.Store(int64(2 * time.Minute))
		if got := p.consume(prepCtx(t, id, "a"), id, "a"); got != telemetry.TurnPreparedStale {
			t.Fatalf("consume = %v, want stale after freshness", got)
		}
		if got := mustPrepare(t, p, id, "a", nil); got != types.PrepareStateScheduled {
			t.Fatalf("prepare after expiry = %q, want scheduled", got)
		}
	})
	t.Run("failed reconcile", func(t *testing.T) {
		deps := newFakePrepareDeps()
		deps.clean = false
		p := newTestPreparer(t, deps, testPrepareLimits(), llm.ProviderRouteConfig{})
		now := time.Now()
		var offset atomic.Int64
		p.now = func() time.Time { return now.Add(time.Duration(offset.Load())) }
		id := prepID("t", "u")
		mustPrepare(t, p, id, "a", nil)
		waitPrepared(t, p, id, "a")
		if got := p.consume(prepCtx(t, id, "a"), id, "a"); got != telemetry.TurnPreparedStale {
			t.Fatalf("consume = %v, want stale for a failed preparation", got)
		}
		if got := mustPrepare(t, p, id, "a", nil); got != types.PrepareStateFresh {
			t.Fatalf("retry inside FailedRetry = %q, want fresh (suppressed)", got)
		}
		offset.Store(int64(11 * time.Second))
		if got := mustPrepare(t, p, id, "a", nil); got != types.PrepareStateScheduled {
			t.Fatalf("retry after FailedRetry = %q, want scheduled", got)
		}
	})
	t.Run("unstable inputs", func(t *testing.T) {
		deps := newFakePrepareDeps()
		var n atomic.Int64
		p := newSessionPreparer(slog.New(slog.NewTextHandler(io.Discard, nil)), testPrepareLimits(), llm.ProviderRouteConfig{}, deps.reconcile,
			func(context.Context, identity.Identity, string) (string, bool, error) {
				return fmt.Sprintf("s%d", n.Add(1)), true, nil
			})
		t.Cleanup(p.Close)
		id := prepID("t", "u")
		mustPrepare(t, p, id, "a", nil)
		waitPrepared(t, p, id, "a")
		if deps.calls.Load() != 2 {
			t.Fatalf("passes = %d, want the 2-pass bound", deps.calls.Load())
		}
		if got := p.consume(prepCtx(t, id, "a"), id, "a"); got != telemetry.TurnPreparedStale {
			t.Fatalf("consume = %v, want stale for unstable inputs", got)
		}
	})
	t.Run("no stamp support", func(t *testing.T) {
		deps := newFakePrepareDeps()
		deps.stampOK = false
		p := newTestPreparer(t, deps, testPrepareLimits(), llm.ProviderRouteConfig{})
		id := prepID("t", "u")
		mustPrepare(t, p, id, "a", nil)
		waitPrepared(t, p, id, "a")
		if deps.calls.Load() != 1 {
			t.Fatalf("warm-only passes = %d, want 1", deps.calls.Load())
		}
		if got := p.consume(prepCtx(t, id, "a"), id, "a"); got != telemetry.TurnPreparedStale {
			t.Fatalf("consume = %v, want stale without a stamp", got)
		}
	})
	t.Run("stamp error", func(t *testing.T) {
		deps := newFakePrepareDeps()
		p := newTestPreparer(t, deps, testPrepareLimits(), llm.ProviderRouteConfig{})
		id := prepID("t", "u")
		mustPrepare(t, p, id, "a", nil)
		waitPrepared(t, p, id, "a")
		deps.mu.Lock()
		deps.stampErr = errors.New("store down")
		deps.mu.Unlock()
		if got := p.consume(prepCtx(t, id, "a"), id, "a"); got != telemetry.TurnPreparedStale {
			t.Fatalf("consume = %v, want stale on a stamp error", got)
		}
	})
}

func TestSessionPrepare_IsolationAcrossTenantsUsersAgents(t *testing.T) {
	deps := newFakePrepareDeps()
	p := newTestPreparer(t, deps, testPrepareLimits(), llm.ProviderRouteConfig{})
	id := prepID("tenant-a", "user-a")
	mustPrepare(t, p, id, "agent-a", nil)
	waitPrepared(t, p, id, "agent-a")
	for _, other := range []struct {
		id    identity.Identity
		agent string
	}{
		{prepID("tenant-b", "user-a"), "agent-a"},
		{prepID("tenant-a", "user-b"), "agent-a"},
		{prepID("tenant-a", "user-a"), "agent-b"},
	} {
		if got := p.consume(prepCtx(t, other.id, other.agent), other.id, other.agent); got != telemetry.TurnPreparedMiss {
			t.Fatalf("consume(%+v, %s) = %v, want miss", other.id, other.agent, got)
		}
	}
	// The background work ran under the preparing principal's own identity.
	seen := make(chan identity.Quadruple, 1)
	deps2 := newFakePrepareDeps()
	deps2.onRun = func(ctx context.Context, q identity.Quadruple) {
		ctxQ, _ := identity.QuadrupleFrom(ctx)
		verified, _ := identity.FromVerified(ctx)
		if ctxQ != q || verified != q.Identity {
			t.Errorf("work ctx identity %+v / verified %+v, want %+v", ctxQ, verified, q)
		}
		seen <- q
	}
	p2 := newTestPreparer(t, deps2, testPrepareLimits(), llm.ProviderRouteConfig{})
	mustPrepare(t, p2, prepID("tenant-c", "user-c"), "agent-c", nil)
	q := <-seen
	if q.TenantID != "tenant-c" || q.UserID != "user-c" || !strings.HasPrefix(q.RunID, "prepare-run-") {
		t.Fatalf("work quadruple = %+v", q)
	}
}

func TestSessionPrepare_DetachedFromRequestCancellation(t *testing.T) {
	deps := newFakePrepareDeps()
	deps.gate = make(chan struct{})
	p := newTestPreparer(t, deps, testPrepareLimits(), llm.ProviderRouteConfig{})
	id := prepID("t", "u")
	reqCtx, cancel := context.WithCancel(prepCtx(t, id, "a"))
	if _, err := p.PrepareSession(reqCtx, protocol.SessionPrepareRequest{Identity: id, EffectiveAgentID: "a"}); err != nil {
		t.Fatal(err)
	}
	cancel() // the HTTP request returned immediately
	close(deps.gate)
	waitPrepared(t, p, id, "a")
	if got := p.consume(prepCtx(t, id, "a"), id, "a"); got != telemetry.TurnPreparedHit {
		t.Fatalf("consume = %v: a returned request must not cancel the background work", got)
	}
}

func TestSessionPrepare_ConcurrentPreparesCoalesce(t *testing.T) {
	deps := newFakePrepareDeps()
	deps.gate = make(chan struct{})
	p := newTestPreparer(t, deps, testPrepareLimits(), llm.ProviderRouteConfig{})
	id := prepID("t", "u")
	var scheduled, inFlight atomic.Int64
	var wg sync.WaitGroup
	for range 128 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			switch mustPrepare(t, p, id, "a", nil) {
			case types.PrepareStateScheduled:
				scheduled.Add(1)
			case types.PrepareStateInFlight:
				inFlight.Add(1)
			default:
				t.Error("unexpected state")
			}
		}()
	}
	wg.Wait()
	close(deps.gate)
	waitPrepared(t, p, id, "a")
	if scheduled.Load() != 1 || inFlight.Load() != 127 || deps.calls.Load() != 1 {
		t.Fatalf("scheduled=%d in_flight=%d reconciles=%d, want 1/127/1", scheduled.Load(), inFlight.Load(), deps.calls.Load())
	}
	// Distinct principals run concurrently and independently, each once.
	deps2 := newFakePrepareDeps()
	p2 := newTestPreparer(t, deps2, testPrepareLimits(), llm.ProviderRouteConfig{})
	var wg2 sync.WaitGroup
	for i := range 4 {
		wg2.Add(1)
		go func() {
			defer wg2.Done()
			mustPrepare(t, p2, prepID("t", fmt.Sprintf("u%d", i)), "a", nil)
		}()
	}
	wg2.Wait()
	for i := range 4 {
		waitPrepared(t, p2, prepID("t", fmt.Sprintf("u%d", i)), "a")
		if got := p2.consume(prepCtx(t, prepID("t", fmt.Sprintf("u%d", i)), "a"), prepID("t", fmt.Sprintf("u%d", i)), "a"); got != telemetry.TurnPreparedHit {
			t.Fatalf("user %d consume = %v", i, got)
		}
	}
}

func TestSessionPrepare_WorkersBoundedAndBusy(t *testing.T) {
	deps := newFakePrepareDeps()
	deps.gate = make(chan struct{})
	limits := testPrepareLimits()
	limits.Workers = 2
	p := newTestPreparer(t, deps, limits, llm.ProviderRouteConfig{})
	if mustPrepare(t, p, prepID("t", "u1"), "a", nil) != types.PrepareStateScheduled ||
		mustPrepare(t, p, prepID("t", "u2"), "a", nil) != types.PrepareStateScheduled {
		t.Fatal("first two prepares not scheduled")
	}
	if got := mustPrepare(t, p, prepID("t", "u3"), "a", nil); got != types.PrepareStateBusy {
		t.Fatalf("third prepare = %q, want busy", got)
	}
	if got := p.consume(prepCtx(t, prepID("t", "u3"), "a"), prepID("t", "u3"), "a"); got != telemetry.TurnPreparedMiss {
		t.Fatalf("dropped prepare left state: %v", got)
	}
	close(deps.gate)
}

func TestSessionPrepare_MemoryBounded(t *testing.T) {
	deps := newFakePrepareDeps()
	limits := testPrepareLimits()
	limits.MaxEntries = 8
	p := newTestPreparer(t, deps, limits, llm.ProviderRouteConfig{})
	now := time.Now()
	var offset atomic.Int64
	p.now = func() time.Time { return now.Add(time.Duration(offset.Load())) }
	for i := range 100 {
		id := prepID("t", fmt.Sprintf("u%d", i))
		if got := mustPrepare(t, p, id, "a", nil); got == types.PrepareStateBusy {
			t.Fatalf("prepare %d busy: completed records must be reclaimable", i)
		}
		waitPrepared(t, p, id, "a")
		offset.Add(int64(time.Second))
		if n := p.entryCount(); n > 8 {
			t.Fatalf("records = %d, cap 8", n)
		}
	}
}

func TestSessionPrepare_CloseJoinsWorkersAndRefusesWork(t *testing.T) {
	baseline := runtime.NumGoroutine()
	deps := newFakePrepareDeps()
	deps.gate = make(chan struct{}) // never released: only Close can end the work
	p := newSessionPreparer(slog.New(slog.NewTextHandler(io.Discard, nil)), testPrepareLimits(), llm.ProviderRouteConfig{}, deps.reconcile, deps.stamp)
	for i := range 4 {
		mustPrepare(t, p, prepID("t", fmt.Sprintf("u%d", i)), "a", nil)
	}
	done := make(chan struct{})
	go func() { p.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not cancel and join in-flight preparations")
	}
	if _, err := p.PrepareSession(prepCtx(t, prepID("t", "x"), "a"), protocol.SessionPrepareRequest{Identity: prepID("t", "x"), EffectiveAgentID: "a"}); !errors.Is(err, protocol.ErrSessionPrepareUnavailable) {
		t.Fatalf("prepare after Close err = %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
		runtime.Gosched()
		time.Sleep(5 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > baseline {
		t.Fatalf("goroutines = %d, baseline %d", n, baseline)
	}
	p.Close() // idempotent
}

func TestSessionPrepare_RejectsIncompleteRequests(t *testing.T) {
	p := newTestPreparer(t, newFakePrepareDeps(), testPrepareLimits(), llm.ProviderRouteConfig{})
	if _, err := p.PrepareSession(context.Background(), protocol.SessionPrepareRequest{Identity: identity.Identity{TenantID: "t"}, EffectiveAgentID: "a"}); err == nil {
		t.Fatal("incomplete identity accepted")
	}
	if _, err := p.PrepareSession(context.Background(), protocol.SessionPrepareRequest{Identity: prepID("t", "u"), EffectiveAgentID: " "}); err == nil {
		t.Fatal("empty agent accepted")
	}
	var bridge SessionPrepareBridge
	if _, err := bridge.PrepareSession(context.Background(), protocol.SessionPrepareRequest{}); !errors.Is(err, protocol.ErrSessionPrepareUnavailable) {
		t.Fatalf("unbound bridge err = %v", err)
	}
	bridge.Bind(p)
	if state, err := bridge.PrepareSession(prepCtx(t, prepID("t", "u"), "a"), protocol.SessionPrepareRequest{Identity: prepID("t", "u"), EffectiveAgentID: "a"}); err != nil || state != types.PrepareStateScheduled {
		t.Fatalf("bound bridge state=%q err=%v", state, err)
	}
}

type prepareRouteResolver struct{ selects atomic.Int64 }

func (r *prepareRouteResolver) SelectProviderRoute(_ context.Context, req llm.ProviderRouteRequest) (llm.SelectedProviderRoute, error) {
	r.selects.Add(1)
	if !strings.HasPrefix(req.LogicalRunID, "prepare-run-") || !strings.HasPrefix(req.TaskID, "prepare-task-") || req.Purpose != llm.ProviderRoutePurposeRun {
		return llm.SelectedProviderRoute{}, errors.New("unexpected preparation request shape")
	}
	return llm.SelectedProviderRoute{Provider: "openai", Model: "m", KeyName: "k", RouteID: req.RouteID, RouteGeneration: req.RouteGeneration,
		ProviderConnectionID: req.ProviderConnectionID, ProviderConnectionGeneration: req.ProviderConnectionGeneration,
		CredentialAssetGeneration: req.CredentialAssetGeneration, ModelSelector: req.ModelSelector, ExpiresAt: time.Now().Add(2 * time.Minute)}, nil
}

func (*prepareRouteResolver) ResolveProviderRoute(context.Context, llm.ProviderRouteRequest) (llm.ResolvedProviderRoute, error) {
	panic("prepare must never resolve a credential")
}

func TestSessionPrepare_PreselectsProviderRoute(t *testing.T) {
	resolver := &prepareRouteResolver{}
	cache := llm.NewProviderRouteSelectionCache(16)
	route := &llm.ProviderRoute{RouteID: "r", RouteGeneration: 1, ProviderConnectionID: "c", ProviderConnectionGeneration: 1, CredentialAssetGeneration: 1, ModelSelector: "planner"}
	var logs bytes.Buffer
	deps := newFakePrepareDeps()
	p := newSessionPreparer(slog.New(slog.NewJSONHandler(&logs, nil)), testPrepareLimits(), llm.ProviderRouteConfig{Resolver: resolver, SelectionCache: cache}, deps.reconcile, deps.stamp)
	id := prepID("t", "u")
	mustPrepare(t, p, id, "a", route)
	waitPrepared(t, p, id, "a")
	p.Close() // joins the worker so the log line is written
	key := llm.ProviderRouteSelectionKey{TenantID: "t", UserID: "u", EffectiveAgentID: "a", RouteID: "r", RouteGeneration: 1,
		ProviderConnectionID: "c", ProviderConnectionGeneration: 1, CredentialAssetGeneration: 1, ModelSelector: "planner",
		ModelProfileSupported: true, Purpose: llm.ProviderRoutePurposeRun}
	if _, ok := cache.Lookup(key, time.Now()); !ok || resolver.selects.Load() != 1 {
		t.Fatalf("route not pre-selected (cached=%v selects=%d)", ok, resolver.selects.Load())
	}
	var record map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &record); err != nil {
		t.Fatalf("log: %v: %s", err, logs.String())
	}
	if record["msg"] != "session.prepare" || record["outcome"] != "ready" || record["route"] != "selected" || record["tenant_id"] != "t" {
		t.Fatalf("unexpected prepare log: %v", record)
	}

	// Fresh reconcile + cached route: a repeated prepare does no work at all.
	p2 := newTestPreparer(t, deps, testPrepareLimits(), llm.ProviderRouteConfig{Resolver: resolver, SelectionCache: cache})
	before := deps.calls.Load()
	mustPrepare(t, p2, id, "a", route)
	waitPrepared(t, p2, id, "a")
	if got := mustPrepare(t, p2, id, "a", route); got != types.PrepareStateFresh {
		t.Fatalf("repeat = %q, want fresh", got)
	}
	if resolver.selects.Load() != 1 || deps.calls.Load() != before+1 {
		t.Fatalf("selects=%d reconciles=%d", resolver.selects.Load(), deps.calls.Load()-before)
	}
}

// TestSessionPrepare_RunStartUsesPreparedState drives the PRODUCTION
// run-start reconcile concretes (attach/detach against a spec-derived MCP
// fixture) and the production catalog stamp: prepare attaches the declared
// connection, the run start skips its reconcile, and a config change after
// prepare makes the next run start reconcile normally and converge.
func TestSessionPrepare_RunStartUsesPreparedState(t *testing.T) {
	fixture := reattachFixtureServer(t)
	attacher, catalog, registry, _ := reAttacherFor(t, WithReattachTimeout(15*time.Second))
	st, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close(context.Background()) })
	agents := lifecycleTestRegistry(t, st)
	overlay, err := sessionoverlay.NewStore(st, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	declared := agentcfg.ConfigPayload{Connections: &agentcfg.ConnectionsSection{Servers: []agentcfg.MCPConnectionDescriptor{reHTTPDesc("conv", fixture.URL)}}}
	if _, err := agents.SetRevision(ctx, identity.Quadruple{Identity: reQuad().Identity}, reAgent, agentcfg.ConfigScopeAgent, declared, agentcfg.SetOptions{}); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	d := &RunLoopDriver{
		logger: logger, agentConfig: agents, sessionOverlay: overlay, catalog: catalog,
		connectionDetacher:   NewMCPConnectionDetacher(catalog, registry, logger),
		connectionReattacher: attacher,
	}
	d.preparer = newSessionPreparer(logger, testPrepareLimits(), llm.ProviderRouteConfig{}, d.reconcileConnectionsPass, d.prepareStamp)
	t.Cleanup(d.preparer.Close)

	id := reQuad().Identity
	if _, err := d.preparer.PrepareSession(prepCtx(t, id, reAgent), protocol.SessionPrepareRequest{Identity: id, EffectiveAgentID: reAgent}); err != nil {
		t.Fatal(err)
	}
	waitPrepared(t, d.preparer, id, reAgent)
	attachedGeneration := registry.RegistryGeneration()
	if len(d.connectionDetacher.(*MCPConnectionDetacher).AttachedSources(ctx, reOwner())) != 1 {
		t.Fatal("prepare did not attach the declared connection")
	}

	runTurn := func(admitted bool) map[string]any {
		t.Helper()
		var out bytes.Buffer
		base := ctx
		if admitted {
			base = prepCtx(t, reQuad().Identity, reAgent)
		}
		turnCtx := telemetry.BeginTurnTiming(base, slog.New(slog.NewJSONHandler(&out, nil)), time.Now())
		d.reconcileConnections(turnCtx, reAgent, reQuad(), admitted)
		telemetry.ObserveFirstModelRequest(turnCtx)
		var record map[string]any
		if err := json.Unmarshal(out.Bytes(), &record); err != nil {
			t.Fatal(err)
		}
		return record
	}
	record := runTurn(true)
	if record["prepared"] != "hit" {
		t.Fatalf("turn.pre_model prepared = %v, want hit: %v", record["prepared"], record)
	}
	if _, attached := record["tool_attach_ms"]; attached {
		t.Fatal("a prepared run start still ran the connection reconcile")
	}
	if registry.RegistryGeneration() != attachedGeneration {
		t.Fatal("a prepared run start changed the registry")
	}
	// A run without verified reach admission never consumes prepared state.
	if record := runTurn(false); record["prepared"] != nil {
		t.Fatalf("unadmitted run consumed prepared state: %v", record)
	}

	// Remove the declaration after prepare: the stamp moves, the run start
	// reconciles normally and detaches.
	if _, err := agents.SetRevision(ctx, identity.Quadruple{Identity: reQuad().Identity}, reAgent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{}, agentcfg.SetOptions{}); err != nil {
		t.Fatal(err)
	}
	if record := runTurn(true); record["prepared"] != "stale" {
		t.Fatalf("prepared = %v after a config change, want stale", record["prepared"])
	}
	if n := len(d.connectionDetacher.(*MCPConnectionDetacher).AttachedSources(ctx, reOwner())); n != 0 {
		t.Fatalf("stale run start did not reconcile: %d sources attached", n)
	}
}

// TestSessionPrepare_RequiresAdmittedAuthority — the preparer boundary
// accepts only a context carrying the verified identity, the reach narrowed
// to the target agent, and a sealed admission for exactly that subject.
func TestSessionPrepare_RequiresAdmittedAuthority(t *testing.T) {
	p := newTestPreparer(t, newFakePrepareDeps(), testPrepareLimits(), llm.ProviderRouteConfig{})
	id := prepID("t", "u")
	req := protocol.SessionPrepareRequest{Identity: id, EffectiveAgentID: "a"}
	verified := func() context.Context {
		ctx, err := identity.WithVerified(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		return ctx
	}
	admitFor := func(ctx context.Context, admitID identity.Identity, agent string) context.Context {
		ctx, err := testAdmissions.Admit(ctx, admitID, agent)
		if err != nil {
			t.Fatal(err)
		}
		return ctx
	}
	other := identity.Identity{TenantID: "t", UserID: "u2", SessionID: "home-u"}
	cases := map[string]context.Context{
		"no verified identity":      auth.WithAgentReach(context.Background(), []string{"a"}),
		"different verified":        auth.WithAgentReach(func() context.Context { c, _ := identity.WithVerified(context.Background(), other); return c }(), []string{"a"}),
		"no reach":                  admitFor(verified(), id, "a"),
		"wider reach":               admitFor(auth.WithAgentReach(verified(), []string{"a", "b"}), id, "a"),
		"no admission":              auth.WithAgentReach(verified(), []string{"a"}),
		"admission for other user":  admitFor(auth.WithAgentReach(verified(), []string{"a"}), other, "a"),
		"admission for other agent": admitFor(auth.WithAgentReach(verified(), []string{"a"}), id, "b"),
	}
	for name, ctx := range cases {
		if _, err := p.PrepareSession(ctx, req); err == nil {
			t.Errorf("%s: prepare accepted", name)
		}
	}
	if n := p.entryCount(); n != 0 {
		t.Fatalf("refused prepares created %d records", n)
	}
}

// TestSessionPrepare_RecordBoundToAdmission — a record is consumed only by a
// run whose own sealed admission names the same runtime, tenant, user and
// agent.
func TestSessionPrepare_RecordBoundToAdmission(t *testing.T) {
	deps := newFakePrepareDeps()
	p := newTestPreparer(t, deps, testPrepareLimits(), llm.ProviderRouteConfig{})
	id := prepID("t", "u")
	mustPrepare(t, p, id, "a", nil)
	waitPrepared(t, p, id, "a")
	run := identity.Identity{TenantID: "t", UserID: "u", SessionID: "chat"}
	if got := p.consume(prepCtxRuntime(t, run, "a", "runtime-2"), run, "a"); got != telemetry.TurnPreparedStale {
		t.Fatalf("consume under another runtime's admission = %v, want stale", got)
	}
	plain, err := identity.WithVerified(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.consume(plain, run, "a"); got == telemetry.TurnPreparedHit || got == telemetry.TurnPreparedJoined {
		t.Fatalf("consume without an admission = %v", got)
	}
	if got := p.consume(prepCtx(t, run, "a"), run, "a"); got != telemetry.TurnPreparedHit {
		t.Fatalf("consume under a matching admission = %v, want hit", got)
	}
}

// TestSessionPrepare_FreshnessFromFinalObservation — the freshness bound runs
// from the start of the final pass's input observation, not from completion,
// and is rechecked after the run-start stamp read.
func TestSessionPrepare_FreshnessFromFinalObservation(t *testing.T) {
	var offset atomic.Int64
	base := time.Now()
	clock := func() time.Time { return base.Add(time.Duration(offset.Load())) }
	t.Run("slow pass", func(t *testing.T) {
		offset.Store(0)
		deps := newFakePrepareDeps()
		deps.onRun = func(context.Context, identity.Quadruple) { offset.Add(int64(80 * time.Second)) }
		p := newTestPreparer(t, deps, testPrepareLimits(), llm.ProviderRouteConfig{})
		p.now = clock
		id := prepID("t", "u")
		mustPrepare(t, p, id, "a", nil)
		waitPrepared(t, p, id, "a")
		offset.Add(int64(15 * time.Second)) // 15 s after completion, 95 s after observation
		if got := p.consume(prepCtx(t, id, "a"), id, "a"); got != telemetry.TurnPreparedStale {
			t.Fatalf("consume = %v, want stale: the observation is older than the freshness bound", got)
		}
	})
	t.Run("slow stamp read", func(t *testing.T) {
		offset.Store(0)
		deps := newFakePrepareDeps()
		var slow atomic.Bool
		p := newSessionPreparer(slog.New(slog.NewTextHandler(io.Discard, nil)), testPrepareLimits(), llm.ProviderRouteConfig{}, deps.reconcile,
			func(ctx context.Context, id identity.Identity, agent string) (string, bool, error) {
				if slow.Load() {
					offset.Add(int64(45 * time.Second))
				}
				return deps.stamp(ctx, id, agent)
			})
		t.Cleanup(p.Close)
		p.now = clock
		id := prepID("t", "u")
		mustPrepare(t, p, id, "a", nil)
		waitPrepared(t, p, id, "a")
		offset.Add(int64(50 * time.Second)) // fresh when consume starts
		slow.Store(true)                    // ... but not once the stamp read returns
		if got := p.consume(prepCtx(t, id, "a"), id, "a"); got != telemetry.TurnPreparedStale {
			t.Fatalf("consume = %v, want stale after the deadline recheck", got)
		}
	})
}

// TestPrepareStamp_CoversSignedOperationGenerations — the reconcile stamp
// moves when a durable signed-capability operation record for the principal
// changes (claim, phase transition, publisher takeover) even though no
// revision pointer or fence moved, and ignores other principals' records.
func TestPrepareStamp_CoversSignedOperationGenerations(t *testing.T) {
	st, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close(context.Background()) })
	agents := lifecycleTestRegistry(t, st)
	overlay, err := sessionoverlay.NewStore(st, nil)
	if err != nil {
		t.Fatal(err)
	}
	ops, err := agentcfg.NewSignedOAuthMCPOperationStore(st)
	if err != nil {
		t.Fatal(err)
	}
	_, catalog, registry, _ := reAttacherFor(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	d := &RunLoopDriver{logger: logger, agentConfig: agents, sessionOverlay: overlay, catalog: catalog,
		connectionDetacher: NewMCPConnectionDetacher(catalog, registry, logger), signedOperations: ops}
	ctx := context.Background()
	id := reQuad().Identity
	if _, err := agents.SetRevision(ctx, identity.Quadruple{Identity: id}, reAgent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{}, agentcfg.SetOptions{}); err != nil {
		t.Fatal(err)
	}
	stamp := func() string {
		t.Helper()
		v, ok, err := d.prepareStamp(ctx, id, reAgent)
		if err != nil || !ok {
			t.Fatalf("stamp ok=%v err=%v", ok, err)
		}
		return v
	}
	binding := func(user, agent string) agentcfg.SignedOAuthMCPBinding {
		return agentcfg.SignedOAuthMCPBinding{TenantID: reTenant, UserID: user, SessionID: "s", AgentID: agent, Broker: "broker",
			ProviderName: "provider", CapabilityRevision: "1", URLDigest: "url", SinkDigest: "sink", Audience: "aud",
			Connection: agentcfg.SignedOAuthMCPConnectionDescriptor{Name: "server", URL: "https://example.test/mcp"}}
	}
	key := func(jti, user string) agentcfg.SignedOAuthMCPReplayKey {
		k := agentcfg.SignedOAuthMCPReplayKey{TenantID: reTenant, TrustAnchorName: "anchor", Issuer: "iss", KeyID: "kid", JTI: jti}
		if user != "" {
			k.UserID, k.SessionID = user, "s"
		}
		return k
	}
	s0 := stamp()
	// Another agent's and another user's user-tier receipts do not move it.
	if _, _, err := ops.Claim(ctx, key("other-agent", ""), binding("registrar", "agent-other"), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ops.Claim(ctx, key("other-user", "user-other"), binding("user-other", reAgent), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := stamp(); got != s0 {
		t.Fatal("a foreign principal's operation record moved the stamp")
	}
	// The principal's agent-tier receipt does.
	claimed, _, err := ops.Claim(ctx, key("mine", ""), binding("registrar", reAgent), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	s1 := stamp()
	if s1 == s0 {
		t.Fatal("claiming a signed operation for the agent did not move the stamp")
	}
	committed, err := ops.Advance(ctx, claimed, agentcfg.SignedOAuthMCPPhaseRevisionCommitted, "rev-1")
	if err != nil {
		t.Fatal(err)
	}
	s2 := stamp()
	if s2 == s1 {
		t.Fatal("a phase transition did not move the stamp")
	}
	if _, err := ops.AcquirePublisher(ctx, committed); err != nil {
		t.Fatal(err)
	}
	if stamp() == s2 {
		t.Fatal("a publisher takeover did not move the stamp")
	}
	// The acting user's own user-tier receipt also moves it.
	s3 := stamp()
	if _, _, err := ops.Claim(ctx, key("mine-user", id.UserID), binding(id.UserID, reAgent), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if stamp() == s3 {
		t.Fatal("the acting user's user-tier receipt did not move the stamp")
	}
}
