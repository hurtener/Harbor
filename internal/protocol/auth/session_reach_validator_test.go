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
// strictly (duplicate / wrong-shape / signed-null values reject
// authentication), an ABSENT claim key yields a nil reach (D-171
// dynamic selection preserved), and an explicitly empty claim yields a
// present empty reach (grants no session, enforced at the middleware).
// A signed null is a PRESENT claim and must reject — never be mistaken
// for absence.
func TestValidator_SessionReach_StrictClaimAndVerifiedAuthority(t *testing.T) {
	v, priv := newRSValidator(t, fixedNow)
	tooLong := strings.Repeat("a", 129) // maxSessionReachIDBytes is 128
	for _, tc := range []struct {
		name    string
		present bool
		reach   any
		want    []string
		bad     bool
	}{
		{name: "absent"},
		{name: "signed null", present: true, reach: nil, bad: true},
		{name: "empty", present: true, reach: []string{}, want: []string{}},
		{name: "allowed", present: true, reach: []string{"sess-a"}, want: []string{"sess-a"}},
		{name: "duplicate", present: true, reach: []string{"sess-a", "sess-a"}, bad: true},
		{name: "blank", present: true, reach: []string{" "}, bad: true},
		{name: "oversize", present: true, reach: []string{tooLong}, bad: true},
		{name: "wrong shape", present: true, reach: "sess-a", bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims := validClaims(fixedNow)
			if tc.present {
				// Assigning nil seats the key with a JSON-null value —
				// the signed-null shape the claim must reject.
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
			if !tc.present && verified.SessionReach != nil {
				t.Fatalf("absent claim must yield a nil SessionReach, got %#v", verified.SessionReach)
			}
		})
	}
}
