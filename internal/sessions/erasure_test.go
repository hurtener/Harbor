package sessions_test

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/events/drivers/durable"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
	_ "github.com/hurtener/Harbor/internal/memory/drivers/inmem"
	"github.com/hurtener/Harbor/internal/sessions"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"

	"github.com/hurtener/Harbor/internal/artifacts"
	_ "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
)

// erasureFixture bundles the real stores + registry + durable bus the
// cascade drives, all sharing the ONE StateStore (so the durable event
// stream and the session-lifecycle record live under the same keys the
// cascade deletes).
type erasureFixture struct {
	store  state.StateStore
	mem    memory.MemoryStore
	arts   artifacts.ArtifactStore
	bus    events.EventBus
	reg    *sessions.Registry
	eraser *sessions.CascadeEraser
}

// busOf returns the fixture's event bus (a small accessor so the
// mid-cascade-error test can build a second eraser over the same bus).
func busOf(_ *testing.T, f erasureFixture) events.EventBus { return f.bus }

func newErasureFixture(t *testing.T, probe sessions.RunningProbe) erasureFixture {
	t.Helper()
	ctx := context.Background()
	red := auditpatterns.New()

	store, err := state.Open(ctx, config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(ctx) })

	bus, err := durable.New(ctx, config.EventsConfig{
		Driver:                   "durable",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     256,
		IdleTimeout:              60 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         64,
	}, red, store)
	if err != nil {
		t.Fatalf("durable.New: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(ctx) })

	mem, err := memory.Open(ctx, memory.ConfigSnapshot{
		Driver: "inmem", Strategy: memory.StrategyTruncation, BudgetTokens: 1000,
	}, memory.Deps{State: store, Bus: bus})
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	t.Cleanup(func() { _ = mem.Close(ctx) })

	arts, err := artifacts.Open(ctx, config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("artifacts.Open: %v", err)
	}
	t.Cleanup(func() { _ = arts.Close(ctx) })

	opts := []sessions.Option{}
	if probe != nil {
		opts = append(opts, sessions.WithGCPolicy(sessions.GCPolicy{RunningProbe: probe}))
	}
	reg, err := sessions.New(store, config.SessionsConfig{
		IdleTTL: 24 * time.Hour, HardCap: 720 * time.Hour, SweepInterval: time.Hour,
	}, bus, opts...)
	if err != nil {
		t.Fatalf("sessions.New: %v", err)
	}
	t.Cleanup(func() { _ = reg.CloseRegistry(ctx) })

	eraser, err := sessions.NewCascadeEraser(sessions.CascadeEraserDeps{
		Registry:  reg,
		State:     store,
		Memory:    mem,
		Artifacts: arts,
		Bus:       bus,
		Redactor:  red,
	})
	if err != nil {
		t.Fatalf("NewCascadeEraser: %v", err)
	}
	return erasureFixture{store: store, mem: mem, arts: arts, bus: bus, reg: reg, eraser: eraser}
}

// TestCascadeEraser_FullErasure_CascadesAndAudits is the cascade's
// happy-path end-to-end: open a session with state + a turn + an
// artifact, erase it, and assert every scoped store is empty, the counts
// are reported, and the content-free `session.erased` audit record is
// keyed under the actor's observability scope — NOT the erased triple
// (whose durable event stream is gone, leaving state.history empty).
func TestCascadeEraser_FullErasure_CascadesAndAudits(t *testing.T) {
	f := newErasureFixture(t, nil)
	ctx := context.Background()
	id := ident("t1", "u1", "s1")
	ictx := ctxFor(id)

	// Open the session (writes the session.lifecycle record + emits
	// session.opened onto the durable bus under the erased triple).
	if _, err := f.reg.Open(ictx, id.SessionID, id); err != nil {
		t.Fatalf("Open: %v", err)
	}
	// A memory turn.
	if err := f.mem.AddTurn(ictx, identity.Quadruple{Identity: id}, memory.ConversationTurn{
		UserMessage: "hello", AssistantResponse: "world",
	}); err != nil {
		t.Fatalf("AddTurn: %v", err)
	}
	// An artifact under the session scope.
	scope := artifacts.ArtifactScope{TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID}
	if _, err := f.arts.PutBytes(ctx, scope, []byte("blob"), artifacts.PutOpts{Namespace: "test"}); err != nil {
		t.Fatalf("PutBytes: %v", err)
	}

	// Sanity: the durable event stream exists under the erased triple.
	if _, err := f.store.Load(ctx, identity.Quadruple{Identity: id}, "events.durable.head"); err != nil {
		t.Fatalf("pre-erasure durable head missing: %v", err)
	}

	resp, err := f.eraser.Erase(ctx, id)
	if err != nil {
		t.Fatalf("Erase: %v", err)
	}
	if !resp.Deleted || !resp.MemoryPurged {
		t.Errorf("response = %+v, want Deleted && MemoryPurged", resp)
	}
	if resp.SessionID != id.SessionID {
		t.Errorf("response SessionID = %q, want %q", resp.SessionID, id.SessionID)
	}
	if resp.ArtifactsDeleted != 1 {
		t.Errorf("ArtifactsDeleted = %d, want 1", resp.ArtifactsDeleted)
	}
	if resp.StateRecordsDeleted <= 0 {
		t.Errorf("StateRecordsDeleted = %d, want > 0", resp.StateRecordsDeleted)
	}

	// Artifacts gone.
	refs, err := f.arts.List(ctx, scope)
	if err != nil {
		t.Fatalf("List after erasure: %v", err)
	}
	if len(refs) != 0 {
		t.Errorf("artifacts survived erasure: %d", len(refs))
	}
	// Memory clean.
	patch, err := f.mem.GetLLMContext(ctx, identity.Quadruple{Identity: id})
	if err != nil {
		t.Fatalf("GetLLMContext: %v", err)
	}
	if len(patch.RecentTurns) != 0 {
		t.Errorf("memory survived erasure: %d turns", len(patch.RecentTurns))
	}
	// The session-lifecycle record is hard-deleted.
	if _, err := f.store.Load(ctx, identity.Quadruple{Identity: id}, "session.lifecycle"); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("session.lifecycle survived erasure: err=%v", err)
	}
	// The erased triple's durable event stream is gone — state.history for
	// the erased triple is empty (no re-persisted erasure record).
	if _, err := f.store.Load(ctx, identity.Quadruple{Identity: id}, "events.durable.head"); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("erased triple's durable head survived (state.history non-empty): err=%v", err)
	}

	// The `session.erased` audit record is keyed under the ACTOR's
	// observability scope: a durable head exists under (t1, u1) with a
	// session id that is NOT the erased session.
	heads, err := f.store.ListKind(ctx, state.ListScope{MaintenanceScoped: true}, "events.durable.head")
	if err != nil {
		t.Fatalf("ListKind durable heads: %v", err)
	}
	foundActorHead := false
	for _, h := range heads {
		if h.Identity.SessionID == id.SessionID {
			t.Errorf("a durable head is still keyed under the erased session %q", id.SessionID)
		}
		if h.Identity.TenantID == id.TenantID && h.Identity.UserID == id.UserID && h.Identity.SessionID != id.SessionID {
			foundActorHead = true
		}
	}
	if !foundActorHead {
		t.Errorf("no session.erased durable record keyed under the actor (t1,u1) — got %d heads", len(heads))
	}
}

// TestCascadeEraser_RunningTask_Refused_TouchesNothing pins the fail-loud
// refusal: a session with a RUNNING task is refused with ErrSessionRunning
// and NO store is touched (the artifact + memory + state survive).
func TestCascadeEraser_RunningTask_Refused_TouchesNothing(t *testing.T) {
	f := newErasureFixture(t, func(context.Context, identity.Quadruple) (bool, error) { return true, nil })
	ctx := context.Background()
	id := ident("t1", "u1", "s1")
	ictx := ctxFor(id)
	if _, err := f.reg.Open(ictx, id.SessionID, id); err != nil {
		t.Fatalf("Open: %v", err)
	}
	scope := artifacts.ArtifactScope{TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID}
	if _, err := f.arts.PutBytes(ctx, scope, []byte("blob"), artifacts.PutOpts{Namespace: "test"}); err != nil {
		t.Fatalf("PutBytes: %v", err)
	}

	_, err := f.eraser.Erase(ctx, id)
	if !errors.Is(err, sessions.ErrSessionRunning) {
		t.Fatalf("Erase err=%v, want ErrSessionRunning", err)
	}
	// Stores untouched.
	if _, lerr := f.store.Load(ctx, identity.Quadruple{Identity: id}, "session.lifecycle"); lerr != nil {
		t.Errorf("running-refusal deleted the session record: %v", lerr)
	}
	refs, lerr := f.arts.List(ctx, scope)
	if lerr != nil {
		t.Fatalf("List: %v", lerr)
	}
	if len(refs) != 1 {
		t.Errorf("running-refusal touched artifacts: %d, want 1", len(refs))
	}
}

// TestCascadeEraser_NotFound pins existence non-disclosure on the cascade:
// erasing a never-opened session returns ErrSessionNotFound.
func TestCascadeEraser_NotFound(t *testing.T) {
	f := newErasureFixture(t, nil)
	_, err := f.eraser.Erase(context.Background(), ident("t1", "u1", "ghost"))
	if !errors.Is(err, sessions.ErrSessionNotFound) {
		t.Fatalf("Erase err=%v, want ErrSessionNotFound", err)
	}
}

// flakyArts wraps a real ArtifactStore and fails Delete while its toggle
// is set, so a test can force a mid-cascade store error.
type flakyArts struct {
	artifacts.ArtifactStore
	fail *atomic.Bool
}

func (f *flakyArts) Delete(ctx context.Context, scope artifacts.ArtifactScope, id string) (bool, error) {
	if f.fail.Load() {
		return false, errors.New("flaky artifact store: forced delete failure")
	}
	return f.ArtifactStore.Delete(ctx, scope, id)
}

// TestCascadeEraser_MidCascadeError_LoudAndRetrySafe pins the
// fail-loud-and-idempotent contract: a forced error mid-cascade (an
// artifact Delete failure) returns LOUDLY (never a partial silent
// success), leaves the still-undeleted stores intact (DeleteScope, a
// later step, never ran), and the cascade is safe to re-invoke to
// convergence once the transient fault clears.
func TestCascadeEraser_MidCascadeError_LoudAndRetrySafe(t *testing.T) {
	f := newErasureFixture(t, nil)
	ctx := context.Background()
	id := ident("t1", "u1", "s1")
	ictx := ctxFor(id)
	if _, err := f.reg.Open(ictx, id.SessionID, id); err != nil {
		t.Fatalf("Open: %v", err)
	}
	scope := artifacts.ArtifactScope{TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID}
	if _, err := f.arts.PutBytes(ctx, scope, []byte("blob"), artifacts.PutOpts{Namespace: "test"}); err != nil {
		t.Fatalf("PutBytes: %v", err)
	}

	var fail atomic.Bool
	fail.Store(true)
	flaky := &flakyArts{ArtifactStore: f.arts, fail: &fail}
	eraser, err := sessions.NewCascadeEraser(sessions.CascadeEraserDeps{
		Registry: f.reg, State: f.store, Memory: f.mem, Artifacts: flaky,
		Bus: busOf(t, f),
	})
	if err != nil {
		t.Fatalf("NewCascadeEraser: %v", err)
	}

	// First attempt: the artifact Delete fails → loud error.
	if _, derr := eraser.Erase(ctx, id); derr == nil {
		t.Fatal("mid-cascade artifact failure did not surface loudly")
	}
	// The state record survived — the later DeleteScope step never ran
	// (no partial silent erasure).
	if _, lerr := f.store.Load(ctx, identity.Quadruple{Identity: id}, "session.lifecycle"); lerr != nil {
		t.Fatalf("mid-cascade failure deleted the session record: %v", lerr)
	}

	// Clear the transient fault and re-invoke — the cascade converges.
	fail.Store(false)
	resp, derr := eraser.Erase(ctx, id)
	if derr != nil {
		t.Fatalf("retry after fault cleared failed: %v", derr)
	}
	if !resp.Deleted {
		t.Errorf("retry response = %+v, want Deleted", resp)
	}
	if _, lerr := f.store.Load(ctx, identity.Quadruple{Identity: id}, "session.lifecycle"); !errors.Is(lerr, state.ErrNotFound) {
		t.Errorf("session.lifecycle survived the retry: err=%v", lerr)
	}
}

// TestNewCascadeEraser_Misconfigured_FailsLoud pins the fail-closed
// construction contract: each missing mandatory dependency fails loud
// with ErrEraserMisconfigured rather than building a nil-panicking eraser.
func TestNewCascadeEraser_Misconfigured_FailsLoud(t *testing.T) {
	f := newErasureFixture(t, nil)
	bus := busOf(t, f)
	full := sessions.CascadeEraserDeps{
		Registry: f.reg, State: f.store, Memory: f.mem, Artifacts: f.arts, Bus: bus,
	}
	cases := map[string]func(d *sessions.CascadeEraserDeps){
		"nil registry":  func(d *sessions.CascadeEraserDeps) { d.Registry = nil },
		"nil state":     func(d *sessions.CascadeEraserDeps) { d.State = nil },
		"nil memory":    func(d *sessions.CascadeEraserDeps) { d.Memory = nil },
		"nil artifacts": func(d *sessions.CascadeEraserDeps) { d.Artifacts = nil },
		"nil bus":       func(d *sessions.CascadeEraserDeps) { d.Bus = nil },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			d := full
			mutate(&d)
			if _, err := sessions.NewCascadeEraser(d); !errors.Is(err, sessions.ErrEraserMisconfigured) {
				t.Fatalf("err=%v, want ErrEraserMisconfigured", err)
			}
		})
	}
	// The fully-wired deps construct cleanly.
	if _, err := sessions.NewCascadeEraser(full); err != nil {
		t.Fatalf("fully-wired NewCascadeEraser: %v", err)
	}
}

// TestCascadeEraser_Concurrent_DistinctSessions_NoCrossTalk is the
// mandatory concurrent-reuse + cross-session-isolation gate (D-025 / §11):
// N≥100 concurrent erasures of DISTINCT sessions against a single shared
// CascadeEraser + shared stores, asserting no data race (run under -race),
// no cross-talk (each erasure removes only its own session), and a
// never-erased sentinel session survives intact (erase-A-while-B-reads).
// A goroutine-baseline check guards against leaks.
func TestCascadeEraser_Concurrent_DistinctSessions_NoCrossTalk(t *testing.T) {
	f := newErasureFixture(t, nil)
	ctx := context.Background()
	const n = 120

	// A sentinel session that is NEVER erased — it must survive the
	// concurrent storm untouched.
	keep := ident("t1", "u1", "keep")
	if _, err := f.reg.Open(ctxFor(keep), keep.SessionID, keep); err != nil {
		t.Fatalf("open keep: %v", err)
	}
	keepScope := artifacts.ArtifactScope{TenantID: keep.TenantID, UserID: keep.UserID, SessionID: keep.SessionID}
	if _, err := f.arts.PutBytes(ctx, keepScope, []byte("keep"), artifacts.PutOpts{Namespace: "k"}); err != nil {
		t.Fatalf("PutBytes keep: %v", err)
	}

	// Seed N distinct sessions each with a memory turn + an artifact.
	ids := make([]identity.Identity, n)
	for i := range n {
		id := ident("t1", "u1", fmt.Sprintf("s-%03d", i))
		ids[i] = id
		ictx := ctxFor(id)
		if _, err := f.reg.Open(ictx, id.SessionID, id); err != nil {
			t.Fatalf("open %s: %v", id.SessionID, err)
		}
		if err := f.mem.AddTurn(ictx, identity.Quadruple{Identity: id}, memory.ConversationTurn{
			UserMessage: "u", AssistantResponse: "a",
		}); err != nil {
			t.Fatalf("AddTurn %s: %v", id.SessionID, err)
		}
		sc := artifacts.ArtifactScope{TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID}
		if _, err := f.arts.PutBytes(ctx, sc, []byte(id.SessionID), artifacts.PutOpts{Namespace: "k"}); err != nil {
			t.Fatalf("PutBytes %s: %v", id.SessionID, err)
		}
	}

	baseline := runtime.NumGoroutine()
	var wg sync.WaitGroup
	var fails atomic.Int64
	wg.Add(n)
	for i := range n {
		go func(id identity.Identity) {
			defer wg.Done()
			resp, err := f.eraser.Erase(ctx, id)
			if err != nil || !resp.Deleted || resp.SessionID != id.SessionID {
				fails.Add(1)
			}
		}(ids[i])
	}
	wg.Wait()
	if fails.Load() != 0 {
		t.Fatalf("%d concurrent erasures failed or crossed sessions", fails.Load())
	}

	// Every erased session is gone (no cross-talk left a record behind).
	for _, id := range ids {
		if _, err := f.store.Load(ctx, identity.Quadruple{Identity: id}, "session.lifecycle"); !errors.Is(err, state.ErrNotFound) {
			t.Fatalf("session %s survived its erasure: err=%v", id.SessionID, err)
		}
	}
	// The sentinel session is untouched.
	if _, err := f.store.Load(ctx, identity.Quadruple{Identity: keep}, "session.lifecycle"); err != nil {
		t.Fatalf("sentinel session was erased by a concurrent erasure: %v", err)
	}
	refs, err := f.arts.List(ctx, keepScope)
	if err != nil {
		t.Fatalf("List keep: %v", err)
	}
	if len(refs) != 1 {
		t.Errorf("sentinel artifact touched: %d, want 1", len(refs))
	}

	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if delta := runtime.NumGoroutine() - baseline; delta > 0 {
		t.Errorf("goroutine leak after concurrent erasures: baseline=%d after=%d", baseline, runtime.NumGoroutine())
	}
}
