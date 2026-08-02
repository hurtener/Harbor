// phase145_attempt_accounting_test.go — the governance↔LLM-edge seam
// E2E for attempt-level cost accounting (D-275). It wires the REAL LLM
// wrapper chain via llm.Open (blank-imported corrections / output / retry
// wrappers seated through the SAME registry path bifrost uses) with real
// inmem state / bus / artifacts drivers, then composes governance OUTSIDE
// it via the documented headless path governance.Wrap(inner, sub) — the
// same Subsystem-factory seam assemble.Assemble installs, without touching
// the process-global SetFactory (kept out so this suite stays parallel-safe
// against other integration tests). Legs:
//
//   - happy retry + identity propagation — one corrective re-ask; the
//     accumulator folds the intermediate attempt's cost with the final
//     response's cost, and the llm.retry_with_feedback event fires scoped
//     to THIS call's (tenant, user, session) triple;
//   - retry exhaustion still accounts every attempt — all responses are
//     rejected; ErrRetryExhausted surfaces, and the accumulator total is
//     the sum of every provider attempt (non-final via the tap + the final
//     lastResp via resp.Cost), exactly once;
//   - state-store failure on PostCall surfaces loudly — a Save-failing
//     store strands the folded cost; the PostCall error names the at-risk
//     amount (observed via the wrapper's Warn), and the caller still gets
//     its normal (resp, err) outcome (PostCall errors do not supplant).
//
// Run under -race. No mocks at the seam except the scripted LLM DRIVER
// (registered through the real registry path).
package integration_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/artifacts"
	_ "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/governance"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	_ "github.com/hurtener/Harbor/internal/llm/corrections"
	_ "github.com/hurtener/Harbor/internal/llm/output"
	_ "github.com/hurtener/Harbor/internal/llm/retry"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

// phase145Driver decides its response from request CONTENT (deterministic
// under any call ordering): a request carrying the retry wrapper's
// corrective marker gets the "good" response, else the "bad" one.
type phase145Driver struct {
	badCost, goodCost float64
	alwaysBad         bool
	closed            atomic.Bool
}

func (d *phase145Driver) Complete(_ context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
	if d.closed.Load() {
		return llm.CompleteResponse{}, llm.ErrClientClosed
	}
	corrective := false
	for _, m := range req.Messages {
		if m.Content.Text != nil && strings.Contains(*m.Content.Text, "failed validation") {
			corrective = true
			break
		}
	}
	if corrective && !d.alwaysBad {
		return llm.CompleteResponse{Content: "good", Cost: llm.Cost{TotalCost: d.goodCost}}, nil
	}
	cost := d.badCost
	if corrective {
		cost = d.goodCost // second (final) rejected attempt in the exhaustion leg
	}
	return llm.CompleteResponse{Content: "bad", Cost: llm.Cost{TotalCost: cost}}, nil
}

func (d *phase145Driver) Close(_ context.Context) error {
	d.closed.CompareAndSwap(false, true)
	return nil
}

var phase145DriverSeq atomic.Int64

func registerPhase145Driver(drv *phase145Driver) string {
	name := fmt.Sprintf("phase145-scripted-%d", phase145DriverSeq.Add(1))
	llm.Register(name, func(_ llm.ConfigSnapshot, _ llm.Deps) (llm.Driver, error) { return drv, nil })
	return name
}

func phase145Bus(t *testing.T) events.EventBus {
	t.Helper()
	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     64,
		IdleTimeout:              60 * time.Second,
		DropWindow:               time.Second,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	return bus
}

func phase145Artifacts(t *testing.T) artifacts.ArtifactStore {
	t.Helper()
	store, err := artifacts.Open(context.Background(), config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("artifacts.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	return store
}

func phase145State(t *testing.T) state.StateStore {
	t.Helper()
	st, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close(context.Background()) })
	return st
}

func phase145Config(driver string, maxRetries int) llm.ConfigSnapshot {
	return llm.ConfigSnapshot{
		Driver:               driver,
		ContextWindowReserve: 0.05,
		HeavyOutputThreshold: 32_768,
		ModelProfiles:        map[string]llm.ModelProfile{"m": {ContextWindowTokens: 1000, MaxRetries: maxRetries}},
	}
}

func phase145Governed(t *testing.T, cfg llm.ConfigSnapshot, deps llm.Deps, acc governance.Subsystem) llm.LLMClient {
	t.Helper()
	inner, err := llm.Open(context.Background(), cfg, deps)
	if err != nil {
		t.Fatalf("llm.Open: %v", err)
	}
	t.Cleanup(func() { _ = inner.Close(context.Background()) })
	return governance.Wrap(inner, acc)
}

func phase145Validator(r llm.CompleteResponse) error {
	if r.Content == "good" {
		return nil
	}
	return errors.New("not good enough")
}

func phase145Request() llm.CompleteRequest {
	text := "please answer"
	return llm.CompleteRequest{
		Model:     "m",
		Messages:  []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &text}}},
		Validator: phase145Validator,
	}
}

// TestE2E_Phase145_AttemptAccounting_HappyRetry drives one corrective
// re-ask through the real chain and asserts the folded per-identity total
// plus identity-scoped retry event propagation.
func TestE2E_Phase145_AttemptAccounting_HappyRetry(t *testing.T) {
	bus := phase145Bus(t)
	st := phase145State(t)

	drv := &phase145Driver{badCost: 0.01, goodCost: 1.0}
	cfg := phase145Config(registerPhase145Driver(drv), 3)
	acc, err := governance.NewCostAccumulator(st, bus, governance.Config{})
	if err != nil {
		t.Fatalf("NewCostAccumulator: %v", err)
	}
	t.Cleanup(func() { _ = acc.Close(context.Background()) })
	client := phase145Governed(t, cfg, llm.Deps{Artifacts: phase145Artifacts(t), Bus: bus}, acc)

	id := identity.Identity{TenantID: "tnt", UserID: "usr", SessionID: "sess"}
	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant: "tnt", User: "usr", Session: "sess",
		Types: []events.EventType{llm.EventTypeRetryWithFeedback},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	ctx, err := identity.WithRun(context.Background(), id, "run-1")
	if err != nil {
		t.Fatalf("WithRun: %v", err)
	}
	resp, err := client.Complete(ctx, phase145Request())
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Content != "good" {
		t.Fatalf("Content = %q, want good", resp.Content)
	}

	// Identity propagation: the retry event is scoped to the triple.
	select {
	case ev := <-sub.Events():
		if ev.Identity.TenantID != "tnt" || ev.Identity.UserID != "usr" || ev.Identity.SessionID != "sess" {
			t.Errorf("retry event identity = %+v, want tnt/usr/sess", ev.Identity)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not observe retry_with_feedback within 2s")
	}

	// Fold: 0.01 (intermediate attempt via tap) + 1.0 (final via resp.Cost).
	q := identity.MustQuadrupleFrom(ctx)
	total, _, err := acc.Snapshot(ctx, q)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if !phase145Near(total, 1.01) {
		t.Fatalf("accumulator total = %v, want 1.01", total)
	}
}

// TestE2E_Phase145_AttemptAccounting_RetryExhaustionAccountsAll asserts
// that on retry exhaustion every provider attempt is still accounted
// exactly once.
func TestE2E_Phase145_AttemptAccounting_RetryExhaustionAccountsAll(t *testing.T) {
	bus := phase145Bus(t)
	st := phase145State(t)

	// alwaysBad: both attempts rejected. Attempt 1 → 0.01 (tap, non-final);
	// attempt 2 → 1.0 (final lastResp, returned with ErrRetryExhausted →
	// accounted via resp.Cost).
	drv := &phase145Driver{badCost: 0.01, goodCost: 1.0, alwaysBad: true}
	cfg := phase145Config(registerPhase145Driver(drv), 1)
	acc, err := governance.NewCostAccumulator(st, bus, governance.Config{})
	if err != nil {
		t.Fatalf("NewCostAccumulator: %v", err)
	}
	t.Cleanup(func() { _ = acc.Close(context.Background()) })
	client := phase145Governed(t, cfg, llm.Deps{Artifacts: phase145Artifacts(t), Bus: bus}, acc)

	ctx, err := identity.WithRun(context.Background(),
		identity.Identity{TenantID: "tnt", UserID: "usr", SessionID: "sess"}, "run-x")
	if err != nil {
		t.Fatalf("WithRun: %v", err)
	}
	_, err = client.Complete(ctx, phase145Request())
	if !errors.Is(err, llm.ErrRetryExhausted) {
		t.Fatalf("err = %v, want ErrRetryExhausted", err)
	}
	q := identity.MustQuadrupleFrom(ctx)
	total, _, err := acc.Snapshot(ctx, q)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if !phase145Near(total, 1.01) { // 0.01 (tap) + 1.0 (final lastResp)
		t.Fatalf("accumulator total = %v, want 1.01 (all attempts accounted)", total)
	}
}

// TestE2E_Phase145_AttemptAccounting_StateFailSurfacesLoud asserts that a
// PostCall persistence failure strands the folded cost LOUDLY — the
// wrapper's Warn names the at-risk amount — while the caller still receives
// its normal (resp) outcome (PostCall errors do not supplant the result).
func TestE2E_Phase145_AttemptAccounting_StateFailSurfacesLoud(t *testing.T) {
	bus := phase145Bus(t)

	// Capture the governance wrapper's Warn output.
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	drv := &phase145Driver{badCost: 0.01, goodCost: 1.0}
	cfg := phase145Config(registerPhase145Driver(drv), 3)
	// A store whose Load succeeds (keyState resolves, the fold happens) but
	// whose Save fails (persist path) → PostCall returns the loud error.
	acc, err := governance.NewCostAccumulator(&phase145SaveFailStore{inner: phase145State(t)}, bus, governance.Config{})
	if err != nil {
		t.Fatalf("NewCostAccumulator: %v", err)
	}
	t.Cleanup(func() { _ = acc.Close(context.Background()) })
	client := phase145Governed(t, cfg, llm.Deps{Artifacts: phase145Artifacts(t), Bus: bus}, acc)

	ctx, err := identity.WithRun(context.Background(),
		identity.Identity{TenantID: "tnt", UserID: "usr", SessionID: "sess"}, "run-s")
	if err != nil {
		t.Fatalf("WithRun: %v", err)
	}
	// The caller still gets its normal (good) outcome despite PostCall
	// failing (observability-only).
	resp, err := client.Complete(ctx, phase145Request())
	if err != nil {
		t.Fatalf("Complete: %v (PostCall error must not supplant the result)", err)
	}
	if resp.Content != "good" {
		t.Fatalf("Content = %q, want good", resp.Content)
	}
	logged := buf.String()
	if !strings.Contains(logged, "PostCall error") {
		t.Fatalf("expected a governance PostCall Warn; got:\n%s", logged)
	}
	// The folded at-risk amount (0.01 tap + 1.0 final = 1.0...) is named.
	if !strings.Contains(logged, "at risk") {
		t.Fatalf("PostCall Warn must name the at-risk amount; got:\n%s", logged)
	}
}

// phase145SaveFailStore wraps a real StateStore, letting Load succeed but
// forcing Save to fail so the PostCall persist path errors after a
// successful fold.
type phase145SaveFailStore struct {
	inner state.StateStore
}

func (s *phase145SaveFailStore) Save(_ context.Context, _ state.StateRecord) error {
	return errors.New("io: simulated save failure")
}

func (s *phase145SaveFailStore) SaveIf(_ context.Context, _ []state.SlotExpectation, _ state.StateRecord) error {
	return errors.New("io: simulated save failure")
}

func (s *phase145SaveFailStore) Load(ctx context.Context, q identity.Quadruple, kind string) (state.StateRecord, error) {
	return s.inner.Load(ctx, q, kind)
}

func (s *phase145SaveFailStore) LoadByEventID(ctx context.Context, id state.EventID) (state.StateRecord, error) {
	return s.inner.LoadByEventID(ctx, id)
}

func (s *phase145SaveFailStore) Delete(ctx context.Context, q identity.Quadruple, kind string) error {
	return s.inner.Delete(ctx, q, kind)
}

func (s *phase145SaveFailStore) DeleteScope(ctx context.Context, id identity.Identity) (int, error) {
	return s.inner.DeleteScope(ctx, id)
}

func (s *phase145SaveFailStore) ListKind(ctx context.Context, scope state.ListScope, kind string) ([]state.StateRecord, error) {
	return s.inner.ListKind(ctx, scope, kind)
}

func (s *phase145SaveFailStore) ListKindForIdentity(ctx context.Context, q identity.Quadruple, kind string) ([]state.StateRecord, error) {
	return s.inner.ListKindForIdentity(ctx, q, kind)
}

func (s *phase145SaveFailStore) ListKindForIdentityBounded(ctx context.Context, q identity.Quadruple, kind string, limit int) ([]state.StateRecord, error) {
	return s.inner.ListKindForIdentityBounded(ctx, q, kind, limit)
}

func (s *phase145SaveFailStore) ScanKindForTenant(ctx context.Context, scope state.ListScope, tenantID, kind string, limit int, continuation string) (state.StateScanPage, error) {
	return s.inner.ScanKindForTenant(ctx, scope, tenantID, kind, limit, continuation)
}

func (s *phase145SaveFailStore) Close(ctx context.Context) error { return s.inner.Close(ctx) }

func phase145Near(a, b float64) bool {
	const eps = 1e-9
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < eps
}
