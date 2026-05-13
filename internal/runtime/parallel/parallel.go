// Package parallel ships Harbor's runtime parallel-call executor —
// the consumer of [planner.CallParallel] Decisions (RFC §6.2, Phase 47,
// D-056). The Planner emits CallParallel; this package dispatches the
// branches concurrently with three settled invariants:
//
//  1. **Atomic setup validation.** Every branch's args is validated via
//     the resolved [tools.ToolDescriptor.Validate] BEFORE any branch
//     dispatches. A single invalid-args branch fails the whole call
//     with wrapped [planner.ErrParallelBranchInvalidArgs] — no branch
//     has executed; no side-effect tool has fired; the planner's next
//     step sees a clean failure observation.
//
//  2. **System cap on branch count.** Any CallParallel with
//     `len(Branches) > planner.AbsoluteMaxParallel` (=50) fails the
//     whole call with [planner.ErrParallelCapExceeded] — defence in
//     depth against a runaway emission.
//
//  3. **Parallel-pause atomicity.** No branch starts side-effecting
//     tools, or all reach checkpointed observation before pause
//     commits (RFC §6.2). The unified pause/resume primitive lands at
//     Phase 50; for Phase 47 a mid-execution pause request fails loud
//     with [planner.ErrParallelPauseUnsupported]. Phase 50 upgrades
//     this path to a checkpointed atomic pause.
//
// Three join shapes are supported (D-056):
//
//   - [planner.JoinAll]: wait for every branch to terminate; return
//     the result slice in branch-index order. Default.
//   - [planner.JoinFirstSuccess]: return the first successful branch's
//     result; cancel the remainder mid-flight. Failures do NOT cancel
//     until all remaining branches terminate.
//   - [planner.JoinN]: wait until N branches succeed, then cancel the
//     remainder. JoinSpec.N carries the threshold; 0 < N ≤ len(Branches)
//     is validated at setup time.
//
// **Import-graph contract (§13).** This package lives in
// `internal/runtime/parallel`, OUTSIDE the planner subtree, so it MAY
// import `internal/planner`. The reverse (planner → runtime) is
// FORBIDDEN by the Phase 42 conformance lint; the executor is the
// one-way dispatch site that consumes the typed planner shape.
//
// **Identity (§6 rule 9).** Every dispatch reads the run's identity
// quadruple from ctx; missing identity returns wrapped
// [tools.ErrIdentityRequired]. Branches inherit the parent ctx so
// identity propagates unchanged to every tool invocation.
//
// **Concurrent reuse (D-025).** The [Executor] struct is immutable
// after construction; per-call state lives on the stack and in ctx.
// `concurrent_test.go` pins N≥128 concurrent invocations against one
// shared instance under `-race`.
//
// **Deterministic merge keys.** Each [Result] entry carries the
// branch's input index AND its tool name. JoinAll returns results in
// branch-index order; JoinFirstSuccess returns a single-entry slice
// keyed on the successful branch's index; JoinN returns the first N
// successful branches in completion order (the per-branch index +
// tool name is the deterministic key for downstream observation
// rendering).
package parallel

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/tools"
)

// Resolver is the narrow descriptor-lookup view the executor depends
// on. The full [tools.ToolCatalog] surface (Register / List) is not
// needed at dispatch — only Resolve. This narrowing lets tests inject
// a tiny stub resolver without standing up the production catalog.
type Resolver interface {
	// Resolve returns the descriptor for name. found=false on miss.
	Resolve(name string) (tools.ToolDescriptor, bool)
}

// Executor dispatches [planner.CallParallel] Decisions. Constructed
// once; safe for N concurrent invocations against a shared instance
// (D-025).
//
// The executor is intentionally minimal — it does NOT apply
// [tools.ToolPolicy] retry / timeout (that is the per-tool dispatch
// shell's job; the executor just invokes the descriptor's Invoke once
// per branch). Future runtime phases that wire the per-tool policy
// shell at the dispatcher should compose the shell INSIDE the
// per-branch goroutine.
type Executor struct {
	// resolver is the descriptor lookup. Required.
	resolver Resolver
}

// New constructs an Executor backed by the supplied [Resolver]. Nil
// resolver panics — composition error caught at boot.
func New(resolver Resolver) *Executor {
	if resolver == nil {
		panic("parallel.New: nil Resolver")
	}
	return &Executor{resolver: resolver}
}

// Result is the per-branch outcome the executor produces. Each entry
// carries the branch's input index + tool name (the deterministic
// merge key), the [tools.ToolResult] on success, and the error on
// failure.
//
// Either Result is populated (success) or Err is populated (failure)
// — never both. Cancelled branches surface Err = context.Canceled.
type Result struct {
	// Index is the branch's position in the input [planner.CallParallel.Branches]
	// slice. Stable for the lifetime of the call.
	Index int
	// Tool is the branch's tool name. Same as Branches[Index].Tool.
	Tool string
	// Result is the [tools.ToolResult] on success. Nil on failure /
	// cancellation.
	Result *tools.ToolResult
	// Err is the upstream error on failure. Nil on success.
	Err error
}

// Execute dispatches the [planner.CallParallel] Decision per the
// JoinSpec semantics and returns the per-branch results in
// deterministic order.
//
// Step 1 (atomic setup validation):
//
//   - Branch count vs. [planner.AbsoluteMaxParallel] (=50). Exceeded →
//     [planner.ErrParallelCapExceeded].
//   - JoinSpec shape (JoinKind known; JoinN threshold in range).
//     Malformed → [planner.ErrParallelInvalidJoin].
//   - Every branch's tool resolves via [Resolver.Resolve]. Missing →
//     [tools.ErrToolNotFound] wrapped with the branch index.
//   - Every branch's args validates via the descriptor's Validate.
//     Failed → [planner.ErrParallelBranchInvalidArgs] wrapped with
//     the branch index + upstream error.
//
// If any setup step fails, NO branch dispatches. The error is the
// only return; results is nil. This is the load-bearing
// "atomicity-contract" surface RFC §6.2 names.
//
// Step 2 (dispatch):
//
// Each surviving branch fires in its own goroutine with the supplied
// ctx (cancellation, identity, deadline propagate). Per the JoinSpec:
//
//   - JoinAll: wait for every branch; return all results in
//     branch-index order.
//   - JoinFirstSuccess: return the first successful branch; cancel the
//     remainder via a derived ctx; the returned slice is single-entry.
//     If every branch fails, the slice is empty + the error is a
//     joined error of every branch's failure.
//   - JoinN: wait until N branches succeed; cancel the remainder. The
//     returned slice carries the N successes in COMPLETION order
//     (each Result still carries its original branch Index for
//     deterministic merge-key consumption downstream).
//
// Returns (results, error). The error path is reserved for
// setup-validation failures and JoinFirstSuccess/JoinN exhaustion (no
// branch met the threshold). Per-branch failures land on
// Result.Err — the caller (planner step adapter) decides how to
// surface mixed-success-and-failure observations.
func (e *Executor) Execute(ctx context.Context, call planner.CallParallel) ([]Result, error) {
	// Identity (§6 rule 9). The executor reads ctx for the run's
	// identity quadruple; missing identity rejects fail-closed
	// BEFORE any branch dispatches.
	if _, ok := identity.QuadrupleFrom(ctx); !ok {
		return nil, fmt.Errorf("%w: parallel executor refuses missing-identity ctx", identity.ErrIdentityMissing)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Step 1 (a): branch-count cap.
	branches := call.Branches
	if len(branches) > planner.AbsoluteMaxParallel {
		return nil, fmt.Errorf(
			"%w: emitted %d branches, cap is %d",
			planner.ErrParallelCapExceeded, len(branches), planner.AbsoluteMaxParallel,
		)
	}
	if len(branches) == 0 {
		// Defensive: an empty CallParallel is the planner's bug
		// (Phase 44 repair loop guards against this; we still reject
		// explicitly per §13 fail-loudly).
		return nil, fmt.Errorf(
			"%w: CallParallel must carry at least one branch",
			planner.ErrInvalidDecision,
		)
	}

	// Step 1 (b): JoinSpec shape.
	join := normaliseJoin(call.Join)
	if err := validateJoin(join, len(branches)); err != nil {
		return nil, err
	}

	// Step 1 (c) + (d): resolve every descriptor + validate every
	// args. The two checks are bundled in one pass — a single
	// invalid-args branch fails the whole call.
	descriptors := make([]tools.ToolDescriptor, len(branches))
	for i, b := range branches {
		desc, ok := e.resolver.Resolve(b.Tool)
		if !ok {
			return nil, fmt.Errorf(
				"%w: parallel branch[%d] tool %q not registered",
				tools.ErrToolNotFound, i, b.Tool,
			)
		}
		descriptors[i] = desc
		if desc.Validate != nil {
			if err := desc.Validate(b.Args); err != nil {
				return nil, fmt.Errorf(
					"%w: branch[%d] tool=%q: %v",
					planner.ErrParallelBranchInvalidArgs, i, b.Tool, err,
				)
			}
		}
	}

	// Step 2: dispatch.
	switch join.Kind {
	case planner.JoinAll:
		return e.dispatchAll(ctx, branches, descriptors)
	case planner.JoinFirstSuccess:
		return e.dispatchFirstSuccess(ctx, branches, descriptors)
	case planner.JoinN:
		return e.dispatchN(ctx, branches, descriptors, join.N)
	case planner.JoinKeyed:
		// JoinKeyed is a documented future surface (D-056 — Phase 47
		// ships JoinAll / JoinFirstSuccess / JoinN; JoinKeyed merge
		// semantics land at a later runtime phase). Fail-loudly per
		// §13 — never silently treat as JoinAll.
		return nil, fmt.Errorf(
			"%w: JoinKeyed not implemented at Phase 47 (D-056 reserves the constant for a later runtime phase)",
			planner.ErrParallelInvalidJoin,
		)
	default:
		return nil, fmt.Errorf(
			"%w: unknown JoinKind %q",
			planner.ErrParallelInvalidJoin, join.Kind,
		)
	}
}

// normaliseJoin returns a non-nil JoinSpec defaulting to JoinAll. The
// planner is allowed to emit a nil JoinSpec on a CallParallel — the
// executor's policy is "Join unspecified → JoinAll".
func normaliseJoin(j *planner.JoinSpec) planner.JoinSpec {
	if j == nil {
		return planner.JoinSpec{Kind: planner.JoinAll}
	}
	out := *j
	if out.Kind == "" {
		out.Kind = planner.JoinAll
	}
	return out
}

// validateJoin checks JoinSpec invariants. Returns wrapped
// [planner.ErrParallelInvalidJoin] on any violation.
func validateJoin(j planner.JoinSpec, branchCount int) error {
	switch j.Kind {
	case planner.JoinAll, planner.JoinFirstSuccess:
		// Both shapes are valid regardless of N (N is ignored).
		return nil
	case planner.JoinN:
		if j.N <= 0 {
			return fmt.Errorf("%w: JoinN requires N > 0 (got %d)", planner.ErrParallelInvalidJoin, j.N)
		}
		if j.N > branchCount {
			return fmt.Errorf(
				"%w: JoinN N=%d exceeds branch count %d",
				planner.ErrParallelInvalidJoin, j.N, branchCount,
			)
		}
		return nil
	case planner.JoinKeyed:
		// Allowed shape; the dispatch switch rejects it with the
		// "not implemented at Phase 47" error.
		return nil
	default:
		return fmt.Errorf("%w: unknown JoinKind %q", planner.ErrParallelInvalidJoin, j.Kind)
	}
}

// dispatchAll fires every branch concurrently and waits for all of
// them to terminate. Results are returned in branch-index order.
// Cancellation: each branch inherits the caller's ctx. If the caller
// cancels mid-flight, branches that honour ctx exit promptly; the
// returned slice still carries one Result per branch (the cancelled
// branches surface Err = context.Canceled or the upstream wrap).
func (e *Executor) dispatchAll(
	ctx context.Context,
	branches []planner.CallTool,
	descriptors []tools.ToolDescriptor,
) ([]Result, error) {
	results := make([]Result, len(branches))
	var wg sync.WaitGroup
	for i := range branches {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results[idx] = invokeBranch(ctx, idx, branches[idx], descriptors[idx])
		}(i)
	}
	wg.Wait()
	return results, nil
}

// dispatchFirstSuccess fires every branch concurrently. On the FIRST
// successful branch, derives ctx is cancelled to release the rest;
// the returned slice carries the single successful Result. If every
// branch fails, returns an empty slice + a joined error wrapping each
// branch's Err.
//
// Cancelled branches (those cancelled by the executor's derived ctx)
// are NOT considered failures for the joined-error path — the caller
// asked for "first success" and we delivered.
func (e *Executor) dispatchFirstSuccess(
	ctx context.Context,
	branches []planner.CallTool,
	descriptors []tools.ToolDescriptor,
) ([]Result, error) {
	derivedCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	type signal struct {
		res Result
	}
	sigCh := make(chan signal, len(branches))
	var wg sync.WaitGroup
	for i := range branches {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			sigCh <- signal{res: invokeBranch(derivedCtx, idx, branches[idx], descriptors[idx])}
		}(i)
	}

	go func() {
		wg.Wait()
		close(sigCh)
	}()

	failures := make([]Result, 0, len(branches))
	for s := range sigCh {
		if s.res.Err == nil {
			// First success — cancel the rest, drain the channel
			// asynchronously (the goroutine above closes sigCh once
			// every branch settles), and return.
			cancel()
			// Drain remaining without blocking on them (the goroutine
			// above will close sigCh when all wg.Done() fire).
			go func() {
				for range sigCh { //nolint:revive // drain
				}
			}()
			return []Result{s.res}, nil
		}
		failures = append(failures, s.res)
	}

	// Every branch failed (or was cancelled by ctx parent). Join the
	// errors so the planner observation can include all of them.
	errs := make([]error, 0, len(failures))
	for _, f := range failures {
		if f.Err != nil {
			errs = append(errs, fmt.Errorf("branch[%d] tool=%q: %w", f.Index, f.Tool, f.Err))
		}
	}
	return nil, errors.Join(errs...)
}

// dispatchN waits for N branches to succeed; cancels the rest. The
// returned slice carries the N successes in COMPLETION order (each
// Result retains its original branch Index for the deterministic
// merge key downstream).
//
// If fewer than N branches succeed before every branch terminates,
// returns the partial successes (may be empty) plus a joined error
// wrapping every failed branch's Err.
func (e *Executor) dispatchN(
	ctx context.Context,
	branches []planner.CallTool,
	descriptors []tools.ToolDescriptor,
	n int,
) ([]Result, error) {
	derivedCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	type signal struct {
		res Result
	}
	sigCh := make(chan signal, len(branches))
	var wg sync.WaitGroup
	for i := range branches {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			sigCh <- signal{res: invokeBranch(derivedCtx, idx, branches[idx], descriptors[idx])}
		}(i)
	}
	go func() {
		wg.Wait()
		close(sigCh)
	}()

	successes := make([]Result, 0, n)
	failures := make([]Result, 0, len(branches))
	for s := range sigCh {
		if s.res.Err == nil {
			successes = append(successes, s.res)
			if len(successes) >= n {
				cancel()
				// Drain the rest async.
				go func() {
					for range sigCh { //nolint:revive // drain
					}
				}()
				return successes, nil
			}
		} else {
			failures = append(failures, s.res)
		}
	}

	// Fell short of N. Join every failure's error.
	errs := make([]error, 0, len(failures))
	for _, f := range failures {
		if f.Err != nil {
			errs = append(errs, fmt.Errorf("branch[%d] tool=%q: %w", f.Index, f.Tool, f.Err))
		}
	}
	joined := errors.Join(errs...)
	return successes, fmt.Errorf(
		"%w: JoinN N=%d reached %d successes only: %v",
		planner.ErrInvalidDecision, n, len(successes), joined,
	)
}

// invokeBranch dispatches one branch. Catches ctx.Err() before the
// invoke (the typical cancel-while-waiting-for-goroutine-slot case)
// and on Invoke returning errors.Is(ctx.Err()).
func invokeBranch(
	ctx context.Context,
	idx int,
	branch planner.CallTool,
	desc tools.ToolDescriptor,
) Result {
	if err := ctx.Err(); err != nil {
		return Result{Index: idx, Tool: branch.Tool, Err: err}
	}
	if desc.Invoke == nil {
		return Result{
			Index: idx,
			Tool:  branch.Tool,
			Err:   fmt.Errorf("parallel: tool %q descriptor has nil Invoke", branch.Tool),
		}
	}
	res, err := desc.Invoke(ctx, branch.Args)
	if err != nil {
		return Result{Index: idx, Tool: branch.Tool, Err: err}
	}
	return Result{Index: idx, Tool: branch.Tool, Result: &res}
}
