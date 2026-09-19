package llm_test

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/llm/grant"
	"github.com/hurtener/Harbor/internal/llm/leases"
	"github.com/hurtener/Harbor/internal/llm/summarizer"
	"github.com/hurtener/Harbor/internal/planner"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem" // A focused test uses the real state/lease seam without importing every production driver.
)

type compactionReceiptSink struct {
	mu       sync.Mutex
	receipts []llm.AttemptUsageReceipt
}

func (s *compactionReceiptSink) Enqueue(_ context.Context, r llm.AttemptUsageReceipt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.receipts = append(s.receipts, r)
	return nil
}

type governedCompactionDriver struct {
	verified, calls int
	requests        []llm.CompleteRequest
}

func (d *governedCompactionDriver) Complete(ctx context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
	d.calls++
	if _, ok := llm.VerifiedGrantContextFrom(ctx); ok {
		d.verified++
	}
	d.requests = append(d.requests, req)
	return llm.CompleteResponse{Content: maintenanceNarrative, FinishReason: "stop", Usage: llm.Usage{PromptTokens: 10, CompletionTokens: 10, TotalTokens: 20}}, nil
}
func (*governedCompactionDriver) Close(context.Context) error { return nil }

func TestCompactionGrant_ComposedClientKeepsParentAuthority(t *testing.T) {
	for _, mode := range []llm.ExternalGrantMode{llm.ExternalGrantRequired, llm.ExternalGrantOptional} {
		t.Run(string(mode), func(t *testing.T) {
			deps, cleanup := makeDeps(t)
			defer cleanup()
			now := time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC)
			clock := func() time.Time { return now }
			signer, err := grant.NewSigner("key", "audience", nil, clock)
			if err != nil {
				t.Fatal(err)
			}
			verifier, err := grant.NewVerifier(grant.VerifierConfig{Audience: "audience", RuntimeID: "runtime", Keys: map[string]ed25519.PublicKey{"key": signer.PublicKey()}, Clock: clock})
			if err != nil {
				t.Fatal(err)
			}
			id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}
			claims := llm.ExternalGrant{Version: 1, GrantID: "g", RouteMode: llm.ExternalGrantRouteRuntimeDefault, OrganizationID: "org", RuntimeID: "runtime", TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID, LogicalRunID: "run", MaxOutputTokens: 1000, MaxReasoning: llm.ReasoningLow, PolicyGeneration: 1, IssuedAt: now, ExpiresAt: now.Add(time.Hour), Lease: llm.ComputeLease{LeaseID: "lease", TokenUnits: 100000, ExpiresAt: now.Add(time.Hour)}}
			signed, err := signer.Sign(claims)
			if err != nil {
				t.Fatal(err)
			}
			initial, err := json.Marshal(signed)
			if err != nil {
				t.Fatal(err)
			}
			state, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = state.Close(context.Background()) }()
			reservations, err := leases.New(state, clock)
			if err != nil {
				t.Fatal(err)
			}
			sink := &compactionReceiptSink{}
			deps.ExternalGrant = llm.ExternalGrantConfig{Mode: mode, Verifier: verifier, Reservations: reservations, ReceiptSink: sink, ReceiptRequired: true}
			driver := &governedCompactionDriver{}
			name := uniqueDriverName("governed-summary")
			llm.Register(name, func(llm.ConfigSnapshot, llm.Deps) (llm.Driver, error) { return driver, nil })
			cfg := makeSnapshot("m", 4096)
			cfg.Model, cfg.Provider, cfg.Driver = "m", "openai", name
			cfg.DisableCorrections, cfg.DisableDowngrade, cfg.DisableRetry, cfg.DisableGovernance = true, true, true, true
			client, err := llm.Open(t.Context(), cfg, deps)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = client.Close(context.Background()) }()
			sum, err := summarizer.NewTrajectorySummariser(client)
			if err != nil {
				t.Fatal(err)
			}
			ctx, err := identity.WithVerified(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			ctx, err = identity.WithRun(ctx, id, "run")
			if err != nil {
				t.Fatal(err)
			}
			ctx = llm.WithAttemptStep(llm.WithVerifiedOrganization(ctx, "org"), 3)
			tr := &planner.Trajectory{Query: "edit"}
			for i := 0; i < 3; i++ {
				tr.Steps = append(tr.Steps, planner.Step{LLMObservation: fmt.Sprintf("receipt-%d %s", i, strings.Repeat("x", 5000))})
			}
			rc := planner.RunContext{Quadruple: identity.Quadruple{Identity: id, RunID: "run"}, Query: "edit"} // The signed parent request, not this optional carrier, is authoritative.
			preparations := 0
			ctx = llm.WithContextPreparation(ctx, llm.ContextPreparation{InputTarget: 3000, Compact: func(inner context.Context, _, _ int) (bool, error) {
				preparations++
				_, err := sum.Summarise(inner, rc, tr)
				return err == nil, err
			}})
			huge, small := strings.Repeat("x", 16000), "continue"
			request := llm.CompleteRequest{Model: "m", ExternalGrant: &signed, Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &huge}}}, RebuildMessages: func() ([]llm.ChatMessage, error) {
				return []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &small}}}, nil
			}}
			if _, err := client.Complete(ctx, request); err != nil {
				t.Fatalf("composed compaction: %v", err)
			}
			if preparations != 1 || driver.calls != 4 || driver.verified != 4 || len(sink.receipts) != 4 {
				t.Fatalf("calls/verified/receipts/preparations=%d/%d/%d/%d", driver.calls, driver.verified, len(sink.receipts), preparations)
			}
			for i, r := range sink.receipts {
				want := signed.LogicalCallID + "/step/3"
				if i < 3 {
					want += fmt.Sprintf("/compaction/%d", i+1)
				}
				if r.LogicalCallID != want || llm.ValidateAttemptUsageReceiptAgainstGrant(r, signed) != nil {
					t.Fatal("invalid receipt derivation")
				}
			}
			for _, r := range driver.requests {
				if r.MaxTokens == nil || *r.MaxTokens > 1000 {
					t.Fatal("grant ceiling not inherited")
				}
			}
			after, err := json.Marshal(signed)
			if err != nil || string(initial) != string(after) {
				t.Fatal("signed parent mutated")
			}
			if _, err := client.Complete(ctx, request); !errors.Is(err, llm.ErrExternalGrantAttemptSettled) {
				t.Fatalf("replay=%v", err)
			}
			if driver.calls != 4 {
				t.Fatal("response-loss replay spent again")
			}
		})
	}
}
