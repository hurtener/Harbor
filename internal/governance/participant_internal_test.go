package governance

import (
	"context"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
)

// Per-user tiers, budgets and rate limits key on the ACTING user (a session
// participant's own), while events go back to the session partition.
func TestGovernanceIdentity_ParticipantKeysOnWriter(t *testing.T) {
	partition := identity.Identity{TenantID: "t", UserID: "owner", SessionID: "s"}
	ctx, err := identity.With(context.Background(), partition)
	if err != nil {
		t.Fatal(err)
	}
	ctx, err = identity.WithRun(ctx, partition, "run")
	if err != nil {
		t.Fatal(err)
	}
	if id, _ := identityFromCtx(ctx); id != partition {
		t.Fatalf("ordinary identity = %+v", id)
	}
	ctx, err = identity.WithParticipant(ctx, identity.Participant{UserID: "writer"})
	if err != nil {
		t.Fatal(err)
	}
	id, err := identityFromCtx(ctx)
	if err != nil || id.UserID != "writer" || id.SessionID != "s" {
		t.Fatalf("participant identity = %+v, %v", id, err)
	}
	q, err := quadrupleFromCtx(ctx)
	if err != nil || q.UserID != "writer" || q.RunID != "run" {
		t.Fatalf("participant quadruple = %+v, %v", q, err)
	}
	if ev := eventQuad(ctx, q); ev.UserID != "owner" || ev.RunID != "run" {
		t.Fatalf("event quadruple = %+v, want the partition", ev)
	}
}
