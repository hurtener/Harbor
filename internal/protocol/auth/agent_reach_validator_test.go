package auth_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/protocol/auth"
)

func TestValidator_AgentReach_StrictClaimAndVerifiedAuthority(t *testing.T) {
	v, priv := newRSValidator(t, fixedNow)
	for _, tc := range []struct {
		name  string
		reach any
		want  []string
		bad   bool
	}{
		{name: "absent"},
		{name: "empty", reach: []string{}, want: []string{}},
		{name: "allowed", reach: []string{"agent-a"}, want: []string{"agent-a"}},
		{name: "duplicate", reach: []string{"agent-a", "agent-a"}, bad: true},
		{name: "wrong shape", reach: "agent-a", bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims := validClaims(fixedNow)
			if tc.reach != nil {
				claims[auth.AgentReachClaim] = tc.reach
			}
			verified, err := v.Validate(context.Background(), signRS256(t, priv, claims, "k1"))
			if tc.bad {
				if !errors.Is(err, auth.ErrAgentReachMalformed) {
					t.Fatalf("Validate() error = %v, want ErrAgentReachMalformed", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
			if len(verified.AgentReach) != len(tc.want) {
				t.Fatalf("Verified.AgentReach = %#v, want %#v", verified.AgentReach, tc.want)
			}
		})
	}
}
