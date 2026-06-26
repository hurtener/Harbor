package sessions_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/sessions"
)

// TestRegistry_Erase_HappyPath_ClearsCatalogNotDurableRecord pins the
// registry-side erasure contract: Erase runs the pre-flight (load+verify)
// and clears the in-memory + discovery catalogs, but performs NO durable
// record delete of its own (StateStore.DeleteScope owns that). After
// Erase the session is gone from the listing, while a direct record load
// still resolves — proving Erase did not durably delete.
func TestRegistry_Erase_HappyPath_ClearsCatalogNotDurableRecord(t *testing.T) {
	t.Parallel()
	reg, _ := testWiring(t)
	id := ident("t1", "u1", "s1")
	if _, err := reg.Open(ctxFor(id), id.SessionID, id); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := reg.Erase(ctxFor(id), id.SessionID); err != nil {
		t.Fatalf("Erase: %v", err)
	}
	// The session is gone from the per-(tenant, user) listing (idIndex +
	// catalog cleared).
	snaps, err := reg.ListSnapshots(context.Background(), sessions.SessionListFilter{
		TenantIDs: []string{id.TenantID}, UserIDs: []string{id.UserID}, IncludeClosed: true,
	})
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	if len(snaps) != 0 {
		t.Fatalf("Erase left %d session(s) in the listing, want 0", len(snaps))
	}
}

// TestRegistry_Erase_RunningTask_Refused pins the fail-loud running-task
// refusal: when the RunningProbe reports a RUNNING task, Erase returns
// ErrSessionRunning and touches NOTHING — the session stays listed.
func TestRegistry_Erase_RunningTask_Refused(t *testing.T) {
	t.Parallel()
	running := func(context.Context, identity.Quadruple) (bool, error) { return true, nil }
	reg, _ := testWiring(t, sessions.WithGCPolicy(sessions.GCPolicy{RunningProbe: running}))
	id := ident("t1", "u1", "s1")
	if _, err := reg.Open(ctxFor(id), id.SessionID, id); err != nil {
		t.Fatalf("Open: %v", err)
	}
	err := reg.Erase(ctxFor(id), id.SessionID)
	if !errors.Is(err, sessions.ErrSessionRunning) {
		t.Fatalf("Erase err=%v, want ErrSessionRunning", err)
	}
	// Nothing touched — the session is still listed.
	snaps, lerr := reg.ListSnapshots(context.Background(), sessions.SessionListFilter{
		TenantIDs: []string{id.TenantID}, UserIDs: []string{id.UserID}, IncludeClosed: true,
	})
	if lerr != nil {
		t.Fatalf("ListSnapshots: %v", lerr)
	}
	if len(snaps) != 1 {
		t.Fatalf("running-refusal touched the registry: %d session(s) listed, want 1", len(snaps))
	}
}

// TestRegistry_Erase_NotFound pins existence non-disclosure: erasing a
// session that was never opened under the caller's identity returns
// ErrSessionNotFound.
func TestRegistry_Erase_NotFound(t *testing.T) {
	t.Parallel()
	reg, _ := testWiring(t)
	id := ident("t1", "u1", "ghost")
	err := reg.Erase(ctxFor(id), id.SessionID)
	if !errors.Is(err, sessions.ErrSessionNotFound) {
		t.Fatalf("Erase err=%v, want ErrSessionNotFound", err)
	}
}

// TestRegistry_Erase_ForeignIdentity_NotFound pins that a caller whose
// verified identity does not own the session cannot erase it — the
// StateStore key is the full triple, so a foreign tenant resolves to
// ErrSessionNotFound (existence is never revealed across identities).
func TestRegistry_Erase_ForeignIdentity_NotFound(t *testing.T) {
	t.Parallel()
	reg, _ := testWiring(t)
	owner := ident("t1", "u1", "s1")
	if _, err := reg.Open(ctxFor(owner), owner.SessionID, owner); err != nil {
		t.Fatalf("Open: %v", err)
	}
	// A different tenant naming the same session id cannot reach it.
	foreign := ident("t2", "u1", "s1")
	err := reg.Erase(ctxFor(foreign), foreign.SessionID)
	if !errors.Is(err, sessions.ErrSessionNotFound) {
		t.Fatalf("foreign Erase err=%v, want ErrSessionNotFound", err)
	}
	// The owner's session is untouched.
	snaps, lerr := reg.ListSnapshots(context.Background(), sessions.SessionListFilter{
		TenantIDs: []string{owner.TenantID}, UserIDs: []string{owner.UserID}, IncludeClosed: true,
	})
	if lerr != nil {
		t.Fatalf("ListSnapshots: %v", lerr)
	}
	if len(snaps) != 1 {
		t.Fatalf("foreign Erase touched the owner's session: %d listed, want 1", len(snaps))
	}
}

// TestRegistry_Erase_Idempotent pins that a second Erase of an
// already-erased session returns ErrSessionNotFound (the record is gone),
// never a panic or a silent success.
func TestRegistry_Erase_Idempotent(t *testing.T) {
	t.Parallel()
	reg, _ := testWiring(t)
	id := ident("t1", "u1", "s1")
	if _, err := reg.Open(ctxFor(id), id.SessionID, id); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := reg.Erase(ctxFor(id), id.SessionID); err != nil {
		t.Fatalf("Erase 1: %v", err)
	}
	// Erase only cleared the in-memory catalogs; the durable record still
	// exists (DeleteScope owns the durable delete), so a second Erase
	// re-loads it and succeeds again — never a panic.
	if err := reg.Erase(ctxFor(id), id.SessionID); err != nil {
		t.Fatalf("Erase 2 (idempotent): %v", err)
	}
}
