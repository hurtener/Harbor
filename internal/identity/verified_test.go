package identity_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
)

var (
	own     = identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}
	sibling = identity.Identity{TenantID: "t1", UserID: "u2", SessionID: "s9"}
	foreign = identity.Identity{TenantID: "t2", UserID: "u1", SessionID: "s1"}
)

// TestWithVerified_SeatsBothTheAnchorAndTheWorkingIdentity — the
// request-edge write establishes the identity every later decision
// reconciles against, and seats it as the working identity in one call.
func TestWithVerified_SeatsBothTheAnchorAndTheWorkingIdentity(t *testing.T) {
	t.Parallel()
	ctx, err := identity.WithVerified(context.Background(), own)
	if err != nil {
		t.Fatalf("WithVerified: %v", err)
	}
	got, ok := identity.FromVerified(ctx)
	if !ok || got != own {
		t.Fatalf("FromVerified = (%+v, %v), want (%+v, true)", got, ok, own)
	}
	working, ok := identity.From(ctx)
	if !ok || working != own {
		t.Fatalf("From = (%+v, %v), want (%+v, true)", working, ok, own)
	}
}

// TestWithVerified_RejectsAnIncompleteTriple — identity is mandatory at
// the anchor too.
func TestWithVerified_RejectsAnIncompleteTriple(t *testing.T) {
	t.Parallel()
	if _, err := identity.WithVerified(context.Background(),
		identity.Identity{TenantID: "t1", UserID: "u1"}); !errors.Is(err, identity.ErrIdentityIncomplete) {
		t.Fatalf("err = %v, want ErrIdentityIncomplete", err)
	}
}

// TestFromVerified_AbsentIsRepresentable — a context that never passed a
// request edge reads back as "no anchor", which a gate must handle
// explicitly rather than defaulting.
func TestFromVerified_AbsentIsRepresentable(t *testing.T) {
	t.Parallel()
	if _, ok := identity.FromVerified(context.Background()); ok {
		t.Fatal("a bare context reported a verified identity")
	}
	// A working identity seated without an anchor stays anchorless.
	ctx, err := identity.With(context.Background(), own)
	if err != nil {
		t.Fatalf("With: %v", err)
	}
	if _, ok := identity.FromVerified(ctx); ok {
		t.Fatal("a working identity was read back as a verified one")
	}
}

// TestWith_NarrowsWithinTheVerifiedTenant — internal re-scoping to
// another user or session inside the verified tenant is ordinary work
// and stays unrestricted.
func TestWith_NarrowsWithinTheVerifiedTenant(t *testing.T) {
	t.Parallel()
	ctx, err := identity.WithVerified(context.Background(), own)
	if err != nil {
		t.Fatalf("WithVerified: %v", err)
	}
	scoped, err := identity.With(ctx, sibling)
	if err != nil {
		t.Fatalf("re-scoping within the verified tenant: %v", err)
	}
	got, _ := identity.From(scoped)
	if got != sibling {
		t.Fatalf("working identity = %+v, want %+v", got, sibling)
	}
	anchor, _ := identity.FromVerified(scoped)
	if anchor != own {
		t.Fatalf("the anchor moved to %+v; it must stay %+v", anchor, own)
	}
}

// TestWith_RefusesToWidenTheTenant — the asymmetry this closes: the
// working identity is written many times, so it must not be the place a
// tenant boundary is crossed.
func TestWith_RefusesToWidenTheTenant(t *testing.T) {
	t.Parallel()
	ctx, err := identity.WithVerified(context.Background(), own)
	if err != nil {
		t.Fatalf("WithVerified: %v", err)
	}
	out, err := identity.With(ctx, foreign)
	if !errors.Is(err, identity.ErrTenantWidening) {
		t.Fatalf("err = %v, want ErrTenantWidening", err)
	}
	got, _ := identity.From(out)
	if got != own {
		t.Fatalf("a refused re-scope left %+v on ctx; it must leave the original %+v", got, own)
	}
}

// TestWith_UnanchoredContextIsUnrestricted — an in-process embedder, a
// background worker or a unit test has no anchor to widen beyond.
func TestWith_UnanchoredContextIsUnrestricted(t *testing.T) {
	t.Parallel()
	ctx, err := identity.With(context.Background(), foreign)
	if err != nil {
		t.Fatalf("With on an unanchored context: %v", err)
	}
	got, _ := identity.From(ctx)
	if got != foreign {
		t.Fatalf("working identity = %+v, want %+v", got, foreign)
	}
}

// TestWithElevated_CrossesTheTenantAndRecordsWhy — the one path across
// the boundary, and it will not travel anonymously.
func TestWithElevated_CrossesTheTenantAndRecordsWhy(t *testing.T) {
	t.Parallel()
	ctx, err := identity.WithVerified(context.Background(), own)
	if err != nil {
		t.Fatalf("WithVerified: %v", err)
	}
	if identity.IsElevated(ctx) {
		t.Fatal("a fresh request reported an elevation")
	}
	elevated, err := identity.WithElevated(ctx, foreign, "fleet read under the admin claim")
	if err != nil {
		t.Fatalf("WithElevated: %v", err)
	}
	got, _ := identity.From(elevated)
	if got != foreign {
		t.Fatalf("working identity = %+v, want %+v", got, foreign)
	}
	if !identity.IsElevated(elevated) {
		t.Fatal("the elevation marker is absent")
	}
	reason, ok := identity.ElevationReason(elevated)
	if !ok || reason != "fleet read under the admin claim" {
		t.Fatalf("reason = (%q, %v), want the recorded reason", reason, ok)
	}
	// The anchor is unmoved: the actor is still who the transport verified.
	anchor, _ := identity.FromVerified(elevated)
	if anchor != own {
		t.Fatalf("anchor = %+v, want it unmoved at %+v", anchor, own)
	}
}

// TestWithElevated_RefusesAnUnnameableCrossing — an elevation nobody can
// name is an elevation nobody can audit.
func TestWithElevated_RefusesAnUnnameableCrossing(t *testing.T) {
	t.Parallel()
	ctx, err := identity.WithVerified(context.Background(), own)
	if err != nil {
		t.Fatalf("WithVerified: %v", err)
	}
	if _, err := identity.WithElevated(ctx, foreign, ""); !errors.Is(err, identity.ErrElevationReasonRequired) {
		t.Fatalf("err = %v, want ErrElevationReasonRequired", err)
	}
}

// TestWithElevated_RefusesAnIncompleteTarget — identity is mandatory on
// the far side of a crossing too.
func TestWithElevated_RefusesAnIncompleteTarget(t *testing.T) {
	t.Parallel()
	ctx, err := identity.WithVerified(context.Background(), own)
	if err != nil {
		t.Fatalf("WithVerified: %v", err)
	}
	if _, err := identity.WithElevated(ctx, identity.Identity{TenantID: "t2"}, "why"); !errors.Is(err, identity.ErrIdentityIncomplete) {
		t.Fatalf("err = %v, want ErrIdentityIncomplete", err)
	}
}

// TestWith_InsideAnAuditedCrossingReScopesFreely — work spawned under a
// recorded crossing re-scopes within the elevated tenant without a
// second gate.
func TestWith_InsideAnAuditedCrossingReScopesFreely(t *testing.T) {
	t.Parallel()
	ctx, err := identity.WithVerified(context.Background(), own)
	if err != nil {
		t.Fatalf("WithVerified: %v", err)
	}
	elevated, err := identity.WithElevated(ctx, foreign, "fleet read under the admin claim")
	if err != nil {
		t.Fatalf("WithElevated: %v", err)
	}
	inner := identity.Identity{TenantID: "t2", UserID: "u7", SessionID: "s7"}
	scoped, err := identity.With(elevated, inner)
	if err != nil {
		t.Fatalf("re-scoping inside an audited crossing: %v", err)
	}
	got, _ := identity.From(scoped)
	if got != inner {
		t.Fatalf("working identity = %+v, want %+v", got, inner)
	}
}

// TestVerified_ConcurrentReuse — the context helpers hold no shared
// state, so N concurrent requests each keep their own anchor.
func TestVerified_ConcurrentReuse(t *testing.T) {
	const n = 200
	done := make(chan error, n)
	for i := range n {
		go func(idx int) {
			id := identity.Identity{
				TenantID:  "tenant-" + string(rune('a'+idx%26)),
				UserID:    "user",
				SessionID: "session",
			}
			ctx, err := identity.WithVerified(context.Background(), id)
			if err != nil {
				done <- err
				return
			}
			if got, _ := identity.FromVerified(ctx); got != id {
				done <- errors.New("anchor bled between concurrent requests")
				return
			}
			if _, err := identity.With(ctx, identity.Identity{
				TenantID: id.TenantID, UserID: "other", SessionID: "other",
			}); err != nil {
				done <- err
				return
			}
			done <- nil
		}(i)
	}
	for range n {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

// TestWithElevated_AuthorizesOneNamedTenantOnly — the marker an audited
// crossing leaves is SCOPED to the tenant it authorized. Authorizing a
// move to T2 must not quietly authorize a later move to T3: the second
// crossing is a second decision, and it meets the same closed door.
func TestWithElevated_AuthorizesOneNamedTenantOnly(t *testing.T) {
	t.Parallel()
	ctx, err := identity.WithVerified(context.Background(), own)
	if err != nil {
		t.Fatalf("WithVerified: %v", err)
	}
	elevated, err := identity.WithElevated(ctx, foreign, "fleet read under the admin claim")
	if err != nil {
		t.Fatalf("WithElevated: %v", err)
	}

	// Re-scoping WITHIN the authorized tenant is permitted.
	inTenant := identity.Identity{TenantID: foreign.TenantID, UserID: "u9", SessionID: "s9"}
	if _, err := identity.With(elevated, inTenant); err != nil {
		t.Errorf("re-scoping within the authorized tenant: %v", err)
	}

	// A move to a THIRD tenant is refused, exactly as it would be without
	// any crossing in force.
	third := identity.Identity{TenantID: "t3", UserID: "u1", SessionID: "s1"}
	if _, err := identity.With(elevated, third); !errors.Is(err, identity.ErrTenantWidening) {
		t.Fatalf("a third tenant under an existing crossing: err = %v, want ErrTenantWidening", err)
	}

	// And the marker reports WHICH tenant it authorized, so a gate running
	// behind another gate can tell "already granted for this" from
	// "already granted for something else".
	got, ok := identity.ElevatedTenant(elevated)
	if !ok || got != foreign.TenantID {
		t.Fatalf("ElevatedTenant = (%q, %v), want (%q, true)", got, ok, foreign.TenantID)
	}
}

// TestElevatedTenant_AbsentOnAnUncrossedRequest — the marker reads as
// absent when no crossing happened, so a gate cannot mistake an ordinary
// request for an authorized one.
func TestElevatedTenant_AbsentOnAnUncrossedRequest(t *testing.T) {
	t.Parallel()
	ctx, err := identity.WithVerified(context.Background(), own)
	if err != nil {
		t.Fatalf("WithVerified: %v", err)
	}
	if _, ok := identity.ElevatedTenant(ctx); ok {
		t.Error("an uncrossed request reports an authorized tenant")
	}
	if identity.IsElevated(ctx) {
		t.Error("an uncrossed request reports itself elevated")
	}
}

// TestWithRun_CarriesTheSameTenantRule — the quadruple is read for
// scoping in its own right, so attaching a run must not become the seam
// that moves the isolation boundary sideways.
func TestWithRun_CarriesTheSameTenantRule(t *testing.T) {
	t.Parallel()
	ctx, err := identity.WithVerified(context.Background(), own)
	if err != nil {
		t.Fatalf("WithVerified: %v", err)
	}

	// Within the verified tenant: ordinary.
	if _, err := identity.WithRun(ctx, sibling, "run-1"); err != nil {
		t.Errorf("WithRun within the verified tenant: %v", err)
	}
	// Across it, with no crossing in force: refused.
	if _, err := identity.WithRun(ctx, foreign, "run-1"); !errors.Is(err, identity.ErrTenantWidening) {
		t.Fatalf("WithRun across the tenant boundary: err = %v, want ErrTenantWidening", err)
	}
	// Across it, under an audited crossing to that tenant: permitted.
	elevated, err := identity.WithElevated(ctx, foreign, "authorized read")
	if err != nil {
		t.Fatalf("WithElevated: %v", err)
	}
	if _, err := identity.WithRun(elevated, foreign, "run-1"); err != nil {
		t.Errorf("WithRun under an audited crossing to that tenant: %v", err)
	}
	// An unanchored ctx is unrestricted, as with With.
	if _, err := identity.WithRun(context.Background(), foreign, "run-1"); err != nil {
		t.Errorf("WithRun on an unanchored context: %v", err)
	}
}
