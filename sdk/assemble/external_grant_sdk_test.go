package assemble_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"testing"
	"time"

	"github.com/hurtener/Harbor/sdk/assemble"
	"github.com/hurtener/Harbor/sdk/config"
	"github.com/hurtener/Harbor/sdk/llm"
	"github.com/hurtener/Harbor/sdk/llm/grant"
	"github.com/hurtener/Harbor/sdk/llm/receipts"
)

type publicReceiptDelivery struct{}

func (publicReceiptDelivery) Deliver(context.Context, llm.AttemptUsageReceipt) error { return nil }

var _ receipts.Delivery = publicReceiptDelivery{}

// TestExternalGrantSurfaceIsNameableFromExternalPackage is intentionally in
// package assemble_test: a downstream embedder must be able to name the
// grant, route-mode, receipt, config, signer, and delivery seams without
// importing Harbor's internal packages.
func TestExternalGrantSurfaceIsNameableFromExternalPackage(t *testing.T) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := grant.NewSigner("key-public", "harbor-runtime", private, func() time.Time {
		return time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	signed, err := signer.Sign(llm.ExternalGrant{
		Version: 1, GrantID: "grant-public", OrganizationID: "org-public", RuntimeID: "runtime-public",
		TenantID: "tenant-public", UserID: "user-public", SessionID: "session-public", LogicalRunID: "run-public",
		RouteMode: llm.ExternalGrantRouteRuntimeDefault, PolicyGeneration: 1,
		MaxReasoning: llm.ReasoningLow, MaxOutputTokens: 64,
		Lease:    llm.ComputeLease{LeaseID: "lease-public", TokenUnits: 64, ExpiresAt: now.Add(time.Minute)},
		IssuedAt: now, ExpiresAt: now.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := grant.NewVerifier(grant.VerifierConfig{
		Audience: "harbor-runtime", RuntimeID: "runtime-public",
		RouteMode: llm.ExternalGrantRouteRuntimeDefault,
		Keys:      map[string]ed25519.PublicKey{"key-public": signer.PublicKey()},
		Clock:     func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if signed.RouteMode != llm.ExternalGrantRouteRuntimeDefault || verifier == nil {
		t.Fatalf("public grant route/verifier = %q/%v", signed.RouteMode, verifier)
	}

	cfg := config.Defaults()
	cfg.LLM.ExternalGrant = config.LLMExternalGrantConfig{
		Mode:      string(llm.ExternalGrantRequired),
		RouteMode: string(llm.ExternalGrantRouteRuntimeDefault),
		Audience:  "harbor-runtime", RuntimeID: "runtime-public",
		PublicKeys: map[string]string{
			"key-public": base64.RawURLEncoding.EncodeToString(signer.PublicKey()),
		},
	}
	opts := assemble.Options{
		ExternalGrant: llm.ExternalGrantConfig{
			Mode:        llm.ExternalGrantRequired,
			RouteMode:   llm.ExternalGrantRouteRuntimeDefault,
			Verifier:    verifier,
			ReceiptSink: nil,
		},
		ExternalGrantDelivery: publicReceiptDelivery{},
	}
	if opts.ExternalGrantDelivery == nil {
		t.Fatal("public receipt delivery was not retained in assemble options")
	}
	if string(opts.ExternalGrant.RouteMode) != cfg.LLM.ExternalGrant.RouteMode {
		t.Fatalf("public option/config route mismatch: %q/%q", opts.ExternalGrant.RouteMode, cfg.LLM.ExternalGrant.RouteMode)
	}
	if err := (publicReceiptDelivery{}).Deliver(context.Background(), llm.AttemptUsageReceipt{}); err != nil {
		t.Fatal(err)
	}
}
