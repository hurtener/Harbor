package tools

import (
	"context"
	"sync"
	"testing"
)

func TestWithInvokingAgent_RoundTrip(t *testing.T) {
	ctx := WithInvokingAgent(context.Background(), "agent-42")
	got, ok := InvokingAgentFrom(ctx)
	if !ok {
		t.Fatal("InvokingAgentFrom: want present, got absent")
	}
	if got != "agent-42" {
		t.Fatalf("InvokingAgentFrom = %q, want %q", got, "agent-42")
	}
}

func TestInvokingAgentFrom_AbsenceIsValid(t *testing.T) {
	// A bare ctx carries no provenance — the common bare-embedder case.
	if got, ok := InvokingAgentFrom(context.Background()); ok || got != "" {
		t.Fatalf("InvokingAgentFrom(empty) = (%q, %v), want (\"\", false)", got, ok)
	}
	// A nil ctx never panics and reports absence.
	if got, ok := InvokingAgentFrom(nil); ok || got != "" { //nolint:staticcheck // deliberately exercising the nil-ctx guard
		t.Fatalf("InvokingAgentFrom(nil) = (%q, %v), want (\"\", false)", got, ok)
	}
}

func TestWithInvokingAgent_EmptyIsNoOp(t *testing.T) {
	// An empty agent id must NOT stamp an empty provenance value — a
	// downstream server would otherwise see an empty agent_id key.
	ctx := WithInvokingAgent(context.Background(), "")
	if got, ok := InvokingAgentFrom(ctx); ok || got != "" {
		t.Fatalf("empty WithInvokingAgent leaked provenance: (%q, %v)", got, ok)
	}
}

func TestWithInvokingAgent_ChildOverrides(t *testing.T) {
	parent := WithInvokingAgent(context.Background(), "agent-a")
	child := WithInvokingAgent(parent, "agent-b")
	if got, _ := InvokingAgentFrom(child); got != "agent-b" {
		t.Fatalf("child override = %q, want agent-b", got)
	}
	if got, _ := InvokingAgentFrom(parent); got != "agent-a" {
		t.Fatalf("parent mutated = %q, want agent-a", got)
	}
}

func TestWithEffectiveAgentConfig_RoundTripAndAbsence(t *testing.T) {
	ctx := WithEffectiveAgentConfig(context.Background(), "agent-selected")
	if got, ok := EffectiveAgentConfigFrom(ctx); !ok || got != "agent-selected" {
		t.Fatalf("EffectiveAgentConfigFrom = (%q, %v), want (agent-selected, true)", got, ok)
	}
	if got, ok := EffectiveAgentConfigFrom(context.Background()); ok || got != "" {
		t.Fatalf("EffectiveAgentConfigFrom(empty) = (%q, %v), want (empty, false)", got, ok)
	}
	if got, ok := EffectiveAgentConfigFrom(WithEffectiveAgentConfig(context.Background(), "")); ok || got != "" {
		t.Fatalf("empty effective selection leaked authority: (%q, %v)", got, ok)
	}
}

// TestWithInvokingAgent_ConcurrentDistinctAgents proves the seam is a pure
// ctx value carrier with no shared state: N goroutines stamp distinct agent
// ids on independent child ctxs and each reads back only its own.
func TestWithInvokingAgent_ConcurrentDistinctAgents(t *testing.T) {
	t.Parallel()
	const n = 200
	base := context.Background()
	var wg sync.WaitGroup
	errs := make([]string, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			want := agentIDForIndex(i)
			ctx := WithInvokingAgent(base, want)
			if got, ok := InvokingAgentFrom(ctx); !ok || got != want {
				errs[i] = got
			}
		}(i)
	}
	wg.Wait()
	for i, e := range errs {
		if e != "" {
			t.Fatalf("goroutine %d saw agent %q, want %q", i, e, agentIDForIndex(i))
		}
	}
}

func agentIDForIndex(i int) string {
	return "agent-" + string(rune('A'+(i%26))) + "-" + itoa(i)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}
