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
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/sessions"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

// --- helpers ---

// fakeClock is a controllable clock used by GC hard-cap / idle tests
// so they don't time.Sleep (forbidden by AGENTS.md §11).
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(t time.Time) *fakeClock { return &fakeClock{now: t} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// testWiring constructs a Registry over real audit + events + state
// drivers. The sweeper's tick interval is overridden to a value that
// won't fire during the test; tests that need GC drive it explicitly
// via Registry.GC.
func testWiring(t *testing.T, opts ...sessions.Option) (*sessions.Registry, events.EventBus, state.StateStore) {
	t.Helper()
	cfg := &config.Config{
		Events: config.EventsConfig{
			Driver:                   "inmem",
			MaxSubscribersPerSession: 16,
			SubscriberBufferSize:     64,
			IdleTimeout:              60 * time.Second,
			DropWindow:               1 * time.Second,
		},
		State:    config.StateConfig{Driver: "inmem"},
		Sessions: config.SessionsConfig{IdleTTL: 24 * time.Hour, HardCap: 720 * time.Hour, SweepInterval: 1 * time.Hour},
	}
	red := auditpatterns.New()
	bus, err := events.Open(context.Background(), cfg.Events, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	store, err := state.Open(context.Background(), cfg.State)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })

	reg, err := sessions.New(store, cfg.Sessions, bus, opts...)
	if err != nil {
		t.Fatalf("sessions.New: %v", err)
	}
	t.Cleanup(func() { _ = reg.CloseRegistry(context.Background()) })
	return reg, bus, store
}

func ctxFor(id identity.Identity) context.Context {
	ctx, _ := identity.With(context.Background(), id)
	return ctx
}

func ident(tenant, user, session string) identity.Identity {
	return identity.Identity{TenantID: tenant, UserID: user, SessionID: session}
}

// --- lifecycle / identity tests ---

func TestRegistry_Open_HappyPath(t *testing.T) {
	t.Parallel()
	reg, _, _ := testWiring(t)
	id := ident("t1", "u1", "s1")
	s, err := reg.Open(ctxFor(id), id.SessionID, id)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if s.Identity != id {
		t.Errorf("Identity = %+v, want %+v", s.Identity, id)
	}
	if s.Closed {
		t.Error("freshly-opened session should not be Closed")
	}
}

func TestRegistry_Open_DuplicateOpenSameTriple_Rejected(t *testing.T) {
	t.Parallel()
	reg, _, _ := testWiring(t)
	id := ident("t1", "u1", "s1")
	if _, err := reg.Open(ctxFor(id), id.SessionID, id); err != nil {
		t.Fatalf("first Open: %v", err)
	}
	_, err := reg.Open(ctxFor(id), id.SessionID, id)
	if !errors.Is(err, sessions.ErrSessionAlreadyOpen) {
		t.Fatalf("err=%v, want ErrSessionAlreadyOpen", err)
	}
}

func TestRegistry_Open_AfterClose_Rejected(t *testing.T) {
	t.Parallel()
	reg, _, _ := testWiring(t)
	id := ident("t1", "u1", "s1")
	if _, err := reg.Open(ctxFor(id), id.SessionID, id); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := reg.Close(ctxFor(id), id.SessionID, "test"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_, err := reg.Open(ctxFor(id), id.SessionID, id)
	if !errors.Is(err, sessions.ErrReopenAfterClose) {
		t.Fatalf("err=%v, want ErrReopenAfterClose", err)
	}
}

func TestRegistry_Open_EmptyIdentity_Rejected(t *testing.T) {
	t.Parallel()
	reg, _, _ := testWiring(t)
	cases := []identity.Identity{
		{TenantID: "", UserID: "u", SessionID: "s"},
		{TenantID: "t", UserID: "", SessionID: "s"},
		{TenantID: "t", UserID: "u", SessionID: ""},
	}
	for i, id := range cases {
		_, err := reg.Open(context.Background(), id.SessionID, id)
		if !errors.Is(err, identity.ErrIdentityIncomplete) {
			t.Errorf("case %d: err=%v, want ErrIdentityIncomplete", i, err)
		}
	}
}

func TestRegistry_CrossTenant_SessionIDReuse_Rejected(t *testing.T) {
	t.Parallel()
	reg, _, _ := testWiring(t)
	idA := ident("tA", "uA", "shared-sid")
	idB := ident("tB", "uB", "shared-sid")
	if _, err := reg.Open(ctxFor(idA), idA.SessionID, idA); err != nil {
		t.Fatalf("tenant A Open: %v", err)
	}
	_, err := reg.Open(ctxFor(idB), idB.SessionID, idB)
	if !errors.Is(err, sessions.ErrSessionIDReuse) {
		t.Fatalf("err=%v, want ErrSessionIDReuse", err)
	}
}

func TestRegistry_Touch_UpdatesLastSeen(t *testing.T) {
	t.Parallel()
	clock := newFakeClock(time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC))
	reg, _, _ := testWiring(t, sessions.WithClock(clock))
	id := ident("t1", "u1", "s1")
	s0, err := reg.Open(ctxFor(id), id.SessionID, id)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	clock.Advance(1 * time.Hour)
	if err := reg.Touch(ctxFor(id), id.SessionID); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	s1, err := reg.Get(ctxFor(id), id.SessionID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !s1.LastSeen.After(s0.LastSeen) {
		t.Errorf("LastSeen did not advance: before=%v after=%v", s0.LastSeen, s1.LastSeen)
	}
}

func TestRegistry_Touch_OnClosed_Rejected(t *testing.T) {
	t.Parallel()
	reg, _, _ := testWiring(t)
	id := ident("t1", "u1", "s1")
	if _, err := reg.Open(ctxFor(id), id.SessionID, id); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := reg.Close(ctxFor(id), id.SessionID, "done"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	err := reg.Touch(ctxFor(id), id.SessionID)
	if !errors.Is(err, sessions.ErrReopenAfterClose) {
		t.Fatalf("err=%v, want ErrReopenAfterClose", err)
	}
}

func TestRegistry_Close_Idempotent(t *testing.T) {
	t.Parallel()
	reg, _, _ := testWiring(t)
	id := ident("t1", "u1", "s1")
	if _, err := reg.Open(ctxFor(id), id.SessionID, id); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := reg.Close(ctxFor(id), id.SessionID, "first"); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	// Second close: no error, original reason wins.
	if err := reg.Close(ctxFor(id), id.SessionID, "second"); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	s, err := reg.Get(ctxFor(id), id.SessionID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if s.ClosedReason != "first" {
		t.Errorf("ClosedReason=%q, want %q", s.ClosedReason, "first")
	}
}

func TestRegistry_Close_EmitsEvent(t *testing.T) {
	t.Parallel()
	reg, bus, _ := testWiring(t)
	id := ident("t1", "u1", "s1")
	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant: id.TenantID, User: id.UserID, Session: id.SessionID,
		Types: []events.EventType{sessions.EventTypeSessionClosed},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()
	if _, err := reg.Open(ctxFor(id), id.SessionID, id); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := reg.Close(ctxFor(id), id.SessionID, "fin"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case ev := <-sub.Events():
		if ev.Type != sessions.EventTypeSessionClosed {
			t.Errorf("type=%v, want session.closed", ev.Type)
		}
		p, ok := ev.Payload.(sessions.SessionClosedPayload)
		if !ok {
			t.Fatalf("payload type=%T, want SessionClosedPayload", ev.Payload)
		}
		if p.Reason != "fin" {
			t.Errorf("Reason=%q, want %q", p.Reason, "fin")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not observe session.closed within 2s")
	}
}

func TestRegistry_Inspect_RunningFromProbe(t *testing.T) {
	t.Parallel()
	reg, _, _ := testWiring(t, sessions.WithGCPolicy(sessions.GCPolicy{
		RunningProbe: func(_ context.Context, _ identity.Quadruple) (bool, error) {
			return true, nil
		},
	}))
	id := ident("t1", "u1", "s1")
	if _, err := reg.Open(ctxFor(id), id.SessionID, id); err != nil {
		t.Fatalf("Open: %v", err)
	}
	snap, err := reg.Inspect(ctxFor(id), id.SessionID)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if !snap.Running {
		t.Errorf("Running=false, want true (probe returned true)")
	}
}

func TestRegistry_Inspect_OnClosed(t *testing.T) {
	t.Parallel()
	reg, _, _ := testWiring(t)
	id := ident("t1", "u1", "s1")
	if _, err := reg.Open(ctxFor(id), id.SessionID, id); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := reg.Close(ctxFor(id), id.SessionID, "fin"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	snap, err := reg.Inspect(ctxFor(id), id.SessionID)
	if err != nil {
		t.Fatalf("Inspect closed: %v", err)
	}
	if !snap.Closed {
		t.Errorf("Closed=false, want true")
	}
}

func TestRegistry_Identity_Immutable_AcrossTouch(t *testing.T) {
	t.Parallel()
	reg, _, _ := testWiring(t)
	storedID := ident("t1", "u1", "s1")
	if _, err := reg.Open(ctxFor(storedID), storedID.SessionID, storedID); err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Different ctx Identity — must be rejected. The StateStore is
	// keyed by the full triple, so a different ctx Identity for the
	// same SessionID naturally fails to find the record. The security
	// guarantee is "you can't Touch a session you didn't Open"; the
	// concrete error is ErrSessionNotFound (the underlying record
	// keyed by (t1,uOTHER,s1) does not exist).
	otherID := ident("t1", "uOTHER", "s1")
	err := reg.Touch(ctxFor(otherID), storedID.SessionID)
	if !errors.Is(err, sessions.ErrSessionNotFound) {
		t.Fatalf("err=%v, want ErrSessionNotFound", err)
	}
}

// --- GC tests ---

func TestRegistry_GC_NeverReapsRunning(t *testing.T) {
	t.Parallel()
	clock := newFakeClock(time.Date(2026, 5, 9, 0, 0, 0, 0, time.UTC))
	policy := sessions.GCPolicy{
		IdleTTL:       1 * time.Hour,
		HardCap:       2 * time.Hour,
		SweepInterval: 1 * time.Hour,
		RunningProbe:  func(_ context.Context, _ identity.Quadruple) (bool, error) { return true, nil },
	}
	reg, _, _ := testWiring(t, sessions.WithClock(clock), sessions.WithGCPolicy(policy))
	id := ident("t1", "u1", "s1")
	if _, err := reg.Open(ctxFor(id), id.SessionID, id); err != nil {
		t.Fatalf("Open: %v", err)
	}
	clock.Advance(48 * time.Hour) // way past hard cap
	n, err := reg.GC(context.Background(), policy)
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if n != 0 {
		t.Errorf("reaped=%d, want 0 (probe says RUNNING)", n)
	}
	// Session must still be open.
	s, err := reg.Get(ctxFor(id), id.SessionID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if s.Closed {
		t.Error("session was closed despite RunningProbe=true")
	}
}

func TestRegistry_GC_ReapsIdleSession(t *testing.T) {
	t.Parallel()
	clock := newFakeClock(time.Date(2026, 5, 9, 0, 0, 0, 0, time.UTC))
	policy := sessions.GCPolicy{
		IdleTTL:       1 * time.Hour,
		HardCap:       100 * time.Hour,
		SweepInterval: 1 * time.Hour,
	}
	reg, _, _ := testWiring(t, sessions.WithClock(clock), sessions.WithGCPolicy(policy))
	id := ident("t1", "u1", "s1")
	if _, err := reg.Open(ctxFor(id), id.SessionID, id); err != nil {
		t.Fatalf("Open: %v", err)
	}
	clock.Advance(2 * time.Hour) // past IdleTTL
	n, err := reg.GC(context.Background(), policy)
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if n != 1 {
		t.Fatalf("reaped=%d, want 1", n)
	}
	s, err := reg.Get(ctxFor(id), id.SessionID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !s.Closed || s.ClosedReason != "gc:idle" {
		t.Errorf("Closed=%v Reason=%q, want true / gc:idle", s.Closed, s.ClosedReason)
	}
}

func TestRegistry_GC_HardCapWins_OverRecentTouch(t *testing.T) {
	t.Parallel()
	clock := newFakeClock(time.Date(2026, 5, 9, 0, 0, 0, 0, time.UTC))
	policy := sessions.GCPolicy{
		IdleTTL:       100 * time.Hour,
		HardCap:       3 * time.Hour,
		SweepInterval: 1 * time.Hour,
	}
	reg, _, _ := testWiring(t, sessions.WithClock(clock), sessions.WithGCPolicy(policy))
	id := ident("t1", "u1", "s1")
	if _, err := reg.Open(ctxFor(id), id.SessionID, id); err != nil {
		t.Fatalf("Open: %v", err)
	}
	clock.Advance(4 * time.Hour) // past HardCap
	if err := reg.Touch(ctxFor(id), id.SessionID); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	// Touch refreshes LastSeen, but HardCap wins because it's measured
	// from OpenedAt, not LastSeen.
	n, err := reg.GC(context.Background(), policy)
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if n != 1 {
		t.Fatalf("reaped=%d, want 1 (HardCap exceeded)", n)
	}
	s, err := reg.Get(ctxFor(id), id.SessionID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !s.Closed || s.ClosedReason != "gc:hard_cap" {
		t.Errorf("Closed=%v Reason=%q, want true / gc:hard_cap", s.Closed, s.ClosedReason)
	}
}

func TestRegistry_GC_EmitsGCReapedEvent(t *testing.T) {
	t.Parallel()
	clock := newFakeClock(time.Date(2026, 5, 9, 0, 0, 0, 0, time.UTC))
	policy := sessions.GCPolicy{
		IdleTTL:       1 * time.Hour,
		HardCap:       100 * time.Hour,
		SweepInterval: 1 * time.Hour,
	}
	reg, bus, _ := testWiring(t, sessions.WithClock(clock), sessions.WithGCPolicy(policy))
	id := ident("t1", "u1", "s1")
	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant: id.TenantID, User: id.UserID, Session: id.SessionID,
		Types: []events.EventType{sessions.EventTypeSessionGCReaped},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()
	if _, err := reg.Open(ctxFor(id), id.SessionID, id); err != nil {
		t.Fatalf("Open: %v", err)
	}
	clock.Advance(2 * time.Hour)
	if _, err := reg.GC(context.Background(), policy); err != nil {
		t.Fatalf("GC: %v", err)
	}
	select {
	case ev := <-sub.Events():
		if ev.Type != sessions.EventTypeSessionGCReaped {
			t.Errorf("type=%v, want session.gc_reaped", ev.Type)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not observe session.gc_reaped within 2s")
	}
}

func TestRegistry_Sweeper_StartsAndStops_NoLeak(t *testing.T) {
	t.Parallel()
	baseline := runtime.NumGoroutine()
	for i := 0; i < 10; i++ {
		reg, _, _ := testWiring(t)
		_ = reg.CloseRegistry(context.Background())
	}
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline+2 && time.Now().Before(deadline) {
		runtime.Gosched()
		time.Sleep(10 * time.Millisecond)
	}
	if delta := runtime.NumGoroutine() - baseline; delta > 2 {
		t.Errorf("goroutine leak: baseline=%d after=%d (delta=%d)", baseline, runtime.NumGoroutine(), delta)
	}
}

// --- Concurrent reuse + cross-tenant isolation (D-025) ---

func TestRegistry_ConcurrentReuse_ReuseContract(t *testing.T) {
	t.Parallel()
	reg, _, _ := testWiring(t)

	const tenants = 8
	const sessionsPerTenant = 16

	var wg sync.WaitGroup
	openErrs := atomic.Int64{}
	for ti := 0; ti < tenants; ti++ {
		for si := 0; si < sessionsPerTenant; si++ {
			wg.Add(1)
			go func(tenant, sess int) {
				defer wg.Done()
				id := ident(fmt.Sprintf("t-%d", tenant), fmt.Sprintf("u-%d", tenant), fmt.Sprintf("s-%d-%d", tenant, sess))
				_, err := reg.Open(ctxFor(id), id.SessionID, id)
				if err != nil {
					openErrs.Add(1)
					t.Errorf("Open: %v", err)
					return
				}
				if err := reg.Touch(ctxFor(id), id.SessionID); err != nil {
					t.Errorf("Touch: %v", err)
					return
				}
				snap, err := reg.Inspect(ctxFor(id), id.SessionID)
				if err != nil {
					t.Errorf("Inspect: %v", err)
					return
				}
				if snap.Identity.TenantID != id.TenantID {
					t.Errorf("identity bleed: snap=%q want=%q", snap.Identity.TenantID, id.TenantID)
				}
				if err := reg.Close(ctxFor(id), id.SessionID, "ok"); err != nil {
					t.Errorf("Close: %v", err)
				}
			}(ti, si)
		}
	}
	wg.Wait()
	if openErrs.Load() != 0 {
		t.Errorf("Open errors observed: %d", openErrs.Load())
	}
}

func TestRegistry_CrossTenant_OpenIsolation(t *testing.T) {
	t.Parallel()
	reg, _, _ := testWiring(t)
	const tenants = 8
	for ti := 0; ti < tenants; ti++ {
		for si := 0; si < 4; si++ {
			id := ident(fmt.Sprintf("t-%d", ti), fmt.Sprintf("u-%d", ti), fmt.Sprintf("s-%d-%d", ti, si))
			if _, err := reg.Open(ctxFor(id), id.SessionID, id); err != nil {
				t.Fatalf("Open(%s): %v", id.SessionID, err)
			}
		}
	}
	// Tenant A asking for Tenant B's session must not succeed via Get.
	idAccess := ident("t-0", "u-0", "s-1-2") // belongs to t-1
	_, err := reg.Get(ctxFor(idAccess), idAccess.SessionID)
	if err == nil {
		t.Errorf("Get across tenants should not have succeeded")
	}
}

func TestRegistry_NoGoroutineLeak_AfterClose(t *testing.T) {
	baseline := runtime.NumGoroutine()
	reg, _, _ := testWiring(t)
	id := ident("t1", "u1", "s1")
	if _, err := reg.Open(ctxFor(id), id.SessionID, id); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := reg.CloseRegistry(context.Background()); err != nil {
		t.Fatalf("CloseRegistry: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline+2 && time.Now().Before(deadline) {
		runtime.Gosched()
		time.Sleep(10 * time.Millisecond)
	}
	if delta := runtime.NumGoroutine() - baseline; delta > 2 {
		t.Errorf("goroutine leak: baseline=%d after=%d", baseline, runtime.NumGoroutine())
	}
}
