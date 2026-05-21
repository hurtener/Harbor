package benchmarks

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/runtime/engine"
	"github.com/hurtener/Harbor/internal/runtime/messages"
)

// passthroughBench is the engine's perf escape hatch made concrete:
// a node with the zero-value NodePolicy (validate=none) so the
// measured number reflects the engine's intrinsic dispatch cost,
// not JSON-schema validation (brief 01 §"Validate is per-node").
func passthroughBench(_ context.Context, in messages.Envelope, _ *engine.NodeContext) (messages.Envelope, error) {
	return in, nil
}

// linearBenchGraph builds A -> B -> C: an inlet, an intermediate,
// and an outlet. Three nodes so the throughput number includes at
// least two inter-node channel hops, not a single-node degenerate
// case.
func linearBenchGraph() []engine.Adjacency {
	a := engine.Node{Name: "A", Func: passthroughBench}
	b := engine.Node{Name: "B", Func: passthroughBench}
	c := engine.Node{Name: "C", Func: passthroughBench}
	return []engine.Adjacency{
		{From: a, To: []engine.Node{b}},
		{From: b, To: []engine.Node{c}},
		{From: c, To: nil},
	}
}

func benchEnvelope(runID string) messages.Envelope {
	return messages.Envelope{
		Headers: messages.Headers{
			TenantID: "bench-tenant",
			UserID:   "bench-user",
		},
		SessionID: "bench-session",
		RunID:     runID,
	}
}

// BenchmarkEngineThroughput measures engine envelope throughput —
// envelopes/sec — under N concurrent runs against a single SHARED
// *engine.Engine. The concurrent-run shape is deliberate: brief 01
// §"Backpressure inside streaming" warns that parallel runs contend
// through shared bounded queues, so a single-run benchmark would
// under-report the realistic cost.
//
// Shape: per iteration, `runs` producer goroutines each Emit one
// envelope while a single drainer goroutine pulls exactly
// `runs` envelopes back off the engine's egress via Fetch (any-run).
// Fetch (not FetchByRun) is the right consumer here — the engine's
// dispatcher fans every egress envelope onto the any-run FIFO, so a
// throughput benchmark must drain that FIFO or the dispatcher
// back-pressures and stalls; per-run correlation is not what a
// *throughput* number needs. The benchmark reports a custom
// `envelopes/sec` metric so the baseline carries a human-meaningful
// throughput number alongside `ns/op`.
func BenchmarkEngineThroughput(b *testing.B) {
	for _, runs := range []int{1, 8, 32} {
		runs := runs
		b.Run(fmt.Sprintf("runs=%d", runs), func(b *testing.B) {
			eng, err := engine.New(linearBenchGraph(), engine.WithQueueSize(256))
			if err != nil {
				b.Fatalf("engine.New: %v", err)
			}
			if err := eng.Run(context.Background()); err != nil {
				b.Fatalf("engine.Run: %v", err)
			}
			b.Cleanup(func() { _ = eng.Stop(context.Background()) })

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				var wg sync.WaitGroup
				wg.Add(runs)
				for r := 0; r < runs; r++ {
					go func(r int) {
						defer wg.Done()
						env := benchEnvelope(fmt.Sprintf("run-%d-%d", i, r))
						if err := eng.Emit(context.Background(), env); err != nil {
							b.Errorf("Emit: %v", err)
						}
					}(r)
				}
				// Drain exactly `runs` envelopes off egress while the
				// producers run — keeps the dispatcher's any-run FIFO
				// flowing so the engine never back-pressures.
				for d := 0; d < runs; d++ {
					if _, err := eng.Fetch(context.Background()); err != nil {
						b.Fatalf("Fetch: %v", err)
					}
				}
				wg.Wait()
			}
			b.StopTimer()

			// envelopes/sec: total envelopes processed divided by
			// elapsed seconds. b.Elapsed() is the timed window.
			total := float64(b.N) * float64(runs)
			secs := b.Elapsed().Seconds()
			if secs > 0 {
				b.ReportMetric(total/secs, "envelopes/sec")
			}
		})
	}
}

// BenchmarkEngineStreamingThroughput measures the per-run streaming
// path: a node that emits K stream frames per invocation via
// EmitChunk, exercising the per-run capacity waiter (Phase 12 /
// brief 01 §"Backpressure inside streaming" — "Harbor must port
// this — it is *not* a nice-to-have"). The benchmark confirms the
// backpressure path stays fast and does not deadlock under load.
//
// The frames are drained via Fetch (any-run): EmitChunk frames land
// on the engine's egress, and the dispatcher's any-run FIFO must be
// drained or the producing node back-pressures against its run
// capacity. WithRunCapacity is sized above framesPerInvoke so the
// capacity waiter is exercised without self-deadlocking.
func BenchmarkEngineStreamingThroughput(b *testing.B) {
	const framesPerInvoke = 16

	producer := func(ctx context.Context, in messages.Envelope, nctx *engine.NodeContext) (messages.Envelope, error) {
		for s := 0; s < framesPerInvoke; s++ {
			frame := engine.StreamFrame{
				StreamID: in.RunID,
				Text:     "frame",
				Done:     s == framesPerInvoke-1,
			}
			if err := nctx.EmitChunk(ctx, frame); err != nil {
				return in, err
			}
		}
		return in, nil
	}

	node := engine.Node{Name: "producer", Func: producer}
	eng, err := engine.New([]engine.Adjacency{{From: node}}, engine.WithQueueSize(256))
	if err != nil {
		b.Fatalf("engine.New: %v", err)
	}
	if err := eng.Run(context.Background()); err != nil {
		b.Fatalf("engine.Run: %v", err)
	}
	b.Cleanup(func() { _ = eng.Stop(context.Background()) })

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		env := benchEnvelope(fmt.Sprintf("stream-run-%d", i))
		if err := eng.Emit(context.Background(), env, engine.WithRunCapacity(framesPerInvoke*2)); err != nil {
			b.Fatalf("Emit: %v", err)
		}
		// Drain everything this run puts on egress: the
		// framesPerInvoke EmitChunk frames PLUS the producer node's
		// own returned envelope (the node is the engine's sole
		// outlet, so its return value is delivered to egress too).
		// Draining all of it keeps the run fully consumed before the
		// next Emit — no run is left mid-flight at Stop.
		for f := 0; f < framesPerInvoke+1; f++ {
			if _, err := eng.Fetch(context.Background()); err != nil {
				b.Fatalf("Fetch: %v", err)
			}
		}
	}
	b.StopTimer()

	total := float64(b.N) * float64(framesPerInvoke)
	secs := b.Elapsed().Seconds()
	if secs > 0 {
		b.ReportMetric(total/secs, "frames/sec")
	}
}
