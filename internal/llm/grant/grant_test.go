package grant

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/tools"
)

func testClock() time.Time { return time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC) }

func testGrant() llm.ExternalGrant {
	now := testClock()
	return llm.ExternalGrant{
		Version: 1, GrantID: "grant-1", OrganizationID: "org-a", RuntimeID: "runtime-1",
		TenantID: "tenant-a", UserID: "user-a", SessionID: "session-a", LogicalRunID: "run-a",
		Provider: "openai", ProviderModelID: "model-fast", ProviderConnectionID: "connection-a", ProviderConnectionGeneration: 1,
		RouteID: "route-a", CredentialBindingHandle: "binding-a", CredentialAssetGeneration: 1,
		PolicyGeneration: 7, MaxReasoning: llm.ReasoningMedium, MaxOutputTokens: 1000,
		Lease:    llm.ComputeLease{LeaseID: "lease-a", TokenUnits: 2000, ExpiresAt: now.Add(10 * time.Minute)},
		IssuedAt: now, ExpiresAt: now.Add(5 * time.Minute),
	}
}

func testContext(t *testing.T, organization string) context.Context {
	t.Helper()
	id := identity.Identity{TenantID: "tenant-a", UserID: "user-a", SessionID: "session-a"}
	ctx, err := identity.WithVerified(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	ctx, err = identity.WithRun(ctx, id, "run-a")
	if err != nil {
		t.Fatal(err)
	}
	return llm.WithVerifiedOrganization(ctx, organization)
}

func TestVerifier_ExpiredGrantHasTypedStrictOutcomeAndNarrowRenewalPreflight(t *testing.T) {
	now := testClock()
	signer, err := NewSigner("key-renew", "harbor-runtime", nil, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewVerifier(VerifierConfig{
		Audience: "harbor-runtime", RuntimeID: "runtime-1",
		Keys: map[string]ed25519.PublicKey{"key-renew": signer.PublicKey()}, Clock: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	claims := testGrant()
	claims.IssuedAt = now.Add(-20 * time.Minute)
	claims.ExpiresAt = now.Add(-15 * time.Minute)
	claims.Lease.ExpiresAt = now.Add(-10 * time.Minute)
	expired, err := signer.Sign(claims)
	if err != nil {
		t.Fatal(err)
	}
	ctx := testContext(t, "org-a")
	req := llm.CompleteRequest{Model: "model-fast"}
	if err := verifier.Verify(ctx, expired, req); !errors.Is(err, llm.ErrExternalGrantExpired) {
		t.Fatalf("strict Verify=%v, want ErrExternalGrantExpired", err)
	}
	if err := verifier.VerifyRenewalPredecessor(ctx, expired, req); err != nil {
		t.Fatalf("renewal preflight rejected authenticated elapsed grant: %v", err)
	}

	tests := map[string]struct {
		grant llm.ExternalGrant
		ctx   context.Context
		req   llm.CompleteRequest
	}{
		"bad signature":         {grant: func() llm.ExternalGrant { g := expired; g.Signature = "bad"; return g }(), ctx: ctx, req: req},
		"audience mismatch":     {grant: func() llm.ExternalGrant { g := expired; g.Audience = "other"; return g }(), ctx: ctx, req: req},
		"organization mismatch": {grant: expired, ctx: testContext(t, "other-org"), req: req},
		"identity mismatch": {grant: expired, ctx: func() context.Context {
			id := identity.Identity{TenantID: "tenant-a", UserID: "other-user", SessionID: "session-a"}
			bound, bindErr := identity.WithVerified(context.Background(), id)
			if bindErr != nil {
				t.Fatal(bindErr)
			}
			bound, bindErr = identity.WithRun(bound, id, "run-a")
			if bindErr != nil {
				t.Fatal(bindErr)
			}
			return llm.WithVerifiedOrganization(bound, "org-a")
		}(), req: req},
		"model mismatch":    {grant: expired, ctx: ctx, req: llm.CompleteRequest{Model: "other-model"}},
		"reasoning exceeds": {grant: expired, ctx: ctx, req: llm.CompleteRequest{Model: "model-fast", ReasoningEffort: llm.ReasoningHigh}},
		"output exceeds": {grant: expired, ctx: ctx, req: func() llm.CompleteRequest {
			n := expired.MaxOutputTokens + 1
			return llm.CompleteRequest{Model: "model-fast", MaxTokens: &n}
		}()},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if err := verifier.VerifyRenewalPredecessor(tc.ctx, tc.grant, tc.req); err == nil {
				t.Fatal("unsafe renewal predecessor accepted")
			}
		})
	}

	futureClaims := claims
	futureClaims.IssuedAt = now.Add(2 * time.Minute)
	futureClaims.ExpiresAt = futureClaims.IssuedAt.Add(5 * time.Minute)
	futureClaims.Lease.ExpiresAt = futureClaims.IssuedAt.Add(10 * time.Minute)
	future, err := signer.Sign(futureClaims)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifier.VerifyRenewalPredecessor(ctx, future, req); !errors.Is(err, llm.ErrExternalGrantInvalid) || errors.Is(err, llm.ErrExternalGrantExpired) {
		t.Fatalf("future-issued renewal=%v, want invalid and not expired", err)
	}
}

func TestVerifier_RenewalPreflightPreservesV2EffectiveAgentBinding(t *testing.T) {
	now := testClock()
	signer, err := NewSigner("key-v2-renew", "harbor-runtime", nil, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewVerifier(VerifierConfig{Audience: "harbor-runtime", RuntimeID: "runtime-1", Keys: map[string]ed25519.PublicKey{"key-v2-renew": signer.PublicKey()}, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	claims := testGrant()
	claims.Version = llm.ExternalGrantVersionAgentBound
	claims.AgentID = "agent-a"
	claims.IssuedAt = now.Add(-20 * time.Minute)
	claims.ExpiresAt = now.Add(-15 * time.Minute)
	claims.Lease.ExpiresAt = now.Add(-10 * time.Minute)
	expired, err := signer.Sign(claims)
	if err != nil {
		t.Fatal(err)
	}
	req := llm.CompleteRequest{Model: "model-fast"}
	if err := verifier.VerifyRenewalPredecessor(agentBoundTestContext(t, "org-a", "agent-a"), expired, req); err != nil {
		t.Fatalf("matching v2 renewal=%v", err)
	}
	if err := verifier.VerifyRenewalPredecessor(agentBoundTestContext(t, "org-a", "agent-b"), expired, req); !errors.Is(err, llm.ErrExternalGrantInvalid) {
		t.Fatalf("cross-agent renewal=%v, want invalid", err)
	}
}

func agentBoundTestContext(t *testing.T, organization, agentID string) context.Context {
	t.Helper()
	return tools.WithEffectiveAgentConfig(testContext(t, organization), agentID)
}

func signedTestGrant(t *testing.T) llm.ExternalGrant {
	t.Helper()
	signer, err := NewSigner("key-1", "harbor-runtime", nil, testClock)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := signer.Sign(testGrant())
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

func receiptForGrant(t *testing.T, grant llm.ExternalGrant, step int) llm.AttemptUsageReceipt {
	t.Helper()
	ctx := context.Background()
	if step > 0 {
		ctx = llm.WithAttemptStep(ctx, step)
	}
	_, scope, err := llm.EnsureGrantAttemptScope(ctx, grant)
	if err != nil {
		t.Fatal(err)
	}
	now := testClock()
	receipt := llm.AttemptUsageReceipt{
		ReceiptID:                    llm.CanonicalAttemptID(grant.GrantID, scope.LogicalCallID, scope.AttemptNonce, 1, 0, 0, 0),
		GrantID:                      grant.GrantID,
		RouteMode:                    grant.RouteMode,
		LogicalCallID:                scope.LogicalCallID,
		AttemptNonce:                 scope.AttemptNonce,
		ParentLogicalCallID:          scope.ParentLogicalCallID,
		ParentAttemptNonce:           scope.ParentAttemptNonce,
		PlannerStep:                  scope.PlannerStep,
		OrganizationID:               grant.OrganizationID,
		RuntimeID:                    grant.RuntimeID,
		AgentID:                      grant.AgentID,
		TenantID:                     grant.TenantID,
		UserID:                       grant.UserID,
		SessionID:                    grant.SessionID,
		LogicalRunID:                 grant.LogicalRunID,
		Provider:                     grant.Provider,
		ProviderModelID:              grant.ProviderModelID,
		ProviderConnectionID:         grant.ProviderConnectionID,
		ProviderConnectionGeneration: grant.ProviderConnectionGeneration,
		RouteID:                      grant.RouteID,
		CredentialAssetGeneration:    grant.CredentialAssetGeneration,
		PolicyGeneration:             grant.PolicyGeneration,
		AttemptNumber:                1,
		RequestedReasoning:           grant.MaxReasoning,
		EffectiveReasoning:           grant.MaxReasoning,
		Status:                       "success",
		StartedAt:                    now,
		CompletedAt:                  now.Add(time.Second),
	}
	if llm.EffectiveExternalGrantRouteMode(grant.RouteMode) == llm.ExternalGrantRouteRuntimeDefault {
		receipt.Provider = "mock"
		receipt.ProviderModelID = "model-fast"
	}
	receipt.IdempotencyKey = receipt.ReceiptID
	receipt.CanonicalBodyHash, err = llm.CanonicalAttemptUsageReceiptBodyHash(receipt)
	if err != nil {
		t.Fatal(err)
	}
	return receipt
}

func TestRuntimeDefaultGrantShapeAndReceiptDerivation(t *testing.T) {
	signer, err := NewSigner("key-default", "harbor-runtime", nil, testClock)
	if err != nil {
		t.Fatal(err)
	}
	base := testGrant()
	base.RouteMode = llm.ExternalGrantRouteRuntimeDefault
	base.Provider = ""
	base.ProviderModelID = ""
	base.ProviderConnectionID = ""
	base.ProviderConnectionGeneration = 0
	base.RouteID = ""
	base.CredentialBindingHandle = ""
	base.CredentialAssetGeneration = 0
	signed, err := signer.Sign(base)
	if err != nil {
		t.Fatalf("runtime-default sign: %v", err)
	}
	wireBytes, err := json.Marshal(signed)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(wireBytes, &wire); err != nil {
		t.Fatal(err)
	}
	for _, claim := range []string{"provider", "provider_model_id", "provider_connection_id", "provider_connection_generation", "route_id", "credential_binding_handle", "credential_asset_generation"} {
		if _, present := wire[claim]; present {
			t.Fatalf("runtime-default wire carries coordinator claim %q: %s", claim, wireBytes)
		}
	}
	verifier, err := NewVerifier(VerifierConfig{
		Audience: "harbor-runtime", RuntimeID: "runtime-1", RouteMode: llm.ExternalGrantRouteRuntimeDefault,
		Keys: map[string]ed25519.PublicKey{"key-default": signer.PublicKey()}, Clock: testClock,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := verifier.Verify(testContext(t, "org-a"), signed, llm.CompleteRequest{Model: "model-fast"}); err != nil {
		t.Fatalf("runtime-default verify: %v", err)
	}
	mixed := base
	mixed.Provider = "openai"
	if _, err := signer.Sign(mixed); !errors.Is(err, ErrInvalidGrantShape) {
		t.Fatalf("runtime-default mixed route = %v, want ErrInvalidGrantShape", err)
	}

	root := receiptForGrant(t, signed, 0)
	if err := llm.ValidateAttemptUsageReceiptAgainstGrant(root, signed); err != nil {
		t.Fatalf("root receipt validation: %v", err)
	}
	child := receiptForGrant(t, signed, 2)
	if err := llm.ValidateAttemptUsageReceiptAgainstGrant(child, signed); err != nil {
		t.Fatalf("planner child receipt validation: %v", err)
	}
	forged := child
	forged.AttemptNonce = signed.AttemptNonce
	forged.ReceiptID = llm.CanonicalAttemptID(signed.GrantID, forged.LogicalCallID, forged.AttemptNonce, forged.AttemptNumber, forged.RetryNumber, forged.DowngradeNumber, forged.FallbackHop)
	forged.IdempotencyKey = forged.ReceiptID
	forged.CanonicalBodyHash, err = llm.CanonicalAttemptUsageReceiptBodyHash(forged)
	if err != nil {
		t.Fatal(err)
	}
	if err := llm.ValidateAttemptUsageReceiptAgainstGrant(forged, signed); !errors.Is(err, llm.ErrInvalidUsageReceipt) {
		t.Fatalf("forged planner child = %v, want ErrInvalidUsageReceipt", err)
	}
	forgedRoot := root
	forgedRoot.LogicalCallID = "forged-root"
	forgedRoot.ReceiptID = llm.CanonicalAttemptID(signed.GrantID, forgedRoot.LogicalCallID, forgedRoot.AttemptNonce, forgedRoot.AttemptNumber, forgedRoot.RetryNumber, forgedRoot.DowngradeNumber, forgedRoot.FallbackHop)
	forgedRoot.IdempotencyKey = forgedRoot.ReceiptID
	forgedRoot.CanonicalBodyHash, err = llm.CanonicalAttemptUsageReceiptBodyHash(forgedRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := llm.ValidateAttemptUsageReceiptAgainstGrant(forgedRoot, signed); !errors.Is(err, llm.ErrInvalidUsageReceipt) {
		t.Fatalf("forged root receipt = %v, want ErrInvalidUsageReceipt", err)
	}
}

func TestVerifierAcceptsLegacyV1300CoordinatorBoundSignature(t *testing.T) {
	signer, err := NewSigner("key-legacy", "harbor-runtime", nil, testClock)
	if err != nil {
		t.Fatal(err)
	}
	legacy := testGrant()
	legacy.KeyID = "key-legacy"
	legacy.Audience = "harbor-runtime"
	legacy.LogicalCallID = derivedIdentity("call", legacy.GrantID, legacy.LogicalRunID)
	legacy.AttemptNonce = derivedIdentity("nonce", legacy.GrantID, legacy.LogicalRunID)
	// v1.30.0 signed this exact document before route_mode existed. The
	// omitempty compatibility path must preserve those signature bytes.
	document, err := canonicalDocument(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(document, []byte(`"agent_id"`)) {
		t.Fatalf("legacy canonical document gained agent_id: %s", document)
	}
	legacy.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(signer.private, document))
	verifier, err := NewVerifier(VerifierConfig{
		Audience: "harbor-runtime", RuntimeID: "runtime-1",
		Keys: map[string]ed25519.PublicKey{"key-legacy": signer.PublicKey()}, Clock: testClock,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := verifier.Verify(testContext(t, "org-a"), legacy, llm.CompleteRequest{Model: "model-fast"}); err != nil {
		t.Fatalf("verify v1.30.0 coordinator-bound signature: %v", err)
	}
}

func TestSignerVerifier_BindsVerifiedContextAndRoute(t *testing.T) {
	signer, err := NewSigner("key-1", "harbor-runtime", nil, testClock)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewVerifier(VerifierConfig{
		Audience: "harbor-runtime", RuntimeID: "runtime-1", Keys: map[string]ed25519.PublicKey{"key-1": signer.PublicKey()}, Clock: testClock,
	})
	if err != nil {
		t.Fatal(err)
	}
	signed, err := signer.Sign(testGrant())
	if err != nil {
		t.Fatal(err)
	}
	ctx := testContext(t, "org-a")
	if err := verifier.Verify(ctx, signed, llm.CompleteRequest{Model: "model-fast", ReasoningEffort: llm.ReasoningLow}); err != nil {
		t.Fatalf("Verify valid grant: %v", err)
	}
	if err := verifier.Verify(testContext(t, "org-b"), signed, llm.CompleteRequest{Model: "model-fast"}); !errors.Is(err, llm.ErrExternalGrantInvalid) {
		t.Fatalf("Verify wrong org = %v, want ErrExternalGrantInvalid", err)
	}
	wrongModel := signed
	wrongModel.ProviderModelID = "model-other"
	if err := verifier.Verify(ctx, wrongModel, llm.CompleteRequest{Model: "model-fast"}); !errors.Is(err, llm.ErrExternalGrantSignature) {
		t.Fatalf("Verify tampered model = %v, want signature failure", err)
	}
}

func TestSignerVerifier_V2RequiresExactReachAdmittedAgentForBothRoutes(t *testing.T) {
	signer, err := NewSigner("key-v2", "harbor-runtime", nil, testClock)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewVerifier(VerifierConfig{
		Audience: "harbor-runtime", RuntimeID: "runtime-1",
		Keys: map[string]ed25519.PublicKey{"key-v2": signer.PublicKey()}, Clock: testClock,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []llm.ExternalGrantRouteMode{llm.ExternalGrantRouteCoordinatorBound, llm.ExternalGrantRouteRuntimeDefault} {
		t.Run(string(mode), func(t *testing.T) {
			claims := testGrant()
			claims.Version = llm.ExternalGrantVersionAgentBound
			claims.AgentID = "agent-a"
			claims.RouteMode = mode
			if mode == llm.ExternalGrantRouteRuntimeDefault {
				claims.Provider, claims.ProviderModelID, claims.ProviderConnectionID = "", "", ""
				claims.ProviderConnectionGeneration, claims.RouteID = 0, ""
				claims.CredentialBindingHandle, claims.CredentialAssetGeneration = "", 0
			}
			signed, signErr := signer.Sign(claims)
			if signErr != nil {
				t.Fatal(signErr)
			}
			request := llm.CompleteRequest{Model: "model-fast"}
			if err := verifier.Verify(agentBoundTestContext(t, "org-a", "agent-a"), signed, request); err != nil {
				t.Fatalf("matching admitted agent: %v", err)
			}
			receipt := receiptForGrant(t, signed, 1)
			if err := llm.ValidateAttemptUsageReceiptAgainstGrant(receipt, signed); err != nil {
				t.Fatalf("matching agent-bound receipt: %v", err)
			}
			wrongReceipt := receipt
			wrongReceipt.AgentID = "agent-b"
			wrongReceipt.CanonicalBodyHash, signErr = llm.CanonicalAttemptUsageReceiptBodyHash(wrongReceipt)
			if signErr != nil {
				t.Fatal(signErr)
			}
			if err := llm.ValidateAttemptUsageReceiptAgainstGrant(wrongReceipt, signed); !errors.Is(err, llm.ErrInvalidUsageReceipt) {
				t.Fatalf("mismatched agent-bound receipt = %v, want ErrInvalidUsageReceipt", err)
			}
			for name, ctx := range map[string]context.Context{
				"missing":  testContext(t, "org-a"),
				"mismatch": agentBoundTestContext(t, "org-a", "agent-b"),
			} {
				if err := verifier.Verify(ctx, signed, request); !errors.Is(err, llm.ErrExternalGrantInvalid) {
					t.Fatalf("%s agent context = %v, want ErrExternalGrantInvalid", name, err)
				}
			}
			tampered := signed
			tampered.AgentID = "agent-b"
			if err := verifier.Verify(agentBoundTestContext(t, "org-a", "agent-b"), tampered, request); !errors.Is(err, llm.ErrExternalGrantSignature) {
				t.Fatalf("tampered signed agent = %v, want signature failure", err)
			}
		})
	}

	v1Pretender := testGrant()
	v1Pretender.AgentID = "agent-a"
	if _, err := signer.Sign(v1Pretender); !errors.Is(err, ErrInvalidGrantShape) {
		t.Fatalf("v1 grant pretending signed AgentID = %v, want ErrInvalidGrantShape", err)
	}
	v2Missing := testGrant()
	v2Missing.Version = llm.ExternalGrantVersionAgentBound
	if _, err := signer.Sign(v2Missing); !errors.Is(err, ErrInvalidGrantShape) {
		t.Fatalf("v2 grant missing AgentID = %v, want ErrInvalidGrantShape", err)
	}
}

func TestVerifier_V2ConcurrentAgentsDoNotBleed(t *testing.T) {
	signer, err := NewSigner("key-v2", "harbor-runtime", nil, testClock)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewVerifier(VerifierConfig{Audience: "harbor-runtime", RuntimeID: "runtime-1", Keys: map[string]ed25519.PublicKey{"key-v2": signer.PublicKey()}, Clock: testClock})
	if err != nil {
		t.Fatal(err)
	}
	grants := make(map[string]llm.ExternalGrant, 2)
	for _, agentID := range []string{"agent-a", "agent-b"} {
		claims := testGrant()
		claims.Version, claims.AgentID = llm.ExternalGrantVersionAgentBound, agentID
		claims.GrantID, claims.Lease.LeaseID = "grant-"+agentID, "lease-"+agentID
		claims.LogicalCallID, claims.AttemptNonce = "", ""
		grants[agentID], err = signer.Sign(claims)
		if err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	errs := make(chan error, 100)
	contexts := map[string]context.Context{
		"agent-a": agentBoundTestContext(t, "org-a", "agent-a"),
		"agent-b": agentBoundTestContext(t, "org-a", "agent-b"),
	}
	for i := range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			agentID := []string{"agent-a", "agent-b"}[i%2]
			errs <- verifier.Verify(contexts[agentID], grants[agentID], llm.CompleteRequest{Model: "model-fast"})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestVerifier_UsesAuthorizedOrganizationAllowlistWhenContextHasNoOrganization(t *testing.T) {
	signer, err := NewSigner("key-1", "harbor-runtime", nil, testClock)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewVerifier(VerifierConfig{
		Audience: "harbor-runtime", RuntimeID: "runtime-1", AuthorizedOrganizations: []string{"org-a"},
		Keys: map[string]ed25519.PublicKey{"key-1": signer.PublicKey()}, Clock: testClock,
	})
	if err != nil {
		t.Fatal(err)
	}
	signed, err := signer.Sign(testGrant())
	if err != nil {
		t.Fatal(err)
	}
	id := identity.Identity{TenantID: "tenant-a", UserID: "user-a", SessionID: "session-a"}
	ctx, err := identity.WithVerified(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	ctx, err = identity.WithRun(ctx, id, "run-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := verifier.Verify(ctx, signed, llm.CompleteRequest{Model: "model-fast"}); err != nil {
		t.Fatalf("authorized organization allowlist should authorize matching grant: %v", err)
	}
	wrong := signed
	wrong.OrganizationID = "org-b"
	if err := verifier.Verify(ctx, wrong, llm.CompleteRequest{Model: "model-fast"}); !errors.Is(err, llm.ErrExternalGrantSignature) {
		// The body was changed without resigning; this must fail before any
		// configured organization comparison can be bypassed.
		t.Fatalf("tampered organization = %v, want signature failure", err)
	}
}

func TestVerifier_AllowsTwoSignedOrganizationsOnOneRuntime(t *testing.T) {
	signer, err := NewSigner("key-1", "harbor-runtime", nil, testClock)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewVerifier(VerifierConfig{
		Audience: "harbor-runtime", RuntimeID: "runtime-1",
		Keys: map[string]ed25519.PublicKey{"key-1": signer.PublicKey()}, Clock: testClock,
	})
	if err != nil {
		t.Fatal(err)
	}
	base := testGrant()
	grantA, err := signer.Sign(base)
	if err != nil {
		t.Fatal(err)
	}
	grantB := base
	grantB.GrantID = "grant-b"
	grantB.OrganizationID = "org-b"
	grantB.ProviderConnectionID = "connection-b"
	grantB.CredentialBindingHandle = "binding-b"
	grantB.Lease.LeaseID = "lease-b"
	grantB.LogicalCallID = ""
	grantB.AttemptNonce = ""
	grantB, err = signer.Sign(grantB)
	if err != nil {
		t.Fatal(err)
	}
	id := identity.Identity{TenantID: "tenant-a", UserID: "user-a", SessionID: "session-a"}
	ctx, err := identity.WithVerified(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	ctx, err = identity.WithRun(ctx, id, "run-a")
	if err != nil {
		t.Fatal(err)
	}
	for _, grant := range []llm.ExternalGrant{grantA, grantB} {
		if err := verifier.Verify(ctx, grant, llm.CompleteRequest{Model: "model-fast"}); err != nil {
			t.Fatalf("signed organization %q on shared runtime: %v", grant.OrganizationID, err)
		}
	}
}

func TestBindingStore_RotationRevocationAndOrganizationFence(t *testing.T) {
	store := NewBindingStore()
	binding := Binding{Handle: "binding-a", OrganizationID: "org-a", RuntimeID: "runtime-1", Provider: "openai", ProviderConnectionID: "connection-a", ProviderConnectionGeneration: 1, Generation: 1, Secret: "credential-a"}
	if err := store.Put(binding); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(binding); err != nil {
		t.Fatalf("idempotent binding put: %v", err)
	}
	if err := store.Put(Binding{Handle: "binding-a", OrganizationID: "org-a", RuntimeID: "runtime-1", Provider: "openai", ProviderConnectionID: "connection-a", ProviderConnectionGeneration: 1, Generation: 1, Secret: "different"}); !errors.Is(err, ErrInvalidGrantShape) {
		t.Fatalf("same-generation replacement = %v, want ErrInvalidGrantShape", err)
	}
	if err := store.Put(Binding{Handle: "binding-a", OrganizationID: "org-a", RuntimeID: "runtime-1", Provider: "openai", ProviderConnectionID: "connection-a", ProviderConnectionGeneration: 1, Generation: 0, Secret: "older"}); !errors.Is(err, ErrInvalidGrantShape) {
		t.Fatalf("backward generation = %v, want ErrInvalidGrantShape", err)
	}
	signed := signedTestGrant(t)
	ctx := testContext(t, "org-a")
	resolved, err := store.Resolve(ctx, signed)
	if err != nil || resolved.Secret != "credential-a" {
		t.Fatalf("Resolve = %+v, %v", resolved, err)
	}
	if err := store.Rotate("binding-a", "credential-a-rotated", 2); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Resolve(ctx, signed); !errors.Is(err, llm.ErrExternalGrantRevoked) {
		t.Fatalf("stale generation = %v, want ErrExternalGrantRevoked", err)
	}
	rotated := signed
	rotated.CredentialAssetGeneration = 2
	if err := store.Revoke("binding-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Resolve(ctx, rotated); !errors.Is(err, llm.ErrExternalGrantRevoked) {
		t.Fatalf("revoked generation = %v, want ErrExternalGrantRevoked", err)
	}
	if _, err := store.Resolve(testContext(t, "org-b"), signed); !errors.Is(err, llm.ErrExternalGrantRevoked) {
		t.Fatalf("wrong org = %v, want ErrExternalGrantRevoked", err)
	}
}

func TestBindingStore_ConcurrentTwoOrganizationsNoBleed(t *testing.T) {
	store := NewBindingStore()
	for _, binding := range []Binding{
		{Handle: "binding-a", OrganizationID: "org-a", RuntimeID: "runtime-1", Provider: "openai", ProviderConnectionID: "connection-a", ProviderConnectionGeneration: 1, Generation: 1, Secret: "credential-a"},
		{Handle: "binding-b", OrganizationID: "org-b", RuntimeID: "runtime-1", Provider: "openai", ProviderConnectionID: "connection-b", ProviderConnectionGeneration: 1, Generation: 1, Secret: "credential-b"},
	} {
		if err := store.Put(binding); err != nil {
			t.Fatal(err)
		}
	}
	grantA := signedTestGrant(t)
	grantB := grantA
	grantB.GrantID, grantB.OrganizationID, grantB.CredentialBindingHandle, grantB.ProviderConnectionID = "grant-b", "org-b", "binding-b", "connection-b"
	signer, err := NewSigner("key-1", "harbor-runtime", nil, testClock)
	if err != nil {
		t.Fatal(err)
	}
	grantB, err = signer.Sign(grantB)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errCh := make(chan error, 200)
	for range 100 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			resolved, resolveErr := store.Resolve(testContext(t, "org-a"), grantA)
			if resolveErr != nil || resolved.Secret != "credential-a" {
				errCh <- errors.New("organization A credential crossed or failed")
			}
		}()
		go func() {
			defer wg.Done()
			resolved, resolveErr := store.Resolve(testContext(t, "org-b"), grantB)
			if resolveErr != nil || resolved.Secret != "credential-b" {
				errCh <- errors.New("organization B credential crossed or failed")
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
}
