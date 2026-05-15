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
	"github.com/hurtener/Harbor/internal/memory"
)

// stubClient is a test-local llm.LLMClient that records every Complete
// call's request + identity, optionally forces an error, and returns a
// canned response. It is intentionally NOT registered through the
// production llm.Register seam — it lives in this _test.go and never
// reaches a production binary path.
type stubClient struct {
	mu       sync.Mutex
	calls    []recordedCall
	response llm.CompleteResponse
	err      error
}

type recordedCall struct {
	id      identity.Identity
	req     llm.CompleteRequest
	content string
}

func (s *stubClient) Complete(ctx context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
	id, _ := identity.From(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	rc := recordedCall{id: id, req: req}
	for _, m := range req.Messages {
		if m.Content.Text != nil {
			rc.content += *m.Content.Text + "\n"
		}
	}
	s.calls = append(s.calls, rc)
	if s.err != nil {
		return llm.CompleteResponse{}, s.err
	}
	return s.response, nil
}

func (s *stubClient) Close(_ context.Context) error { return nil }

func (s *stubClient) seenCalls() []recordedCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]recordedCall, len(s.calls))
	copy(out, s.calls)
	return out
}

func newStubClient(content string) *stubClient {
	return &stubClient{
		response: llm.CompleteResponse{Content: content},
	}
}

func mkID(session string) identity.Quadruple {
	return identity.Quadruple{
		Identity: identity.Identity{
			TenantID: "t-1", UserID: "u-1", SessionID: session,
		},
		RunID: "r-1",
	}
}

// TestNew_NilClient_FailsLoud — constraint #3 / D-089 fail-loud: a nil
// LLMClient is rejected at construction rather than left to nil-panic.
func TestNew_NilClient_FailsLoud(t *testing.T) {
	_, err := summarizer.New(nil)
	if err == nil {
		t.Fatal("New(nil) returned err=nil; want non-nil error")
	}
}

// TestSummarize_TextRoundTrip — the headline happy path: a previous
// summary + two turns produces a deterministic user-payload shape that
// the LLM client receives, and the LLM's response becomes the new
// rolling summary.
func TestSummarize_TextRoundTrip(t *testing.T) {
	client := newStubClient("updated rolling summary v2")
	s, err := summarizer.New(client)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	resp, err := s.Summarize(context.Background(), mkID("s-1"), memory.SummarizeRequest{
		PreviousSummary: "prior",
		Turns: []memory.ConversationTurn{
			{UserMessage: "u1", AssistantResponse: "a1"},
			{UserMessage: "u2", AssistantResponse: "a2"},
		},
	})
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if resp.Summary != "updated rolling summary v2" {
		t.Errorf("Summary = %q, want %q", resp.Summary, "updated rolling summary v2")
	}
	calls := client.seenCalls()
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(calls))
	}
	got := calls[0].content
	// User payload must include the previous summary AND each turn's
	// labeled pair. The shape is pinned (deterministic) so a future
	// refactor that drops one of these labels surfaces here.
	for _, want := range []string{"prior", "U: u1", "A: a1", "U: u2", "A: a2"} {
		if !strings.Contains(got, want) {
			t.Errorf("user payload missing %q\npayload:\n%s", want, got)
		}
	}
}

// TestSummarize_EmptyPreviousSummary_RendersNoneSentinel — when there
// is no prior summary, the user payload includes a stable "(none)"
// sentinel rather than an empty line. Deterministic input shape.
func TestSummarize_EmptyPreviousSummary_RendersNoneSentinel(t *testing.T) {
	client := newStubClient("summary")
	s, _ := summarizer.New(client)
	_, err := s.Summarize(context.Background(), mkID("s-1"), memory.SummarizeRequest{
		Turns: []memory.ConversationTurn{{UserMessage: "u1", AssistantResponse: "a1"}},
	})
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	calls := client.seenCalls()
	if len(calls) != 1 {
		t.Fatalf("calls = %d", len(calls))
	}
	if !strings.Contains(calls[0].content, "[Previous summary]\n(none)") {
		t.Errorf("missing '(none)' sentinel for empty prior summary; content:\n%s", calls[0].content)
	}
}

// TestSummarize_EmptyTurns_RendersNoneSentinel — symmetric to the
// previous test: an empty turns list renders "(none)".
func TestSummarize_EmptyTurns_RendersNoneSentinel(t *testing.T) {
	client := newStubClient("summary")
	s, _ := summarizer.New(client)
	_, err := s.Summarize(context.Background(), mkID("s-1"), memory.SummarizeRequest{
		PreviousSummary: "prior",
	})
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	calls := client.seenCalls()
	if !strings.Contains(calls[0].content, "[New turns]\n(none)") {
		t.Errorf("missing '(none)' sentinel for empty turns; content:\n%s", calls[0].content)
	}
}

// TestSummarize_IdentityPropagatesIntoCtx — the identity in the
// Quadruple lands on the LLM client's ctx so the safety pass (and
// every downstream layer) sees the same triple. Multi-isolation
// requires this propagation.
func TestSummarize_IdentityPropagatesIntoCtx(t *testing.T) {
	client := newStubClient("summary")
	s, _ := summarizer.New(client)
	id := mkID("s-isolation")
	_, err := s.Summarize(context.Background(), id, memory.SummarizeRequest{
		Turns: []memory.ConversationTurn{{UserMessage: "u", AssistantResponse: "a"}},
	})
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	calls := client.seenCalls()
	if calls[0].id.SessionID != "s-isolation" {
		t.Errorf("client ctx identity session=%q, want s-isolation", calls[0].id.SessionID)
	}
}

// TestSummarize_LLMError_PropagatesFailLoud — when the LLM call fails,
// the summariser surfaces the wrapped error verbatim. No silent
// degradation to an echo (CLAUDE.md §5).
func TestSummarize_LLMError_PropagatesFailLoud(t *testing.T) {
	stubErr := errors.New("llm: forced failure")
	client := &stubClient{err: stubErr}
	s, _ := summarizer.New(client)
	_, err := s.Summarize(context.Background(), mkID("s-1"), memory.SummarizeRequest{
		Turns: []memory.ConversationTurn{{UserMessage: "u", AssistantResponse: "a"}},
	})
	if err == nil {
		t.Fatal("Summarize returned nil err; want propagated LLM error")
	}
	if !errors.Is(err, stubErr) {
		t.Errorf("err = %v; want errors.Is(err, stubErr)", err)
	}
}

// TestSummarize_CtxCancelled_FailsLoud — a cancelled ctx surfaces a
// wrapped ctx.Err() rather than calling the LLM client.
func TestSummarize_CtxCancelled_FailsLoud(t *testing.T) {
	client := newStubClient("summary")
	s, _ := summarizer.New(client)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := s.Summarize(ctx, mkID("s-1"), memory.SummarizeRequest{
		Turns: []memory.ConversationTurn{{UserMessage: "u", AssistantResponse: "a"}},
	})
	if err == nil {
		t.Fatal("Summarize returned nil err on cancelled ctx")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v; want errors.Is(err, context.Canceled)", err)
	}
}

// TestSummarize_WithModel_PinsRequestModel — WithModel("foo") propagates
// onto every Complete request.
func TestSummarize_WithModel_PinsRequestModel(t *testing.T) {
	client := newStubClient("summary")
	s, _ := summarizer.New(client, summarizer.WithModel("anthropic/claude-haiku"))
	_, _ = s.Summarize(context.Background(), mkID("s-1"), memory.SummarizeRequest{
		Turns: []memory.ConversationTurn{{UserMessage: "u", AssistantResponse: "a"}},
	})
	calls := client.seenCalls()
	if calls[0].req.Model != "anthropic/claude-haiku" {
		t.Errorf("req.Model = %q, want %q", calls[0].req.Model, "anthropic/claude-haiku")
	}
}

// TestSummarize_ConcurrentReuse — D-025: one Summarizer is safe to
// share across N goroutines under -race. Each call carries its own
// session identity through to the client.
func TestSummarize_ConcurrentReuse(t *testing.T) {
	client := newStubClient("summary")
	s, _ := summarizer.New(client)

	const N = 100
	baseline := runtime.NumGoroutine()

	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			session := fmt.Sprintf("s-%d", idx)
			_, err := s.Summarize(context.Background(), mkID(session), memory.SummarizeRequest{
				PreviousSummary: "prior",
				Turns:           []memory.ConversationTurn{{UserMessage: "u", AssistantResponse: "a"}},
			})
			if err != nil {
				t.Errorf("goroutine %d: %v", idx, err)
			}
		}(i)
	}
	wg.Wait()

	// Every goroutine recorded exactly one call.
	if got := len(client.seenCalls()); got != N {
		t.Errorf("calls = %d, want %d", got, N)
	}
	// Identity bleed check: every call's session id must match one of
	// the per-goroutine sessions (no cross-talk).
	seen := make(map[string]int)
	for _, c := range client.seenCalls() {
		seen[c.id.SessionID]++
	}
	for i := 0; i < N; i++ {
		session := fmt.Sprintf("s-%d", i)
		if seen[session] != 1 {
			t.Errorf("session %s observed %d times; want exactly 1", session, seen[session])
		}
	}

	// Goroutine-leak check.
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline+4 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if leak := runtime.NumGoroutine() - baseline; leak > 4 {
		t.Errorf("goroutine leak: baseline=%d, final=%d (+%d)", baseline, runtime.NumGoroutine(), leak)
	}
}
