// tracebridge.go — the bus→tracer bridge (Phase 111f, D-203).
//
// BridgeBusToTracer is the OTel-span derivation of the canonical event
// bus, symmetric in shape and lifecycle with BridgeBusToMetrics
// (subscribe → drain goroutine → stop func; fail-loud on a nil bus /
// tracer). It closes the brief 06 "No OpenTelemetry in the runtime"
// anti-pattern's trace half: metrics got their bus bridge in PR #91
// (D-082); traces had exporters blank-imported but NewTracer was never
// constructed on a production path and no bridge existed. Phase 111f
// constructs the tracer in `assemble.Assemble` and starts this bridge
// alongside the metrics bridge.
//
// # Span model (brief 06 §"span lifecycle from events")
//
// Canonical lifecycle pairs open and end spans:
//
//   - task.started   → task.completed / task.failed / task.cancelled
//   - tool.invoked   → tool.completed / tool.failed /
//     tool.invalid_args / tool.policy_exhausted
//
// A close event whose Type carries a failure suffix (.failed,
// .invalid_args, .policy_exhausted, .cancelled) marks the span status
// codes.Error. An opener nests under the most recent still-open span
// of the SAME identity quadruple (a tool span becomes a child of its
// task span), so the trace tree mirrors the run hierarchy. Every
// non-lifecycle event attaches as a span event on the most recent
// open span for its quadruple; with no enclosing span it is recorded
// as a standalone instantaneous span so nothing is silently dropped
// (CLAUDE.md §13).
//
// Identity + run IDs ride as span attributes via Tracer.SpanFromEvent
// — the bridge never widens the metrics derivation (which stays
// Type/Producer/Node only; the brief 06 cardinality split is enforced
// by keeping the two bridges separate, not blurred).
//
// # Volume control
//
// A chatty bus produces span floods on busy runs; the bridge takes an
// events.Filter so the production assembly scopes the subscription.
// DefaultTraceBridgeFilter limits the production bridge to the
// lifecycle pairs above (streaming chunks et al. never become span
// traffic); embedders that want span events for every type pass a
// broader filter. See docs/recipes/observe-an-embedded-runtime.md.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
)

// ErrTraceBridgeMisconfigured — BridgeBusToTracer was called with a
// nil bus or tracer. Fails closed (CLAUDE.md §5), mirroring
// ErrBridgeMisconfigured on the metrics side.
var ErrTraceBridgeMisconfigured = errors.New("telemetry: BridgeBusToTracer missing a mandatory dependency")

// traceLifecycleOpeners maps each span-opening event type to the
// lifecycle kind it opens. The kind is the pairing key a closer
// matches against.
var traceLifecycleOpeners = map[events.EventType]string{
	"task.started": "task",
	"tool.invoked": "tool",
}

// traceLifecycleClosers maps each span-ending event type to the
// lifecycle kind it closes.
var traceLifecycleClosers = map[events.EventType]string{
	"task.completed":        "task",
	"task.failed":           "task",
	"task.cancelled":        "task",
	"tool.completed":        "tool",
	"tool.failed":           "tool",
	"tool.invalid_args":     "tool",
	"tool.policy_exhausted": "tool",
}

// traceFailureClosers is the subset of closers that mark the ended
// span's status codes.Error.
var traceFailureClosers = map[events.EventType]bool{
	"task.failed":           true,
	"task.cancelled":        true,
	"tool.failed":           true,
	"tool.invalid_args":     true,
	"tool.policy_exhausted": true,
}

// DefaultTraceBridgeFilter is the production assembly's subscription
// filter: a fleet-wide (Admin) subscription narrowed to the canonical
// lifecycle pairs, so a chatty bus (streaming chunks, memory traffic)
// never becomes span traffic. The Admin claim mirrors the metrics
// bridge's fleet-wide derivation (a runtime-internal infra consumer;
// it does not widen the isolation boundary — identity still rides
// every span as attributes). Embedders that want broader span events
// pass their own filter to BridgeBusToTracer.
func DefaultTraceBridgeFilter() events.Filter {
	types := make([]events.EventType, 0, len(traceLifecycleOpeners)+len(traceLifecycleClosers))
	for t := range traceLifecycleOpeners {
		types = append(types, t)
	}
	for t := range traceLifecycleClosers {
		types = append(types, t)
	}
	return events.Filter{Admin: true, Types: types}
}

// openTraceSpan is one still-open lifecycle span the bridge tracks.
// The span handle is kept (never a ctx — CLAUDE.md §5 forbids storing
// ctx in structs); parent contexts are rebuilt on demand via
// trace.ContextWithSpan.
type openTraceSpan struct {
	kind string
	span trace.Span
}

// BridgeBusToTracer wires an events.EventBus → *Tracer bridge: it
// subscribes to the bus with the supplied filter and derives OTel
// spans per the package-doc span model. The returned stop function
// cancels the subscription, joins the drain goroutine, and ends any
// still-open spans; it is idempotent. Production callers defer it at
// process teardown (the assembly registers it on the closer chain).
//
// Both bus and tracer are mandatory; a nil either returns
// ErrTraceBridgeMisconfigured. The drain goroutine runs until the
// subscription channel closes — the same lifecycle contract as
// BridgeBusToMetrics. Per-event work is in-memory span bookkeeping
// and cannot fail, so the bridge has no per-event error path.
func BridgeBusToTracer(ctx context.Context, bus events.EventBus, tracer *Tracer, f events.Filter) (stop func(), err error) {
	if bus == nil {
		return nil, fmt.Errorf("%w: events.EventBus is nil", ErrTraceBridgeMisconfigured)
	}
	if tracer == nil {
		return nil, fmt.Errorf("%w: *Tracer is nil", ErrTraceBridgeMisconfigured)
	}
	sub, err := bus.Subscribe(ctx, f)
	if err != nil {
		return nil, fmt.Errorf("telemetry: trace bridge subscribe failed: %w", err)
	}

	done := make(chan struct{})
	// open is owned exclusively by the drain goroutine — every map
	// access happens on that one goroutine, so no lock is needed.
	open := map[identity.Quadruple][]openTraceSpan{}
	go func() {
		defer close(done)
		for ev := range sub.Events() {
			bridgeEvent(tracer, open, ev)
		}
		// Subscription closed: end every still-open span so no span
		// leaks past the bridge's lifetime (a missing close event is
		// surfaced as a span event, never silently dropped).
		for q, stack := range open {
			for i := len(stack) - 1; i >= 0; i-- {
				stack[i].span.AddEvent("harbor.trace_bridge.stopped_before_close")
				stack[i].span.End()
			}
			delete(open, q)
		}
	}()

	var once sync.Once
	return func() {
		once.Do(func() {
			sub.Cancel()
			<-done
		})
	}, nil
}

// bridgeEvent applies one bus event to the open-span bookkeeping.
// Runs exclusively on the drain goroutine.
func bridgeEvent(tracer *Tracer, open map[identity.Quadruple][]openTraceSpan, ev events.Event) {
	q := ev.Identity
	stack := open[q]

	if kind, ok := traceLifecycleOpeners[ev.Type]; ok {
		// Opener: nest under the quadruple's most recent open span so
		// the trace tree mirrors the run hierarchy.
		parent := context.Background()
		if n := len(stack); n > 0 {
			parent = trace.ContextWithSpan(parent, stack[n-1].span)
		}
		_, span := tracer.SpanFromEvent(parent, ev)
		open[q] = append(stack, openTraceSpan{kind: kind, span: span})
		return
	}

	if kind, ok := traceLifecycleClosers[ev.Type]; ok {
		// Closer: end the most recent open span of the matching kind.
		for i := len(stack) - 1; i >= 0; i-- {
			if stack[i].kind != kind {
				continue
			}
			span := stack[i].span
			span.AddEvent(string(ev.Type))
			if traceFailureClosers[ev.Type] {
				span.SetStatus(codes.Error, string(ev.Type))
			}
			span.End()
			open[q] = append(stack[:i], stack[i+1:]...)
			if len(open[q]) == 0 {
				delete(open, q)
			}
			return
		}
		// No matching opener was seen (the bridge attached mid-run, or
		// the opener was filtered out). Record a standalone span so
		// the terminal event is still visible in the trace backend.
		standaloneSpan(tracer, ev)
		return
	}

	// Non-lifecycle event: attach as a span event on the quadruple's
	// most recent open span; with no enclosing span, record standalone.
	if n := len(stack); n > 0 {
		stack[n-1].span.AddEvent(string(ev.Type))
		return
	}
	standaloneSpan(tracer, ev)
}

// standaloneSpan records ev as an instantaneous span (started and
// ended immediately). Used for events with no enclosing open span so
// the bridge never silently drops an observation. A failure-suffixed
// type still carries codes.Error.
func standaloneSpan(tracer *Tracer, ev events.Event) {
	_, span := tracer.SpanFromEvent(context.Background(), ev)
	if traceFailureClosers[ev.Type] || strings.HasSuffix(string(ev.Type), ".failed") {
		span.SetStatus(codes.Error, string(ev.Type))
	}
	span.End()
}
