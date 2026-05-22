// Phase 78 — chaos / fault-injection harness: the fault-injecting
// decorators.
//
// This file holds the thin fault-injecting decorators the Phase 78
// chaos harness (phase78_chaos_fault_injection_test.go) drives. Every
// decorator here:
//
//  1. WRAPS a real production component (`state.StateStore`,
//     `llm.LLMClient`) — it never re-implements the component's
//     behaviour. Non-faulting calls delegate verbatim to the real
//     driver opened through its production registry factory.
//  2. Lives in a `_test.go` file in the `integration_test` package —
//     it is NEVER registered as a registry driver, NEVER a
//     `DefaultDriver`, NEVER reachable by the `harbor` binary. The
//     runtime resolves only real drivers at boot.
//
// This is the CLAUDE.md §17.3 "real drivers at the seam" pattern with
// a fault overlay — NOT the §13 "test stub as production default"
// anti-pattern. The decorator decorates; it does not replace. See
// D-137 for the full §13-compliance rationale.
//
// The fault model every decorator shares: a fault is ARMED for a
// bounded number of calls, then auto-clears — this lets a single test
// row inject a fault AND assert the documented recovery path (the
// component works again once the fault clears) without re-constructing
// the component.
package integration_test

import (
	"context"
	"errors"
	"sync"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/state"
)

// errStateDisconnected is the loud transport-style error the
// fault-injecting StateStore decorator returns while its fault is
// armed. A real driver disconnect surfaces a wrapped driver error;
// the decorator mimics that shape so the harness can assert the error
// is surfaced loudly (never silently swallowed — CLAUDE.md §13).
var errStateDisconnected = errors.New("chaos: statestore transport disconnected")

// faultyStateStore decorates a real state.StateStore. While its fault
// is armed (faultsLeft > 0) every Save / Load / LoadByEventID / Delete
// returns errStateDisconnected and decrements the counter; once the
// counter reaches zero the call delegates to the real wrapped store —
// the documented "reconnect" recovery path.
//
// faultyStateStore is internally synchronised (faultMu guards the
// counter) so it is safe to share across goroutines, matching the
// state.StateStore concurrency contract (D-025).
type faultyStateStore struct {
	inner   state.StateStore
	faultMu sync.Mutex
	// faultsLeft is the number of upcoming calls that will fail with
	// errStateDisconnected before the store "reconnects". Guarded by
	// faultMu.
	faultsLeft int
}

// newFaultyStateStore wraps a real StateStore. The returned decorator
// starts with NO fault armed — the harness arms it explicitly via
// armDisconnect so the test controls exactly when the fault fires.
func newFaultyStateStore(inner state.StateStore) *faultyStateStore {
	return &faultyStateStore{inner: inner}
}

// armDisconnect arms the fault for the next n calls. After n faulted
// calls the store auto-recovers (delegates to the real inner store).
func (f *faultyStateStore) armDisconnect(n int) {
	f.faultMu.Lock()
	defer f.faultMu.Unlock()
	f.faultsLeft = n
}

// faulted reports whether this call should fail; it decrements the
// armed-fault counter as a side effect. A faulted call NEVER reaches
// the real inner store — the fault is observable, not silent.
func (f *faultyStateStore) faulted() bool {
	f.faultMu.Lock()
	defer f.faultMu.Unlock()
	if f.faultsLeft > 0 {
		f.faultsLeft--
		return true
	}
	return false
}

func (f *faultyStateStore) Save(ctx context.Context, r state.StateRecord) error {
	if f.faulted() {
		return errStateDisconnected
	}
	return f.inner.Save(ctx, r)
}

func (f *faultyStateStore) Load(ctx context.Context, id identity.Quadruple, kind string) (state.StateRecord, error) {
	if f.faulted() {
		return state.StateRecord{}, errStateDisconnected
	}
	return f.inner.Load(ctx, id, kind)
}

func (f *faultyStateStore) LoadByEventID(ctx context.Context, eventID state.EventID) (state.StateRecord, error) {
	if f.faulted() {
		return state.StateRecord{}, errStateDisconnected
	}
	return f.inner.LoadByEventID(ctx, eventID)
}

func (f *faultyStateStore) Delete(ctx context.Context, id identity.Quadruple, kind string) error {
	if f.faulted() {
		return errStateDisconnected
	}
	return f.inner.Delete(ctx, id, kind)
}

// Close delegates verbatim — teardown is never faulted (a faulted
// Close would leak the real store's resources, which is a different
// failure class than the one this harness injects).
func (f *faultyStateStore) Close(ctx context.Context) error {
	return f.inner.Close(ctx)
}

var _ state.StateStore = (*faultyStateStore)(nil)

// quirkLLMDriver is a fault-injecting LLM driver: it decorates nothing
// (there is no real provider in CI) but it is a deliberate, explicit
// chaos fixture, NOT a production default — it lives in a `_test.go`
// file in the `integration_test` package and is never registered with
// `llm.Open`. The harness wraps it in the REAL `retry.Wrap`
// retry-with-feedback layer (the production consumer of provider
// quirks), so the seam under test — the retry/correction path — is
// the real one.
//
// quirkLLMDriver returns a configurable "malformed" response for the
// first `badResponses` calls, then a "valid" response. Paired with a
// rejecting Validator in the harness, this drives the retry loop:
//   - badResponses >= maxRetries+1 -> the loop exhausts loudly with
//     llm.ErrRetryExhausted (the provider-quirk-not-recovered path);
//   - badResponses < maxRetries+1 -> the loop recovers and returns the
//     valid response (the provider-quirk-recovered path).
//
// quirkLLMDriver is safe for concurrent use (callMu guards the
// counter) — the LLMClient contract requires it (D-025).
type quirkLLMDriver struct {
	callMu sync.Mutex
	// callsLeft is the number of upcoming Complete calls that return
	// the malformed response. Guarded by callMu.
	callsLeft int
	// goodContent is the valid response content returned once the
	// malformed-response budget is exhausted.
	goodContent string
	// badContent is the malformed response content returned while
	// the budget is non-zero (e.g. truncated / non-JSON output a
	// quirky provider might emit).
	badContent string
	// closed records Close so a post-Close Complete fails loud.
	closed bool
}

// newQuirkLLMDriver builds a quirk driver that returns badContent for
// the first badResponses calls, then goodContent.
func newQuirkLLMDriver(badResponses int, badContent, goodContent string) *quirkLLMDriver {
	return &quirkLLMDriver{
		callsLeft:   badResponses,
		goodContent: goodContent,
		badContent:  badContent,
	}
}

// Complete returns the malformed response while the budget is
// non-zero, then the valid response. It honours ctx cancellation
// loudly — a cancelled ctx returns ctx.Err(), never a stale response.
func (q *quirkLLMDriver) Complete(ctx context.Context, _ llm.CompleteRequest) (llm.CompleteResponse, error) {
	if err := ctx.Err(); err != nil {
		return llm.CompleteResponse{}, err
	}
	q.callMu.Lock()
	defer q.callMu.Unlock()
	if q.closed {
		return llm.CompleteResponse{}, llm.ErrClientClosed
	}
	content := q.goodContent
	if q.callsLeft > 0 {
		q.callsLeft--
		content = q.badContent
	}
	return llm.CompleteResponse{Content: content}, nil
}

// Close marks the driver closed; subsequent Complete calls fail loud
// with llm.ErrClientClosed. Idempotent.
func (q *quirkLLMDriver) Close(context.Context) error {
	q.callMu.Lock()
	defer q.callMu.Unlock()
	q.closed = true
	return nil
}

var _ llm.LLMClient = (*quirkLLMDriver)(nil)
