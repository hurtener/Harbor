// stream.go — the WithStream streaming sink for RunOnce.
//
// An embed adopter that wants live output as a run progresses — token
// deltas, planner-step boundaries, tool dispatches — registers a sink
// via the WithStream RunOption. The sink rides the SAME blocking RunOnce
// call: RunOnce still blocks and returns the terminal AnswerEnvelope,
// but the sink receives StreamEvents as they occur on the run goroutine.
//
// There is NO separate streaming package and NO sync/async method split.
// The sink is wired to the two callbacks the run loop already fires
// SYNCHRONOUSLY on the run goroutine — planner.RunContext.OnChunk (token
// + step) and steering.RunSpec.OnToolDispatched (tool dispatch). Because
// both fire inline before RunOnce returns, every StreamEvent is
// delivered before the final envelope: the ordering is deterministic,
// not a race to engineer around.

package assemble

import (
	"context"

	"github.com/hurtener/Harbor/internal/planner"
)

// StreamEventKind enumerates the streaming-sink event kinds a WithStream
// sink observes. It is a sealed string enum.
type StreamEventKind string

const (
	// StreamToken is an incremental output delta from the LLM provider —
	// the run's answer (or its reasoning trace) as it is generated. Text
	// carries the delta; Reasoning is true when the delta rides the
	// reasoning ("thinking") channel rather than the answer channel.
	StreamToken StreamEventKind = "token"

	// StreamToolDispatched marks a tool the run dispatched and the
	// executor returned for WITHOUT error. Text is empty: the dispatch is
	// the signal, and raw tool arguments/results are never streamed (they
	// route through the audit redactor — CLAUDE.md §7). The sink receives
	// one event PER dispatched tool: a parallel tool call emits N events
	// (one per branch), and spawning or awaiting a child task emits none —
	// those are runtime decisions, not tool invocations.
	StreamToolDispatched StreamEventKind = "tool_dispatched"

	// StreamStep marks a planner-step boundary — one LLM call finished
	// streaming. Reasoning distinguishes the reasoning-channel terminal
	// from the answer-channel terminal.
	StreamStep StreamEventKind = "step"
)

// StreamEvent is one observation a WithStream sink receives while RunOnce
// drives a run. It is intentionally minimal: the streaming surface
// carries progress signals (token deltas, step + tool boundaries), never
// raw tool arguments or results.
type StreamEvent struct {
	// Kind discriminates the event (token / tool_dispatched / step).
	Kind StreamEventKind
	// Text is the incremental output delta for a StreamToken event;
	// empty for StreamStep and StreamToolDispatched.
	Text string
	// Reasoning is true when a StreamToken / StreamStep event rides the
	// reasoning ("thinking") channel rather than the answer channel.
	Reasoning bool
}

// WithStream registers a per-run streaming sink on a RunOnce invocation.
// RunOnce still BLOCKS and returns the terminal planner.AnswerEnvelope;
// the sink receives StreamEvents — token deltas, planner-step
// boundaries, and tool dispatches — as they occur. Because the
// underlying callbacks fire synchronously on the run goroutine, every
// StreamEvent is delivered before RunOnce returns: the ordering is
// deterministic.
//
// The sink is called on the run goroutine, so it must not block — a
// blocking sink stalls the run; push to a buffered channel for fan-out.
// A nil sink is a no-op. Each RunOnce call captures its own sink, so
// concurrent runs against one shared Stack never cross chunks (the
// concurrent-reuse contract, CLAUDE.md §5).
//
// WithStream composes with any bus-backed streaming the stack already
// wires: the sink is additive, so a Console attached to the same run's
// event bus and an embed sink both observe the run's chunks.
func WithStream(sink func(StreamEvent)) RunOption {
	return func(c *runOnceConfig) { c.stream = sink }
}

// streamChunkEvent maps an OnChunk callback (a token delta or a
// step-done terminal) onto a StreamEvent. A done=true callback marks the
// planner-step boundary; otherwise it is a token delta.
func streamChunkEvent(delta string, done bool, kind planner.ChunkKind) StreamEvent {
	reasoning := kind == planner.ChunkReasoning
	if done {
		return StreamEvent{Kind: StreamStep, Reasoning: reasoning}
	}
	return StreamEvent{Kind: StreamToken, Text: delta, Reasoning: reasoning}
}

// streamDispatchHook builds the OnToolDispatched wrapper a WithStream
// run installs: it chains any previously-installed hook (its error
// short-circuits), then emits one StreamToolDispatched event PER
// dispatched tool — the run loop's count is 1 for a single tool call
// and len(Branches) for a parallel call, so a parallel dispatch streams
// N events, not one. Spawn/await dispatches never reach the hook
// (count 0 is skipped by the run loop), so they emit nothing.
func streamDispatchHook(prev func(ctx context.Context, count int) error, sink func(StreamEvent)) func(ctx context.Context, count int) error {
	return func(hookCtx context.Context, count int) error {
		if prev != nil {
			if err := prev(hookCtx, count); err != nil {
				return err
			}
		}
		for range count {
			sink(StreamEvent{Kind: StreamToolDispatched})
		}
		return nil
	}
}
