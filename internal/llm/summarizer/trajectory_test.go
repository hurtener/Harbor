// trajectory_test.go — Phase 111e (D-202): the production LLM-backed
// planner.Summariser. Unit tests for prompt composition, the
// five-field parse, the fail-loud contract, option overrides, and the
// D-025 concurrent-reuse guarantees for the shared summariser +
// shared CompressionRunner pair.
package summarizer_test

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/llm/summarizer"
	"github.com/hurtener/Harbor/internal/planner"
)

// funcClient is a test-local llm.LLMClient whose Complete is a
// caller-supplied function — used by the concurrency tests to derive
// the response from the request so cross-run bleed is detectable.
type funcClient struct {
	fn func(ctx context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error)
}

func (f *funcClient) Complete(ctx context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
	return f.fn(ctx, req)
}

func (f *funcClient) Close(_ context.Context) error { return nil }

// trajRC builds a complete-identity RunContext for the given run id.
func trajRC(runID string) planner.RunContext {
	return planner.RunContext{
		Quadruple: identity.Quadruple{
			Identity: identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"},
			RunID:    runID,
		},
		Query: "what is the access code?",
		Goal:  "find the access code",
	}
}

// trajFixture builds a small two-step trajectory.
func trajFixture() *planner.Trajectory {
	return &planner.Trajectory{
		Query: "what is the access code?",
		Steps: []planner.Step{
			{
				Action:         map[string]any{"tool": "vault_read", "args": map[string]any{"key": "code"}},
				Observation:    map[string]any{"raw": "ACCESS-CODE-1457 plus a lot of raw bytes"},
				LLMObservation: map[string]any{"value": "ACCESS-CODE-1457"},
				ReasoningTrace: "read the vault first",
			},
			{
				Action: map[string]any{"tool": "vault_list"},
				Error:  "vault_list timed out",
			},
		},
	}
}

const goodSummaryJSON = `{
	"goals": ["find the access code"],
	"facts": ["the access code is ACCESS-CODE-1457"],
	"pending": ["report the code"],
	"last_output_digest": "vault_list timed out",
	"note": "compacted by test"
}`

func TestNewTrajectorySummariser_NilClient_FailsLoud(t *testing.T) {
	t.Parallel()
	if _, err := summarizer.NewTrajectorySummariser(nil); err == nil {
		t.Fatal("NewTrajectorySummariser(nil) returned nil error, want failure")
	}
}

func TestTrajectorySummariser_Summarise_HappyPath(t *testing.T) {
	t.Parallel()
	client := &stubClient{response: llm.CompleteResponse{Content: goodSummaryJSON}}
	s, err := summarizer.NewTrajectorySummariser(client)
	if err != nil {
		t.Fatalf("NewTrajectorySummariser: %v", err)
	}

	sum, err := s.Summarise(context.Background(), trajRC("r1"), trajFixture())
	if err != nil {
		t.Fatalf("Summarise: %v", err)
	}
	if sum == nil {
		t.Fatal("Summarise returned nil summary with nil error")
	}
	if len(sum.Facts) != 1 || !strings.Contains(sum.Facts[0], "ACCESS-CODE-1457") {
		t.Errorf("Facts = %v, want the access-code fact", sum.Facts)
	}
	if sum.Note != "compacted by test" {
		t.Errorf("Note = %q, want %q", sum.Note, "compacted by test")
	}

	calls := client.seenCalls()
	if len(calls) != 1 {
		t.Fatalf("LLM saw %d calls, want 1", len(calls))
	}
	call := calls[0]
	// Identity propagated into ctx for the LLM-edge layers.
	if call.id.TenantID != "t1" || call.id.SessionID != "s1" {
		t.Errorf("LLM call identity = %+v, want the rc's triple", call.id)
	}
	// Structured-output mode: json_schema with the five-field schema.
	if call.req.ResponseFormat == nil || call.req.ResponseFormat.Kind != llm.FormatJSONSchema {
		t.Errorf("ResponseFormat = %+v, want FormatJSONSchema", call.req.ResponseFormat)
	}
	if !strings.Contains(string(call.req.ResponseFormat.JSONSchema), "last_output_digest") {
		t.Error("JSONSchema does not carry the five-field shape")
	}
	// Payload composition: query, goal, per-step action + LLM-facing
	// observation + error render; raw reasoning stays private.
	for _, want := range []string{
		"what is the access code?",
		"find the access code",
		"vault_read",
		"ACCESS-CODE-1457",
		"vault_list timed out",
	} {
		if !strings.Contains(call.content, want) {
			t.Errorf("compaction payload missing %q\npayload:\n%s", want, call.content)
		}
	}
	if strings.Contains(call.content, "read the vault first") {
		t.Error("raw internal reasoning entered compaction input")
	}
	// D-026 discipline: the payload renders the LLM-facing projection,
	// not the raw observation.
	if strings.Contains(call.content, "a lot of raw bytes") {
		t.Error("compaction payload leaked the RAW observation; must render LLMObservation")
	}
}

func TestTrajectorySummariser_Summarise_StripsMarkdownFence(t *testing.T) {
	t.Parallel()
	fenced := "```json\n" + goodSummaryJSON + "\n```"
	client := &stubClient{response: llm.CompleteResponse{Content: fenced}}
	s, err := summarizer.NewTrajectorySummariser(client)
	if err != nil {
		t.Fatalf("NewTrajectorySummariser: %v", err)
	}
	sum, err := s.Summarise(context.Background(), trajRC("r1"), trajFixture())
	if err != nil {
		t.Fatalf("Summarise(fenced JSON): %v", err)
	}
	if len(sum.Goals) != 1 {
		t.Errorf("Goals = %v, want one goal", sum.Goals)
	}
}

func TestTrajectorySummariser_Summarise_GarbageOutput_FailsLoud(t *testing.T) {
	t.Parallel()
	client := &stubClient{response: llm.CompleteResponse{Content: "this is not json at all"}}
	s, err := summarizer.NewTrajectorySummariser(client)
	if err != nil {
		t.Fatalf("NewTrajectorySummariser: %v", err)
	}
	if _, err := s.Summarise(context.Background(), trajRC("r1"), trajFixture()); err == nil {
		t.Fatal("Summarise(garbage) returned nil error, want loud parse failure")
	}
}

func TestTrajectorySummariser_Summarise_EmptyOrVacuous_ErrEmptySummary(t *testing.T) {
	t.Parallel()
	for name, content := range map[string]string{
		"empty content": "",
		"vacuous object": `{"goals":[],"facts":[],"pending":[],` +
			`"last_output_digest":"","note":""}`,
	} {
		client := &stubClient{response: llm.CompleteResponse{Content: content}}
		s, err := summarizer.NewTrajectorySummariser(client)
		if err != nil {
			t.Fatalf("NewTrajectorySummariser: %v", err)
		}
		_, err = s.Summarise(context.Background(), trajRC("r1"), trajFixture())
		if !errors.Is(err, planner.ErrEmptySummary) {
			t.Errorf("%s: err = %v, want ErrEmptySummary", name, err)
		}
	}
}

func TestTrajectorySummariser_Summarise_LLMErrorPropagatesVerbatim(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("provider exploded")
	client := &stubClient{err: sentinel}
	s, err := summarizer.NewTrajectorySummariser(client)
	if err != nil {
		t.Fatalf("NewTrajectorySummariser: %v", err)
	}
	_, err = s.Summarise(context.Background(), trajRC("r1"), trajFixture())
	if !errors.Is(err, sentinel) {
		t.Fatalf("Summarise err = %v, want the wrapped provider error", err)
	}
}

func TestTrajectorySummariser_Summarise_NilTrajectory_FailsLoud(t *testing.T) {
	t.Parallel()
	client := &stubClient{response: llm.CompleteResponse{Content: goodSummaryJSON}}
	s, err := summarizer.NewTrajectorySummariser(client)
	if err != nil {
		t.Fatalf("NewTrajectorySummariser: %v", err)
	}
	_, err = s.Summarise(context.Background(), trajRC("r1"), nil)
	if !errors.Is(err, planner.ErrNilTrajectory) {
		t.Fatalf("Summarise(nil trajectory) err = %v, want ErrNilTrajectory", err)
	}
}

func TestTrajectorySummariser_Summarise_CancelledCtx_FailsLoud(t *testing.T) {
	t.Parallel()
	client := &stubClient{response: llm.CompleteResponse{Content: goodSummaryJSON}}
	s, err := summarizer.NewTrajectorySummariser(client)
	if err != nil {
		t.Fatalf("NewTrajectorySummariser: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Summarise(ctx, trajRC("r1"), trajFixture()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Summarise(cancelled ctx) err = %v, want context.Canceled", err)
	}
	if got := len(client.seenCalls()); got != 0 {
		t.Errorf("LLM saw %d calls on a pre-cancelled ctx, want 0", got)
	}
}

func TestTrajectorySummariser_Options_ModelPromptMaxTokens(t *testing.T) {
	t.Parallel()
	client := &stubClient{response: llm.CompleteResponse{Content: goodSummaryJSON}}
	s, err := summarizer.NewTrajectorySummariser(client,
		summarizer.WithTrajectoryModel("cheap-compactor"),
		summarizer.WithTrajectorySystemPrompt("CUSTOM COMPACTION PERSONA"),
		summarizer.WithTrajectoryMaxSummaryTokens(512),
	)
	if err != nil {
		t.Fatalf("NewTrajectorySummariser: %v", err)
	}
	if _, err := s.Summarise(context.Background(), trajRC("r1"), trajFixture()); err != nil {
		t.Fatalf("Summarise: %v", err)
	}
	call := client.seenCalls()[0]
	if call.req.Model != "cheap-compactor" {
		t.Errorf("Model = %q, want the WithTrajectoryModel override", call.req.Model)
	}
	if !strings.Contains(call.content, "CUSTOM COMPACTION PERSONA") {
		t.Error("system prompt override did not reach the request")
	}
	if call.req.MaxTokens == nil || *call.req.MaxTokens != 512 {
		t.Errorf("MaxTokens = %v, want 512", call.req.MaxTokens)
	}
}

func TestTrajectorySummariser_PromptExtensionPreservesBaseline(t *testing.T) {
	t.Parallel()
	for _, extra := range []string{"", " \n ", " Preserve the approved navigation. "} {
		client := &stubClient{response: llm.CompleteResponse{Content: goodSummaryJSON}}
		s, err := summarizer.NewTrajectorySummariser(client, summarizer.WithTrajectoryPromptExtension(extra))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Summarise(t.Context(), trajRC("r1"), trajFixture()); err != nil {
			t.Fatal(err)
		}
		text := *client.seenCalls()[0].req.Messages[0].Content.Text
		if !strings.HasPrefix(text, "You summarize historical agent execution") || !strings.Contains(text, "Do not emit checkpoint coverage") {
			t.Fatal("extension replaced baseline instructions")
		}
		if trimmed := strings.TrimSpace(extra); trimmed != "" {
			if !strings.HasSuffix(text, trimmed) || !strings.Contains(text, "extend the above; do not override it") {
				t.Fatal("operator extension not appended")
			}
		} else if strings.Contains(text, "Additional operator instructions") {
			t.Fatal("blank extension changed baseline")
		}
	}
}

func TestTrajectorySummariser_Payload_PreservesCompleteFragments(t *testing.T) {
	t.Parallel()
	client := &stubClient{response: llm.CompleteResponse{Content: goodSummaryJSON}}
	s, err := summarizer.NewTrajectorySummariser(client)
	if err != nil {
		t.Fatalf("NewTrajectorySummariser: %v", err)
	}
	tr := trajFixture()
	// A step whose only observation is RAW and oversized (the
	// pre-projection-split legacy shape) — retain it when it fits.
	big := strings.Repeat("x", 64*1024)
	tr.Steps = append(tr.Steps, planner.Step{
		Action:      map[string]any{"tool": "big_dump"},
		Observation: big,
	})
	if _, err := s.Summarise(context.Background(), trajRC("r1"), tr); err != nil {
		t.Fatalf("Summarise: %v", err)
	}
	payload := client.seenCalls()[0].content
	if !strings.Contains(payload, big) || strings.Contains(payload, "…[truncated]") {
		t.Error("permitted source body was clipped before summarization")
	}
	// Bound against the DERIVED budget rather than a re-typed literal:
	// phase 213 moved the LLM-context heavy-output threshold and a
	// hardcoded sentinel here stops tracking the thing it guards.
	if budget := llm.DefaultHeavyOutputThreshold - 4096; len(payload) > budget {
		t.Errorf("payload is %d bytes — over the derived budget %d; the per-fragment cap failed to bound it",
			len(payload), budget)
	}
}

// TestTrajectorySummariser_ConcurrentReuse_D025 — the §11 mandatory
// concurrent-reuse test for BOTH new shared artifacts: ONE
// TrajectorySummariser + ONE planner.CompressionRunner shared across
// N≥100 concurrent runs under -race. Asserts: no data races (the
// detector), no cross-run summary bleed (each run's stamped summary
// derives from ITS query), no cancellation cross-talk, and the
// goroutine baseline is restored.
func TestTrajectorySummariser_ConcurrentReuse_D025(t *testing.T) {
	const n = 120

	// The fake client derives the summary from the request payload so
	// bleed is detectable: the run marker rides the trajectory query.
	client := &funcClient{fn: func(ctx context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
		if err := ctx.Err(); err != nil {
			return llm.CompleteResponse{}, err
		}
		var payload string
		for _, m := range req.Messages {
			if m.Content.Text != nil {
				payload += *m.Content.Text
			}
		}
		marker := ""
		for i := range n {
			if strings.Contains(payload, fmt.Sprintf("run-marker-%03d", i)) {
				marker = fmt.Sprintf("run-marker-%03d", i)
				break
			}
		}
		if marker == "" {
			return llm.CompleteResponse{}, errors.New("no run marker in payload")
		}
		content := fmt.Sprintf(`{"goals":["g"],"facts":["fact for %s"],"pending":[],"last_output_digest":"d","note":"n"}`, marker)
		return llm.CompleteResponse{Content: content}, nil
	}}

	s, err := summarizer.NewTrajectorySummariser(client)
	if err != nil {
		t.Fatalf("NewTrajectorySummariser: %v", err)
	}
	runner := planner.NewCompressionRunner(s, planner.WithTokenEstimator(
		func(tr *planner.Trajectory) (int, error) { return 1_000_000, nil }, // always over budget
	))

	baseline := runtime.NumGoroutine()

	var wg sync.WaitGroup
	trajectories := make([]*planner.Trajectory, n)
	errs := make([]error, n)
	for i := range n {
		marker := fmt.Sprintf("run-marker-%03d", i)
		tr := &planner.Trajectory{Query: marker, Steps: []planner.Step{{LLMObservation: "older"}, {LLMObservation: "fresh"}}}
		trajectories[i] = tr
		rc := trajRC(fmt.Sprintf("run-%03d", i))
		rc.Query = marker
		rc.Goal = marker
		rc.Budget = planner.Budget{TokenBudget: 10}
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = runner.MaybeCompress(context.Background(), rc, tr)
		}()
	}
	wg.Wait()

	for i := range n {
		if errs[i] != nil {
			t.Fatalf("run %d: MaybeCompress: %v", i, errs[i])
		}
		sum := trajectories[i].Summary
		if sum == nil {
			t.Fatalf("run %d: summary not stamped", i)
		}
		want := fmt.Sprintf("run-marker-%03d", i)
		if len(sum.Facts) != 1 || !strings.Contains(sum.Facts[0], want) {
			t.Fatalf("run %d: summary facts = %v, want fact carrying %q — CROSS-RUN BLEED", i, sum.Facts, want)
		}
	}

	// Cancellation cross-talk: cancel half the runs mid-summarise; the
	// other half must complete normally against the SAME shared pair.
	blockCh := make(chan struct{})
	blockClient := &funcClient{fn: func(ctx context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
		var payload string
		for _, m := range req.Messages {
			if m.Content.Text != nil {
				payload += *m.Content.Text
			}
		}
		if strings.Contains(payload, "[User query]\nblocker\n") {
			select {
			case <-ctx.Done():
				return llm.CompleteResponse{}, ctx.Err()
			case <-blockCh:
				return llm.CompleteResponse{}, errors.New("blocker released without cancel")
			}
		}
		return llm.CompleteResponse{Content: goodSummaryJSON}, nil
	}}
	s2, err := summarizer.NewTrajectorySummariser(blockClient)
	if err != nil {
		t.Fatalf("NewTrajectorySummariser: %v", err)
	}
	runner2 := planner.NewCompressionRunner(s2, planner.WithTokenEstimator(
		func(tr *planner.Trajectory) (int, error) { return 1_000_000, nil },
	))

	const half = 20
	var wg2 sync.WaitGroup
	blockErrs := make([]error, half)
	freeErrs := make([]error, half)
	freeTrajs := make([]*planner.Trajectory, half)
	cancelCtx, cancelBlocked := context.WithCancel(context.Background())
	for i := range half {
		// Blocked runs: the fake client parks on ctx.Done.
		trB := &planner.Trajectory{Query: "blocker", Steps: []planner.Step{{LLMObservation: "older"}, {LLMObservation: "fresh"}}}
		rcB := trajRC(fmt.Sprintf("blocked-%03d", i))
		rcB.Budget = planner.Budget{TokenBudget: 10}
		wg2.Add(1)
		go func() {
			defer wg2.Done()
			blockErrs[i] = runner2.MaybeCompress(cancelCtx, rcB, trB)
		}()
		// Free runs: independent ctx; must succeed despite siblings
		// being cancelled mid-summarise.
		trF := &planner.Trajectory{Query: "free", Steps: []planner.Step{{LLMObservation: "older"}, {LLMObservation: "fresh"}}}
		freeTrajs[i] = trF
		rcF := trajRC(fmt.Sprintf("free-%03d", i))
		rcF.Budget = planner.Budget{TokenBudget: 10}
		wg2.Add(1)
		go func() {
			defer wg2.Done()
			freeErrs[i] = runner2.MaybeCompress(context.Background(), rcF, trF)
		}()
	}
	cancelBlocked()
	wg2.Wait()
	close(blockCh)

	for i := range half {
		if !errors.Is(blockErrs[i], context.Canceled) {
			t.Errorf("blocked run %d: err = %v, want context.Canceled", i, blockErrs[i])
		}
		if freeErrs[i] != nil {
			t.Errorf("free run %d: err = %v — cancellation cross-talk (a sibling's cancel poisoned this run)", i, freeErrs[i])
		}
		if freeTrajs[i].Summary == nil {
			t.Errorf("free run %d: summary not stamped", i)
		}
	}

	// Goroutine baseline restored (bounded wait, no sleep-as-sync).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= baseline+2 {
			break
		}
		runtime.Gosched()
		time.Sleep(5 * time.Millisecond)
	}
	if got := runtime.NumGoroutine(); got > baseline+2 {
		t.Errorf("goroutine count %d did not return to baseline %d — leak", got, baseline)
	}
}

// Every selected exchange must reach a bounded chronological request; an older
// step cannot disappear just because the aggregate exceeds one request.
func TestTrajectorySummariser_Payload_AggregateBudget_Chronological(t *testing.T) {
	t.Parallel()
	client := &stubClient{response: llm.CompleteResponse{Content: goodSummaryJSON}}
	s, err := summarizer.NewTrajectorySummariser(client)
	if err != nil {
		t.Fatalf("NewTrajectorySummariser: %v", err)
	}
	tr := trajFixture()
	tr.Steps = nil
	// Each step's observation is ~3 KiB — comfortably under the 4 KiB
	// per-fragment cap — but the step COUNT is derived from the
	// heavy-output threshold so the naive concatenation always
	// overshoots the aggregate budget, whatever that threshold is. A
	// hardcoded step count silently stopped overflowing when phase 213
	// raised the threshold.
	const chunkBytes = 3 * 1024
	steps := (llm.DefaultHeavyOutputThreshold / chunkBytes) + 12
	chunk := strings.Repeat("y", chunkBytes)
	for range steps {
		obs := chunk
		tr.Steps = append(tr.Steps, planner.Step{
			Action:         map[string]any{"tool": "chatty_tool"},
			LLMObservation: obs,
		})
	}

	if _, err := s.Summarise(context.Background(), trajRC("r-agg"), tr); err != nil {
		t.Fatalf("Summarise: %v", err)
	}
	assertChronologicalCalls(t, client.seenCalls(), steps, llm.DefaultHeavyOutputThreshold)

}

// TestTrajectorySummariser_Payload_UnderBudget_NoElision pins the
// no-op side: a short trajectory renders every step with no elision
// marker — the aggregate budget changes nothing when nothing
// overflows.
func TestTrajectorySummariser_Payload_UnderBudget_NoElision(t *testing.T) {
	t.Parallel()
	client := &stubClient{response: llm.CompleteResponse{Content: goodSummaryJSON}}
	s, err := summarizer.NewTrajectorySummariser(client)
	if err != nil {
		t.Fatalf("NewTrajectorySummariser: %v", err)
	}
	if _, err := s.Summarise(context.Background(), trajRC("r-small"), trajFixture()); err != nil {
		t.Fatalf("Summarise: %v", err)
	}
	payload := client.seenCalls()[0].content
	if strings.Contains(payload, "elided to fit") {
		t.Error("under-budget trajectory rendered an elision marker")
	}
	if !strings.Contains(payload, "Step 1 action:") {
		t.Error("under-budget trajectory dropped its first step")
	}
}

// TestTrajectorySummariser_Payload_ThreadedThresholdShrinksBudget pins
// WithTrajectoryHeavyOutputThreshold: an operator-configured threshold
// smaller than the default bounds the payload to (threshold −
// headroom), so a summariser assembled against a tightened LLM edge
// still composes a passable payload.
func TestTrajectorySummariser_Payload_ThreadedThresholdShrinksBudget(t *testing.T) {
	t.Parallel()
	const threshold = 16 * 1024
	client := &stubClient{response: llm.CompleteResponse{Content: goodSummaryJSON}}
	s, err := summarizer.NewTrajectorySummariser(client,
		summarizer.WithTrajectoryHeavyOutputThreshold(threshold))
	if err != nil {
		t.Fatalf("NewTrajectorySummariser: %v", err)
	}
	tr := trajFixture()
	tr.Steps = nil
	chunk := strings.Repeat("z", 3*1024)
	for range 20 {
		obs := chunk
		tr.Steps = append(tr.Steps, planner.Step{
			Action:         map[string]any{"tool": "chatty_tool"},
			LLMObservation: obs,
		})
	}
	if _, err := s.Summarise(context.Background(), trajRC("r-thr"), tr); err != nil {
		t.Fatalf("Summarise: %v", err)
	}
	assertChronologicalCalls(t, client.seenCalls(), 20, threshold)

}

func assertChronologicalCalls(t *testing.T, calls []recordedCall, steps, threshold int) {
	t.Helper()
	if len(calls) < 2 || len(calls) > 16 {
		t.Fatalf("got %d calls, want bounded multi-chunk summary", len(calls))
	}
	all := ""
	for i, call := range calls {
		if len(call.req.Messages) != 2 || len(call.req.Tools) != 0 || call.req.MaxTokens == nil || *call.req.MaxTokens <= 0 {
			t.Fatalf("unbounded or tool-bearing summary request %d", i)
		}
		payload := *call.req.Messages[1].Content.Text
		if len(payload) >= threshold || strings.Contains(payload, "elided to fit") || strings.Contains(payload, "…[truncated]") {
			t.Fatalf("request %d violates input bound or silently drops evidence", i)
		}
		if i > 0 && !strings.Contains(payload, `"the access code is ACCESS-CODE-1457"`) {
			t.Fatalf("request %d lost previous narrative", i)
		}
		all += payload
	}
	last := -1
	for step := 1; step <= steps; step++ {
		marker := fmt.Sprintf("Step %d action:", step)
		pos := strings.Index(all, marker)
		if strings.Count(all, marker) != 1 || pos <= last {
			t.Fatalf("exchange %d missing, duplicated or out of order", step)
		}
		last = pos
	}
}
