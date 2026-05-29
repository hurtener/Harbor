package react_test

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/react"
)

// sharedClient is the shared inner LLM client for the D-025 stress.
// It produces a deterministic per-goroutine answer by reading the
// run's identity from ctx (the planner contract: per-call state lives
// in ctx, not on the receiver — the client honours the same).
type sharedClient struct{}

func (s *sharedClient) Complete(ctx context.Context, _ llm.CompleteRequest) (llm.CompleteResponse, error) {
	id, _ := identity.QuadrupleFrom(ctx)
	// Phase 107c (D-167): emit a native `_finish` ToolCall whose
	// `answer` arg carries the run's RunID. The projector translates
	// reserved `_finish` to Finish{Goal, Payload: <RunID>}; the
	// test's per-goroutine assertion confirms each goroutine's
	// Decision carries its OWN RunID (no identity bleed).
	return llm.CompleteResponse{
		ToolCalls: []llm.ToolCallStructured{{
			ID:   fmt.Sprintf("call_%s", id.RunID),
			Name: "_finish",
			Args: json.RawMessage(fmt.Sprintf(`{"answer":%q}`, id.RunID)),
		}},
	}, nil
}

func (s *sharedClient) Close(_ context.Context) error { return nil }

// TestReactPlanner_ConcurrentReuse_D025 is the D-025 concurrent-reuse
// gate for the Phase 45 ReActPlanner. N≥100 concurrent Next calls
// against ONE shared *ReActPlanner instance. Each goroutine carries
// a unique identity quadruple; the LLM client returns a per-call
// `_finish` envelope whose payload is the run's RunID, so the test
// can assert no identity bleed at the Decision level.
//
// Asserts:
//
//   - No data races (the race detector is the gate).
//   - No identity bleed: each call's Finish.Payload (or
//     Metadata["run_id"] for the breaker / cancellation paths)
//     matches the goroutine's RunID.
//   - No cancellation cross-talk: a pre-cancelled ctx on i%5==0
//     returns ctx.Err() without affecting siblings.
//   - No goroutine leak: baseline runtime.NumGoroutine restored after
//     the WaitGroup join (within 500ms slack).
//
// N=128 (above the D-025 floor of 100; power-of-two for scheduler
// friendliness).
func TestReactPlanner_ConcurrentReuse_D025(t *testing.T) {
	const N = 128

	runtime.GC()
	runtime.GC()
	baseline := runtime.NumGoroutine()

	// ONE shared planner — the D-025 contract.
	shared := react.New(&sharedClient{})

	var (
		wg          sync.WaitGroup
		bleedFails  int64
		shapeFails  int64
		cancelFails int64
		errFails    int64
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
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			if i%5 == 0 {
				// Pre-cancel BEFORE the call — sibling goroutines
				// MUST NOT see this cancellation.
				cancel()
			}

			rc := planner.RunContext{
				Quadruple: q,
				Goal:      "d025-stress",
			}

			dec, callErr := shared.Next(ctx, rc)
			if i%5 == 0 {
				// Expected: pre-cancelled ctx returns ctx.Err().
				if callErr == nil {
					atomic.AddInt64(&cancelFails, 1)
				}
				return
			}
			if callErr != nil {
				atomic.AddInt64(&errFails, 1)
				return
			}

			fin, ok := dec.(planner.Finish)
			if !ok {
				atomic.AddInt64(&shapeFails, 1)
				return
			}
			if fin.Reason != planner.FinishGoal {
				atomic.AddInt64(&shapeFails, 1)
				return
			}
			// Identity round-trip via Payload (set by sharedClient
			// based on ctx-derived RunID; the translateFinishCall
			// path copies the LLM-emitted `args.answer`).
			answer, _ := fin.Payload.(string)
			if answer != runID {
				atomic.AddInt64(&bleedFails, 1)
			}
		}()
	}
	wg.Wait()

	if errFails != 0 {
		t.Errorf("D-025: %d concurrent Next calls returned unexpected errors", errFails)
	}
	if shapeFails != 0 {
		t.Errorf("D-025: %d concurrent Next calls returned non-Finish-FinishGoal decisions", shapeFails)
	}
	if cancelFails != 0 {
		t.Errorf("D-025: %d pre-cancelled goroutines did NOT return ctx.Err()", cancelFails)
	}
	if bleedFails != 0 {
		t.Errorf("D-025 identity bleed: %d calls saw another goroutine's RunID in Finish.Payload", bleedFails)
	}

	// Counter: shared.StepsTaken() == N - pre-cancelled count (the
	// breaker / pre-cancel paths short-circuit BEFORE the increment).
	// Count exactly the same way the loop did (i%5 == 0).
	preCancelled := 0
	for i := range N {
		if i%5 == 0 {
			preCancelled++
		}
	}
	wantSteps := int64(N - preCancelled)
	if got := shared.StepsTaken(); got != wantSteps {
		t.Errorf("StepsTaken = %d, want %d (N=%d, preCancelled=%d)", got, wantSteps, N, preCancelled)
	}

	// Goroutine leak check. Allow a small slack for the test
	// runner's own finalisers (matches the Phase 42 + Phase 44 D-025
	// pattern).
	runtime.GC()
	runtime.GC()
	deadline := time.Now().Add(500 * time.Millisecond)
	final := runtime.NumGoroutine()
	for final > baseline+2 && time.Now().Before(deadline) {
		runtime.Gosched()
		final = runtime.NumGoroutine()
	}
	if final > baseline+2 {
		t.Errorf("D-025 goroutine leak: baseline=%d final=%d (delta=%d)", baseline, final, final-baseline)
	}
}

// TestReactPlanner_CancellationDoesNotCrossTalk asserts cancelling
// one ctx does not affect concurrent siblings (D-025 cancellation
// cross-talk contract). Distinct from the bleed test above: this one
// uses a less-aggressive pattern (each goroutine has its OWN fresh
// ctx; odd-indexed ones cancel BEFORE the call).
func TestReactPlanner_CancellationDoesNotCrossTalk(t *testing.T) {
	const N = 32
	shared := react.New(&sharedClient{})

	var (
		wg          sync.WaitGroup
		siblingErrs int64
	)
	wg.Add(N)
	for i := range N {

		go func() {
			defer wg.Done()
			runID := fmt.Sprintf("r-%d", i)
			q := identity.Quadruple{
				Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"},
				RunID:    runID,
			}
			ctx, err := identity.WithRun(context.Background(), q.Identity, runID)
			if err != nil {
				atomic.AddInt64(&siblingErrs, 1)
				return
			}
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			if i%2 == 1 {
				cancel()
			}
			rc := planner.RunContext{
				Quadruple: q,
				Goal:      "cancel-stress",
			}
			_, callErr := shared.Next(ctx, rc)
			if i%2 == 0 && callErr != nil {
				atomic.AddInt64(&siblingErrs, 1)
			}
		}()
	}
	wg.Wait()

	if siblingErrs != 0 {
		t.Errorf("cancellation cross-talk: %d even-indexed calls failed despite their own ctx being live", siblingErrs)
	}
}

// TestReactPlanner_ConcurrentReuse_StructuredPromptBuilder_D025 is the
// Phase 83a concurrent-reuse gate for the structured twelve-section
// prompt builder. The builder is a compiled artifact (no mutable
// state; `extraGuidance` is set once at construction). N≥100
// concurrent Next calls against ONE shared *ReActPlanner constructed
// with WithSystemPromptExtra must pass under -race: no data races, no
// context bleed, no goroutine leak.
//
// N=128 (above the D-025 floor of 100).
func TestReactPlanner_ConcurrentReuse_StructuredPromptBuilder_D025(t *testing.T) {
	const N = 128

	runtime.GC()
	runtime.GC()
	baseline := runtime.NumGoroutine()

	// ONE shared planner with operator-supplied <additional_guidance>.
	shared := react.New(&sharedClient{},
		react.WithSystemPromptExtra("domain rule: always cite sources"))

	var (
		wg         sync.WaitGroup
		bleedFails int64
		shapeFails int64
		errFails   int64
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

			rc := planner.RunContext{
				Quadruple: q,
				Goal:      "d025-structured-prompt",
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
			if answer, _ := fin.Payload.(string); answer != runID {
				atomic.AddInt64(&bleedFails, 1)
			}
		}()
	}
	wg.Wait()

	if errFails != 0 {
		t.Errorf("D-025: %d concurrent Next calls returned unexpected errors", errFails)
	}
	if shapeFails != 0 {
		t.Errorf("D-025: %d concurrent Next calls returned non-Finish-FinishGoal decisions", shapeFails)
	}
	if bleedFails != 0 {
		t.Errorf("D-025 identity bleed: %d calls saw another goroutine's RunID", bleedFails)
	}

	runtime.GC()
	runtime.GC()
	deadline := time.Now().Add(500 * time.Millisecond)
	final := runtime.NumGoroutine()
	for final > baseline+2 && time.Now().Before(deadline) {
		runtime.Gosched()
		final = runtime.NumGoroutine()
	}
	if final > baseline+2 {
		t.Errorf("D-025 goroutine leak: baseline=%d final=%d (delta=%d)", baseline, final, final-baseline)
	}
}

// TestReactPlanner_SharedAcrossIsolatedSessions asserts that one
// planner instance produces decisions whose terminal payloads track
// per-call identity exactly (D-025 isolation guarantee). The test
// runs M sessions sequentially against the SAME planner and verifies
// each session's Finish.Payload reflects its own RunID.
func TestReactPlanner_SharedAcrossIsolatedSessions(t *testing.T) {
	t.Parallel()
	shared := react.New(&sharedClient{})
	const M = 16
	for i := range M {
		runID := fmt.Sprintf("seq-%d", i)
		q := identity.Quadruple{
			Identity: identity.Identity{
				TenantID:  fmt.Sprintf("t-%d", i),
				UserID:    fmt.Sprintf("u-%d", i),
				SessionID: fmt.Sprintf("s-%d", i),
			},
			RunID: runID,
		}
		ctx, err := identity.WithRun(context.Background(), q.Identity, runID)
		if err != nil {
			t.Fatalf("identity.WithRun: %v", err)
		}
		rc := planner.RunContext{Quadruple: q, Goal: "iso"}
		dec, err := shared.Next(ctx, rc)
		if err != nil {
			t.Fatalf("Next[%d]: %v", i, err)
		}
		fin, ok := dec.(planner.Finish)
		if !ok {
			t.Fatalf("dec[%d] = %T, want Finish", i, dec)
		}
		if got, _ := fin.Payload.(string); got != runID {
			t.Errorf("session %d payload = %q, want %q (identity isolation breach)", i, got, runID)
		}
	}
}

// recordingPerRunClient is a per-RunID recording wrapper of
// `sharedClient`. It serves the same per-RunID `_finish` envelope but
// also captures every system-prompt this run saw, so the wave-state
// D-025 stress can assert each goroutine's prompt carried only its
// own per-run state markers.
type recordingPerRunClient struct {
	mu      sync.Mutex
	systems map[string][]string // RunID → ordered system prompts
}

func newRecordingPerRunClient() *recordingPerRunClient {
	return &recordingPerRunClient{systems: make(map[string][]string)}
}

func (c *recordingPerRunClient) Complete(ctx context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
	id, _ := identity.QuadrupleFrom(ctx)
	// Concatenate every system-role message — 83a's base prompt at
	// index 0 carries PlanningHints; 83d's three injection wrappers
	// at indices 1..3 carry memory + skills. Joining lets the
	// per-run-marker assertions span the whole composed system surface.
	var sb strings.Builder
	for _, m := range req.Messages {
		if m.Role == llm.RoleSystem && m.Content.Text != nil {
			sb.WriteString(*m.Content.Text)
			sb.WriteByte('\n')
		}
	}
	c.mu.Lock()
	c.systems[id.RunID] = append(c.systems[id.RunID], sb.String())
	c.mu.Unlock()
	return llm.CompleteResponse{
		ToolCalls: []llm.ToolCallStructured{{
			ID:   fmt.Sprintf("call_%s", id.RunID),
			Name: "_finish",
			Args: json.RawMessage(fmt.Sprintf(`{"answer":%q}`, id.RunID)),
		}},
	}, nil
}

func (c *recordingPerRunClient) Close(_ context.Context) error { return nil }

func (c *recordingPerRunClient) systemsFor(runID string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	src := c.systems[runID]
	out := make([]string, len(src))
	copy(out, src)
	return out
}

// TestReactPlanner_ConcurrentReuse_PromptBandPerRunState_D025 is the
// D-025 stress for the Wave 15 prompt-band per-run state that
// `TestReactPlanner_ConcurrentReuse_StructuredPromptBuilder_D025`
// (the 83a baseline) does NOT exercise. Surfaced by the §17.5 Wave 15
// checkpoint audit (W2): the new mutable state landed by 83c/83d
// — `RepairCounters`, `PlanningHints`, `MemoryBlocks`,
// `SkillsContext` — must satisfy the same N≥100 shared-planner
// guarantees under -race.
//
// Each of N goroutines threads its own per-run state with a unique
// marker into a single shared planner. Asserts:
//
//   - The captured system prompt for each RunID contains that run's
//     own per-run markers (memory + skills + planning).
//   - No other goroutine's marker leaks into this run's prompt.
//   - No data races (-race is the CI gate).
//   - No goroutine leak: NumGoroutine returns to baseline within slack.
//
// N=128 (above the D-025 floor of 100).
func TestReactPlanner_ConcurrentReuse_PromptBandPerRunState_D025(t *testing.T) {
	const N = 128

	runtime.GC()
	runtime.GC()
	baseline := runtime.NumGoroutine()

	client := newRecordingPerRunClient()
	// ONE shared planner. WithReasoningReplay is set to text so the
	// per-run reasoning-trace replay path is also exercised under
	// concurrency (alongside the 83c/83d state).
	shared := react.New(client, react.WithReasoningReplay(planner.ReasoningReplayText))

	var (
		wg         sync.WaitGroup
		ownFails   int64
		bleedFails int64
		errFails   int64
	)

	wg.Add(N)
	for i := range N {
		go func() {
			defer wg.Done()
			// 4-digit padding makes every marker substring-disjoint
			// from every other (avoids r-001 being a prefix of r-0010).
			runID := fmt.Sprintf("r-%04d", i)
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

			marker := "marker-" + runID
			rc := planner.RunContext{
				Quadruple: q,
				Goal:      "d025-prompt-band-state",
				MemoryBlocks: &planner.MemoryBlocks{
					External: map[string]any{"data": marker},
				},
				SkillsContext: []any{
					map[string]any{"name": "skill-" + runID, "body": marker},
				},
				RepairCounters: &planner.RepairCounters{
					// Each goroutine pins its OWN counter. The shared
					// planner must read this pointer (not mutate any
					// internal state) when rendering repair guidance.
					FinishRepair: 0,
				},
				PlanningHints: &planner.PlanningHints{
					Constraints:   "hint-" + runID,
					DisallowTools: []string{"x-" + runID},
				},
			}

			if _, err := shared.Next(ctx, rc); err != nil {
				atomic.AddInt64(&errFails, 1)
				return
			}

			systems := client.systemsFor(runID)
			if len(systems) == 0 {
				atomic.AddInt64(&errFails, 1)
				return
			}
			// Own per-run markers must be present in this run's prompt.
			own := systems[0]
			if !strings.Contains(own, marker) || !strings.Contains(own, "skill-"+runID) || !strings.Contains(own, "hint-"+runID) {
				atomic.AddInt64(&ownFails, 1)
				return
			}
			// No OTHER goroutine's marker may bleed into this prompt.
			for j := range N {
				if j == i {
					continue
				}
				otherMarker := fmt.Sprintf("marker-r-%04d", j)
				if strings.Contains(own, otherMarker) {
					atomic.AddInt64(&bleedFails, 1)
					return
				}
			}
		}()
	}
	wg.Wait()

	if errFails != 0 {
		t.Errorf("D-025 prompt-band: %d concurrent calls returned unexpected errors", errFails)
	}
	if ownFails != 0 {
		t.Errorf("D-025 prompt-band: %d concurrent calls did not see their own per-run markers", ownFails)
	}
	if bleedFails != 0 {
		t.Errorf("D-025 prompt-band BLEED: %d calls saw another goroutine's marker (shared-planner contract violated)", bleedFails)
	}

	runtime.GC()
	runtime.GC()
	deadline := time.Now().Add(500 * time.Millisecond)
	final := runtime.NumGoroutine()
	for final > baseline+2 && time.Now().Before(deadline) {
		runtime.Gosched()
		final = runtime.NumGoroutine()
	}
	if final > baseline+2 {
		t.Errorf("D-025 prompt-band goroutine leak: baseline=%d final=%d (delta=%d)", baseline, final, final-baseline)
	}
}
