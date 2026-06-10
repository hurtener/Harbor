package approval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	patternsAudit "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	agentregistry "github.com/hurtener/Harbor/internal/runtime/registry"
	"github.com/hurtener/Harbor/internal/tools"
)

// authzPending is the PendingInfo fixture the matrix tests use.
var authzPending = PendingInfo{
	Tool:     "demo",
	Token:    pauseresume.Token("tok-1"),
	Identity: identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"},
	Tags:     []string{"sensitive"},
}

// TestIdentityAuthorizer_Matrix pins the Phase 111f (D-203) default
// authorizer's decision table: originating identity passes; the
// control-scope claim passes (any identity); everything else is
// rejected with ErrResolveForbidden.
func TestIdentityAuthorizer_Matrix(t *testing.T) {
	a := NewIdentityAuthorizer()
	owner := authzPending.Identity
	foreign := identity.Identity{TenantID: "t2", UserID: "u2", SessionID: "s2"}

	ownCtx, err := identity.With(context.Background(), owner)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	foreignCtx, err := identity.With(context.Background(), foreign)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}

	cases := []struct {
		name string
		ctx  context.Context
		want error // nil = authorized
	}{
		{"originating identity passes", ownCtx, nil},
		{"foreign identity fails", foreignCtx, ErrResolveForbidden},
		{"no identity fails", context.Background(), ErrResolveForbidden},
		{"control scope passes (own identity)", agentregistry.WithControlScope(ownCtx), nil},
		{"control scope passes (foreign identity)", agentregistry.WithControlScope(foreignCtx), nil},
		{"control scope passes (no identity)", agentregistry.WithControlScope(context.Background()), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := a.AuthorizeResolve(tc.ctx, authzPending)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("AuthorizeResolve: got %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("AuthorizeResolve: got %v, want %v", err, tc.want)
			}
		})
	}
}

// TestResolveApproval_MixedAuthorization_ExactlyOnce is the Phase
// 111f concurrency gate: N≥100 concurrent ResolveApproval attempts —
// a mix of authorized (originating identity / control scope) and
// unauthorized (foreign identity) resolvers — race against ONE gate
// and one pending approval under -race. Exactly one resolution lands;
// every unauthorized attempt fails with ErrResolveForbidden; every
// losing authorized attempt fails with ErrApprovalNotFound (the
// already-resolved answer).
func TestResolveApproval_MixedAuthorization_ExactlyOnce(t *testing.T) {
	red := patternsAudit.New()
	bus := mkTestBus(t, red)
	coord := pauseresume.New()
	g, err := NewApprovalGate(GateDeps{
		Policy:      AlwaysDenyPolicy{},
		Coordinator: coord,
		Bus:         bus,
		Redactor:    red,
		Authorizer:  NewIdentityAuthorizer(),
	})
	if err != nil {
		t.Fatalf("NewApprovalGate: %v", err)
	}
	t.Cleanup(func() { _ = g.Close(context.Background()) })

	owner := identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}
	requestedSub, cancelReq := subscribeTo(t, bus, owner, EventTypeToolApprovalRequested)
	defer cancelReq()

	runCtx, err := identity.With(context.Background(), owner)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	resCh := make(chan error, 1)
	go func() {
		_, rerr := g.RunGuarded(runCtx, &ApprovalRequest{
			Tool:     tools.Tool{Name: "demo"},
			Args:     json.RawMessage(`{}`),
			Identity: owner,
		})
		resCh <- rerr
	}()
	ev := waitEvent(t, requestedSub)
	payload, _ := ev.Payload.(ToolApprovalRequestedPayload)
	token := pauseresume.Token(payload.PauseToken)

	const n = 120
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var ctx context.Context
			switch i % 3 {
			case 0: // authorized: originating identity
				ctx, _ = identity.With(context.Background(), owner)
			case 1: // authorized: control scope (matching identity so
				// the Coordinator's identity check also passes)
				base, _ := identity.With(context.Background(), owner)
				ctx = agentregistry.WithControlScope(base)
			default: // unauthorized: foreign identity, no claim
				ctx, _ = identity.With(context.Background(), identity.Identity{
					TenantID:  fmt.Sprintf("tx%d", i),
					UserID:    fmt.Sprintf("ux%d", i),
					SessionID: fmt.Sprintf("sx%d", i),
				})
			}
			errs[i] = g.ResolveApproval(ctx, token, DecisionApprove, "")
		}(i)
	}
	wg.Wait()

	winners := 0
	for i, rerr := range errs {
		switch {
		case rerr == nil:
			winners++
			if i%3 == 2 {
				t.Errorf("unauthorized resolver %d won the resolution", i)
			}
		case errors.Is(rerr, ErrResolveForbidden):
			if i%3 != 2 {
				t.Errorf("authorized resolver %d got ErrResolveForbidden: %v", i, rerr)
			}
		case errors.Is(rerr, ErrApprovalNotFound):
			// A losing authorized resolver (or an unauthorized one that
			// raced past the resolution) — acceptable.
		default:
			t.Errorf("resolver %d: unexpected error %v", i, rerr)
		}
	}
	if winners != 1 {
		t.Fatalf("exactly-once violated: %d resolutions landed", winners)
	}
	if rerr := <-resCh; rerr != nil {
		t.Fatalf("RunGuarded after approve: %v", rerr)
	}
	if g.pendingLen() != 0 {
		t.Fatalf("pendingLen: %d want 0", g.pendingLen())
	}
}

// TestResolveApproval_CancelledCtx_FailsClosed covers the resolve-side
// ctx pre-check: a cancelled resolver ctx is rejected before any state
// is touched.
func TestResolveApproval_CancelledCtx_FailsClosed(t *testing.T) {
	g, _ := mkGate(t, AlwaysDenyPolicy{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := g.ResolveApproval(ctx, pauseresume.Token("tok"), DecisionApprove, "")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled-ctx Resolve: got %v, want context.Canceled", err)
	}
}

// TestGate_Close_DropsParkedEntries covers Close's pending-map drain:
// a gate closed while an approval is parked drops the entry; the
// parked RunGuarded caller unblocks via its own ctx.
func TestGate_Close_DropsParkedEntries(t *testing.T) {
	g, bus := mkGate(t, AlwaysDenyPolicy{})
	requestedSub, cancelReq := subscribeTo(t, bus, testID, EventTypeToolApprovalRequested)
	defer cancelReq()

	ctx, cancel := context.WithCancel(mkPlainCtx(t, testID))
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, rerr := g.RunGuarded(ctx, &ApprovalRequest{
			Tool:     tools.Tool{Name: "x"},
			Args:     json.RawMessage(`{}`),
			Identity: testID,
		})
		done <- rerr
	}()
	waitEvent(t, requestedSub)
	if g.pendingLen() != 1 {
		t.Fatalf("pendingLen: %d want 1", g.pendingLen())
	}
	if err := g.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if g.pendingLen() != 0 {
		t.Fatalf("pendingLen after Close: %d want 0", g.pendingLen())
	}
	cancel()
	if rerr := <-done; !errors.Is(rerr, ErrApprovalCancelled) {
		t.Fatalf("parked RunGuarded after Close+cancel: got %v, want ErrApprovalCancelled", rerr)
	}
}
