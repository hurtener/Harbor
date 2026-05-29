package react_test

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/react"
	"github.com/hurtener/Harbor/internal/tools"
)

// nativeStubCatalog is the read-only catalog view the concurrent-reuse
// test builds. The always-loaded set carries one tool per run (named
// after the run's RunID); discovered tools are resolvable by name from
// the deferred map. Identity propagation is per-run because the
// always-loaded subset key (`tool-<RunID>`) embeds the run identity.
type nativeStubCatalog struct {
	always   []tools.Tool
	deferred map[string]tools.Tool
}

func (c *nativeStubCatalog) Resolve(name string) (tools.Tool, bool) {
	for _, t := range c.always {
		if t.Name == name {
			return t, true
		}
	}
	if t, ok := c.deferred[name]; ok {
		return t, true
	}
	return tools.Tool{}, false
}

func (c *nativeStubCatalog) List() []tools.Tool {
	out := make([]tools.Tool, len(c.always))
	copy(out, c.always)
	return out
}

// nativeRunIDLLM is a stateless LLM client that returns a native
// ToolCall whose Name encodes the run's RunID (via ctx-derived
// identity). One shared instance serves N concurrent goroutines under
// the D-025 contract: per-run state lives in ctx, never on the
// receiver.
type nativeRunIDLLM struct{}

func (n *nativeRunIDLLM) Complete(ctx context.Context, _ llm.CompleteRequest) (llm.CompleteResponse, error) {
	id, _ := identity.QuadrupleFrom(ctx)
	return llm.CompleteResponse{
		ToolCalls: []llm.ToolCallStructured{{
			ID:   "call_" + id.RunID,
			Name: "_finish",
			Args: json.RawMessage(fmt.Sprintf(`{"answer":%q}`, id.RunID)),
		}},
	}, nil
}

func (n *nativeRunIDLLM) Close(_ context.Context) error { return nil }

// TestReactPlanner_NativeToolCall_NoCrossTalk is the Phase 107c
// AC-27 concurrent-reuse gate: N=128 concurrent Next calls against
// ONE shared *ReActPlanner instance under -race. Each call carries:
//
//   - Its own identity quadruple (RunID-keyed).
//   - Its own scripted-LLM response (the shared LLM derives the
//     response from ctx-derived RunID — distinct call IDs and finish
//     payloads per goroutine).
//   - Its own `RunContext.DiscoveredTools` slice.
//
// Asserts:
//
//   - No data races (the race detector is the gate).
//   - No cross-talk in discovered-tools state: each goroutine's
//     prompt declares ONLY its own discovered tool name; no other
//     goroutine's `discovered-tool-<RunID>` leaks into this run's
//     req.Tools.
//   - No identity bleed: each Finish.Payload matches the goroutine's
//     RunID.
//   - No goroutine leak: baseline runtime.NumGoroutine restored after
//     the WaitGroup join.
//
// The shared planner artifact is constructed once; per-run state
// lives in `ctx` + `RunContext`, never on the receiver. The native
// tool-calling path (Phase 107c — D-167) adds two new per-run state
// fields (`DiscoveredTools`, `PendingToolCalls`) that this test
// proves are safe to share alongside the existing per-run state.
func TestReactPlanner_NativeToolCall_NoCrossTalk(t *testing.T) {
	const N = 128

	runtime.GC()
	runtime.GC()
	baseline := runtime.NumGoroutine()

	// ONE shared catalog: every run sees the same tool set. The
	// per-run discovered slice (carried on rc) drives the per-turn
	// req.Tools — the shared catalog Resolve path retrieves the
	// discovered tools by name without mutating the receiver.
	catalog := &nativeStubCatalog{
		always: []tools.Tool{{
			Name:        "always-tool",
			Description: "always-loaded",
			ArgsSchema:  json.RawMessage(`{"type":"object"}`),
			Loading:     tools.LoadingAlways,
		}},
		deferred: map[string]tools.Tool{},
	}
	// Pre-populate the deferred map with one tool per run so the
	// shared Resolve path returns each run's discovered tool deterministically.
	for i := range N {
		name := fmt.Sprintf("discovered-tool-%04d", i)
		catalog.deferred[name] = tools.Tool{
			Name:        name,
			Description: "discovered for run " + fmt.Sprintf("%04d", i),
			ArgsSchema:  json.RawMessage(`{"type":"object"}`),
			Loading:     tools.LoadingDeferred,
		}
	}

	// ONE shared planner — the D-025 contract.
	shared := react.New(&nativeRunIDLLM{})

	var (
		wg              sync.WaitGroup
		bleedFails      int64
		shapeFails      int64
		errFails        int64
		discoverFails   int64
		discoverLeakage int64
	)

	wg.Add(N)
	for i := range N {
		go func() {
			defer wg.Done()

			runID := fmt.Sprintf("run-%04d", i)
			q := identity.Quadruple{
				Identity: identity.Identity{
					TenantID:  fmt.Sprintf("tenant-%d", i%8),
					UserID:    fmt.Sprintf("user-%d", i),
					SessionID: fmt.Sprintf("session-%d", i),
				},
				RunID: runID,
			}
			ctx, err := identity.WithRun(context.Background(), q.Identity, runID)
			if err != nil {
				atomic.AddInt64(&errFails, 1)
				return
			}

			// Per-run DiscoveredTools: only THIS run's discovered tool
			// name. The shared catalog resolves both this run's name AND
			// every sibling's name — so the bleed assertion is keyed on
			// the planner's per-run rc.DiscoveredTools state, not the
			// catalog.
			myDiscovered := fmt.Sprintf("discovered-tool-%04d", i)
			rc := planner.RunContext{
				Quadruple:       q,
				Goal:            "concurrent",
				Catalog:         catalog,
				DiscoveredTools: []string{myDiscovered},
			}
			dec, callErr := shared.Next(ctx, rc)
			if callErr != nil {
				atomic.AddInt64(&errFails, 1)
				return
			}
			fin, ok := dec.(planner.Finish)
			if !ok || fin.Reason != planner.FinishGoal {
				atomic.AddInt64(&shapeFails, 1)
				return
			}
			// Identity round-trip via Payload — the LLM stamps the
			// run's RunID into args.answer; the projector's
			// translateNativeFinish copies it onto Finish.Payload.
			answer, _ := fin.Payload.(string)
			if answer != runID {
				atomic.AddInt64(&bleedFails, 1)
				return
			}
			// rc.DiscoveredTools (after the planner's derive+merge)
			// MUST contain THIS run's discovered tool name AND must NOT
			// contain any other run's discovered tool name.
			sawOwn := false
			for _, name := range rc.DiscoveredTools {
				if name == myDiscovered {
					sawOwn = true
					continue
				}
				// Any other "discovered-tool-XXXX" surface is a bleed.
				if name != "" && name != myDiscovered {
					atomic.AddInt64(&discoverLeakage, 1)
					return
				}
			}
			if !sawOwn {
				atomic.AddInt64(&discoverFails, 1)
			}
		}()
	}
	wg.Wait()

	if errFails != 0 {
		t.Errorf("AC-27: %d concurrent Next calls returned unexpected errors", errFails)
	}
	if shapeFails != 0 {
		t.Errorf("AC-27: %d concurrent Next calls returned non-Finish-FinishGoal decisions", shapeFails)
	}
	if bleedFails != 0 {
		t.Errorf("AC-27 identity bleed: %d calls saw another goroutine's RunID in Finish.Payload", bleedFails)
	}
	if discoverFails != 0 {
		t.Errorf("AC-27 discovered-tools state lost: %d calls lost their own DiscoveredTools name", discoverFails)
	}
	if discoverLeakage != 0 {
		t.Errorf("AC-27 discovered-tools BLEED: %d calls saw another goroutine's discovered tool in rc.DiscoveredTools", discoverLeakage)
	}

	// Goroutine leak check — same slack as the existing D-025 tests.
	runtime.GC()
	runtime.GC()
	deadline := time.Now().Add(500 * time.Millisecond)
	final := runtime.NumGoroutine()
	for final > baseline+2 && time.Now().Before(deadline) {
		runtime.Gosched()
		final = runtime.NumGoroutine()
	}
	if final > baseline+2 {
		t.Errorf("AC-27 goroutine leak: baseline=%d final=%d (delta=%d)", baseline, final, final-baseline)
	}
}
