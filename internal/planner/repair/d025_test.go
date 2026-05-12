package repair_test

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/repair"
)

// perRunStubClient is a per-run llm.LLMClient: each .Run gets its
// own instance keyed by the run's identity. This lets us isolate
// each goroutine's response stream so the only shared state is the
// RepairLoop itself — which is the D-025 contract.
type perRunStubClient struct {
	mu        sync.Mutex
	responses []llm.CompleteResponse
	cursor    int
	// recorded ctx-identity per call (per-goroutine assertion)
	seenIDs []identity.Quadruple
}

func (s *perRunStubClient) Complete(ctx context.Context, _ llm.CompleteRequest) (llm.CompleteResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, _ := identity.QuadrupleFrom(ctx)
	s.seenIDs = append(s.seenIDs, id)
	if s.cursor >= len(s.responses) {
		idx := len(s.responses) - 1
		if idx < 0 {
			return llm.CompleteResponse{}, errors.New("perRunStubClient: empty response table")
		}
		return s.responses[idx], nil
	}
	resp := s.responses[s.cursor]
	s.cursor++
	return resp, nil
}

func (s *perRunStubClient) Close(_ context.Context) error { return nil }

func (s *perRunStubClient) snapshot() []identity.Quadruple {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]identity.Quadruple, len(s.seenIDs))
	copy(out, s.seenIDs)
	return out
}

// TestRepairLoop_ConcurrentReuse_D025 is the D-025 concurrent-reuse
// gate: N≥100 concurrent Run calls against ONE shared RepairLoop
// instance. Each goroutine carries a unique identity quadruple; ~half
// force a single repair via a per-goroutine validator. Asserts:
//
//   - No data races (race detector is the gate).
//   - No identity bleed: each call's RunID round-trips through the
//     per-call stub client AND, on graceful-failure paths, through
//     the emitted RepairExhaustedPayload's Identity field.
//   - No cancellation cross-talk: a pre-cancelled ctx on i%5==0
//     returns ctx.Err() without affecting siblings.
//   - No goroutine leak: baseline runtime.NumGoroutine restored after
//     the WaitGroup join.
//
// N=128 (above the D-025 floor of 100; power-of-two for scheduler
// friendliness).
func TestRepairLoop_ConcurrentReuse_D025(t *testing.T) {
	const N = 128

	runtime.GC()
	runtime.GC()
	baseline := runtime.NumGoroutine()

	// ONE shared RepairLoop instance. The D-025 contract pins it
	// safe for concurrent use.
	loop := repair.New(repair.Config{
		ArgFillEnabled:            true,
		RepairAttempts:            3,
		MaxConsecutiveArgFailures: 2,
	})

	var (
		wg              sync.WaitGroup
		bleedFails      atomic.Int64
		ctxBleedFails   atomic.Int64
		unexpectedShape atomic.Int64
		cancelFails     atomic.Int64
		emitMisses      atomic.Int64
	)

	wg.Add(N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()

			runID := fmt.Sprintf("run-%04d", i)
			id := identity.Identity{
				TenantID:  fmt.Sprintf("t-%d", i),
				UserID:    fmt.Sprintf("u-%d", i),
				SessionID: fmt.Sprintf("s-%d", i),
			}
			q := identity.Quadruple{Identity: id, RunID: runID}

			ctx, err := identity.WithRun(context.Background(), id, runID)
			if err != nil {
				t.Errorf("identity.WithRun: %v", err)
				return
			}

			// Pre-cancelled ctx every 5th goroutine — sibling
			// goroutines should still complete cleanly.
			if i%5 == 0 {
				cctx, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cctx
				_, rerr := loop.Run(ctx, makeRC(q, nil), &perRunStubClient{},
					sampleRequest(), passValidator)
				if !errors.Is(rerr, context.Canceled) {
					cancelFails.Add(1)
				}
				return
			}

			// Per-goroutine response table: every i%2==0 forces ONE
			// repair (parser-correction path), then succeeds. The
			// success envelope carries the goroutine's RunID in the
			// `reasoning` field for per-call identity assertion.
			responses := buildPerGoroutineResponses(i, runID)
			client := &perRunStubClient{responses: responses}

			// Per-goroutine emit closure — collects events for the
			// failure path. We use an atomic counter as the "saw
			// event" probe.
			var sawEmit atomic.Bool
			rc := makeRC(q, func(ev events.Event) {
				sawEmit.Store(true)
				// Per-call identity assertion on the payload.
				payload, ok := ev.Payload.(planner.RepairExhaustedPayload)
				if !ok {
					return
				}
				if payload.Identity.RunID != runID {
					ctxBleedFails.Add(1)
				}
			})

			dec, runErr := loop.Run(ctx, rc, client, sampleRequest(), passValidator)
			if runErr != nil {
				t.Errorf("[%d] unexpected Run err: %v", i, runErr)
				return
			}

			// Per-call identity assertion: verify the stub client's
			// recorded ctx-identities match this goroutine's runID.
			for _, sid := range client.snapshot() {
				if sid.RunID != runID {
					bleedFails.Add(1)
				}
			}

			switch d := dec.(type) {
			case planner.CallTool:
				// Reasoning carries the per-call RunID — confirms
				// the success-path response made it back.
				if d.Reasoning != runID {
					ctxBleedFails.Add(1)
				}
			case planner.CallParallel:
				// First branch carries the per-call RunID — confirms
				// the success-path response made it back.
				if len(d.Branches) == 0 || d.Branches[0].Reasoning != runID {
					ctxBleedFails.Add(1)
				}
			case planner.Finish:
				// Graceful-failure path. The metadata MUST carry the
				// followup signal AND the emit MUST have fired.
				if got, _ := d.Metadata["followup"].(bool); !got {
					unexpectedShape.Add(1)
				}
				if !sawEmit.Load() {
					emitMisses.Add(1)
				}
			default:
				unexpectedShape.Add(1)
			}
		}()
	}
	wg.Wait()

	if bleedFails.Load() != 0 {
		t.Errorf("D-025: %d ctx-identity bleed(s) detected at LLM-client boundary", bleedFails.Load())
	}
	if ctxBleedFails.Load() != 0 {
		t.Errorf("D-025: %d ctx-identity bleed(s) in decision payload or emit", ctxBleedFails.Load())
	}
	if unexpectedShape.Load() != 0 {
		t.Errorf("D-025: %d goroutine(s) returned unexpected Decision shape", unexpectedShape.Load())
	}
	if cancelFails.Load() != 0 {
		t.Errorf("D-025 cancellation: %d pre-cancelled ctxes did NOT return context.Canceled", cancelFails.Load())
	}
	if emitMisses.Load() != 0 {
		t.Errorf("D-025 fail-loudly: %d graceful-failure paths did NOT emit planner.repair_exhausted", emitMisses.Load())
	}

	// Goroutine leak check. Run a couple GCs to drain any test-
	// runner finalisers.
	runtime.GC()
	runtime.GC()
	deadline := time.Now().Add(500 * time.Millisecond)
	final := runtime.NumGoroutine()
	for final > baseline+2 && time.Now().Before(deadline) {
		runtime.Gosched()
		final = runtime.NumGoroutine()
	}
	if final > baseline+5 {
		t.Errorf("D-025 goroutine leak: baseline=%d final=%d delta=%d",
			baseline, final, final-baseline)
	}
}

// buildPerGoroutineResponses returns the response table for goroutine
// i. The pattern (deterministic by i) covers four shapes across
// goroutines:
//
//   - i%4 == 0: clean salvage path — one valid envelope, no repair.
//   - i%4 == 1: parser-correction path — first response is junk, then
//     a valid envelope.
//   - i%4 == 2: multi-action salvage — array of two valid envelopes.
//   - i%4 == 3: graceful-failure path — all junk, loop must Finish{NoPath}.
//
// Each success envelope carries `runID` in its reasoning field so the
// per-call identity assertion can verify no context bleed.
func buildPerGoroutineResponses(i int, runID string) []llm.CompleteResponse {
	switch i % 4 {
	case 0:
		return []llm.CompleteResponse{
			{Content: fmt.Sprintf(`{"tool":"x","args":{},"reasoning":"%s"}`, runID)},
		}
	case 1:
		return []llm.CompleteResponse{
			{Content: `junk`},
			{Content: fmt.Sprintf(`{"tool":"x","args":{},"reasoning":"%s"}`, runID)},
		}
	case 2:
		// Multi-action salvage produces CallParallel — Reasoning
		// lives on the FIRST branch only (per envelope). Round-trip
		// it via the first branch.
		return []llm.CompleteResponse{
			{Content: fmt.Sprintf(`[{"tool":"a","args":{},"reasoning":"%s"}, {"tool":"b","args":{}}]`, runID)},
		}
	default:
		// All junk — graceful-failure path.
		return []llm.CompleteResponse{{Content: `still junk`}}
	}
}

// makeRC builds a planner.RunContext with the supplied Emit closure.
func makeRC(q identity.Quadruple, emit func(events.Event)) planner.RunContext {
	return planner.RunContext{Quadruple: q, Emit: emit}
}

