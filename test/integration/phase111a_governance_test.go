// phase111a_governance_test.go — Phase 111a (D-198): governance
// enforcement assembled from operator config, proven end-to-end on the
// REAL production assembly (CLAUDE.md §17.3 — no mocks at the seam; the
// mock LLM is the sanctioned D-089 dev driver, explicitly
// blank-imported).
//
// What this pins:
//
//  1. THE THREE ENFORCERS GATE A REAL ASSEMBLED STACK — a configured
//     tier's cost ceiling fails Complete with ErrBudgetExceeded +
//     `governance.budget_exceeded` on the bus; a 1-call rate bucket
//     fails the second call with ErrRateLimited +
//     `governance.rate_limited`; an over-cap MaxTokens request fails
//     with ErrMaxTokensExceeded + `governance.maxtokens_exceeded`.
//     Identity propagation is asserted on every emitted event.
//  2. The LATENT DEFAULT (D-044) holds byte-for-byte: empty
//     `identity_tiers` → every call permits, zero `governance.*`
//     events.
//  3. CROSS-SESSION ISOLATION (§6 rule 10): one session exhausting its
//     budget does not gate a sibling session.
//  4. Failure mode: a Complete with NO identity in ctx fails closed.
//  5. D-025 concurrent reuse: N=100 concurrent Completes through ONE
//     assembled (governance-wrapped) client — no cross-talk, no
//     cancellation cross-talk, goroutine baseline restored after Close.
package integration_test

import (
	"context"
	"errors"
	"fmt"
	goruntime "runtime"
	"strings"
	"sync"
	"testing"
	"time"

	// The production driver aggregator (Phase 110c, D-196).
	_ "github.com/hurtener/Harbor/internal/drivers/prod"
	// Dev-only mock LLM (D-089) — explicit opt-in, never in the
	// aggregator.
	_ "github.com/hurtener/Harbor/internal/llm/mock"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/governance"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/runtime/assemble"
)

// phase111aCfg builds a validated config whose governance block carries
// ONE tier (resolved for every identity via default_tier) — the same
// YAML shape an operator declares in harbor.yaml.
func phase111aCfg(t *testing.T, tier config.GovernanceTierConfig) *config.Config {
	t.Helper()
	cfg := config.Defaults()
	cfg.LLM.Driver = "mock"
	cfg.LLM.Model = "mock/echo"
	cfg.LLM.Timeout = 5 * time.Second
	cfg.LLM.ModelProfiles = map[string]config.LLMModelProfileConfig{
		"mock/echo": {
			ContextWindowTokens: 100000,
			TokenEstimator:      "chars_div_4",
		},
	}
	cfg.Governance.DefaultTier = "phase111a"
	cfg.Governance.IdentityTiers = map[string]config.GovernanceTierConfig{
		"phase111a": tier,
	}
	if err := cfg.ValidateCore(); err != nil {
		t.Fatalf("ValidateCore: %v", err)
	}
	return cfg
}

// phase111aStack assembles the production stack for the given tier and
// registers cleanup.
func phase111aStack(t *testing.T, tier config.GovernanceTierConfig) *assemble.Stack {
	t.Helper()
	stack, err := assemble.Assemble(context.Background(), phase111aCfg(t, tier), assemble.Options{})
	if err != nil {
		if stack != nil {
			_ = stack.Close(context.Background())
		}
		t.Fatalf("assemble: %v", err)
	}
	t.Cleanup(func() { _ = stack.Close(context.Background()) })
	return stack
}

// phase111aCtx attaches the identity TRIPLE (no run) so per-identity
// governance state keys stay stable across calls in one scenario.
func phase111aCtx(t *testing.T, session string) context.Context {
	t.Helper()
	ctx, err := identity.With(context.Background(), identity.Identity{
		TenantID:  "phase111a",
		UserID:    "operator",
		SessionID: session,
	})
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	return ctx
}

// phase111aComplete drives one Complete through the assembled
// (governance-wrapped) LLM client.
func phase111aComplete(ctx context.Context, stack *assemble.Stack, text string, maxTokens *int) (llm.CompleteResponse, error) {
	return stack.LLM.Complete(ctx, llm.CompleteRequest{
		Model:     "mock/echo",
		MaxTokens: maxTokens,
		Messages: []llm.ChatMessage{
			{Role: llm.RoleUser, Content: llm.Content{Text: &text}},
		},
	})
}

// subscribeGovernance opens an identity-filtered subscription on the
// stack's bus for the governance rejection events.
func subscribeGovernance(t *testing.T, stack *assemble.Stack, session string) events.Subscription {
	t.Helper()
	sub, err := stack.Bus.Subscribe(context.Background(), events.Filter{
		Tenant:  "phase111a",
		User:    "operator",
		Session: session,
		Types: []events.EventType{
			governance.EventTypeBudgetExceeded,
			governance.EventTypeRateLimited,
			governance.EventTypeMaxTokensExceeded,
		},
	})
	if err != nil {
		t.Fatalf("bus.Subscribe: %v", err)
	}
	t.Cleanup(sub.Cancel)
	return sub
}

// awaitGovernanceEvent blocks (bounded) for the next governance event
// and asserts its type + identity propagation.
func awaitGovernanceEvent(t *testing.T, sub events.Subscription, wantType events.EventType, session string) {
	t.Helper()
	select {
	case ev, ok := <-sub.Events():
		if !ok {
			t.Fatalf("subscription closed before %s arrived", wantType)
		}
		if ev.Type != wantType {
			t.Fatalf("event type = %s, want %s", ev.Type, wantType)
		}
		if ev.Identity.TenantID != "phase111a" || ev.Identity.UserID != "operator" || ev.Identity.SessionID != session {
			t.Errorf("identity propagation on %s: got %+v", wantType, ev.Identity)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", wantType)
	}
}

// TestE2E_Phase111a_TierEnforcement_AllThreeEnforcers — the acceptance
// E2E: cost ceiling, rate limit, and MaxTokens each gate a real
// assembled stack, with the matching `governance.*` event on the bus.
func TestE2E_Phase111a_TierEnforcement_AllThreeEnforcers(t *testing.T) {
	t.Run("cost ceiling", func(t *testing.T) {
		// Any positive ceiling below one mock call's synthetic cost:
		// call 1 permits (total 0 < ceiling) and accumulates past the
		// ceiling; call 2 fails closed.
		stack := phase111aStack(t, config.GovernanceTierConfig{BudgetCeilingUSD: 0.0000001})
		ctx := phase111aCtx(t, "s-budget")
		sub := subscribeGovernance(t, stack, "s-budget")

		resp, err := phase111aComplete(ctx, stack, "spend the budget", nil)
		if err != nil {
			t.Fatalf("first Complete: %v", err)
		}
		if !strings.Contains(resp.Content, "spend the budget") {
			t.Errorf("mock echo missing: %q", resp.Content)
		}
		_, err = phase111aComplete(ctx, stack, "one more", nil)
		if !errors.Is(err, governance.ErrBudgetExceeded) {
			t.Fatalf("second Complete: got %v, want ErrBudgetExceeded", err)
		}
		awaitGovernanceEvent(t, sub, governance.EventTypeBudgetExceeded, "s-budget")
	})

	t.Run("rate limit", func(t *testing.T) {
		// A 1-call one-shot bucket (capacity 1, no refill): the first
		// call drains it; the second underflows.
		stack := phase111aStack(t, config.GovernanceTierConfig{
			RateLimit: config.GovernanceRateLimitConfig{Capacity: 1},
		})
		ctx := phase111aCtx(t, "s-rate")
		sub := subscribeGovernance(t, stack, "s-rate")

		if _, err := phase111aComplete(ctx, stack, "first is free", nil); err != nil {
			t.Fatalf("first Complete: %v", err)
		}
		_, err := phase111aComplete(ctx, stack, "second is throttled", nil)
		if !errors.Is(err, governance.ErrRateLimited) {
			t.Fatalf("second Complete: got %v, want ErrRateLimited", err)
		}
		awaitGovernanceEvent(t, sub, governance.EventTypeRateLimited, "s-rate")
	})

	t.Run("max tokens", func(t *testing.T) {
		stack := phase111aStack(t, config.GovernanceTierConfig{MaxTokens: 50})
		ctx := phase111aCtx(t, "s-maxtokens")
		sub := subscribeGovernance(t, stack, "s-maxtokens")

		under := 50
		if _, err := phase111aComplete(ctx, stack, "at the cap", &under); err != nil {
			t.Fatalf("at-cap Complete: %v", err)
		}
		over := 100
		_, err := phase111aComplete(ctx, stack, "over the cap", &over)
		if !errors.Is(err, governance.ErrMaxTokensExceeded) {
			t.Fatalf("over-cap Complete: got %v, want ErrMaxTokensExceeded", err)
		}
		awaitGovernanceEvent(t, sub, governance.EventTypeMaxTokensExceeded, "s-maxtokens")
	})
}

// TestE2E_Phase111a_LatentDefault_EmptyTiers — D-044 golden
// pass-through: with no tiers configured the assembly clears the
// factory, every call permits, and the governance event types stay
// silent. A marker event published AFTER the calls bounds the
// assertion window without sleep-as-sync.
func TestE2E_Phase111a_LatentDefault_EmptyTiers(t *testing.T) {
	cfg := config.Defaults()
	cfg.LLM.Driver = "mock"
	cfg.LLM.Model = "mock/echo"
	cfg.LLM.Timeout = 5 * time.Second
	cfg.LLM.ModelProfiles = map[string]config.LLMModelProfileConfig{
		"mock/echo": {ContextWindowTokens: 100000, TokenEstimator: "chars_div_4"},
	}
	if err := cfg.ValidateCore(); err != nil {
		t.Fatalf("ValidateCore: %v", err)
	}
	if len(cfg.Governance.IdentityTiers) != 0 {
		t.Fatalf("Defaults() must carry the latent governance default")
	}
	stack, err := assemble.Assemble(context.Background(), cfg, assemble.Options{})
	if err != nil {
		if stack != nil {
			_ = stack.Close(context.Background())
		}
		t.Fatalf("assemble: %v", err)
	}
	defer func() { _ = stack.Close(context.Background()) }()

	ctx := phase111aCtx(t, "s-latent")
	sub, err := stack.Bus.Subscribe(context.Background(), events.Filter{
		Tenant: "phase111a", User: "operator", Session: "s-latent",
		Types: []events.EventType{
			governance.EventTypeBudgetExceeded,
			governance.EventTypeRateLimited,
			governance.EventTypeMaxTokensExceeded,
			events.EventTypeRuntimeWarning, // the marker
		},
	})
	if err != nil {
		t.Fatalf("bus.Subscribe: %v", err)
	}
	defer sub.Cancel()

	over := 1 << 20 // would trip any plausible MaxTokens cap if one were enforced
	for i := range 8 {
		if _, err := phase111aComplete(ctx, stack, fmt.Sprintf("latent call %d", i), &over); err != nil {
			t.Fatalf("call %d under the latent default must permit: %v", i, err)
		}
	}

	// Publish the marker; the subscription is FIFO per subscriber, so
	// the marker arriving FIRST proves no governance event preceded it.
	if err := stack.Bus.Publish(ctx, events.Event{
		Type: events.EventTypeRuntimeWarning,
		Identity: identity.Quadruple{Identity: identity.Identity{
			TenantID: "phase111a", UserID: "operator", SessionID: "s-latent",
		}},
		Payload: phase111aMarker{Note: "latent-marker"},
	}); err != nil {
		t.Fatalf("Publish marker: %v", err)
	}
	select {
	case ev, ok := <-sub.Events():
		if !ok {
			t.Fatalf("subscription closed before the marker")
		}
		if ev.Type != events.EventTypeRuntimeWarning {
			t.Fatalf("latent default leaked a %s event before the marker", ev.Type)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for the marker event")
	}
}

// phase111aMarker is a redactor-visible marker payload.
type phase111aMarker struct {
	events.Sealed
	Note string
}

// TestE2E_Phase111a_CrossSessionIsolation — §6 rule 10: session A
// exhausting its budget must not gate session B under the same
// (tenant, user).
func TestE2E_Phase111a_CrossSessionIsolation(t *testing.T) {
	stack := phase111aStack(t, config.GovernanceTierConfig{BudgetCeilingUSD: 0.0000001})

	ctxA := phase111aCtx(t, "s-iso-a")
	if _, err := phase111aComplete(ctxA, stack, "session A spends", nil); err != nil {
		t.Fatalf("A first Complete: %v", err)
	}
	if _, err := phase111aComplete(ctxA, stack, "session A again", nil); !errors.Is(err, governance.ErrBudgetExceeded) {
		t.Fatalf("A second Complete: got %v, want ErrBudgetExceeded", err)
	}

	ctxB := phase111aCtx(t, "s-iso-b")
	if _, err := phase111aComplete(ctxB, stack, "session B is clean", nil); err != nil {
		t.Errorf("sibling session gated by another session's budget: %v", err)
	}
}

// TestE2E_Phase111a_MissingIdentity_FailsClosed — the failure mode: a
// Complete with no identity in ctx fails closed with the typed
// governance sentinel (§6 rule 9). The sentinel assertion pins WHICH
// gate rejected it — the governance PreCall chain, per wrap.go's
// contract — not merely that something errored (Wave C checkpoint
// audit tightening; the mock echo would otherwise succeed, so a
// rejection here is the gate's). The unit-level
// TestNewSubsystemFromConfig_RejectShortCircuits_InnerNeverCalled pins
// the never-reaches-the-provider half with a call counter.
func TestE2E_Phase111a_MissingIdentity_FailsClosed(t *testing.T) {
	stack := phase111aStack(t, config.GovernanceTierConfig{BudgetCeilingUSD: 100})
	_, err := phase111aComplete(context.Background(), stack, "anonymous", nil)
	if err == nil {
		t.Fatalf("Complete without identity must fail closed")
	}
	if !errors.Is(err, governance.ErrIdentityRequired) {
		t.Fatalf("Complete without identity: got %v, want errors.Is ErrIdentityRequired (the governance gate, not an incidental failure)", err)
	}
}

// TestE2E_Phase111a_ConcurrentReuse_OneWrappedClient — D-025 against
// the assembled artifact: N=100 concurrent Completes through ONE
// governance-wrapped client under a generous (but enforcing) tier.
// Every response carries its own prompt (no context bleed), every 10th
// call runs under a pre-cancelled ctx and must fail without affecting
// its siblings (no cancellation cross-talk), and the goroutine baseline
// is restored after Close (no leaks).
func TestE2E_Phase111a_ConcurrentReuse_OneWrappedClient(t *testing.T) {
	baseline := goruntime.NumGoroutine()
	stack := phase111aStack(t, config.GovernanceTierConfig{
		BudgetCeilingUSD: 1_000,
		RateLimit: config.GovernanceRateLimitConfig{
			Capacity:       1_000_000,
			RefillTokens:   1_000_000,
			RefillInterval: time.Second,
		},
		MaxTokens: 1_000_000,
	})

	const n = 100
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx := phase111aCtx(t, fmt.Sprintf("s-conc-%d", i%10))
			prompt := fmt.Sprintf("concurrent prompt %04d", i)
			if i%10 == 9 {
				// Cancellation cross-talk probe: this call's ctx is
				// already cancelled; the call must fail and its
				// siblings must not notice.
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				if _, err := phase111aComplete(cancelled, stack, prompt, nil); err == nil {
					errs <- fmt.Errorf("call %d: pre-cancelled ctx must fail", i)
				}
				return
			}
			resp, err := phase111aComplete(ctx, stack, prompt, nil)
			if err != nil {
				errs <- fmt.Errorf("call %d: %w", i, err)
				return
			}
			if !strings.Contains(resp.Content, prompt) {
				errs <- fmt.Errorf("call %d: context bleed — response %q does not carry its own prompt", i, resp.Content)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	if err := stack.Close(context.Background()); err != nil {
		t.Errorf("Close: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		goruntime.GC()
		if goruntime.NumGoroutine() <= baseline+3 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("goroutine baseline not restored: started %d, now %d", baseline, goruntime.NumGoroutine())
}

// TestE2E_Phase111a_ClearFactoryOnClose — the Wave C checkpoint-audit
// regression gate for the assemble.go Close-side ClearFactory closer
// (D-198; the documented "Stack.Close clears it again" guarantee had
// no pinning test). A tier-bearing stack proves enforcement is LIVE,
// is then Closed, and a FRESH llm.Open — with no Assemble in between,
// so the else-branch clear cannot mask a dropped closer — must compose
// an UNWRAPPED client: the same identity that was budget-rejected
// pre-Close completes freely, with zero governance.* events (the
// marker-bounded window, no sleep-as-sync).
func TestE2E_Phase111a_ClearFactoryOnClose(t *testing.T) {
	cfg := phase111aCfg(t, config.GovernanceTierConfig{BudgetCeilingUSD: 0.0000001})
	stack, err := assemble.Assemble(context.Background(), cfg, assemble.Options{})
	if err != nil {
		if stack != nil {
			_ = stack.Close(context.Background())
		}
		t.Fatalf("assemble: %v", err)
	}
	ctx := phase111aCtx(t, "s-close-clear")

	// Prove the factory IS installed: the tiny ceiling rejects call 2.
	if _, err := phase111aComplete(ctx, stack, "spend it", nil); err != nil {
		_ = stack.Close(context.Background())
		t.Fatalf("first Complete: %v", err)
	}
	if _, err := phase111aComplete(ctx, stack, "again", nil); !errors.Is(err, governance.ErrBudgetExceeded) {
		_ = stack.Close(context.Background())
		t.Fatalf("second Complete: got %v, want ErrBudgetExceeded (factory not installed?)", err)
	}

	// Close runs the reverse closer chain — including governance.ClearFactory.
	if err := stack.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Fresh real drivers for a DIRECT llm.Open (no second Assemble).
	red, err := audit.Open(context.Background(), cfg.Audit)
	if err != nil {
		t.Fatalf("audit.Open: %v", err)
	}
	bus, err := events.Open(context.Background(), cfg.Events, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	defer func() { _ = bus.Close(context.Background()) }()
	artStore, err := artifacts.Open(context.Background(), cfg.Artifacts)
	if err != nil {
		t.Fatalf("artifacts.Open: %v", err)
	}
	defer func() { _ = artStore.Close(context.Background()) }()

	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant: "phase111a", User: "operator", Session: "s-close-clear",
		Types: []events.EventType{
			governance.EventTypeBudgetExceeded,
			governance.EventTypeRateLimited,
			governance.EventTypeMaxTokensExceeded,
			events.EventTypeRuntimeWarning, // the marker
		},
	})
	if err != nil {
		t.Fatalf("bus.Subscribe: %v", err)
	}
	defer sub.Cancel()

	client, err := llm.Open(context.Background(), llm.SnapshotFromConfig(cfg.LLM, cfg.Artifacts), llm.Deps{
		Artifacts: artStore,
		Bus:       bus,
	})
	if err != nil {
		t.Fatalf("llm.Open after Close: %v", err)
	}
	defer func() { _ = client.Close(context.Background()) }()

	// The SAME identity that was budget-rejected pre-Close: with the
	// factory cleared this client is unwrapped, so the call permits.
	text := "post-close call must be ungoverned"
	resp, err := client.Complete(ctx, llm.CompleteRequest{
		Model: "mock/echo",
		Messages: []llm.ChatMessage{
			{Role: llm.RoleUser, Content: llm.Content{Text: &text}},
		},
	})
	if err != nil {
		t.Fatalf("post-Close Complete: %v (a dropped ClearFactory closer would re-wrap or fail here)", err)
	}
	if !strings.Contains(resp.Content, "post-close call") {
		t.Errorf("mock echo missing: %q", resp.Content)
	}

	// Marker-bounded zero-event window (FIFO per subscriber): the
	// marker arriving first proves no governance event preceded it.
	if err := bus.Publish(ctx, events.Event{
		Type: events.EventTypeRuntimeWarning,
		Identity: identity.Quadruple{Identity: identity.Identity{
			TenantID: "phase111a", UserID: "operator", SessionID: "s-close-clear",
		}},
		Payload: phase111aMarker{Note: "close-clear-marker"},
	}); err != nil {
		t.Fatalf("Publish marker: %v", err)
	}
	select {
	case ev, ok := <-sub.Events():
		if !ok {
			t.Fatalf("subscription closed before the marker")
		}
		if ev.Type != events.EventTypeRuntimeWarning {
			t.Fatalf("post-Close client emitted a %s event — the factory survived Stack.Close", ev.Type)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for the marker event")
	}
}
