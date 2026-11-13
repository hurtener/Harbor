package a2a

import (
	"context"
	"net/http"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
)

func TestApplyIdentityHeader_ParticipantForwardsWriter(t *testing.T) {
	ctx, err := identity.With(context.Background(), identity.Identity{TenantID: "t", UserID: "owner", SessionID: "s"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, err = identity.WithParticipant(ctx, identity.Participant{UserID: "writer"})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, "http://example.invalid", nil)
	applyIdentityHeader(ctx, req)
	if got := req.Header.Get("X-Harbor-User"); got != "writer" {
		t.Fatalf("forwarded user = %q, want the writer", got)
	}
}
