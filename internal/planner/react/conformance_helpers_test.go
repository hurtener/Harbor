package react_test

import (
	"context"
	"sync"

	"github.com/hurtener/Harbor/internal/llm"
)

// scriptedConformanceLLM is a tiny `llm.LLMClient` shipping a
// scripted sequence of CompleteResponse contents. Used by the
// Phase 49 conformance pack's wake-mode round-trip scenario (push
// mode) where ReAct's two-step emission needs distinct LLM
// responses: SpawnTask first, Finish second.
//
// Once the script exhausts, the last response repeats forever
// (parity with the in-test `scriptedClient` shape; a runaway-loop
// bug surfaces as Finish{NoPath} via Phase 45's MaxSteps breaker
// rather than a panic).
//
// Concurrent reuse: a single instance is safe for N concurrent
// Complete calls (mutex-guarded cursor advance). The conformance
// pack's D-025 scenario does not call this shape directly (it uses
// the default Factory which builds a one-shot mock); the conformance
// pack's wake-round-trip scenario calls Next exactly twice on the
// SAME planner so the cursor advance is single-threaded under test.
type scriptedConformanceLLM struct {
	contents []string
	cursor   int
	mu       sync.Mutex
}

func newScriptedLLMForConformance(contents []string) *scriptedConformanceLLM {
	return &scriptedConformanceLLM{
		contents: contents,
	}
}

// Complete returns the next scripted content. Identity bleed is
// caller-detected — the conformance pack's wake-round-trip wires its
// own identity quadruple and asserts via the resulting Decision.
func (s *scriptedConformanceLLM) Complete(_ context.Context, _ llm.CompleteRequest) (llm.CompleteResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cursor >= len(s.contents) {
		idx := len(s.contents) - 1
		if idx < 0 {
			return llm.CompleteResponse{}, nil
		}
		return llm.CompleteResponse{Content: s.contents[idx]}, nil
	}
	out := s.contents[s.cursor]
	s.cursor++
	return llm.CompleteResponse{Content: out}, nil
}

// Close is a no-op for the conformance scripted LLM.
func (s *scriptedConformanceLLM) Close(_ context.Context) error {
	return nil
}
