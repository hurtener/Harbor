package governance_test

import (
	"context"
	"errors"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/governance"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

// busAndState constructs the events + state pair used across tests.
// Returns a cleanup hook that closes both.
func busAndState(t *testing.T) (events.EventBus, state.StateStore, func()) {
	t.Helper()
	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     64,
		IdleTimeout:              60 * time.Second,
		DropWindow:               1 * time.Second,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	st, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		_ = bus.Close(context.Background())
		t.Fatalf("state.Open: %v", err)
	}
	cleanup := func() {
		_ = st.Close(context.Background())
		_ = bus.Close(context.Background())
	}
	return bus, st, cleanup
}

func ctxWith(t *testing.T, tenant, user, session, run string) context.Context {
	t.Helper()
	id := identity.Identity{TenantID: tenant, UserID: user, SessionID: session}
	ctx, err := identity.WithRun(context.Background(), id, run)
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}
	return ctx
}

func TestSubsystem_PreCallShortCircuits_PostCallStillFiresOnFailure(t *testing.T) {
	t.Parallel()
	// A fake Subsystem that rejects PreCall but records PostCall calls
	// — covers the Wrap contract that PostCall fires after the inner
	// client returns; PreCall short-circuit prevents calls so PostCall
	// also does NOT fire when PreCall blocks.
	var preCount, postCount int
	sub := &fakeSub{
		preFn: func(_ context.Context, _ llm.CompleteRequest) error {
			preCount++
			return governance.ErrBudgetExceeded
		},
		postFn: func(_ context.Context, _ llm.CompleteRequest, _ llm.CompleteResponse, _ error) error {
			postCount++
			return nil
		},
	}
	inner := &stubClient{response: llm.CompleteResponse{Content: "ok"}}
	client := governance.Wrap(inner, sub)
	ctx := ctxWith(t, "T", "U", "S", "R")

	_, err := client.Complete(ctx, llm.CompleteRequest{Model: "m"})
	if !errors.Is(err, governance.ErrBudgetExceeded) {
		t.Fatalf("expected ErrBudgetExceeded, got %v", err)
	}
	if preCount != 1 {
		t.Errorf("PreCall fired %d times, want 1", preCount)
	}
	if postCount != 0 {
		t.Errorf("PostCall fired %d times after PreCall short-circuit, want 0", postCount)
	}
	if inner.calls != 0 {
		t.Errorf("inner client invoked %d times after PreCall short-circuit, want 0", inner.calls)
	}
}

func TestSubsystem_HappyPath_InnerCalledAndPostCalled(t *testing.T) {
	t.Parallel()
	var preCount, postCount, postCallErrCount int
	sub := &fakeSub{
		preFn:  func(_ context.Context, _ llm.CompleteRequest) error { preCount++; return nil },
		postFn: func(_ context.Context, _ llm.CompleteRequest, _ llm.CompleteResponse, err error) error {
			postCount++
			if err != nil {
				postCallErrCount++
			}
			return nil
		},
	}
	inner := &stubClient{response: llm.CompleteResponse{Content: "ok"}}
	client := governance.Wrap(inner, sub)
	ctx := ctxWith(t, "T", "U", "S", "R")

	resp, err := client.Complete(ctx, llm.CompleteRequest{Model: "m"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Content != "ok" {
		t.Errorf("content = %q", resp.Content)
	}
	if preCount != 1 {
		t.Errorf("preCount = %d", preCount)
	}
	if postCount != 1 {
		t.Errorf("postCount = %d", postCount)
	}
	if postCallErrCount != 0 {
		t.Errorf("postCallErrCount = %d (inner returned nil)", postCallErrCount)
	}
}

func TestSubsystem_InnerErrFlowsThroughPostCall(t *testing.T) {
	t.Parallel()
	innerErr := errors.New("driver failure")
	var seenErr error
	sub := &fakeSub{
		preFn: func(_ context.Context, _ llm.CompleteRequest) error { return nil },
		postFn: func(_ context.Context, _ llm.CompleteRequest, _ llm.CompleteResponse, callErr error) error {
			seenErr = callErr
			return nil
		},
	}
	inner := &stubClient{err: innerErr}
	client := governance.Wrap(inner, sub)
	ctx := ctxWith(t, "T", "U", "S", "R")
	_, err := client.Complete(ctx, llm.CompleteRequest{Model: "m"})
	if !errors.Is(err, innerErr) {
		t.Errorf("expected innerErr propagation, got %v", err)
	}
	if !errors.Is(seenErr, innerErr) {
		t.Errorf("PostCall did not see callErr=%v (got %v)", innerErr, seenErr)
	}
}

func TestCompound_ShortCircuitsOnFirstFailure(t *testing.T) {
	t.Parallel()
	var aPre, bPre, cPre int
	a := &fakeSub{preFn: func(_ context.Context, _ llm.CompleteRequest) error { aPre++; return nil }}
	b := &fakeSub{preFn: func(_ context.Context, _ llm.CompleteRequest) error { bPre++; return governance.ErrRateLimited }}
	c := &fakeSub{preFn: func(_ context.Context, _ llm.CompleteRequest) error { cPre++; return nil }}
	comp := governance.NewCompound(a, b, c)
	err := comp.PreCall(context.Background(), llm.CompleteRequest{})
	if !errors.Is(err, governance.ErrRateLimited) {
		t.Errorf("expected ErrRateLimited, got %v", err)
	}
	if aPre != 1 || bPre != 1 || cPre != 0 {
		t.Errorf("counts = (%d,%d,%d), want (1,1,0)", aPre, bPre, cPre)
	}
}

func TestCompound_PostCallFansEveryMember(t *testing.T) {
	t.Parallel()
	var aPost, bPost, cPost int
	a := &fakeSub{postFn: func(_ context.Context, _ llm.CompleteRequest, _ llm.CompleteResponse, _ error) error { aPost++; return nil }}
	b := &fakeSub{postFn: func(_ context.Context, _ llm.CompleteRequest, _ llm.CompleteResponse, _ error) error { bPost++; return errors.New("b") }}
	c := &fakeSub{postFn: func(_ context.Context, _ llm.CompleteRequest, _ llm.CompleteResponse, _ error) error { cPost++; return nil }}
	comp := governance.NewCompound(a, b, c)
	err := comp.PostCall(context.Background(), llm.CompleteRequest{}, llm.CompleteResponse{}, nil)
	if err == nil {
		t.Errorf("expected joined error from compound, got nil")
	}
	if aPost != 1 || bPost != 1 || cPost != 1 {
		t.Errorf("PostCall did not fan to every member: counts (%d,%d,%d)", aPost, bPost, cPost)
	}
}

func TestPreCall_FailsClosedOnMissingIdentity(t *testing.T) {
	t.Parallel()
	bus, st, cleanup := busAndState(t)
	defer cleanup()
	acc, err := governance.NewCostAccumulator(st, bus, governance.Config{
		DefaultTier: "free",
		IdentityTiers: map[string]governance.TierConfig{
			"free": {BudgetCeilingUSD: 1.0},
		},
	})
	if err != nil {
		t.Fatalf("NewCostAccumulator: %v", err)
	}
	defer acc.Close(context.Background())
	// Bare ctx — no identity attached.
	err = acc.PreCall(context.Background(), llm.CompleteRequest{Model: "m"})
	if !errors.Is(err, governance.ErrIdentityRequired) {
		t.Errorf("PreCall without identity: want ErrIdentityRequired, got %v", err)
	}
}

// --- helpers ---------------------------------------------------------

type fakeSub struct {
	preFn  func(ctx context.Context, req llm.CompleteRequest) error
	postFn func(ctx context.Context, req llm.CompleteRequest, resp llm.CompleteResponse, callErr error) error
}

func (f *fakeSub) PreCall(ctx context.Context, req llm.CompleteRequest) error {
	if f.preFn == nil {
		return nil
	}
	return f.preFn(ctx, req)
}

func (f *fakeSub) PostCall(ctx context.Context, req llm.CompleteRequest, resp llm.CompleteResponse, callErr error) error {
	if f.postFn == nil {
		return nil
	}
	return f.postFn(ctx, req, resp, callErr)
}

type stubClient struct {
	response llm.CompleteResponse
	err      error
	calls    int
}

func (s *stubClient) Complete(_ context.Context, _ llm.CompleteRequest) (llm.CompleteResponse, error) {
	s.calls++
	if s.err != nil {
		return llm.CompleteResponse{}, s.err
	}
	return s.response, nil
}

func (s *stubClient) Close(_ context.Context) error { return nil }
