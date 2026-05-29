package harbortest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/hurtener/Harbor/internal/tools"
)

// FaultInjector wraps a tools.ToolCatalog so the kit can schedule
// per-tool failures before exercising an Agent. The injector is
// transparent on the read path — Resolve returns a ToolDescriptor
// whose Invoke closure pops a counter on each call; when the
// counter is exhausted the original descriptor's Invoke runs
// unmodified.
//
// Concurrent reuse (D-025). The injector serialises access to its
// counter map behind a mutex; counter updates are atomic per tool.
// Resolve / List are safe for N concurrent goroutines.
type FaultInjector struct {
	inner tools.ToolCatalog

	mu     sync.Mutex
	queues map[string][]injection // tool name -> FIFO of pending injections
}

// injection is one scheduled failure. When n > 0, the next call to
// the tool returns class-typed error and decrements n; when n
// reaches 0 the entry is popped and the next entry (if any) takes
// effect.
type injection struct {
	class tools.ErrorClass
	n     int
}

// NewFaultInjector wraps cat. Subsequent calls to inj.Catalog()
// return a tools.ToolCatalog that participates in the kit's
// failure-injection mechanism. The wrapped catalog's Register +
// List + Resolve all defer to cat; only Resolve adds the
// injection-counter wrapper around the returned descriptor's
// Invoke.
//
// NewFaultInjector panics with a clear message if cat is nil — a
// nil catalog at the kit boundary is a test-author bug, not a
// production fail-loud concern (CLAUDE.md §5 forbids panic in
// production code; this surface is test-only).
func NewFaultInjector(cat tools.ToolCatalog) *FaultInjector {
	if cat == nil {
		panic("harbortest.NewFaultInjector: catalog is nil")
	}
	return &FaultInjector{
		inner:  cat,
		queues: make(map[string][]injection),
	}
}

// Catalog returns the wrapped ToolCatalog. Pass this to the Agent's
// interior (typically via tools.WithCatalog on the run ctx) so the
// injector intercepts every tool resolution.
func (f *FaultInjector) Catalog() tools.ToolCatalog {
	return &injectingCatalog{inj: f}
}

// SimulateFailure schedules the next n invocations of toolName to
// fail with the given error class. Subsequent invocations (after
// the n failures pop) resume normal behaviour. Calling
// SimulateFailure twice on the same tool stacks the counters in
// FIFO order: a (transient, 2) then a (permanent, 1) yields two
// transient failures followed by one permanent failure followed by
// success.
//
// n must be positive; calls with n <= 0 are silent no-ops (the
// caller intent is unclear and we choose the conservative reading).
//
// Per the test-only nature of this surface, SimulateFailure does
// NOT validate that toolName is a registered tool — the test author
// may schedule failures on a tool name they're about to register.
// A tool name with no scheduled failures behaves identically to a
// tool never wrapped at all.
func SimulateFailure(f *FaultInjector, toolName string, class tools.ErrorClass, n int) {
	if f == nil || toolName == "" || n <= 0 {
		return
	}
	f.mu.Lock()
	f.queues[toolName] = append(f.queues[toolName], injection{class: class, n: n})
	f.mu.Unlock()
}

// pop dequeues the next-effective failure for toolName, decrementing
// its counter. Returns (class, true) when a failure should fire;
// (zero, false) when no failure is scheduled.
func (f *FaultInjector) pop(toolName string) (tools.ErrorClass, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	q, ok := f.queues[toolName]
	if !ok || len(q) == 0 {
		return "", false
	}
	head := &q[0]
	class := head.class
	head.n--
	if head.n <= 0 {
		q = q[1:]
	}
	if len(q) == 0 {
		delete(f.queues, toolName)
	} else {
		f.queues[toolName] = q
	}
	return class, true
}

// injectingCatalog is the wrapped ToolCatalog returned by
// FaultInjector.Catalog. It defers Register / List to the inner
// catalog and intercepts Resolve.
type injectingCatalog struct {
	inj *FaultInjector
}

// Register forwards to the inner catalog.
func (c *injectingCatalog) Register(d tools.ToolDescriptor) error {
	return c.inj.inner.Register(d)
}

// List forwards to the inner catalog.
func (c *injectingCatalog) List(filter tools.CatalogFilter) []tools.Tool {
	return c.inj.inner.List(filter)
}

// Resolve wraps the inner descriptor's Invoke with a counter-
// popping shell. When no failure is scheduled, the call is a thin
// passthrough. When a failure is scheduled, the wrapper returns
// without invoking the inner Invoke at all — this is intentional:
// real tool calls are usually expensive (network, side effects),
// and "test author wants this tool to fail before doing anything"
// is the natural read of SimulateFailure.
func (c *injectingCatalog) Resolve(name string) (tools.ToolDescriptor, bool) {
	d, ok := c.inj.inner.Resolve(name)
	if !ok {
		return tools.ToolDescriptor{}, false
	}
	// Wrap Invoke. The wrapper keeps Validate untouched so arg
	// validation still happens at the catalog edge (a malformed-
	// args test path is orthogonal to failure injection).
	innerInvoke := d.Invoke
	d.Invoke = func(ctx context.Context, args json.RawMessage) (tools.ToolResult, error) {
		if class, fire := c.inj.pop(name); fire {
			return tools.ToolResult{}, simulatedFailure(name, class)
		}
		return innerInvoke(ctx, args)
	}
	return d, true
}

// Search delegates to the inner catalog (Phase 107c / D-167).
func (c *injectingCatalog) Search(ctx context.Context, query string, tags []string, limit int) []tools.Tool {
	return c.inj.inner.Search(ctx, query, tags, limit)
}

// ErrSimulatedFailure is the sentinel a SimulateFailure-triggered
// error wraps. Callers compare via errors.Is to distinguish
// kit-injected failures from genuine tool errors.
var ErrSimulatedFailure = errors.New("harbortest: simulated tool failure")

// simulatedFailure builds the error returned for a scheduled
// failure. The wrapped sentinel is tools.ErrToolInvalidArgs when
// the class is ErrClassPermanent (so the policy shell classifies
// the failure as permanent + non-retryable), context.DeadlineExceeded
// when the class is ErrClassTimeout (so the policy shell sees
// timeout-class), and ErrSimulatedFailure for the transient + 5xx
// classes (the policy shell's classifyError() falls through to
// ErrClassTransient for unknown wraps, which is the right default
// for a test author who said "give me a transient failure").
//
// The returned error's message is grep-friendly: it begins with
// "harbortest: simulated" so test failures are obvious in CI logs.
func simulatedFailure(toolName string, class tools.ErrorClass) error {
	switch class {
	case tools.ErrClassPermanent:
		return fmt.Errorf("harbortest: simulated %s failure for tool %q: %w",
			class, toolName, tools.ErrToolInvalidArgs)
	case tools.ErrClassTimeout:
		return fmt.Errorf("harbortest: simulated %s failure for tool %q: %w",
			class, toolName, context.DeadlineExceeded)
	default:
		// Transient / 5xx / unknown — the policy shell's default
		// classification is ErrClassTransient.
		return fmt.Errorf("harbortest: simulated %s failure for tool %q: %w",
			class, toolName, ErrSimulatedFailure)
	}
}
