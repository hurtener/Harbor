package grant

import (
	"context"
	"crypto/ed25519"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
)

// participantContext is a run in user-owner's session written by user-a
// (the grant's user) as a session participant.
func participantContext(t *testing.T, writer string) context.Context {
	t.Helper()
	partition := identity.Identity{TenantID: "tenant-a", UserID: "user-owner", SessionID: "session-a"}
	ctx, err := identity.WithVerified(context.Background(), partition)
	if err != nil {
		t.Fatal(err)
	}
	ctx, err = identity.With(ctx, partition)
	if err != nil {
		t.Fatal(err)
	}
	ctx, err = identity.WithRun(ctx, partition, "run-a")
	if err != nil {
		t.Fatal(err)
	}
	ctx, err = identity.WithParticipant(ctx, identity.Participant{UserID: writer})
	if err != nil {
		t.Fatal(err)
	}
	return llm.WithVerifiedOrganization(ctx, "org-a")
}

// A participant's turn verifies and resolves only a grant issued to the
// participant: a grant naming the partition owner (or anyone else) fails.
func TestGrant_ParticipantMustCarryWritersGrant(t *testing.T) {
	signer, err := NewSigner("key-1", "harbor-runtime", nil, testClock)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewVerifier(VerifierConfig{Audience: "harbor-runtime", RuntimeID: "runtime-1", Keys: map[string]ed25519.PublicKey{"key-1": signer.PublicKey()}, Clock: testClock})
	if err != nil {
		t.Fatal(err)
	}
	writers, err := signer.Sign(testGrant()) // UserID user-a
	if err != nil {
		t.Fatal(err)
	}
	ownerGrant := testGrant()
	ownerGrant.UserID = "user-owner"
	owners, err := signer.Sign(ownerGrant)
	if err != nil {
		t.Fatal(err)
	}
	req := llm.CompleteRequest{Model: "model-fast"}
	if err := verifier.Verify(participantContext(t, "user-a"), writers, req); err != nil {
		t.Fatalf("writer's grant on the writer's turn: %v", err)
	}
	if err := verifier.Verify(participantContext(t, "user-a"), owners, req); !errors.Is(err, llm.ErrExternalGrantInvalid) {
		t.Fatalf("owner's grant on a participant's turn = %v, want invalid", err)
	}
	if err := verifier.Verify(participantContext(t, "user-other"), writers, req); !errors.Is(err, llm.ErrExternalGrantInvalid) {
		t.Fatalf("another writer's grant = %v, want invalid", err)
	}

	store := NewBindingStore()
	if err := store.Put(Binding{Handle: "binding-a", OrganizationID: "org-a", RuntimeID: "runtime-1", Provider: "openai", ProviderConnectionID: "connection-a", ProviderConnectionGeneration: 1, Generation: 1, Secret: "credential-a"}); err != nil {
		t.Fatal(err)
	}
	signed := signedTestGrant(t)
	if _, err := store.Resolve(participantContext(t, signed.UserID), signed); err != nil {
		t.Fatalf("resolve writer's grant: %v", err)
	}
	if _, err := store.Resolve(participantContext(t, "user-other"), signed); !errors.Is(err, llm.ErrExternalGrantRevoked) {
		t.Fatalf("resolve on another writer's turn = %v", err)
	}
}
