package auth_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/protocol/auth"
)

// TestValidator_SessionReach_StrictClaimAndVerifiedAuthority pins the
// validator-side contract: the optional session_reach claim parses
// strictly (duplicate / wrong-shape values reject authentication), an
// absent claim yields a nil reach (D-171 dynamic selection preserved),
// and an explicitly empty claim yields a present empty reach (grants no
// session, enforced at the middleware).
func TestValidator_SessionReach_StrictClaimAndVerifiedAuthority(t *testing.T) {
	v, priv := newRSValidator(t, fixedNow)
	tooLong := strings.Repeat("a", 129) // maxSessionReachIDBytes is 128
	for _, tc := range []struct {
		name  string
		reach any
		want  []string
		bad   bool
	}{
		{name: "absent"},
		{name: "empty", reach: []string{}, want: []string{}},
		{name: "allowed", reach: []string{"sess-a"}, want: []string{"sess-a"}},
		{name: "duplicate", reach: []string{"sess-a", "sess-a"}, bad: true},
		{name: "blank", reach: []string{" "}, bad: true},
		{name: "oversize", reach: []string{tooLong}, bad: true},
		{name: "wrong shape", reach: "sess-a", bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims := validClaims(fixedNow)
			if tc.reach != nil {
				claims[auth.SessionReachClaim] = tc.reach
			}
			verified, err := v.Validate(context.Background(), signRS256(t, priv, claims, "k1"))
			if tc.bad {
				if !errors.Is(err, auth.ErrSessionReachMalformed) {
					t.Fatalf("Validate() error = %v, want ErrSessionReachMalformed", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
			if len(verified.SessionReach) != len(tc.want) {
				t.Fatalf("Verified.SessionReach = %#v, want %#v", verified.SessionReach, tc.want)
			}
			for i := range verified.SessionReach {
				if verified.SessionReach[i] != tc.want[i] {
					t.Fatalf("Verified.SessionReach = %#v, want %#v", verified.SessionReach, tc.want)
				}
			}
			// Absence must stay nil, not empty — the middleware branches
			// on nil-vs-present to preserve D-171 dynamic selection.
			if tc.reach == nil && verified.SessionReach != nil {
				t.Fatalf("absent claim must yield a nil SessionReach, got %#v", verified.SessionReach)
			}
		})
	}
}
