package grant

import (
	"context"
	"crypto/ed25519"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
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
