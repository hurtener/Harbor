package protocol

import (
	"context"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
)

// The decider attributed to a control is the verified ACTING principal:
// a participant deciding inside another user's session is named as
// themselves (with their signed label), never as the partition owner.
func TestVerifiedDecider_NamesActingParticipant(t *testing.T) {
	owner := identity.Identity{TenantID: "t", UserID: "owner", SessionID: "s"}
	ctx, err := identity.WithVerified(context.Background(), owner)
	if err != nil {
		t.Fatal(err)
	}
	if got := verifiedDecider(ctx, owner); got != (pauseresume.Decider{TenantID: "t", UserID: "owner", SessionID: "s"}) {
		t.Fatalf("owner decider = %+v", got)
	}
	pctx, err := identity.WithParticipant(ctx, identity.Participant{UserID: "guest", Speaker: "Guest G"})
	if err != nil {
		t.Fatal(err)
	}
	want := pauseresume.Decider{TenantID: "t", UserID: "guest", SessionID: "s", Speaker: "Guest G"}
	if got := verifiedDecider(pctx, owner); got != want {
		t.Fatalf("participant decider = %+v, want %+v", got, want)
	}
}

// An impersonating admin's decision names the admin (the verified
// identity), never the impersonated target the working identity carries.
func TestVerifiedDecider_ImpersonationNamesAdmin(t *testing.T) {
	admin := identity.Identity{TenantID: "t", UserID: "admin", SessionID: "admin-s"}
	ctx, err := identity.WithVerified(context.Background(), admin)
	if err != nil {
		t.Fatal(err)
	}
	target := identity.Identity{TenantID: "t", UserID: "target", SessionID: "target-s"}
	ctx, err = identity.WithElevated(ctx, target, "test impersonation")
	if err != nil {
		t.Fatal(err)
	}
	want := pauseresume.Decider{TenantID: "t", UserID: "admin", SessionID: "admin-s"}
	if got := verifiedDecider(ctx, target); got != want {
		t.Fatalf("impersonated decider = %+v, want %+v", got, want)
	}
}
