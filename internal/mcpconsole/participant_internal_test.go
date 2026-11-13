package mcpconsole

import (
	"context"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools/auth"
)

// A participant sees its own user-scoped servers and never the partition
// owner's.
func TestVisibleRegistration_ParticipantUsesWriter(t *testing.T) {
	partition := identity.Identity{TenantID: "t", UserID: "owner", SessionID: "s"}
	ctx, err := identity.With(context.Background(), partition)
	if err != nil {
		t.Fatal(err)
	}
	ctx, err = identity.WithParticipant(ctx, identity.Participant{UserID: "writer"})
	if err != nil {
		t.Fatal(err)
	}
	a := &SourceAuthorizer{}
	ownerServer := auth.Owner{Tenant: "t", Agent: "agent", User: "owner"}
	if visible, err := a.VisibleRegistration(ctx, "src", ownerServer, "logical", ""); visible || err != nil {
		t.Fatalf("owner's server visible to participant: %v %v", visible, err)
	}
	writerServer := auth.Owner{Tenant: "t", Agent: "agent", User: "writer"}
	// Past the principal check the (absent) reach authority answers.
	if _, err := a.VisibleRegistration(ctx, "src", writerServer, "logical", ""); !errors.Is(err, ErrSourceAuthorityUnavailable) {
		t.Fatalf("writer's server did not pass the principal check: %v", err)
	}
}
