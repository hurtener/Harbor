package mcp

import (
	"context"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
)

// A connect started from a participant's run keeps acting as that writer on
// every reconnect.
func TestPreparationBase_CarriesParticipant(t *testing.T) {
	partition := identity.Identity{TenantID: "t", UserID: "owner", SessionID: "s"}
	ctx, err := identity.With(context.Background(), partition)
	if err != nil {
		t.Fatal(err)
	}
	ctx, err = identity.WithParticipant(ctx, identity.Participant{UserID: "writer"})
	if err != nil {
		t.Fatal(err)
	}
	acting, got, ok := identity.ActingFrom(preparationBase(ctx))
	if !ok || acting.UserID != "writer" || got != partition {
		t.Fatalf("preparation base acting=%+v partition=%+v", acting, got)
	}
	meta, err := buildIdentityMeta(preparationBase(ctx), nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, v := range meta {
		if v == "writer" {
			found = true
		}
		if v == "owner" {
			t.Fatalf("identity stamp carries the partition owner: %v", meta)
		}
	}
	if !found {
		t.Fatalf("identity stamp lacks the writer: %v", meta)
	}
}
