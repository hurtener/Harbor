// memory_fetch_test.go — unit tests for FetchMemoryBlocks.
//
// Tests use real in-memory store drivers (no mocks at the seam — per
// CLAUDE.md §17.4). Native semantic retrieval has been retired.

package runctx_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	_ "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
	_ "github.com/hurtener/Harbor/internal/memory/drivers/inmem"
	memStrategy "github.com/hurtener/Harbor/internal/memory/strategy"
	"github.com/hurtener/Harbor/internal/runtime/runctx"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

// ---- helpers ---------------------------------------------------------------

func newFetchTestBus(t *testing.T) events.EventBus {
	t.Helper()
	red, err := audit.Open(context.Background(), config.AuditConfig{})
	if err != nil {
		t.Fatalf("audit.Open: %v", err)
	}
	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     64,
		IdleTimeout:              30 * time.Second,
		DropWindow:               time.Second,
	}, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	return bus
}

// newFetchTestStore builds the remaining pair-store fixture during retirement.
func newFetchTestStore(t *testing.T, bus events.EventBus, strategy memory.Strategy) memory.MemoryStore {
	t.Helper()
	st, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	deps := memory.Deps{
		State: st,
		Bus:   bus,
	}
	if strategy == memory.StrategyRollingSummary {
		deps.Summarizer = memStrategy.EchoSummarizer{}
	}
	mem, err := memory.Open(context.Background(), memory.ConfigSnapshot{
		Driver:   "inmem",
		Strategy: strategy,
	}, deps)
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	t.Cleanup(func() {
		_ = mem.Close(context.Background())
		_ = st.Close(context.Background())
	})
	return mem
}

func fetchTestQuad(session string) identity.Quadruple {
	return identity.Quadruple{Identity: identity.Identity{
		TenantID: "acme", UserID: "alice", SessionID: session,
	}}
}

// ---- default parity (semantic OFF) ----------------------------------------

// TestFetchMemoryBlocks_DefaultParity asserts that with recall off the
// output is identical to calling GetLLMContext + ProjectMemoryBlocks
// directly — byte-for-byte prompt parity.
func TestFetchMemoryBlocks_DefaultParity(t *testing.T) {
	t.Parallel()
	bus := newFetchTestBus(t)
	mem := newFetchTestStore(t, bus, memory.StrategyRollingSummary)
	ctx := context.Background()
	q := fetchTestQuad("sess-parity")

	// Seed a turn.
	if err := mem.AddTurn(ctx, q, memory.ConversationTurn{
		UserMessage:       "hello world",
		AssistantResponse: "hi there",
		Timestamp:         time.Now(),
	}); err != nil {
		t.Fatalf("AddTurn: %v", err)
	}

	// FetchMemoryBlocks with recall off.
	got, err := runctx.FetchMemoryBlocks(ctx, mem, q)
	if err != nil {
		t.Fatalf("FetchMemoryBlocks: %v", err)
	}

	// Direct projection via the old path.
	patch, err := mem.GetLLMContext(ctx, q)
	if err != nil {
		t.Fatalf("GetLLMContext: %v", err)
	}
	want := runctx.ProjectMemoryBlocks(patch)

	// Both should have identical Conversation tiers.
	if got == nil && want == nil {
		return
	}
	if got == nil || want == nil {
		t.Fatalf("FetchMemoryBlocks nil=%v, direct projection nil=%v", got == nil, want == nil)
	}
	// External must be nil (recall off).
	if got.External != nil {
		t.Errorf("External tier non-nil with recall off: %v", got.External)
	}
	// Conversation must match.
	gotJSON := marshalAny(t, got.Conversation)
	wantJSON := marshalAny(t, want.Conversation)
	if gotJSON != wantJSON {
		t.Errorf("Conversation mismatch:\n  got:  %s\n  want: %s", gotJSON, wantJSON)
	}
}

// ---- concurrent reuse (§11 / D-025) ----------------------------------------

// TestFetchMemoryBlocks_ConcurrentReuse preserves identity-isolated projection under race.
func TestFetchMemoryBlocks_ConcurrentReuse(t *testing.T) {
	// The leak assertion measures the whole process. Keep sibling tests out
	// of that baseline; the N=128 invocations below still run concurrently.
	bus := newFetchTestBus(t)
	mem := newFetchTestStore(t, bus, memory.StrategyTruncation)
	ctx := context.Background()

	// Seed N sessions each with a distinguishable turn.
	const N = 128
	sessions := make([]identity.Quadruple, N)
	for i := range N {
		sessions[i] = fetchTestQuad(fmt.Sprintf("sess-concurrent-%03d", i))
		if err := mem.AddTurn(ctx, sessions[i], memory.ConversationTurn{
			UserMessage:       fmt.Sprintf("unique sailing message %d", i),
			AssistantResponse: fmt.Sprintf("response for %d", i),
			Timestamp:         time.Now(),
		}); err != nil {
			t.Fatalf("AddTurn session %d: %v", i, err)
		}
	}

	// Record baseline goroutine count.
	baseline := runtime.NumGoroutine()

	var wg sync.WaitGroup
	errs := make([]error, N)
	for i := range N {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got, err := runctx.FetchMemoryBlocks(ctx, mem, sessions[i])
			errs[i] = err
			if err == nil && (got == nil || got.External != nil ||
				!strings.Contains(marshalAny(t, got.Conversation), fmt.Sprintf("unique sailing message %d", i))) {
				errs[i] = fmt.Errorf("session %d projection mismatch", i)
			}
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("session %d: FetchMemoryBlocks error: %v", i, err)
		}
	}

	// Goroutine-leak check: allow some headroom for GC lag.
	after := runtime.NumGoroutine()
	if after > baseline+10 {
		t.Errorf("possible goroutine leak: baseline=%d after=%d", baseline, after)
	}
}

// ---- nil store (helper robustness) -----------------------------------------

// TestFetchMemoryBlocks_NilStore_GetLLMContextError verifies that when
// the inner store's GetLLMContext errors, the error is propagated.
type errGetLLMStore struct {
	memory.MemoryStore
	err error
}

func (e *errGetLLMStore) GetLLMContext(_ context.Context, _ identity.Quadruple) (memory.LLMContextPatch, error) {
	return memory.LLMContextPatch{}, e.err
}

func TestFetchMemoryBlocks_GetLLMContextError_Propagates(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("test: GetLLMContext failure")
	store := &errGetLLMStore{err: sentinel}
	q := fetchTestQuad("sess-llm-err")
	_, err := runctx.FetchMemoryBlocks(context.Background(), store, q)
	if !errors.Is(err, sentinel) {
		t.Errorf("error does not wrap sentinel: %v", err)
	}
}

// ---- helpers ---------------------------------------------------------------

func marshalAny(t *testing.T, v any) string {
	t.Helper()
	if v == nil {
		return "null"
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return string(b)
}
