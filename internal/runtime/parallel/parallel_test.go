package parallel_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/parallel"
	"github.com/hurtener/Harbor/internal/tools"
)

// stubResolver is a tiny [parallel.Resolver] backed by an in-memory
// map. Each tool's Invoke + Validate functions are caller-supplied so
// each test can program the behaviour (success / failure / sleep / etc.).
type stubResolver struct {
	mu    sync.RWMutex
	tools map[string]tools.ToolDescriptor
}

func newStub() *stubResolver {
	return &stubResolver{tools: map[string]tools.ToolDescriptor{}}
}

func (s *stubResolver) Register(name string, invoke func(ctx context.Context, args json.RawMessage) (tools.ToolResult, error), validate func(args json.RawMessage) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tools[name] = tools.ToolDescriptor{
		Tool:     tools.Tool{Name: name},
		Invoke:   invoke,
		Validate: validate,
	}
}

func (s *stubResolver) Resolve(name string) (tools.ToolDescriptor, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	d, ok := s.tools[name]
	return d, ok
}

// fixedQ builds a populated identity quadruple for tests.
func fixedQ(t *testing.T, runID string) identity.Quadruple {
	t.Helper()
	return identity.Quadruple{
		Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"},
		RunID:    runID,
	}
}

func ctxWithQ(t *testing.T, q identity.Quadruple) context.Context {
	t.Helper()
	ctx, err := identity.WithRun(t.Context(), q.Identity, q.RunID)
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}
	return ctx
}

// echoTool returns a descriptor whose Invoke echoes args under
// Value["tool"]=name, Value["args"]=raw.
func echoTool(name string) (func(ctx context.Context, args json.RawMessage) (tools.ToolResult, error), func(args json.RawMessage) error) {
	invoke := func(_ context.Context, args json.RawMessage) (tools.ToolResult, error) {
		return tools.ToolResult{Value: map[string]any{"tool": name, "args": string(args)}}, nil
	}
	return invoke, nil
}

// TestExecute_JoinAll_AllSucceed pins the load-bearing Phase 47
// acceptance criterion: 3 branches all succeed → JoinAll returns 3
// results in branch-index order.
func TestExecute_JoinAll_AllSucceed(t *testing.T) {
	t.Parallel()
	resolver := newStub()
	for _, n := range []string{"alpha", "beta", "gamma"} {
		inv, val := echoTool(n)
		resolver.Register(n, inv, val)
	}
	exec := parallel.New(resolver)
	q := fixedQ(t, "r-joinall")

	call := planner.CallParallel{
		Branches: []planner.CallTool{
			{Tool: "alpha", Args: json.RawMessage(`{"x":1}`)},
			{Tool: "beta", Args: json.RawMessage(`{"x":2}`)},
			{Tool: "gamma", Args: json.RawMessage(`{"x":3}`)},
		},
		Join: &planner.JoinSpec{Kind: planner.JoinAll},
	}
	results, err := exec.Execute(ctxWithQ(t, q), call)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("len(results) = %d, want 3", len(results))
	}
	wantNames := []string{"alpha", "beta", "gamma"}
	for i, r := range results {
		if r.Index != i {
			t.Errorf("results[%d].Index = %d, want %d (deterministic merge key)", i, r.Index, i)
		}
		if r.Tool != wantNames[i] {
			t.Errorf("results[%d].Tool = %q, want %q", i, r.Tool, wantNames[i])
		}
		if r.Err != nil {
			t.Errorf("results[%d].Err = %v, want nil", i, r.Err)
		}
	}
}

// TestExecute_JoinAll_RejectsInvalidArgsBranchAtomically pins the
// atomic-setup-validation contract: ANY branch's invalid args fails
// the whole call BEFORE execution.
func TestExecute_JoinAll_RejectsInvalidArgsBranchAtomically(t *testing.T) {
	t.Parallel()
	resolver := newStub()

	// good tool — happy invoke + permissive validator.
	invG, _ := echoTool("good")
	resolver.Register("good", invG, nil)

	// bad tool — validator rejects everything.
	var invoked atomic.Int64
	invB := func(_ context.Context, _ json.RawMessage) (tools.ToolResult, error) {
		invoked.Add(1)
		return tools.ToolResult{}, nil
	}
	valB := func(_ json.RawMessage) error {
		return fmt.Errorf("test-validator-rejects-everything")
	}
	resolver.Register("bad", invB, valB)

	exec := parallel.New(resolver)
	q := fixedQ(t, "r-atomic")

	call := planner.CallParallel{
		Branches: []planner.CallTool{
			{Tool: "good", Args: json.RawMessage(`{}`)},
			{Tool: "bad", Args: json.RawMessage(`{}`)},
			{Tool: "good", Args: json.RawMessage(`{}`)},
		},
		Join: &planner.JoinSpec{Kind: planner.JoinAll},
	}
	results, err := exec.Execute(ctxWithQ(t, q), call)
	if err == nil {
		t.Fatal("Execute returned nil err, want ErrParallelBranchInvalidArgs")
	}
	if !errors.Is(err, planner.ErrParallelBranchInvalidArgs) {
		t.Errorf("err = %v, want errors.Is ErrParallelBranchInvalidArgs", err)
	}
	if results != nil {
		t.Errorf("results = %v, want nil (atomic-setup rejects BEFORE dispatch)", results)
	}
	if invoked.Load() != 0 {
		t.Errorf("invoked = %d, want 0 (no branch may execute on setup failure)", invoked.Load())
	}
}

// TestExecute_JoinFirstSuccess_CancelsRemainder pins the
// first-success cancellation semantics: when one branch succeeds, the
// remaining branches are cancelled via a derived ctx.
func TestExecute_JoinFirstSuccess_CancelsRemainder(t *testing.T) {
	t.Parallel()
	resolver := newStub()

	// fast tool — returns immediately.
	invFast, _ := echoTool("fast")
	resolver.Register("fast", invFast, nil)

	// slow tool — blocks on ctx; counts cancellation observations.
	var slowCancelled atomic.Int64
	invSlow := func(ctx context.Context, _ json.RawMessage) (tools.ToolResult, error) {
		select {
		case <-ctx.Done():
			slowCancelled.Add(1)
			return tools.ToolResult{}, ctx.Err()
		case <-time.After(2 * time.Second):
			return tools.ToolResult{Value: "slow"}, nil
		}
	}
	resolver.Register("slow", invSlow, nil)

	exec := parallel.New(resolver)
	q := fixedQ(t, "r-first-success")

	call := planner.CallParallel{
		Branches: []planner.CallTool{
			{Tool: "slow", Args: json.RawMessage(`{}`)},
			{Tool: "fast", Args: json.RawMessage(`{}`)},
			{Tool: "slow", Args: json.RawMessage(`{}`)},
		},
		Join: &planner.JoinSpec{Kind: planner.JoinFirstSuccess},
	}
	start := time.Now()
	results, err := exec.Execute(ctxWithQ(t, q), call)
	dur := time.Since(start)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1 (first success)", len(results))
	}
	if results[0].Tool != "fast" {
		t.Errorf("results[0].Tool = %q, want %q", results[0].Tool, "fast")
	}
	if dur > 1*time.Second {
		t.Errorf("Execute took %v — first-success should not wait on slow branches", dur)
	}
	// Slow branches may have been cancelled — give them a beat to
	// observe ctx.Done().
	// Polling pattern (no time.Sleep for sync): the cancel signal must
	// propagate quickly, but we don't strictly require both slow
	// branches to have observed it before Execute returned (they may
	// still be unwinding). Just assert at least one observed cancel.
	deadline := time.After(500 * time.Millisecond)
loop:
	for slowCancelled.Load() < 1 {
		select {
		case <-deadline:
			t.Errorf("slowCancelled = %d, want ≥ 1 (first-success must derive a cancel ctx)", slowCancelled.Load())
			break loop
		default:
		}
	}
}

// TestExecute_AbsoluteMaxParallelCap pins the system cap on branch
// counts: 51 branches → ErrParallelCapExceeded.
func TestExecute_AbsoluteMaxParallelCap(t *testing.T) {
	t.Parallel()
	resolver := newStub()
	inv, _ := echoTool("tool")
	resolver.Register("tool", inv, nil)
	exec := parallel.New(resolver)
	q := fixedQ(t, "r-cap")

	branches := make([]planner.CallTool, planner.AbsoluteMaxParallel+1)
	for i := range branches {
		branches[i] = planner.CallTool{Tool: "tool", Args: json.RawMessage(`{}`)}
	}
	results, err := exec.Execute(ctxWithQ(t, q), planner.CallParallel{
		Branches: branches,
		Join:     &planner.JoinSpec{Kind: planner.JoinAll},
	})
	if err == nil {
		t.Fatal("Execute returned nil err, want ErrParallelCapExceeded")
	}
	if !errors.Is(err, planner.ErrParallelCapExceeded) {
		t.Errorf("err = %v, want errors.Is ErrParallelCapExceeded", err)
	}
	if results != nil {
		t.Errorf("results = %v, want nil", results)
	}
}

// TestExecute_AbsoluteMaxParallelExactly50Allowed asserts the cap is
// inclusive at 50.
func TestExecute_AbsoluteMaxParallelExactly50Allowed(t *testing.T) {
	t.Parallel()
	resolver := newStub()
	inv, _ := echoTool("tool")
	resolver.Register("tool", inv, nil)
	exec := parallel.New(resolver)
	q := fixedQ(t, "r-cap-50")

	branches := make([]planner.CallTool, planner.AbsoluteMaxParallel)
	for i := range branches {
		branches[i] = planner.CallTool{Tool: "tool", Args: json.RawMessage(`{}`)}
	}
	results, err := exec.Execute(ctxWithQ(t, q), planner.CallParallel{
		Branches: branches,
		Join:     &planner.JoinSpec{Kind: planner.JoinAll},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(results) != planner.AbsoluteMaxParallel {
		t.Errorf("len(results) = %d, want %d", len(results), planner.AbsoluteMaxParallel)
	}
}

// TestExecute_MissingIdentityFailsClosed pins §6 rule 9: identity is
// mandatory at the executor boundary.
func TestExecute_MissingIdentityFailsClosed(t *testing.T) {
	t.Parallel()
	resolver := newStub()
	inv, _ := echoTool("alpha")
	resolver.Register("alpha", inv, nil)
	exec := parallel.New(resolver)

	results, err := exec.Execute(context.Background(), planner.CallParallel{
		Branches: []planner.CallTool{
			{Tool: "alpha", Args: json.RawMessage(`{}`)},
		},
		Join: &planner.JoinSpec{Kind: planner.JoinAll},
	})
	if err == nil {
		t.Fatal("Execute returned nil err, want identity-missing error")
	}
	if !errors.Is(err, identity.ErrIdentityMissing) {
		t.Errorf("err = %v, want errors.Is identity.ErrIdentityMissing", err)
	}
	if results != nil {
		t.Errorf("results = %v, want nil", results)
	}
}

// TestExecute_ToolNotFoundFailsAtomically asserts that a missing tool
// fails the whole call BEFORE dispatch.
func TestExecute_ToolNotFoundFailsAtomically(t *testing.T) {
	t.Parallel()
	resolver := newStub()
	inv, _ := echoTool("registered")
	resolver.Register("registered", inv, nil)
	exec := parallel.New(resolver)
	q := fixedQ(t, "r-notfound")

	call := planner.CallParallel{
		Branches: []planner.CallTool{
			{Tool: "registered", Args: json.RawMessage(`{}`)},
			{Tool: "absent", Args: json.RawMessage(`{}`)},
		},
		Join: &planner.JoinSpec{Kind: planner.JoinAll},
	}
	_, err := exec.Execute(ctxWithQ(t, q), call)
	if err == nil {
		t.Fatal("Execute returned nil err, want ErrToolNotFound")
	}
	if !errors.Is(err, tools.ErrToolNotFound) {
		t.Errorf("err = %v, want errors.Is tools.ErrToolNotFound", err)
	}
}

// TestExecute_JoinN_WaitsForNSuccesses pins the JoinN semantics.
func TestExecute_JoinN_WaitsForNSuccesses(t *testing.T) {
	t.Parallel()
	resolver := newStub()
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		inv, _ := echoTool(n)
		resolver.Register(n, inv, nil)
	}
	exec := parallel.New(resolver)
	q := fixedQ(t, "r-joinn")

	call := planner.CallParallel{
		Branches: []planner.CallTool{
			{Tool: "a", Args: json.RawMessage(`{}`)},
			{Tool: "b", Args: json.RawMessage(`{}`)},
			{Tool: "c", Args: json.RawMessage(`{}`)},
			{Tool: "d", Args: json.RawMessage(`{}`)},
			{Tool: "e", Args: json.RawMessage(`{}`)},
		},
		Join: &planner.JoinSpec{Kind: planner.JoinN, N: 3},
	}
	results, err := exec.Execute(ctxWithQ(t, q), call)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(results) != 3 {
		t.Errorf("len(results) = %d, want 3", len(results))
	}
	for _, r := range results {
		if r.Err != nil {
			t.Errorf("results carry non-nil Err: %v", r.Err)
		}
	}
}

// TestExecute_JoinN_InvalidThresholdFailsLoudly asserts validation of
// JoinN.N at setup time.
func TestExecute_JoinN_InvalidThresholdFailsLoudly(t *testing.T) {
	t.Parallel()
	resolver := newStub()
	inv, _ := echoTool("a")
	resolver.Register("a", inv, nil)
	exec := parallel.New(resolver)
	q := fixedQ(t, "r-joinn-bad")

	cases := []struct {
		name string
		n    int
	}{
		{"zero", 0},
		{"negative", -1},
		{"exceeds-branches", 10},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			_, err := exec.Execute(ctxWithQ(t, q), planner.CallParallel{
				Branches: []planner.CallTool{
					{Tool: "a", Args: json.RawMessage(`{}`)},
				},
				Join: &planner.JoinSpec{Kind: planner.JoinN, N: tc.n},
			})
			if err == nil {
				t.Fatal("Execute returned nil err, want ErrParallelInvalidJoin")
			}
			if !errors.Is(err, planner.ErrParallelInvalidJoin) {
				t.Errorf("err = %v, want errors.Is ErrParallelInvalidJoin", err)
			}
		})
	}
}

// TestExecute_NilResolverPanics pins the boot-time guard.
func TestExecute_NilResolverPanics(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("New(nil) did not panic")
		}
	}()
	_ = parallel.New(nil)
}

// TestExecute_EmptyBranchesFailsLoudly asserts the defensive check
// against the Phase 44 loop's empty-CallParallel edge.
func TestExecute_EmptyBranchesFailsLoudly(t *testing.T) {
	t.Parallel()
	resolver := newStub()
	exec := parallel.New(resolver)
	q := fixedQ(t, "r-empty")

	_, err := exec.Execute(ctxWithQ(t, q), planner.CallParallel{
		Branches: nil,
		Join:     &planner.JoinSpec{Kind: planner.JoinAll},
	})
	if err == nil {
		t.Fatal("Execute returned nil err, want ErrInvalidDecision")
	}
	if !errors.Is(err, planner.ErrInvalidDecision) {
		t.Errorf("err = %v, want errors.Is ErrInvalidDecision", err)
	}
}

// TestExecute_JoinKindUnknownFailsLoudly asserts an unknown JoinKind
// surfaces as ErrParallelInvalidJoin.
func TestExecute_JoinKindUnknownFailsLoudly(t *testing.T) {
	t.Parallel()
	resolver := newStub()
	inv, _ := echoTool("a")
	resolver.Register("a", inv, nil)
	exec := parallel.New(resolver)
	q := fixedQ(t, "r-unknown-kind")

	_, err := exec.Execute(ctxWithQ(t, q), planner.CallParallel{
		Branches: []planner.CallTool{{Tool: "a", Args: json.RawMessage(`{}`)}},
		Join:     &planner.JoinSpec{Kind: "made_up"},
	})
	if err == nil {
		t.Fatal("Execute returned nil err, want ErrParallelInvalidJoin")
	}
	if !errors.Is(err, planner.ErrParallelInvalidJoin) {
		t.Errorf("err = %v, want errors.Is ErrParallelInvalidJoin", err)
	}
}

// TestExecute_JoinKeyedNotImplementedFailsLoudly asserts the
// documented future-surface guard.
func TestExecute_JoinKeyedNotImplementedFailsLoudly(t *testing.T) {
	t.Parallel()
	resolver := newStub()
	inv, _ := echoTool("a")
	resolver.Register("a", inv, nil)
	exec := parallel.New(resolver)
	q := fixedQ(t, "r-keyed")

	_, err := exec.Execute(ctxWithQ(t, q), planner.CallParallel{
		Branches: []planner.CallTool{{Tool: "a", Args: json.RawMessage(`{}`)}},
		Join:     &planner.JoinSpec{Kind: planner.JoinKeyed},
	})
	if err == nil {
		t.Fatal("Execute returned nil err, want ErrParallelInvalidJoin")
	}
	if !errors.Is(err, planner.ErrParallelInvalidJoin) {
		t.Errorf("err = %v, want errors.Is ErrParallelInvalidJoin", err)
	}
}

// TestExecute_JoinAllSurfacesPerBranchFailures asserts that on
// JoinAll, per-branch failures populate Result.Err while successful
// branches populate Result.Result.
func TestExecute_JoinAllSurfacesPerBranchFailures(t *testing.T) {
	t.Parallel()
	resolver := newStub()
	invOK, _ := echoTool("ok")
	resolver.Register("ok", invOK, nil)
	wantErr := errors.New("upstream failure")
	resolver.Register("oops", func(_ context.Context, _ json.RawMessage) (tools.ToolResult, error) {
		return tools.ToolResult{}, wantErr
	}, nil)
	exec := parallel.New(resolver)
	q := fixedQ(t, "r-joinall-mix")

	results, err := exec.Execute(ctxWithQ(t, q), planner.CallParallel{
		Branches: []planner.CallTool{
			{Tool: "ok", Args: json.RawMessage(`{}`)},
			{Tool: "oops", Args: json.RawMessage(`{}`)},
		},
		Join: &planner.JoinSpec{Kind: planner.JoinAll},
	})
	if err != nil {
		t.Fatalf("Execute: %v (JoinAll surfaces per-branch failures via Result.Err, not the call-level error)", err)
	}
	if len(results) != 2 {
		t.Fatalf("len(results) = %d, want 2", len(results))
	}
	if results[0].Err != nil {
		t.Errorf("results[0].Err = %v, want nil", results[0].Err)
	}
	if !errors.Is(results[1].Err, wantErr) {
		t.Errorf("results[1].Err = %v, want errors.Is wantErr", results[1].Err)
	}
}

// TestExecute_NilJoinDefaultsToJoinAll asserts the nil-JoinSpec
// fall-through.
func TestExecute_NilJoinDefaultsToJoinAll(t *testing.T) {
	t.Parallel()
	resolver := newStub()
	inv, _ := echoTool("a")
	resolver.Register("a", inv, nil)
	exec := parallel.New(resolver)
	q := fixedQ(t, "r-nil-join")

	results, err := exec.Execute(ctxWithQ(t, q), planner.CallParallel{
		Branches: []planner.CallTool{{Tool: "a", Args: json.RawMessage(`{}`)}},
		Join:     nil,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
}
