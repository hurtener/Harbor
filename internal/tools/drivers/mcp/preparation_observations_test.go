package mcp

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/tools/auth"
)

func TestPreparationObservations_DoNotContaminatePriorAndTransferToExactStage(t *testing.T) {
	ctx := context.Background()
	reg := NewRegistry()
	owner := auth.Owner{Tenant: "tenant", Agent: "agent"}
	old := &stubProvider{id: "same"}
	if err := reg.Register(ctx, ServerRegistration{Provider: old, Transport: "http+sse", Owner: owner}); err != nil {
		t.Fatalf("seed old registration: %v", err)
	}
	oldChallenge := AuthChallenge{Scheme: "Bearer", ResourceMetadataURL: "https://old.invalid/metadata", Raw: "old"}
	reg.RecordAuthChallenge("same", oldChallenge)

	observations := &preparationObservations{}
	newChallenge := AuthChallenge{Scheme: "Bearer", ResourceMetadataURL: "https://new.invalid/metadata", Raw: "Bearer secret-looking-server-text"}
	observations.recordChallenge(newChallenge)
	before, _, _, err := reg.OAuthDiscoveryTarget("same")
	if err != nil || before == nil || before.ResourceMetadataURL != oldChallenge.ResourceMetadataURL {
		t.Fatalf("private observation contaminated prior: challenge=%+v err=%v", before, err)
	}
	authErr := observations.authRequired()
	var typed *PreparationAuthRequiredError
	if !errors.As(authErr, &typed) || typed.Challenge.ResourceMetadataURL != newChallenge.ResourceMetadataURL {
		t.Fatalf("typed auth error = %#v", authErr)
	}
	if typed.Challenge.Raw != "" {
		t.Fatalf("typed auth challenge retained untrusted raw header: %q", typed.Challenge.Raw)
	}
	if strings.Contains(authErr.Error(), newChallenge.Raw) {
		t.Fatalf("typed auth error leaked raw challenge: %q", authErr)
	}

	staged := &stubProvider{id: "same"}
	swap, err := reg.StageRegistration(ServerRegistration{Provider: staged, Transport: "http+sse", Owner: owner}, nil)
	if err != nil {
		t.Fatalf("StageRegistration: %v", err)
	}
	observations.transfer(swap)
	during, _, _, err := reg.OAuthDiscoveryTarget("same")
	if err != nil || during == nil || during.ResourceMetadataURL != oldChallenge.ResourceMetadataURL {
		t.Fatalf("private staged observation leaked through prior live entry: challenge=%+v err=%v", during, err)
	}
	if err := swap.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	after, _, _, err := reg.OAuthDiscoveryTarget("same")
	if err != nil || after == nil || after.ResourceMetadataURL != oldChallenge.ResourceMetadataURL {
		t.Fatalf("rollback did not restore uncontaminated prior: challenge=%+v err=%v", after, err)
	}
}

func TestClosePreparedAfterFailure_JoinsCleanupError(t *testing.T) {
	cause := errors.New("activation failed")
	cleanup := errors.New("close failed")
	prepared := &PreparedAttachment{closeFn: func(context.Context) error { return cleanup }}
	err := closePreparedAfterFailure(context.Background(), prepared, cause)
	if !errors.Is(err, cause) || !errors.Is(err, cleanup) {
		t.Fatalf("joined error = %v, want activation and cleanup causes", err)
	}
}
