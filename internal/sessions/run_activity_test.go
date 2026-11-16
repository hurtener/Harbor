package sessions_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/sessions"
)

// MarkRunActivity moves LastSeen to the clock's now when the mark lands past
// the coalesce window, and skips the write when it lands inside it.
func TestRegistry_MarkRunActivity_AdvancesAndCoalesces(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	clock := newFakeClock(t0)
	reg, _ := testWiring(t, sessions.WithClock(clock))
	id := ident("t1", "u1", "s1")
	if _, err := reg.Open(ctxFor(id), id.SessionID, id); err != nil {
		t.Fatalf("Open: %v", err)
	}

	clock.Advance(10 * time.Minute)
	if err := reg.MarkRunActivity(context.Background(), id); err != nil {
		t.Fatalf("MarkRunActivity: %v", err)
	}
	s, err := reg.Get(ctxFor(id), id.SessionID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if want := t0.Add(10 * time.Minute); !s.LastSeen.Equal(want) {
		t.Fatalf("LastSeen = %v, want %v", s.LastSeen, want)
	}

	// Inside the coalesce window: no write.
	clock.Advance(sessions.RunActivityCoalesceWindow - time.Second)
	if err := reg.MarkRunActivity(context.Background(), id); err != nil {
		t.Fatalf("MarkRunActivity (coalesced): %v", err)
	}
	s, _ = reg.Get(ctxFor(id), id.SessionID)
	if want := t0.Add(10 * time.Minute); !s.LastSeen.Equal(want) {
		t.Fatalf("coalesced mark wrote LastSeen = %v, want unchanged %v", s.LastSeen, want)
	}

	// Past the window: written.
	clock.Advance(2 * time.Second)
	if err := reg.MarkRunActivity(context.Background(), id); err != nil {
		t.Fatalf("MarkRunActivity: %v", err)
	}
	s, _ = reg.Get(ctxFor(id), id.SessionID)
	if !s.LastSeen.Equal(clock.Now()) {
		t.Fatalf("LastSeen = %v, want %v", s.LastSeen, clock.Now())
	}
}

// The mark is keyed by the stored session triple, not the ctx identity: a
// ctx carrying a different user (a participant writer) still marks the
// owner's session, and nothing is created under the writer's own triple.
func TestRegistry_MarkRunActivity_KeyedByStoredSession(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	clock := newFakeClock(t0)
	reg, _ := testWiring(t, sessions.WithClock(clock))
	owner := ident("t1", "owner", "s1")
	if _, err := reg.Open(ctxFor(owner), owner.SessionID, owner); err != nil {
		t.Fatalf("Open: %v", err)
	}
	writer := ident("t1", "writer", "s1")
	clock.Advance(time.Hour)
	if err := reg.MarkRunActivity(ctxFor(writer), owner); err != nil {
		t.Fatalf("MarkRunActivity: %v", err)
	}
	s, err := reg.Get(ctxFor(owner), owner.SessionID)
	if err != nil {
		t.Fatalf("Get owner: %v", err)
	}
	if !s.LastSeen.Equal(t0.Add(time.Hour)) {
		t.Fatalf("owner LastSeen = %v, want %v", s.LastSeen, t0.Add(time.Hour))
	}
	if _, err := reg.Get(ctxFor(writer), writer.SessionID); !errors.Is(err, sessions.ErrSessionNotFound) {
		t.Fatalf("writer triple Get err = %v, want ErrSessionNotFound (mark must create nothing)", err)
	}
	// A triple that names no session is not created.
	if err := reg.MarkRunActivity(context.Background(), writer); !errors.Is(err, sessions.ErrSessionNotFound) {
		t.Fatalf("MarkRunActivity(unknown) err = %v, want ErrSessionNotFound", err)
	}
	if _, err := reg.Get(ctxFor(writer), writer.SessionID); !errors.Is(err, sessions.ErrSessionNotFound) {
		t.Fatalf("unknown triple materialised: err = %v", err)
	}
	// An incomplete triple fails closed.
	if err := reg.MarkRunActivity(context.Background(), identity.Identity{TenantID: "t1", SessionID: "s1"}); err == nil {
		t.Fatal("MarkRunActivity accepted an incomplete identity")
	}
}

// A run that finishes after its session was closed records the activity but
// never reopens the session.
func TestRegistry_MarkRunActivity_ClosedStaysClosed(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	clock := newFakeClock(t0)
	reg, _ := testWiring(t, sessions.WithClock(clock))
	id := ident("t1", "u1", "s1")
	if _, err := reg.Open(ctxFor(id), id.SessionID, id); err != nil {
		t.Fatalf("Open: %v", err)
	}
	clock.Advance(time.Minute)
	if err := reg.Close(ctxFor(id), id.SessionID, "operator"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	clock.Advance(time.Minute)
	if err := reg.MarkRunActivity(context.Background(), id); err != nil {
		t.Fatalf("MarkRunActivity: %v", err)
	}
	s, _ := reg.Get(ctxFor(id), id.SessionID)
	if !s.Closed || s.ClosedReason != "operator" {
		t.Fatalf("Closed=%v reason=%q, want closed/operator", s.Closed, s.ClosedReason)
	}
	if !s.ClosedAt.Equal(t0.Add(time.Minute)) {
		t.Fatalf("ClosedAt = %v, want %v", s.ClosedAt, t0.Add(time.Minute))
	}
	if !s.LastSeen.Equal(t0.Add(2 * time.Minute)) {
		t.Fatalf("LastSeen = %v, want %v", s.LastSeen, t0.Add(2*time.Minute))
	}
}

// The idle sweep measures IdleTTL from the last run activity: a session used
// within the TTL survives a sweep that would have reaped it by its open time,
// and is reaped only after a full TTL without use.
func TestRegistry_GC_IdleMeasuredFromRunActivity(t *testing.T) {
	t.Parallel()
	clock := newFakeClock(time.Date(2026, 5, 9, 0, 0, 0, 0, time.UTC))
	policy := sessions.GCPolicy{
		IdleTTL:       time.Hour,
		HardCap:       100 * time.Hour,
		SweepInterval: time.Hour,
	}
	reg, _ := testWiring(t, sessions.WithClock(clock), sessions.WithGCPolicy(policy))
	id := ident("t1", "u1", "s1")
	if _, err := reg.Open(ctxFor(id), id.SessionID, id); err != nil {
		t.Fatalf("Open: %v", err)
	}
	clock.Advance(50 * time.Minute)
	if err := reg.MarkRunActivity(context.Background(), id); err != nil {
		t.Fatalf("MarkRunActivity: %v", err)
	}
	clock.Advance(50 * time.Minute) // 100m after open, 50m after use
	if n, err := reg.GC(context.Background(), policy); err != nil || n != 0 {
		t.Fatalf("GC reaped=%d err=%v, want 0 (used within IdleTTL)", n, err)
	}
	if s, _ := reg.Get(ctxFor(id), id.SessionID); s.Closed {
		t.Fatal("session in use was closed by the idle sweep")
	}
	clock.Advance(11 * time.Minute) // 61m after the last use
	if n, err := reg.GC(context.Background(), policy); err != nil || n != 1 {
		t.Fatalf("GC reaped=%d err=%v, want 1 (idle past IdleTTL)", n, err)
	}
	s, _ := reg.Get(ctxFor(id), id.SessionID)
	if !s.Closed || s.ClosedReason != "gc:idle" {
		t.Fatalf("Closed=%v reason=%q, want closed/gc:idle", s.Closed, s.ClosedReason)
	}
}
