package control_test

// artifacts_body_scope_test.go — pins the ARTIFACTS transport adapter's
// per-method body-identity SURFACE SELECTION.
//
// `reconcileArtifactsIdentity` maps each artifacts method onto the
// bodyscope registry row whose posture governs it, and the cluster
// deliberately holds more than one posture across its five methods:
//
//	artifacts.list    → SurfaceArtifacts        (tenant AdminScoped)
//	artifacts.put     → SurfaceArtifactsPut     (tenant AdminScoped, admin only)
//	artifacts.delete  → SurfaceArtifactsDelete  (tenant AdminScoped, admin only)
//	artifacts.get     → SurfaceArtifactsRef     (tenant Pinned — flat refusal)
//	artifacts.get_ref → SurfaceArtifactsRef     (tenant Pinned — flat refusal)
//
// Before this file, DROPPING a `case` arm from that switch broke no test
// at all: the method silently fell through to the default
// (SurfaceArtifacts, AdminScoped), which QUIETLY WIDENS a content read
// into an admin-elevatable one. The surface's own tenant check still
// refused it, so a live smoke stayed green — the two layers covered for
// each other and neither was individually pinned. That is the inert-guard
// shape AGENTS.md §4.2 item 5 names, found by a mutation sweep.
//
// The discriminating probe is a cross-tenant body under the ADMIN claim:
// the admin-scoped rows GRANT the crossing at the transport, and the two
// content rows refuse it flat with CodeScopeMismatch regardless. So a
// content method that lost its arm stops answering 403 and this test
// fails.

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/transports/control"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

// recordingArtifacts is a deterministic ArtifactsSurface that records
// whether the transport let a request through at all. Every assertion
// here is about the TRANSPORT's gate, so the surface below it must never
// be the thing that refuses.
type recordingArtifacts struct {
	calls int
}

func (s *recordingArtifacts) Dispatch(_ context.Context, method methods.Method, _ any) (any, error) {
	s.calls++
	switch method {
	case methods.MethodArtifactsList:
		return &types.ArtifactsListResponse{ProtocolVersion: types.ProtocolVersion}, nil
	case methods.MethodArtifactsPut:
		return &types.ArtifactsPutResponse{ProtocolVersion: types.ProtocolVersion}, nil
	case methods.MethodArtifactsGet:
		return &types.ArtifactsGetResponse{ProtocolVersion: types.ProtocolVersion}, nil
	case methods.MethodArtifactsGetRef:
		return &types.ArtifactsGetRefResponse{ProtocolVersion: types.ProtocolVersion}, nil
	default:
		return &types.ArtifactsDeleteResponse{ProtocolVersion: types.ProtocolVersion}, nil
	}
}

// withAdminIdentity seats the verified identity AND the admin claim, so
// the admin-scoped rows can grant a crossing and the pinned rows can be
// shown to refuse one anyway.
func withAdminIdentity(h http.Handler, id identity.Identity) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, err := identity.WithVerified(r.Context(), id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		ctx = auth.WithScopes(ctx, []auth.Scope{auth.ScopeAdmin, auth.ScopeConsoleFleet})
		h.ServeHTTP(w, r.WithContext(ctx))
	})
}

// artifactScopeBody renders a body carrying exactly the given artifact
// scope triple plus an id, which the two content reads require.
func artifactScopeBody(tenant, user, session string) string {
	return `{"scope":{"tenant":"` + tenant + `","user":"` + user + `","session":"` + session +
		`"},"id":"art_deadbeefdead"}`
}

// TestArtifactsBodyScope_PerMethodSurfaceSelection pins every arm of the
// artifacts transport's method → bodyscope-row mapping.
func TestArtifactsBodyScope_PerMethodSurfaceSelection(t *testing.T) {
	verified := identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}

	for _, tc := range []struct {
		name   string
		method methods.Method
		// wantRefused is true for the rows whose tenant is Pinned: the
		// transport refuses a foreign tenant flat, with the row's own
		// declared deny code, EVEN under both admin-tier claims.
		wantRefused bool
	}{
		{"list is admin-elevatable", methods.MethodArtifactsList, false},
		{"put is admin-elevatable", methods.MethodArtifactsPut, false},
		{"delete is admin-elevatable", methods.MethodArtifactsDelete, false},
		{"get is a flat content read", methods.MethodArtifactsGet, true},
		{"get_ref is a flat content read", methods.MethodArtifactsGetRef, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			surf := &recordingArtifacts{}
			cs, cleanup := newTestSurface(t)
			t.Cleanup(cleanup)
			red := patterns.New()
			bus, err := eventsinmem.New(config.EventsConfig{
				MaxSubscribersPerSession: 4,
				SubscriberBufferSize:     16,
				IdleTimeout:              30 * time.Second,
				DropWindow:               time.Second,
			}, red)
			if err != nil {
				t.Fatalf("events inmem: %v", err)
			}
			t.Cleanup(func() { _ = bus.Close(context.Background()) })

			h, err := control.NewHandler(cs,
				control.WithArtifactsSurface(surf),
				control.WithEventBus(bus),
				control.WithRedactor(red),
			)
			if err != nil {
				t.Fatalf("NewHandler: %v", err)
			}
			mux := http.NewServeMux()
			mux.Handle(control.RoutePattern, withAdminIdentity(h, verified))

			// A body naming a FOREIGN tenant but the caller's own user and
			// session — the only foreign-tenant shape the gate can reach,
			// since user and session are pinned on every row here.
			status, perr := postMethod(t, mux, tc.method, artifactScopeBody("t-other", "u1", "s1"))

			if tc.wantRefused {
				if status == http.StatusOK {
					t.Fatalf("%s: the transport ADMITTED a cross-tenant content read under the admin claim — "+
						"the method has lost its flat body-scope row and fell through to the admin-elevatable default", tc.method)
				}
				if perr.Code != protoerrors.CodeScopeMismatch {
					t.Fatalf("%s: code = %q, want %q (the flat content row's declared deny code)",
						tc.method, perr.Code, protoerrors.CodeScopeMismatch)
				}
				if surf.calls != 0 {
					t.Fatalf("%s: the refused request still reached the surface (%d calls)", tc.method, surf.calls)
				}
				return
			}

			// The admin-scoped rows grant the crossing at the transport, so
			// the request reaches the surface. What is asserted is the
			// ABSENCE of the flat refusal — a row that had silently become
			// Pinned would show up here.
			if perr.Code == protoerrors.CodeScopeMismatch {
				t.Fatalf("%s: refused with scope_mismatch under the admin claim — "+
					"the method has lost its admin-elevatable body-scope row", tc.method)
			}
			if surf.calls != 1 {
				t.Fatalf("%s: surface calls = %d, want 1 (status=%d code=%q)",
					tc.method, surf.calls, status, perr.Code)
			}
		})
	}
}
