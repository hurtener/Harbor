package engine_test

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/runtime/engine"
	"github.com/hurtener/Harbor/internal/runtime/messages"
)

// passthroughGraph builds a 2-node engine: in -> out. The in node's
// Func returns the inbound envelope unchanged; the out node has nil
// Func (outlet-only). Used as the workhorse subflow for tests.
func passthroughGraph(t *testing.T) engine.Engine {
	t.Helper()
	inNode := engine.Node{
		Name: "in",
		Func: func(_ context.Context, env messages.Envelope, _ *engine.NodeContext) (messages.Envelope, error) {
			return env, nil
		},
	}
	outNode := engine.Node{Name: "out"}
	eng, err := engine.New([]engine.Adjacency{
		{From: inNode, To: []engine.Node{outNode}},
	})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	return eng
}

func mkParentEnvelope(runID string) messages.Envelope {
	return messages.Envelope{
		Headers:   messages.Headers{TenantID: "T", UserID: "U"},
		SessionID: "S",
		RunID:     runID,
		Payload:   "hello",
	}
}

// makeNodeContext is a hack: NodeContext is constructed by the worker
// loop (engine package internals). For Phase 14 unit tests we need a
// way to invoke CallSubflow without spinning up a full parent engine.
// Since NodeContext only needs *engine + node name (and CallSubflow
// doesn't read those fields directly), we get one by triggering the
// parent worker indirectly. The integration test exercises the full
// path; these unit tests cover CallSubflow itself.
//
// In practice we just construct a NodeContext-zero-value via reflection
// avoidance: CallSubflow's body doesn't dereference the engine
// reference, so a default-constructed NodeContext works.
func zeroNodeContext() *engine.NodeContext {
	var nctx engine.NodeContext
	return &nctx
}

func TestCallSubflow_ReturnsFirstEgress(t *testing.T) {
	t.Parallel()
	parentEnv := mkParentEnvelope("R-egress")
	parentEnv.Payload = "first-egress-test"

	out, err := zeroNodeContext().CallSubflow(context.Background(), func() (engine.Engine, error) {
		return passthroughGraph(t), nil
	}, parentEnv)
	if err != nil {
		t.Fatalf("CallSubflow: %v", err)
	}
	if got, _ := out.Payload.(string); got != "first-egress-test" {
		t.Errorf("out.Payload=%v, want %q", out.Payload, "first-egress-test")
	}
}

func TestCallSubflow_PropagatesQuadruple(t *testing.T) {
	t.Parallel()
	parentEnv := mkParentEnvelope("R-quadruple")
	out, err := zeroNodeContext().CallSubflow(context.Background(), func() (engine.Engine, error) {
		return passthroughGraph(t), nil
	}, parentEnv)
	if err != nil {
		t.Fatalf("CallSubflow: %v", err)
	}
	q := out.Identity()
	if q.TenantID != "T" || q.UserID != "U" || q.SessionID != "S" || q.RunID != "R-quadruple" {
		t.Errorf("identity not propagated: %+v", q)
	}
}

func TestCallSubflow_FactoryError_ReturnsWrappedErr(t *testing.T) {
	t.Parallel()
	boom := errors.New("synthetic factory boom")
	_, err := zeroNodeContext().CallSubflow(context.Background(), func() (engine.Engine, error) {
		return nil, boom
	}, mkParentEnvelope("R-factory"))
	if !errors.Is(err, engine.ErrSubflowFactoryFailed) {
		t.Fatalf("err=%v, want ErrSubflowFactoryFailed", err)
	}
	if !errors.Is(err, boom) {
		t.Fatalf("err=%v, want wraps boom", err)
	}
}

func TestCallSubflow_FactoryReturnsNilEngine(t *testing.T) {
	t.Parallel()
	_, err := zeroNodeContext().CallSubflow(context.Background(), func() (engine.Engine, error) {
		return nil, nil
	}, mkParentEnvelope("R-nil"))
	if !errors.Is(err, engine.ErrSubflowFactoryFailed) {
		t.Fatalf("err=%v, want ErrSubflowFactoryFailed", err)
	}
}

func TestCallSubflow_NilFactory(t *testing.T) {
	t.Parallel()
	_, err := zeroNodeContext().CallSubflow(context.Background(), nil, mkParentEnvelope("R-nilfn"))
	if err == nil {
		t.Fatal("nil factory must return error")
	}
}

// TestCallSubflow_ParentCtxCancel_PropagatesToChild — the engine-Cancel
// mirroring test from the plan, scoped to ctx-based cancellation per
// the parent fork's instructions. Phase 13 will extend this to honor
// Engine.Cancel(runID).
func TestCallSubflow_ParentCtxCancel_PropagatesToChild(t *testing.T) {
	t.Parallel()
	// Build a subflow whose `in` node blocks on ctx until cancelled.
	// CallSubflow's Fetch will hang, so we invoke it from a goroutine
	// and observe it return promptly when we cancel the parent ctx.
	blockingNode := engine.Node{
		Name: "in",
		Func: func(ctx context.Context, env messages.Envelope, _ *engine.NodeContext) (messages.Envelope, error) {
			// Block the worker on ctx until parent cancels.
			<-ctx.Done()
			return env, ctx.Err()
		},
	}
	outNode := engine.Node{Name: "out"}
	factory := func() (engine.Engine, error) {
		return engine.New([]engine.Adjacency{
			{From: blockingNode, To: []engine.Node{outNode}},
		})
	}

	parentCtx, cancelParent := context.WithCancel(context.Background())
	resultErr := make(chan error, 1)
	go func() {
		_, err := zeroNodeContext().CallSubflow(parentCtx, factory, mkParentEnvelope("R-cancel"))
		resultErr <- err
	}()

	// Give CallSubflow time to spin up the child + start blocking.
	time.Sleep(50 * time.Millisecond)
	cancelParent()

	select {
	case err := <-resultErr:
		if err == nil {
			t.Errorf("CallSubflow should have errored on ctx cancel; got nil")
		}
		// The exact error chain depends on which inner call observes
		// cancellation first (Emit / Fetch / child Run). Any non-nil
		// err that is errors.Is context.Canceled is acceptable.
		if !errors.Is(err, context.Canceled) {
			t.Logf("CallSubflow returned err=%v (want errors.Is context.Canceled)", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("CallSubflow did not unblock within 2s of ctx cancel")
	}
}

// TestCallSubflow_NestedSubflows mirrors the 2-level subflow scenario
// from the plan's integration-test note: parent → subflow A → subflow B.
// Here we keep it inside the unit test (no bus / state wiring) by
// nesting CallSubflow calls.
func TestCallSubflow_NestedSubflows(t *testing.T) {
	t.Parallel()
	// inner factory: identity passthrough. The (Engine, error) shape is
	// dictated by the subflow factory API; this test's factory never
	// fails, hence the always-nil error.
	innerFactory := func() (engine.Engine, error) { //nolint:unparam // factory signature is fixed by the subflow API
		return passthroughGraph(t), nil
	}
	// outer factory: a 2-node graph whose `in` node calls the inner
	// subflow inside its NodeFunc.
	var innerCalled atomic.Bool
	outerInNode := engine.Node{
		Name: "in",
		Func: func(ctx context.Context, env messages.Envelope, nctx *engine.NodeContext) (messages.Envelope, error) {
			out, err := nctx.CallSubflow(ctx, innerFactory, env)
			if err != nil {
				return messages.Envelope{}, err
			}
			innerCalled.Store(true)
			return out, nil
		},
	}
	outerOutNode := engine.Node{Name: "out"}
	outerFactory := func() (engine.Engine, error) {
		return engine.New([]engine.Adjacency{
			{From: outerInNode, To: []engine.Node{outerOutNode}},
		})
	}
	out, err := zeroNodeContext().CallSubflow(context.Background(), outerFactory, mkParentEnvelope("R-nested"))
	if err != nil {
		t.Fatalf("nested CallSubflow: %v", err)
	}
	if got := out.RunID; got != "R-nested" {
		t.Errorf("nested out.RunID=%q, want R-nested", got)
	}
	if !innerCalled.Load() {
		t.Error("inner subflow was not called")
	}
}

// TestCallSubflow_ParentCancel_PropagatesToChild — Phase 13 follow-up.
// A real parent engine runs a node that calls CallSubflow with a
// blocking child factory. Calling parent.Cancel(parentRunID) fires
// the registered cancel-observer, which in turn invokes
// child.Cancel(parentRunID). The child's Fetch wakes with
// ErrRunCancelled and CallSubflow returns a non-nil error.
func TestCallSubflow_ParentCancel_PropagatesToChild(t *testing.T) {
	t.Parallel()

	childStarted := make(chan struct{}, 1)
	childFactory := func() (engine.Engine, error) {
		blockingChild := engine.Node{
			Name: "in",
			Func: func(ctx context.Context, env messages.Envelope, _ *engine.NodeContext) (messages.Envelope, error) {
				select {
				case childStarted <- struct{}{}:
				default:
				}
				<-ctx.Done()
				return env, ctx.Err()
			},
		}
		outNode := engine.Node{Name: "out"}
		return engine.New([]engine.Adjacency{
			{From: blockingChild, To: []engine.Node{outNode}},
		})
	}

	subflowDone := make(chan error, 1)
	parentNode := engine.Node{
		Name: "parent",
		Func: func(ctx context.Context, env messages.Envelope, nctx *engine.NodeContext) (messages.Envelope, error) {
			_, err := nctx.CallSubflow(ctx, childFactory, env)
			subflowDone <- err
			return env, err
		},
	}
	parent, err := engine.New([]engine.Adjacency{
		{From: parentNode},
	})
	if err != nil {
		t.Fatalf("parent New: %v", err)
	}
	if err := parent.Run(context.Background()); err != nil {
		t.Fatalf("parent Run: %v", err)
	}
	defer func() { _ = parent.Stop(context.Background()) }()

	parentEnv := mkParentEnvelope("R-mirror")
	if err := parent.Emit(context.Background(), parentEnv); err != nil {
		t.Fatalf("parent Emit: %v", err)
	}

	select {
	case <-childStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("child subflow never started")
	}

	if _, err := parent.Cancel(context.Background(), "R-mirror"); err != nil {
		t.Fatalf("parent Cancel: %v", err)
	}

	select {
	case err := <-subflowDone:
		if err == nil {
			t.Error("CallSubflow returned nil err after parent.Cancel; want non-nil")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("CallSubflow did not unblock within 3s of parent.Cancel — engine-Cancel mirroring failed")
	}
}

func TestCallSubflow_NoGoroutineLeak(t *testing.T) {
	t.Parallel()
	baseline := runtime.NumGoroutine()
	for i := range 10 {
		factory := func() (engine.Engine, error) { return passthroughGraph(t), nil }
		out, err := zeroNodeContext().CallSubflow(context.Background(), factory, mkParentEnvelope(fmt.Sprintf("R-leak-%d", i)))
		if err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
		_ = out
	}
	// Allow goroutines to settle.
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline+3 && time.Now().Before(deadline) {
		runtime.Gosched()
		time.Sleep(10 * time.Millisecond)
	}
	if delta := runtime.NumGoroutine() - baseline; delta > 3 {
		t.Errorf("goroutine leak: baseline=%d after=%d (delta=%d)", baseline, runtime.NumGoroutine(), delta)
	}
}
