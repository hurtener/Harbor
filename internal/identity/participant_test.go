package identity_test

import (
	"context"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
)

func TestValidateSpeaker(t *testing.T) {
	for _, ok := range []string{"", "Ana", "Ana Pérez", "李雷", strings.Repeat("a", identity.MaxSpeakerBytes)} {
		if err := identity.ValidateSpeaker(ok); err != nil {
			t.Errorf("ValidateSpeaker(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{" Ana", "Ana ", "a\nb", "a\tb", "a\x00b", "\xff", strings.Repeat("a", identity.MaxSpeakerBytes+1)} {
		if err := identity.ValidateSpeaker(bad); err == nil {
			t.Errorf("ValidateSpeaker(%q) = nil, want error", bad)
		}
	}
}

func TestActingFrom(t *testing.T) {
	partition := identity.Identity{TenantID: "t", UserID: "owner", SessionID: "s"}
	ctx, err := identity.With(context.Background(), partition)
	if err != nil {
		t.Fatal(err)
	}
	acting, got, ok := identity.ActingFrom(ctx)
	if !ok || acting != partition || got != partition {
		t.Fatalf("no participant: acting %+v partition %+v", acting, got)
	}
	ctx, err = identity.WithParticipant(ctx, identity.Participant{UserID: "writer"})
	if err != nil {
		t.Fatal(err)
	}
	acting, got, _ = identity.ActingFrom(ctx)
	if acting.UserID != "writer" || acting.SessionID != "s" || got != partition {
		t.Fatalf("participant: acting %+v partition %+v", acting, got)
	}
	if p, _ := identity.ParticipantFrom(ctx); !p.IsGuest(partition) {
		t.Fatal("writer must be a guest of the owner's partition")
	}
	if _, err := identity.WithParticipant(context.Background(), identity.Participant{}); err == nil {
		t.Fatal("an empty participant must be refused")
	}
}
