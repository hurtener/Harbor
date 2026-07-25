package bodyscope_test

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/bodyscope"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

var verified = identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}

// verifiedCtx is a context whose identity has been established, the shape
// a transport hands the gate.
func verifiedCtx(t *testing.T, scopes ...auth.Scope) context.Context {
	t.Helper()
	ctx, err := identity.WithVerified(context.Background(), verified)
	if err != nil {
		t.Fatalf("seat verified identity: %v", err)
	}
	if len(scopes) > 0 {
		ctx = auth.WithScopes(ctx, scopes)
	}
	return ctx
}

// countingAuditor records each granted crossing. Safe for concurrent use
// so the shared-instance stress can share one.
type countingAuditor struct {
	mu      sync.Mutex
	count   atomic.Int64
	records []bodyscope.Elevation
}

func (a *countingAuditor) AdminScopeUsed(_ context.Context, e bodyscope.Elevation) {
	a.count.Add(1)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.records = append(a.records, e)
}

func (a *countingAuditor) snapshot() []bodyscope.Elevation {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]bodyscope.Elevation, len(a.records))
	copy(out, a.records)
	return out
}

// TestReconcile_Contract is the table-driven pin of the whole contract:
// what a body may name, what the verified identity supplies, and what a
// crossing costs.
func TestReconcile_Contract(t *testing.T) {
	t.Parallel()

	scope := func(tenant, user, session string) types.IdentityScope {
		return types.IdentityScope{Tenant: tenant, User: user, Session: session}
	}

	for _, tc := range []struct {
		name string
		// surface names the registry row under test.
		surface bodyscope.Surface
		// body is the caller-supplied identity scope.
		body types.IdentityScope
		// scopes are the claims on the verified scope set.
		scopes []auth.Scope
		// established reports whether the ctx carries a verified identity.
		established bool
		// wantCode is the Protocol code, empty for an accepted request.
		wantCode protoerrors.Code
		// wantBody is the reconciled triple on an accepted request.
		wantBody types.IdentityScope
		// wantElevated reports whether the request carries an audited crossing.
		wantElevated bool
		// wantAudits is the number of crossings recorded.
		wantAudits int64
	}{
		{
			name:        "empty triple is backfilled from the verified identity",
			surface:     bodyscope.SurfaceApps,
			body:        types.IdentityScope{},
			established: true,
			wantBody:    scope("t1", "u1", "s1"),
		},
		{
			name:        "matching triple passes through untouched",
			surface:     bodyscope.SurfaceApps,
			body:        scope("t1", "u1", "s1"),
			established: true,
			wantBody:    scope("t1", "u1", "s1"),
		},
		{
			name:        "user mismatch is refused",
			surface:     bodyscope.SurfaceApps,
			body:        scope("t1", "u-other", "s1"),
			established: true,
			wantCode:    protoerrors.CodeIdentityRequired,
		},
		{
			name:        "session mismatch is refused",
			surface:     bodyscope.SurfaceApps,
			body:        scope("t1", "u1", "s-other"),
			established: true,
			wantCode:    protoerrors.CodeIdentityRequired,
		},
		{
			name:        "tenant mismatch on a pinned surface is refused whatever the claim",
			surface:     bodyscope.SurfaceApps,
			body:        scope("t-other", "u1", "s1"),
			scopes:      []auth.Scope{auth.ScopeAdmin},
			established: true,
			wantCode:    protoerrors.CodeIdentityRequired,
		},
		{
			name:        "tenant mismatch without a claim is refused on a tenant-permissive surface",
			surface:     bodyscope.SurfacePosture,
			body:        scope("t-other", "u1", "s1"),
			established: true,
			wantCode:    protoerrors.CodeScopeMismatch,
		},
		{
			name:         "tenant mismatch under the admin claim is granted and recorded",
			surface:      bodyscope.SurfacePosture,
			body:         scope("t-other", "u1", "s1"),
			scopes:       []auth.Scope{auth.ScopeAdmin},
			established:  true,
			wantBody:     scope("t-other", "u1", "s1"),
			wantElevated: true,
			wantAudits:   1,
		},
		{
			name:         "tenant mismatch under the fleet claim is granted and recorded",
			surface:      bodyscope.SurfacePosture,
			body:         scope("t-other", "u1", "s1"),
			scopes:       []auth.Scope{auth.ScopeConsoleFleet},
			established:  true,
			wantBody:     scope("t-other", "u1", "s1"),
			wantElevated: true,
			wantAudits:   1,
		},
		{
			name:        "a crossing does not carry the user with it",
			surface:     bodyscope.SurfacePosture,
			body:        scope("t-other", "u-other", "s1"),
			scopes:      []auth.Scope{auth.ScopeAdmin},
			established: true,
			wantCode:    protoerrors.CodeIdentityRequired,
		},
		{
			name:        "an empty component is a wildcard where the surface reads one",
			surface:     bodyscope.SurfaceArtifacts,
			body:        scope("t1", "", ""),
			established: true,
			wantBody:    scope("t1", "", ""),
		},
		{
			name:        "no established identity fails closed",
			surface:     bodyscope.SurfaceApps,
			body:        scope("t1", "u1", "s1"),
			established: false,
			wantCode:    protoerrors.CodeIdentityRequired,
		},
		{
			name:        "no established identity fails closed even under the admin claim",
			surface:     bodyscope.SurfacePosture,
			body:        scope("t-other", "u1", "s1"),
			scopes:      []auth.Scope{auth.ScopeAdmin},
			established: false,
			wantCode:    protoerrors.CodeIdentityRequired,
		},
		{
			name:        "an unregistered surface is a loud construction failure",
			surface:     bodyscope.Surface("not-a-surface"),
			body:        scope("t1", "u1", "s1"),
			established: true,
			wantCode:    protoerrors.CodeRuntimeError,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var ctx context.Context
			if tc.established {
				ctx = verifiedCtx(t, tc.scopes...)
			} else {
				ctx = context.Background()
				if len(tc.scopes) > 0 {
					ctx = auth.WithScopes(ctx, tc.scopes)
				}
			}
			aud := &countingAuditor{}
			body := tc.body
			out, perr := bodyscope.Reconcile(ctx, bodyscope.ForIdentityScope(&body), tc.surface, aud)

			if tc.wantCode != "" {
				if perr == nil {
					t.Fatalf("want code %q, got a reconciled request", tc.wantCode)
				}
				if perr.Code != tc.wantCode {
					t.Fatalf("code = %q, want %q (%s)", perr.Code, tc.wantCode, perr.Message)
				}
				if got := aud.count.Load(); got != 0 {
					t.Errorf("a refused request recorded %d crossings, want 0", got)
				}
				return
			}
			if perr != nil {
				t.Fatalf("unexpected refusal: %v", perr)
			}
			if body != tc.wantBody {
				t.Errorf("reconciled body = %+v, want %+v", body, tc.wantBody)
			}
			if got := bodyscope.Elevated(out); got != tc.wantElevated {
				t.Errorf("elevated = %v, want %v", got, tc.wantElevated)
			}
			if got := aud.count.Load(); got != tc.wantAudits {
				t.Errorf("recorded %d crossings, want %d", got, tc.wantAudits)
			}
			if tc.wantElevated {
				rec := aud.snapshot()[0]
				if rec.Actor != verified {
					t.Errorf("audit actor = %+v, want the verified identity %+v", rec.Actor, verified)
				}
				if rec.Target.TenantID != tc.wantBody.Tenant {
					t.Errorf("audit target tenant = %q, want %q", rec.Target.TenantID, tc.wantBody.Tenant)
				}
				if rec.Reason == "" {
					t.Error("audit record carries no reason")
				}
			}
		})
	}
}

// TestReconcile_ASurfaceNamesTheClaimsItAccepts — the fleet-observation
// claim is a READ entitlement. A surface whose crossing is strictly an
// administrative act names `admin` alone, and a read-only fleet token
// cannot take it. The two surfaces that do so are pinned by name so a
// later edit widening either is visible in the diff.
func TestReconcile_ASurfaceNamesTheClaimsItAccepts(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		surface       bodyscope.Surface
		fleetSuffices bool
		wantCode      protoerrors.Code
	}{
		{bodyscope.SurfacePosture, true, ""},
		{bodyscope.SurfaceArtifacts, true, ""},
		{bodyscope.SurfaceTopology, false, protoerrors.CodeScopeMismatch},
		{bodyscope.SurfaceStateHistory, false, protoerrors.CodeNotFound},
	} {
		t.Run(string(tc.surface), func(t *testing.T) {
			t.Parallel()
			body := types.IdentityScope{Tenant: "t-other", User: "u1", Session: "s1"}
			_, perr := bodyscope.Reconcile(verifiedCtx(t, auth.ScopeConsoleFleet),
				bodyscope.ForIdentityScope(&body), tc.surface, &countingAuditor{})
			if tc.fleetSuffices {
				if perr != nil {
					t.Fatalf("the fleet claim was refused on %q: %v", tc.surface, perr)
				}
				return
			}
			if perr == nil {
				t.Fatalf("the fleet claim took an administrative crossing on %q", tc.surface)
			}
			if perr.Code != tc.wantCode {
				t.Fatalf("code = %q, want %q", perr.Code, tc.wantCode)
			}
			// The admin claim still takes it.
			body = types.IdentityScope{Tenant: "t-other", User: "u1", Session: "s1"}
			if _, perr := bodyscope.Reconcile(verifiedCtx(t, auth.ScopeAdmin),
				bodyscope.ForIdentityScope(&body), tc.surface, &countingAuditor{}); perr != nil {
				t.Fatalf("the admin claim was refused on %q: %v", tc.surface, perr)
			}
		})
	}
}

// TestReconcile_TenantPermissiveSurfaceRefusesWithoutAnAuditSink — the
// linkage that used to live in a code comment. A surface whose policy can
// grant a crossing cannot be reconciled without somewhere to record it.
func TestReconcile_TenantPermissiveSurfaceRefusesWithoutAnAuditSink(t *testing.T) {
	t.Parallel()
	body := types.IdentityScope{Tenant: "t1", User: "u1", Session: "s1"}
	_, perr := bodyscope.Reconcile(verifiedCtx(t), bodyscope.ForIdentityScope(&body),
		bodyscope.SurfacePosture, nil)
	if perr == nil {
		t.Fatal("a tenant-permissive surface with no audit sink was reconciled; want a refusal")
	}
	if perr.Code != protoerrors.CodeRuntimeError {
		t.Fatalf("code = %q, want %q", perr.Code, protoerrors.CodeRuntimeError)
	}
}

// TestReconcile_PinnedSurfaceNeedsNoAuditSink — the converse: a surface
// that can never grant a crossing has nothing to record, so a nil sink is
// the correct call.
func TestReconcile_PinnedSurfaceNeedsNoAuditSink(t *testing.T) {
	t.Parallel()
	body := types.IdentityScope{}
	if _, perr := bodyscope.Reconcile(verifiedCtx(t), bodyscope.ForIdentityScope(&body),
		bodyscope.SurfaceApps, nil); perr != nil {
		t.Fatalf("unexpected refusal: %v", perr)
	}
}

// TestReconcile_ArtifactScopeAdapter — the artifacts cluster carries its
// triple in a different wire shape, and the gate reads both through the
// same handle. The Task component is outside the isolation triple and is
// left where the caller put it.
func TestReconcile_ArtifactScopeAdapter(t *testing.T) {
	t.Parallel()
	scope := types.ArtifactScope{Task: "task-7"}
	aud := &countingAuditor{}
	if _, perr := bodyscope.Reconcile(verifiedCtx(t), bodyscope.ForArtifactScope(&scope),
		bodyscope.SurfaceArtifacts, aud); perr != nil {
		t.Fatalf("unexpected refusal: %v", perr)
	}
	if scope.Tenant != "t1" || scope.User != "u1" || scope.Session != "s1" {
		t.Errorf("backfilled scope = %+v, want the verified triple", scope)
	}
	if scope.Task != "task-7" {
		t.Errorf("task = %q, want it untouched", scope.Task)
	}
}

// TestReconcile_SecondGateDoesNotDoubleRecordACrossing — a surface
// re-running the gate behind the transport that fronted it reads the
// crossing already on ctx and does not record it twice.
func TestReconcile_SecondGateDoesNotDoubleRecordACrossing(t *testing.T) {
	t.Parallel()
	aud := &countingAuditor{}
	body := types.IdentityScope{Tenant: "t-other", User: "u1", Session: "s1"}
	ctx, perr := bodyscope.Reconcile(verifiedCtx(t, auth.ScopeAdmin),
		bodyscope.ForIdentityScope(&body), bodyscope.SurfacePosture, aud)
	if perr != nil {
		t.Fatalf("transport gate refused: %v", perr)
	}
	if _, perr = bodyscope.Reconcile(ctx, bodyscope.ForIdentityScope(&body),
		bodyscope.SurfacePosture, aud); perr != nil {
		t.Fatalf("surface gate refused: %v", perr)
	}
	if got := aud.count.Load(); got != 1 {
		t.Fatalf("recorded %d crossings across two gates, want 1", got)
	}
}

// TestReconcile_ConcurrentReuse drives N concurrent requests through the
// one shared registry and the one shared audit sink, asserting no data
// race, no cross-request bleed of the reconciled triple, an exact count
// of granted crossings, and no goroutine left behind.
func TestReconcile_ConcurrentReuse(t *testing.T) {
	const n = 200
	baseline := runtime.NumGoroutine()
	aud := &countingAuditor{}

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			own := identity.Identity{
				TenantID:  fmt.Sprintf("tenant-%03d", idx),
				UserID:    fmt.Sprintf("user-%03d", idx),
				SessionID: fmt.Sprintf("session-%03d", idx),
			}
			ctx, err := identity.WithVerified(context.Background(), own)
			if err != nil {
				errs[idx] = err
				return
			}
			// Every fourth request crosses a tenant under the admin claim.
			crossing := idx%4 == 0
			body := types.IdentityScope{}
			if crossing {
				ctx = auth.WithScopes(ctx, []auth.Scope{auth.ScopeAdmin})
				body = types.IdentityScope{
					Tenant: "fleet-target", User: own.UserID, Session: own.SessionID,
				}
			}
			out, perr := bodyscope.Reconcile(ctx, bodyscope.ForIdentityScope(&body),
				bodyscope.SurfacePosture, aud)
			if perr != nil {
				errs[idx] = fmt.Errorf("request %d refused: %w", idx, perr)
				return
			}
			wantTenant := own.TenantID
			if crossing {
				wantTenant = "fleet-target"
			}
			if body.Tenant != wantTenant || body.User != own.UserID || body.Session != own.SessionID {
				errs[idx] = fmt.Errorf("request %d reconciled to %+v, want tenant %q and its own user/session",
					idx, body, wantTenant)
				return
			}
			if got := bodyscope.Elevated(out); got != crossing {
				errs[idx] = fmt.Errorf("request %d elevated = %v, want %v", idx, got, crossing)
			}
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	if got, want := aud.count.Load(), int64(n/4); got != want {
		t.Errorf("recorded %d crossings, want %d", got, want)
	}
	for _, rec := range aud.snapshot() {
		if rec.Target.TenantID != "fleet-target" {
			t.Errorf("audit record target tenant = %q, want fleet-target", rec.Target.TenantID)
		}
		if rec.Actor.TenantID == "fleet-target" {
			t.Error("audit record actor is the target tenant; the actor must stay the verified caller")
		}
	}

	// The gate starts no goroutines of its own; the pool returns to
	// baseline once the drivers are done.
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline+2 && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if leaked := runtime.NumGoroutine() - baseline; leaked > 2 {
		t.Errorf("goroutine count grew by %d after the stress; want it back at baseline", leaked)
	}
}

// TestReconcile_CancellationDoesNotCrossRequests — one request's
// cancelled context does not disturb a sibling's reconciliation.
func TestReconcile_CancellationDoesNotCrossRequests(t *testing.T) {
	t.Parallel()
	aud := &countingAuditor{}

	cancelledCtx, cancel := context.WithCancel(verifiedCtx(t))
	cancel()
	liveCtx := verifiedCtx(t)

	bodyA := types.IdentityScope{}
	if _, perr := bodyscope.Reconcile(cancelledCtx, bodyscope.ForIdentityScope(&bodyA),
		bodyscope.SurfacePosture, aud); perr != nil {
		t.Fatalf("cancelled request refused: %v", perr)
	}
	bodyB := types.IdentityScope{}
	if _, perr := bodyscope.Reconcile(liveCtx, bodyscope.ForIdentityScope(&bodyB),
		bodyscope.SurfacePosture, aud); perr != nil {
		t.Fatalf("live request refused: %v", perr)
	}
	if bodyB.Tenant != verified.TenantID {
		t.Errorf("live request reconciled to %+v, want the verified triple", bodyB)
	}
}

// TestReconcile_ArtifactsPostureMatrix — the artifacts cluster holds four
// postures, and the transport must grant exactly what the surface will
// honour. A crossing granted here and refused one layer down leaves an
// `audit.admin_scope_used` record for something that never happened, which
// makes the audit trail describe attempts rather than acts.
//
// The emit count is asserted alongside the outcome: a refusal must record
// NOTHING, and only an actually-granted crossing records.
func TestReconcile_ArtifactsPostureMatrix(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		surface bodyscope.Surface
		claim   auth.Scope
		// granted reports whether the crossing is admitted.
		granted bool
		// wantCode is the refusal code when granted is false.
		wantCode protoerrors.Code
	}{
		// Enumerating another tenant is a read: either admin-tier claim.
		{"list under admin", bodyscope.SurfaceArtifacts, auth.ScopeAdmin, true, ""},
		{"list under fleet", bodyscope.SurfaceArtifacts, auth.ScopeConsoleFleet, true, ""},

		// Seeding another tenant is a write: the administrative claim alone.
		{"put under admin", bodyscope.SurfaceArtifactsPut, auth.ScopeAdmin, true, ""},
		{"put under fleet", bodyscope.SurfaceArtifactsPut, auth.ScopeConsoleFleet, false, protoerrors.CodeScopeMismatch},

		// Destroying another tenant's artifact is a write too — the same
		// reasoning as put, which the surface has always enforced.
		{"delete under admin", bodyscope.SurfaceArtifactsDelete, auth.ScopeAdmin, true, ""},
		{"delete under fleet", bodyscope.SurfaceArtifactsDelete, auth.ScopeConsoleFleet, false, protoerrors.CodeScopeMismatch},

		// A presigned reference crosses for nobody.
		{"get_ref under admin", bodyscope.SurfaceArtifactsRef, auth.ScopeAdmin, false, protoerrors.CodeScopeMismatch},
		{"get_ref under fleet", bodyscope.SurfaceArtifactsRef, auth.ScopeConsoleFleet, false, protoerrors.CodeScopeMismatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			aud := &countingAuditor{}
			scope := types.ArtifactScope{Tenant: "t-other", User: "u1", Session: "s1"}
			_, perr := bodyscope.Reconcile(verifiedCtx(t, tc.claim),
				bodyscope.ForArtifactScope(&scope), tc.surface, aud)

			if tc.granted {
				if perr != nil {
					t.Fatalf("crossing refused: %v", perr)
				}
				if got := aud.count.Load(); got != 1 {
					t.Errorf("a granted crossing recorded %d times, want exactly 1", got)
				}
				return
			}
			if perr == nil {
				t.Fatalf("crossing granted; want refusal %q", tc.wantCode)
			}
			if perr.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", perr.Code, tc.wantCode)
			}
			if got := aud.count.Load(); got != 0 {
				t.Errorf("a REFUSED crossing recorded %d times, want 0 — the audit trail must not describe crossings that did not happen", got)
			}
		})
	}
}
