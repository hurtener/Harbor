package grant

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/llm/summarizer"
	"github.com/hurtener/Harbor/internal/planner"
)

const compactionNarrative = `{"goals":["edit"],"facts":["retain prior changes"],"pending":["verify"],"last_output_digest":"read complete","note":"compact"}`

func TestCompactionGrant_EachChunkIsVerifiedMeteredAndReplaySafe(t *testing.T) {
	for _, mode := range []llm.ExternalGrantMode{llm.ExternalGrantRequired, llm.ExternalGrantOptional} {
		t.Run(string(mode), func(t *testing.T) {
			signer, err := NewSigner("compaction-key", "harbor-runtime", nil, testClock)
			if err != nil {
				t.Fatal(err)
			}
			claims := testGrant()
			claims.Lease.TokenUnits = 100000
			signed, err := signer.Sign(claims)
			if err != nil {
				t.Fatal(err)
			}
			verifier, err := NewVerifier(VerifierConfig{Audience: "harbor-runtime", RuntimeID: "runtime-1", Keys: map[string]ed25519.PublicKey{"compaction-key": signer.PublicKey()}, Clock: testClock})
			if err != nil {
				t.Fatal(err)
			}
			provider := &recordingClient{response: llm.CompleteResponse{Content: compactionNarrative, FinishReason: "stop", Usage: llm.Usage{PromptTokens: 10, CompletionTokens: 10, TotalTokens: 20}}}
			sink := &recordingSink{}
			reservations := &recordingReservations{inner: newTestReservations(t)}
			client := Wrap(provider, llm.ConfigSnapshot{}, llm.Deps{ExternalGrant: llm.ExternalGrantConfig{Mode: mode, Verifier: verifier, Credentials: newTestBinding(t), Reservations: reservations, ReceiptSink: sink, ReceiptRequired: true}})
			sum, err := summarizer.NewTrajectorySummariser(client, summarizer.WithTrajectoryHeavyOutputThreshold(8192), summarizer.WithTrajectoryMaxSummaryTokens(512))
			if err != nil {
				t.Fatal(err)
			}
			ctx := llm.WithAttemptStep(testContext(t, "org-a"), 7)
			raw, err := json.Marshal(signed)
			if err != nil {
				t.Fatal(err)
			}
			rc := planner.RunContext{Quadruple: identity.Quadruple{Identity: identity.Identity{TenantID: "tenant-a", UserID: "user-a", SessionID: "session-a"}, RunID: "run-a"}, Query: "edit", ExternalGrant: raw}
			tr := &planner.Trajectory{Query: "edit"}
			for i := range 4 {
				tr.Steps = append(tr.Steps, planner.Step{LLMObservation: fmt.Sprintf("receipt-%d %s", i, strings.Repeat("x", 2800))})
			}
			if _, err := sum.Summarise(ctx, rc, tr); err != nil {
				t.Fatalf("governed summary: %v", err)
			}
			if len(provider.requests) != 4 || len(provider.contexts) != 4 || len(sink.receipts) != 4 || len(reservations.requests) != 4 {
				t.Fatalf("unmetered or missing chunks: requests=%d verified=%d receipts=%d reservations=%d", len(provider.requests), len(provider.contexts), len(sink.receipts), len(reservations.requests))
			}
			ids := map[string]bool{}
			for i, receipt := range sink.receipts {
				want := fmt.Sprintf("%s/step/7/compaction/%d", signed.LogicalCallID, i+1)
				if receipt.LogicalCallID != want || receipt.PlannerStep != 7 || ids[receipt.ReceiptID] || receipt.TotalTokens != 20 {
					t.Fatalf("invalid maintenance receipt: %+v", receipt)
				}
				ids[receipt.ReceiptID] = true
				if err := llm.ValidateAttemptUsageReceiptAgainstGrant(receipt, signed); err != nil {
					t.Fatal(err)
				}
				wire, err := llm.MarshalCanonicalAttemptUsageReceipt(receipt)
				if err != nil {
					t.Fatal(err)
				}
				restored, err := llm.UnmarshalCanonicalAttemptUsageReceipt(wire)
				if err != nil || llm.ValidateAttemptUsageReceiptAgainstGrant(restored, signed) != nil {
					t.Fatalf("receipt round-trip: %v", err)
				}
				if provider.requests[i].ExternalGrant == nil || provider.requests[i].ExternalGrant.Signature != signed.Signature || len(provider.requests[i].Tools) != 0 {
					t.Fatal("lost signed authority or introduced tools")
				}
			}
			for _, mutation := range []func(*llm.AttemptUsageReceipt){
				func(r *llm.AttemptUsageReceipt) { r.PlannerStep++ },
				func(r *llm.AttemptUsageReceipt) { r.LogicalCallID += "0" },
				func(r *llm.AttemptUsageReceipt) { r.AttemptNonce = signed.AttemptNonce },
				func(r *llm.AttemptUsageReceipt) { r.ParentLogicalCallID = "different-root" },
			} {
				forged := sink.receipts[0]
				mutation(&forged)
				forged.ReceiptID = llm.CanonicalAttemptID(forged.GrantID, forged.LogicalCallID, forged.AttemptNonce, forged.AttemptNumber, forged.RetryNumber, forged.DowngradeNumber, forged.FallbackHop)
				forged.IdempotencyKey = forged.ReceiptID
				forged.CanonicalBodyHash, err = llm.CanonicalAttemptUsageReceiptBodyHash(forged)
				if err != nil {
					t.Fatal(err)
				}
				if err := llm.ValidateAttemptUsageReceiptAgainstGrant(forged, signed); !errors.Is(err, llm.ErrInvalidUsageReceipt) {
					t.Fatalf("forged child receipt accepted: %v", err)
				}
			}
			// The main decision owns the original planner-step identity, not a
			// maintenance reservation. A response-loss retry must not call twice.
			req := llm.CompleteRequest{Model: "model-fast", ExternalGrant: &signed}
			if _, err := client.Complete(ctx, req); err != nil {
				t.Fatalf("main decision collided with maintenance: %v", err)
			}
			if sink.receipts[4].LogicalCallID != signed.LogicalCallID+"/step/7" {
				t.Fatal("maintenance contaminated parent context")
			}
			if _, err := sum.Summarise(ctx, rc, tr); !errors.Is(err, llm.ErrExternalGrantAttemptSettled) {
				t.Fatalf("maintenance replay=%v", err)
			}
			if len(provider.requests) != 5 {
				t.Fatal("settled maintenance replay hit provider")
			}
		})
	}
}

func TestCompactionGrant_InvalidGrantCannotUseOptionalUngovernedPath(t *testing.T) {
	signer, err := NewSigner("compaction-key", "harbor-runtime", nil, testClock)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewVerifier(VerifierConfig{Audience: "harbor-runtime", RuntimeID: "runtime-1", Keys: map[string]ed25519.PublicKey{"compaction-key": signer.PublicKey()}, Clock: testClock})
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"signature", "wrong identity", "expired", "missing", "malformed"} {
		t.Run(kind, func(t *testing.T) {
			claims := testGrant()
			if kind == "expired" {
				claims.ExpiresAt = testClock().Add(-1)
				claims.IssuedAt = testClock().Add(-2)
			}
			if kind == "wrong identity" {
				claims.UserID = "someone-else"
			}
			signed, err := signer.Sign(claims)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "signature" {
				signed.Signature = "invalid"
			}
			raw, err := json.Marshal(signed)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "missing" {
				raw = nil
			}
			if kind == "malformed" {
				raw = json.RawMessage(`{"signature":"PRIVATE-MARKER"`)
			}
			provider := &recordingClient{response: llm.CompleteResponse{Content: compactionNarrative, FinishReason: "stop"}}
			mode := llm.ExternalGrantOptional
			if kind == "missing" {
				mode = llm.ExternalGrantRequired
			}
			client := Wrap(provider, llm.ConfigSnapshot{}, llm.Deps{ExternalGrant: llm.ExternalGrantConfig{Mode: mode, Verifier: verifier, Credentials: newTestBinding(t), Reservations: newTestReservations(t), ReceiptSink: &recordingSink{}}})
			sum, err := summarizer.NewTrajectorySummariser(client, summarizer.WithTrajectoryMaxSummaryTokens(512))
			if err != nil {
				t.Fatal(err)
			}
			rc := planner.RunContext{Quadruple: identity.Quadruple{Identity: identity.Identity{TenantID: "tenant-a", UserID: "user-a", SessionID: "session-a"}, RunID: "run-a"}, Query: "edit", ExternalGrant: raw}
			got, err := sum.Summarise(llm.WithAttemptStep(testContext(t, "org-a"), 1), rc, &planner.Trajectory{Query: "edit"})
			if err == nil || got != nil || len(provider.requests) != 0 {
				t.Fatalf("invalid %s authority reached provider: %v", kind, err)
			}
			if strings.Contains(err.Error(), "PRIVATE-MARKER") {
				t.Fatal("grant content leaked")
			}
		})
	}
}
